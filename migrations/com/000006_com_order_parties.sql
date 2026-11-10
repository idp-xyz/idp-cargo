-- IDP-Cargo COM V0.6 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Decision C9 (order side) with C11-C14 of 2026-11-10: an order carries its requested transport mode and
-- movement type, and per role (shipper, consignee, notify party) the ORG party frozen with its identifiers, a
-- documentary address and, where the movement starts or ends at a door, a pickup or delivery address; a
-- contact is optional and stored apart. Every change is a new version; nothing is written back to ORG.
-- Orders recorded before V0.6 have neither transport nor parties; nothing is converted.

CREATE TABLE cargo_com.order_transport (
  tenant_id text NOT NULL,
  order_id text NOT NULL,
  version bigint NOT NULL CHECK (version > 0),
  transport_mode text CHECK (transport_mode IN ('AIR','SEA','ROAD','RAIL')),
  movement_type text CHECK (movement_type IN ('DOOR_TO_DOOR','DOOR_TO_PORT','PORT_TO_DOOR','PORT_TO_PORT')),
  reason text CHECK (char_length(btrim(reason)) BETWEEN 1 AND 500),
  changed_by text NOT NULL CHECK (length(btrim(changed_by)) > 0),
  changed_at timestamptz NOT NULL,
  entered_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, order_id, version),
  FOREIGN KEY (tenant_id, order_id) REFERENCES cargo_com.customer_order (tenant_id, order_id),
  CHECK (transport_mode IS NOT NULL OR movement_type IS NOT NULL),
  CHECK (version = 1 OR reason IS NOT NULL)
);

-- One row per version of an order party. Addresses are frozen as JSON documents with their source (ORG
-- address id, short code and version, or an override with who and why). Contacts are in order_party_contact.
CREATE TABLE cargo_com.order_party (
  tenant_id text NOT NULL,
  order_id text NOT NULL,
  role text NOT NULL CHECK (role IN ('SHIPPER','CONSIGNEE','NOTIFY_PARTY')),
  version bigint NOT NULL CHECK (version > 0),
  kind text NOT NULL CHECK (kind IN ('PARTY','TO_ORDER')),
  to_order_text text CHECK (to_order_text ~ '^TO ORDER( OF \S.*)?$' AND char_length(to_order_text) <= 500),
  party_id text,
  party_revision bigint CHECK (party_revision > 0),
  legal_name text CHECK (char_length(btrim(legal_name)) BETWEEN 1 AND 500),
  english_name text CHECK (english_name ~ '^[ -~]{1,200}$'),
  country_code char(2) CHECK (country_code ~ '^[A-Z]{2}$'),
  identifiers jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(identifiers) = 'array'),
  role_code text,
  role_valid_from timestamptz,
  role_valid_until timestamptz,
  role_evidence_ref text,
  address jsonb CHECK (jsonb_typeof(address) = 'object'),
  pickup jsonb CHECK (jsonb_typeof(pickup) = 'object'),
  delivery jsonb CHECK (jsonb_typeof(delivery) = 'object'),
  has_contact boolean NOT NULL,
  overridden boolean NOT NULL,
  reason text CHECK (char_length(btrim(reason)) BETWEEN 1 AND 500),
  set_by text NOT NULL CHECK (length(btrim(set_by)) > 0),
  set_at timestamptz NOT NULL,
  entered_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, order_id, role, version),
  FOREIGN KEY (tenant_id, order_id) REFERENCES cargo_com.customer_order (tenant_id, order_id),
  CHECK (version = 1 OR reason IS NOT NULL),
  CONSTRAINT order_party_to_order CHECK (CASE kind
    WHEN 'TO_ORDER' THEN role = 'CONSIGNEE' AND to_order_text IS NOT NULL AND party_id IS NULL AND address IS NULL AND NOT has_contact
    ELSE to_order_text IS NULL AND party_id IS NOT NULL AND party_revision IS NOT NULL AND legal_name IS NOT NULL
      AND country_code IS NOT NULL AND address IS NOT NULL END),
  CONSTRAINT order_party_role_confirmation CHECK (kind = 'TO_ORDER' OR CASE role
    WHEN 'NOTIFY_PARTY' THEN role_code IS NULL AND role_valid_from IS NULL AND role_evidence_ref IS NULL
    ELSE role_code = role AND role_valid_from IS NOT NULL AND role_evidence_ref IS NOT NULL END),
  CONSTRAINT order_party_door_addresses CHECK ((pickup IS NULL OR role = 'SHIPPER') AND (delivery IS NULL OR role = 'CONSIGNEE'))
);

