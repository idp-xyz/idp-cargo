-- IDP-Cargo ORG V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Decision C9 as amended on 2026-11-09 and privacy assessment V1.1: ORG keeps each party's addresses and
-- contacts by purpose, in an address book with a revision of its own. Every change of an address or contact
-- is a new version; defaults per purpose and the record of every revision are kept. Erasing a contact clears
-- its name and contact details in every version and keeps the rest.

-- Migration 000001 created party_address and party_contact as encrypted candidates. No code ever wrote them,
-- and V0.3 replaces them. They are dropped only when empty; the owner is exempt from row level security for
-- the check, so rows of any tenant count.
ALTER TABLE cargo_org.party_address NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_org.party_contact NO FORCE ROW LEVEL SECURITY;
DO $empty$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_org.party_address) OR EXISTS (SELECT 1 FROM cargo_org.party_contact) THEN
    RAISE EXCEPTION 'cargo_org: party_address or party_contact holds rows of the V2.3 candidate; convert them before ORG V0.3'
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
END $empty$;
DROP TABLE cargo_org.party_address;
DROP TABLE cargo_org.party_contact;

CREATE TABLE cargo_org.party_address_book (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  revision bigint NOT NULL CHECK (revision > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, party_id),
  FOREIGN KEY (tenant_id, party_id) REFERENCES cargo_org.business_party (tenant_id, party_id)
);

-- One row per revision: what changed, who changed it, why and when ORG recorded it. Reasons must not
-- repeat personal data; erasure does not touch them.
CREATE TABLE cargo_org.party_address_book_change (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  revision bigint NOT NULL CHECK (revision > 0),
  action text NOT NULL CHECK (action IN ('ADD_ADDRESS','CHANGE_ADDRESS','DEACTIVATE_ADDRESS','SET_DEFAULT_ADDRESS',
    'ADD_CONTACT','CHANGE_CONTACT','DEACTIVATE_CONTACT','ERASE_CONTACT','SET_DEFAULT_CONTACT')),
  entry_id text NOT NULL CHECK (length(btrim(entry_id)) BETWEEN 1 AND 128),
  purpose text CHECK (purpose IN ('OFFICE','PICKUP','DELIVERY','BILLING','BOOKING','DOCUMENTS','ACCOUNTS')),
  actor_id text NOT NULL CHECK (length(btrim(actor_id)) > 0),
  reason text CHECK (char_length(btrim(reason)) BETWEEN 1 AND 500),
  recorded_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, party_id, revision),
  FOREIGN KEY (tenant_id, party_id) REFERENCES cargo_org.party_address_book (tenant_id, party_id),
  CHECK ((purpose IS NOT NULL) = (action IN ('SET_DEFAULT_ADDRESS','SET_DEFAULT_CONTACT'))),
  CHECK (reason IS NOT NULL OR action NOT IN ('CHANGE_ADDRESS','DEACTIVATE_ADDRESS','CHANGE_CONTACT','DEACTIVATE_CONTACT','ERASE_CONTACT'))
);

-- The current version, status and short code of each address; the versions follow.
CREATE TABLE cargo_org.party_address (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  address_id text NOT NULL CHECK (length(btrim(address_id)) BETWEEN 1 AND 128),
  short_code text NOT NULL CHECK (short_code ~ '^[A-Z0-9][A-Z0-9_-]{0,19}$'),
  status text NOT NULL CHECK (status IN ('ACTIVE','INACTIVE')),
  version bigint NOT NULL CHECK (version > 0),
  PRIMARY KEY (tenant_id, party_id, address_id),
  FOREIGN KEY (tenant_id, party_id) REFERENCES cargo_org.party_address_book (tenant_id, party_id)
);
CREATE UNIQUE INDEX party_address_active_short_code ON cargo_org.party_address (tenant_id, party_id, short_code) WHERE status = 'ACTIVE';