-- Contacts frozen on the order: personal data, apart from the order (privacy assessment V1.1, controls 4-7).
-- The one change allowed is erasure, which clears every personal field at once (decision P1: after the
-- retention period; the erase command follows before the pilot).
CREATE TABLE cargo_com.order_party_contact (
  tenant_id text NOT NULL,
  order_id text NOT NULL,
  role text NOT NULL,
  version bigint NOT NULL,
  source text NOT NULL CHECK (source IN ('ORG','ORDER')),
  org_contact_id text,
  org_contact_version bigint CHECK (org_contact_version > 0),
  name text CHECK (char_length(name) BETWEEN 1 AND 200),
  phone text CHECK (char_length(phone) BETWEEN 1 AND 40),
  mobile text CHECK (char_length(mobile) BETWEEN 1 AND 40),
  email text CHECK (char_length(email) BETWEEN 1 AND 254),
  overridden_by text,
  override_reason text CHECK (char_length(btrim(override_reason)) BETWEEN 1 AND 500),
  erased boolean NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, order_id, role, version),
  FOREIGN KEY (tenant_id, order_id, role, version) REFERENCES cargo_com.order_party (tenant_id, order_id, role, version),
  CONSTRAINT order_party_contact_source CHECK (CASE source
    WHEN 'ORG' THEN org_contact_id IS NOT NULL AND org_contact_version IS NOT NULL AND overridden_by IS NULL AND override_reason IS NULL
    ELSE overridden_by IS NOT NULL AND override_reason IS NOT NULL END),
  CONSTRAINT order_party_contact_erased CHECK (CASE WHEN erased THEN name IS NULL AND phone IS NULL AND mobile IS NULL AND email IS NULL
    ELSE name IS NOT NULL AND (phone IS NOT NULL OR mobile IS NOT NULL OR email IS NOT NULL) END)
);

CREATE FUNCTION cargo_com.refuse_order_party_rewrite() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_com: % of order %/% cannot be %d', TG_TABLE_NAME, OLD.tenant_id, OLD.order_id, lower(TG_OP)
    USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER order_transport_immutable BEFORE UPDATE OR DELETE ON cargo_com.order_transport
  FOR EACH ROW EXECUTE FUNCTION cargo_com.refuse_order_party_rewrite();
CREATE TRIGGER order_party_immutable BEFORE UPDATE OR DELETE ON cargo_com.order_party
  FOR EACH ROW EXECUTE FUNCTION cargo_com.refuse_order_party_rewrite();

CREATE FUNCTION cargo_com.erase_order_contact_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.erased OR NOT NEW.erased
     OR NEW.name IS NOT NULL OR NEW.phone IS NOT NULL OR NEW.mobile IS NOT NULL OR NEW.email IS NOT NULL
     OR (NEW.tenant_id, NEW.order_id, NEW.role, NEW.version, NEW.source, NEW.org_contact_id, NEW.org_contact_version, NEW.overridden_by, NEW.override_reason)
        IS DISTINCT FROM (OLD.tenant_id, OLD.order_id, OLD.role, OLD.version, OLD.source, OLD.org_contact_id, OLD.org_contact_version, OLD.overridden_by, OLD.override_reason) THEN
    RAISE EXCEPTION 'cargo_com: the contact of % version % on order %/% can only be erased', OLD.role, OLD.version, OLD.tenant_id, OLD.order_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER order_party_contact_erasure_only BEFORE UPDATE OR DELETE ON cargo_com.order_party_contact
  FOR EACH ROW EXECUTE FUNCTION cargo_com.erase_order_contact_only();

-- At commit, every order a transaction touched keeps its party and transport versions numbered from 1, a
-- contact row exactly for the versions that have one, and the rules of C9, C11 and C14 on its current
-- versions: a TO ORDER consignee only on a SEA order and with a notify party; the shipper's pickup address and
-- the consignee's delivery address where the movement starts or ends at a door. Same rules as
-- domain.partyRules; deferred because COM writes the order row before its versions.
CREATE FUNCTION cargo_com.check_order_parties() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  t text := NEW.tenant_id;
  o text := NEW.order_id;
  mode text;
  movement text;
  bad text;
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_com.order_transport v WHERE v.tenant_id = t AND v.order_id = o
             HAVING count(*) <> max(v.version)) THEN
    bad := 'transport versions are not numbered from 1';
  END IF;
  IF bad IS NULL AND EXISTS (SELECT 1 FROM cargo_com.order_party p WHERE p.tenant_id = t AND p.order_id = o
             GROUP BY p.role HAVING count(*) <> max(p.version)) THEN
    bad := 'party versions are not numbered from 1';
  END IF;
  IF bad IS NULL AND EXISTS (SELECT 1 FROM cargo_com.order_party p
             LEFT JOIN cargo_com.order_party_contact c USING (tenant_id, order_id, role, version)
             WHERE p.tenant_id = t AND p.order_id = o AND p.has_contact <> (c.role IS NOT NULL)) THEN
    bad := 'contact rows do not match the party versions';
  END IF;
  SELECT v.transport_mode, v.movement_type INTO mode, movement FROM cargo_com.order_transport v
    WHERE v.tenant_id = t AND v.order_id = o ORDER BY v.version DESC LIMIT 1;
  IF bad IS NULL THEN
    WITH cur AS (SELECT DISTINCT ON (p.role) p.role, p.kind, p.pickup, p.delivery FROM cargo_com.order_party p
                 WHERE p.tenant_id = t AND p.order_id = o ORDER BY p.role, p.version DESC)
    SELECT CASE
      WHEN EXISTS (SELECT 1 FROM cur WHERE role = 'CONSIGNEE' AND kind = 'TO_ORDER') AND mode IS DISTINCT FROM 'SEA'
        THEN 'a TO ORDER consignee needs a SEA order'
      WHEN EXISTS (SELECT 1 FROM cur WHERE role = 'CONSIGNEE' AND kind = 'TO_ORDER') AND NOT EXISTS (SELECT 1 FROM cur WHERE role = 'NOTIFY_PARTY')
        THEN 'a TO ORDER consignee needs a notify party'
      WHEN movement IN ('DOOR_TO_DOOR','DOOR_TO_PORT') AND EXISTS (SELECT 1 FROM cur WHERE role = 'SHIPPER' AND pickup IS NULL)
        THEN 'a door pickup needs the shipper''s pickup address'
      WHEN movement IN ('DOOR_TO_DOOR','PORT_TO_DOOR') AND EXISTS (SELECT 1 FROM cur WHERE role = 'CONSIGNEE' AND delivery IS NULL)
        THEN 'a door delivery needs the consignee''s delivery address'
      END INTO bad;
  END IF;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_com: order %/%: %', t, o, bad USING ERRCODE = 'check_violation', CONSTRAINT = 'order_parties_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER order_transport_parties_consistent AFTER INSERT ON cargo_com.order_transport
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_parties();
CREATE CONSTRAINT TRIGGER order_party_parties_consistent AFTER INSERT ON cargo_com.order_party
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_parties();
CREATE CONSTRAINT TRIGGER order_party_contact_parties_consistent AFTER INSERT ON cargo_com.order_party_contact
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_parties();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['order_transport','order_party','order_party_contact'] LOOP
    EXECUTE format('ALTER TABLE cargo_com.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_com.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format('CREATE POLICY tenant_isolation ON cargo_com.%I USING (tenant_id = current_setting(''app.tenant_id'', true)) '
      'WITH CHECK (tenant_id = current_setting(''app.tenant_id'', true))', tbl);
  END LOOP;
END $rls$;