CREATE TABLE cargo_org.party_address_version (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  address_id text NOT NULL,
  version bigint NOT NULL CHECK (version > 0),
  revision bigint NOT NULL,
  status text NOT NULL CHECK (status IN ('ACTIVE','INACTIVE')),
  short_code text NOT NULL CHECK (short_code ~ '^[A-Z0-9][A-Z0-9_-]{0,19}$'),
  purposes text[] NOT NULL CHECK (cardinality(purposes) BETWEEN 1 AND 4 AND purposes <@ ARRAY['OFFICE','PICKUP','DELIVERY','BILLING']),
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
  lines text[] NOT NULL CHECK (cardinality(lines) BETWEEN 1 AND 4 AND array_position(lines, NULL) IS NULL),
  city text NOT NULL CHECK (char_length(city) BETWEEN 1 AND 100),
  region text CHECK (char_length(region) BETWEEN 1 AND 100),
  postcode text CHECK (char_length(postcode) BETWEEN 1 AND 20),
  country_code char(2) NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
  unlocode text CHECK (unlocode ~ '^[A-Z]{2}[A-Z2-9]{3}$' AND left(unlocode, 2) = country_code),
  residential boolean NOT NULL,
  local_language text CHECK (local_language ~ '^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$'),
  local_name text CHECK (char_length(local_name) BETWEEN 1 AND 200),
  local_lines text[] CHECK (cardinality(local_lines) BETWEEN 1 AND 4 AND array_position(local_lines, NULL) IS NULL),
  local_city text CHECK (char_length(local_city) BETWEEN 1 AND 100),
  local_region text CHECK (char_length(local_region) BETWEEN 1 AND 100),
  PRIMARY KEY (tenant_id, party_id, address_id, version),
  FOREIGN KEY (tenant_id, party_id, address_id) REFERENCES cargo_org.party_address (tenant_id, party_id, address_id),
  FOREIGN KEY (tenant_id, party_id, revision) REFERENCES cargo_org.party_address_book_change (tenant_id, party_id, revision),
  CONSTRAINT party_address_version_local CHECK ((local_language IS NULL) = (local_name IS NULL)
    AND (local_language IS NULL) = (local_lines IS NULL) AND (local_language IS NULL) = (local_city IS NULL)
    AND (local_language IS NOT NULL OR local_region IS NULL))
);

-- A purpose's default address from a revision on; a row without address_id leaves the purpose without one.
CREATE TABLE cargo_org.party_address_default (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  purpose text NOT NULL CHECK (purpose IN ('OFFICE','PICKUP','DELIVERY','BILLING')),
  revision bigint NOT NULL,
  address_id text,
  PRIMARY KEY (tenant_id, party_id, purpose, revision),
  FOREIGN KEY (tenant_id, party_id, revision) REFERENCES cargo_org.party_address_book_change (tenant_id, party_id, revision),
  FOREIGN KEY (tenant_id, party_id, address_id) REFERENCES cargo_org.party_address (tenant_id, party_id, address_id)
);

CREATE TABLE cargo_org.party_contact (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  contact_id text NOT NULL CHECK (length(btrim(contact_id)) BETWEEN 1 AND 128),
  status text NOT NULL CHECK (status IN ('ACTIVE','INACTIVE','ERASED')),
  version bigint NOT NULL CHECK (version > 0),
  PRIMARY KEY (tenant_id, party_id, contact_id),
  FOREIGN KEY (tenant_id, party_id) REFERENCES cargo_org.party_address_book (tenant_id, party_id)
);

-- Contact versions hold personal data. An erased version keeps its purposes and status and nothing else.
CREATE TABLE cargo_org.party_contact_version (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  contact_id text NOT NULL,
  version bigint NOT NULL CHECK (version > 0),
  revision bigint NOT NULL,
  status text NOT NULL CHECK (status IN ('ACTIVE','INACTIVE','ERASED')),
  purposes text[] NOT NULL CHECK (cardinality(purposes) BETWEEN 1 AND 3 AND purposes <@ ARRAY['BOOKING','DOCUMENTS','ACCOUNTS']),
  name text CHECK (char_length(name) BETWEEN 1 AND 200),
  phone text CHECK (char_length(phone) BETWEEN 3 AND 40),
  mobile text CHECK (char_length(mobile) BETWEEN 3 AND 40),
  email text CHECK (char_length(email) BETWEEN 3 AND 254),
  erased boolean NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, party_id, contact_id, version),
  FOREIGN KEY (tenant_id, party_id, contact_id) REFERENCES cargo_org.party_contact (tenant_id, party_id, contact_id),
  FOREIGN KEY (tenant_id, party_id, revision) REFERENCES cargo_org.party_address_book_change (tenant_id, party_id, revision),
  CONSTRAINT party_contact_version_erased CHECK (
    CASE WHEN erased THEN name IS NULL AND phone IS NULL AND mobile IS NULL AND email IS NULL
    ELSE status <> 'ERASED' AND name IS NOT NULL AND (phone IS NOT NULL OR mobile IS NOT NULL OR email IS NOT NULL) END)
);

CREATE TABLE cargo_org.party_contact_default (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  purpose text NOT NULL CHECK (purpose IN ('BOOKING','DOCUMENTS','ACCOUNTS')),
  revision bigint NOT NULL,
  contact_id text,
  PRIMARY KEY (tenant_id, party_id, purpose, revision),
  FOREIGN KEY (tenant_id, party_id, revision) REFERENCES cargo_org.party_address_book_change (tenant_id, party_id, revision),
  FOREIGN KEY (tenant_id, party_id, contact_id) REFERENCES cargo_org.party_contact (tenant_id, party_id, contact_id)
);

-- Receipts of address book commands also name the address or contact and its version.
ALTER TABLE cargo_org.command_receipt
  ADD COLUMN result_entry_id text,
  ADD COLUMN result_entry_version bigint CHECK (result_entry_version > 0),
  ADD CONSTRAINT command_receipt_entry CHECK ((result_entry_id IS NULL) = (result_entry_version IS NULL));

-- Versions, defaults and change records are never rewritten or deleted; books, addresses and contacts are
-- never deleted.
CREATE FUNCTION cargo_org.refuse_address_book_rewrite() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_org: % of party %/% cannot be %d', TG_TABLE_NAME, OLD.tenant_id, OLD.party_id, lower(TG_OP)
    USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER party_address_book_change_immutable BEFORE UPDATE OR DELETE ON cargo_org.party_address_book_change
  FOR EACH ROW EXECUTE FUNCTION cargo_org.refuse_address_book_rewrite();
CREATE TRIGGER party_address_version_immutable BEFORE UPDATE OR DELETE ON cargo_org.party_address_version
  FOR EACH ROW EXECUTE FUNCTION cargo_org.refuse_address_book_rewrite();
CREATE TRIGGER party_address_default_immutable BEFORE UPDATE OR DELETE ON cargo_org.party_address_default
  FOR EACH ROW EXECUTE FUNCTION cargo_org.refuse_address_book_rewrite();
CREATE TRIGGER party_contact_default_immutable BEFORE UPDATE OR DELETE ON cargo_org.party_contact_default
  FOR EACH ROW EXECUTE FUNCTION cargo_org.refuse_address_book_rewrite();
CREATE TRIGGER party_address_book_kept BEFORE DELETE ON cargo_org.party_address_book
  FOR EACH ROW EXECUTE FUNCTION cargo_org.refuse_address_book_rewrite();
CREATE TRIGGER party_address_kept BEFORE DELETE ON cargo_org.party_address
  FOR EACH ROW EXECUTE FUNCTION cargo_org.refuse_address_book_rewrite();
CREATE TRIGGER party_contact_kept BEFORE DELETE ON cargo_org.party_contact
  FOR EACH ROW EXECUTE FUNCTION cargo_org.refuse_address_book_rewrite();

-- The one change to a stored contact version: erasure, which clears every personal field at once, for good.
CREATE FUNCTION cargo_org.erase_contact_version_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.erased OR NOT NEW.erased
     OR NEW.name IS NOT NULL OR NEW.phone IS NOT NULL OR NEW.mobile IS NOT NULL OR NEW.email IS NOT NULL
     OR (NEW.tenant_id, NEW.party_id, NEW.contact_id, NEW.version, NEW.revision, NEW.status, NEW.purposes)
        IS DISTINCT FROM (OLD.tenant_id, OLD.party_id, OLD.contact_id, OLD.version, OLD.revision, OLD.status, OLD.purposes) THEN
    RAISE EXCEPTION 'cargo_org: version % of contact %/%/% can only be erased', OLD.version, OLD.tenant_id, OLD.party_id, OLD.contact_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER party_contact_version_erasure_only BEFORE UPDATE OR DELETE ON cargo_org.party_contact_version
  FOR EACH ROW EXECUTE FUNCTION cargo_org.erase_contact_version_only();

-- At commit, every address book a transaction touched is consistent: one change record per revision; each
-- address and contact at its latest version, numbered from 1, with the status and short code of that
-- version; the versions of an erased contact all erased, those of any other contact none; and each current
-- default an active entry that has
-- the purpose. Deferred, because ORG writes the book row before the rows it counts.
CREATE FUNCTION cargo_org.check_address_book() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  t text := NEW.tenant_id;
  p text := NEW.party_id;
  rev bigint;
  bad text;
BEGIN
  SELECT b.revision INTO rev FROM cargo_org.party_address_book b WHERE b.tenant_id = t AND b.party_id = p;
  IF rev IS NULL OR rev <> (SELECT count(*) FROM cargo_org.party_address_book_change c WHERE c.tenant_id = t AND c.party_id = p)
     OR rev <> (SELECT max(c.revision) FROM cargo_org.party_address_book_change c WHERE c.tenant_id = t AND c.party_id = p) THEN
    bad := 'its revision is not the number of its change records';
  END IF;
  IF bad IS NULL THEN
    SELECT 'address ' || h.address_id || ' does not match its versions' INTO bad
      FROM cargo_org.party_address h
      LEFT JOIN cargo_org.party_address_version v
        ON v.tenant_id = h.tenant_id AND v.party_id = h.party_id AND v.address_id = h.address_id AND v.version = h.version
      WHERE h.tenant_id = t AND h.party_id = p
        AND (v.version IS NULL OR v.status <> h.status OR v.short_code <> h.short_code
          OR (SELECT count(*) FROM cargo_org.party_address_version w
                WHERE w.tenant_id = h.tenant_id AND w.party_id = h.party_id AND w.address_id = h.address_id) <> h.version
          OR EXISTS (SELECT 1 FROM cargo_org.party_address_version w
                WHERE w.tenant_id = h.tenant_id AND w.party_id = h.party_id AND w.address_id = h.address_id AND w.version > h.version))
      LIMIT 1;
  END IF;
  IF bad IS NULL THEN
    SELECT 'contact ' || h.contact_id || ' does not match its versions' INTO bad
      FROM cargo_org.party_contact h
      LEFT JOIN cargo_org.party_contact_version v
        ON v.tenant_id = h.tenant_id AND v.party_id = h.party_id AND v.contact_id = h.contact_id AND v.version = h.version
      WHERE h.tenant_id = t AND h.party_id = p
        AND (v.version IS NULL OR v.status <> h.status
          OR (SELECT count(*) FROM cargo_org.party_contact_version w
                WHERE w.tenant_id = h.tenant_id AND w.party_id = h.party_id AND w.contact_id = h.contact_id) <> h.version
          OR EXISTS (SELECT 1 FROM cargo_org.party_contact_version w
                WHERE w.tenant_id = h.tenant_id AND w.party_id = h.party_id AND w.contact_id = h.contact_id
                  AND (w.version > h.version OR w.erased <> (h.status = 'ERASED'))))
      LIMIT 1;
  END IF;
  IF bad IS NULL THEN
    SELECT 'the ' || d.purpose || ' default address is not active for that purpose' INTO bad
      FROM (SELECT DISTINCT ON (x.purpose) x.purpose, x.address_id FROM cargo_org.party_address_default x
              WHERE x.tenant_id = t AND x.party_id = p ORDER BY x.purpose, x.revision DESC) d
      LEFT JOIN cargo_org.party_address h ON h.tenant_id = t AND h.party_id = p AND h.address_id = d.address_id
      LEFT JOIN cargo_org.party_address_version v
        ON v.tenant_id = t AND v.party_id = p AND v.address_id = h.address_id AND v.version = h.version
      WHERE d.address_id IS NOT NULL AND (h.status IS DISTINCT FROM 'ACTIVE' OR NOT (d.purpose = ANY (v.purposes)))
      LIMIT 1;
  END IF;
  IF bad IS NULL THEN
    SELECT 'the ' || d.purpose || ' default contact is not active for that purpose' INTO bad
      FROM (SELECT DISTINCT ON (x.purpose) x.purpose, x.contact_id FROM cargo_org.party_contact_default x
              WHERE x.tenant_id = t AND x.party_id = p ORDER BY x.purpose, x.revision DESC) d
      LEFT JOIN cargo_org.party_contact h ON h.tenant_id = t AND h.party_id = p AND h.contact_id = d.contact_id
      LEFT JOIN cargo_org.party_contact_version v
        ON v.tenant_id = t AND v.party_id = p AND v.contact_id = h.contact_id AND v.version = h.version
      WHERE d.contact_id IS NOT NULL AND (h.status IS DISTINCT FROM 'ACTIVE' OR NOT (d.purpose = ANY (v.purposes)))
      LIMIT 1;
  END IF;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_org: address book of party %/% is inconsistent: %', t, p, bad
      USING ERRCODE = 'check_violation', CONSTRAINT = 'party_address_book_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER party_address_book_consistent AFTER INSERT OR UPDATE ON cargo_org.party_address_book
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_address_book();
CREATE CONSTRAINT TRIGGER party_address_book_change_consistent AFTER INSERT ON cargo_org.party_address_book_change
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_address_book();
CREATE CONSTRAINT TRIGGER party_address_consistent AFTER INSERT OR UPDATE ON cargo_org.party_address
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_address_book();
CREATE CONSTRAINT TRIGGER party_address_version_consistent AFTER INSERT ON cargo_org.party_address_version
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_address_book();
CREATE CONSTRAINT TRIGGER party_address_default_consistent AFTER INSERT ON cargo_org.party_address_default
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_address_book();
CREATE CONSTRAINT TRIGGER party_contact_consistent AFTER INSERT OR UPDATE ON cargo_org.party_contact
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_address_book();
CREATE CONSTRAINT TRIGGER party_contact_version_consistent AFTER INSERT OR UPDATE ON cargo_org.party_contact_version
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_address_book();
CREATE CONSTRAINT TRIGGER party_contact_default_consistent AFTER INSERT ON cargo_org.party_contact_default
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_address_book();

-- Tenant isolation as for every ORG table (migration 000005).
DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['party_address_book','party_address_book_change','party_address','party_address_version',
    'party_address_default','party_contact','party_contact_version','party_contact_default'] LOOP
    EXECUTE format('ALTER TABLE cargo_org.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_org.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format('CREATE POLICY tenant_isolation ON cargo_org.%I USING (tenant_id = current_setting(''app.tenant_id'', true)) '
      'WITH CHECK (tenant_id = current_setting(''app.tenant_id'', true))', tbl);
  END LOOP;
END $rls$;
