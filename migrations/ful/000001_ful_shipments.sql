-- IDP-Cargo FUL / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A shipment plans COM obligation versions by id and version, with the packs, weight and volume COM
-- reported, for one customer party and legal entity; there are no cross-schema foreign keys.
CREATE SCHEMA IF NOT EXISTS cargo_ful;
CREATE TABLE cargo_ful.shipment (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 customer_party_id text NOT NULL CHECK (length(btrim(customer_party_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 status text NOT NULL CHECK (status IN ('DRAFT','PLANNED','CANCELLED')),
 cancelled_by text CHECK (length(btrim(cancelled_by)) > 0),
 cancelled_at timestamptz,
 cancel_reason text CHECK (length(btrim(cancel_reason)) BETWEEN 1 AND 500),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, shipment_id),
 CONSTRAINT shipment_cancellation CHECK ((status = 'CANCELLED') = (cancelled_at IS NOT NULL)
   AND (cancelled_by IS NULL) = (cancelled_at IS NULL) AND (cancel_reason IS NULL) = (cancelled_at IS NULL))
);
-- Every obligation version the shipment planned, in the order added; a released entry stays as history.
CREATE TABLE cargo_ful.shipment_obligation (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no > 0),
 obligation_id text NOT NULL CHECK (length(btrim(obligation_id)) BETWEEN 1 AND 128),
 obligation_version bigint NOT NULL CHECK (obligation_version > 0),
 order_id text NOT NULL CHECK (length(btrim(order_id)) BETWEEN 1 AND 128),
 customer_party_id text NOT NULL,
 legal_entity_id text NOT NULL,
 packs bigint NOT NULL CHECK (packs > 0),
 package_type text NOT NULL CHECK (package_type ~ '^[A-Z0-9]{2}$'),
 gross_weight_unit text NOT NULL CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint NOT NULL CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume > 0),
 added_by text NOT NULL CHECK (length(btrim(added_by)) > 0),
 added_at timestamptz NOT NULL,
 released_by text CHECK (length(btrim(released_by)) > 0),
 released_at timestamptz,
 PRIMARY KEY (tenant_id, shipment_id, entry_no),
 FOREIGN KEY (tenant_id, shipment_id) REFERENCES cargo_ful.shipment (tenant_id, shipment_id),
 CONSTRAINT shipment_obligation_volume_declared CHECK ((volume_unit IS NULL) = (volume IS NULL)),
 CONSTRAINT shipment_obligation_release CHECK ((released_by IS NULL) = (released_at IS NULL)
   AND (released_at IS NULL OR released_at >= added_at))
);
-- An obligation is planned in at most one shipment that is not cancelled (cancellation releases it).
CREATE UNIQUE INDEX shipment_obligation_one_live_shipment ON cargo_ful.shipment_obligation (tenant_id, obligation_id)
  WHERE released_at IS NULL;
-- Plan versions are immutable: the route, the cargo scope at that version, what changed and why.
CREATE TABLE cargo_ful.shipment_plan_version (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 plan_no integer NOT NULL CHECK (plan_no > 0),
 change text NOT NULL CHECK (change IN ('CREATED','OBLIGATION_ADDED','AMENDED','OBLIGATION_RELEASED')),
 transport_mode text CHECK (transport_mode IN ('AIR','SEA','ROAD','RAIL')),
 origin_scheme text CHECK (origin_scheme = 'UNLOCODE'),
 origin_code text CHECK (origin_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 destination_scheme text CHECK (destination_scheme = 'UNLOCODE'),
 destination_code text CHECK (destination_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 departure_from timestamptz,
 departure_until timestamptz,
 scope_packs bigint NOT NULL CHECK (scope_packs > 0),
 scope_weight_unit text NOT NULL CHECK (scope_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 scope_weight bigint NOT NULL CHECK (scope_weight > 0),
 scope_volume_unit text CHECK (scope_volume_unit ~ '^[A-Z0-9]{1,3}$'),
 scope_volume bigint CHECK (scope_volume > 0),
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 written_by text NOT NULL CHECK (length(btrim(written_by)) > 0),
 written_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, shipment_id, plan_no),
 FOREIGN KEY (tenant_id, shipment_id) REFERENCES cargo_ful.shipment (tenant_id, shipment_id),
 CONSTRAINT shipment_plan_version_locations CHECK ((origin_scheme IS NULL) = (origin_code IS NULL)
   AND (destination_scheme IS NULL) = (destination_code IS NULL)
   AND (origin_code IS NULL OR destination_code IS NULL OR origin_code <> destination_code)),
 CONSTRAINT shipment_plan_version_departure CHECK ((departure_from IS NULL) = (departure_until IS NULL)
   AND (departure_from IS NULL OR departure_from < departure_until)),
 CONSTRAINT shipment_plan_version_scope_volume CHECK ((scope_volume_unit IS NULL) = (scope_volume IS NULL)),
 CONSTRAINT shipment_plan_version_reason CHECK ((change = 'AMENDED') = (reason IS NOT NULL))
);
CREATE TABLE cargo_ful.shipment_plan_confirmation (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 plan_no integer NOT NULL,
 confirmed_by text NOT NULL CHECK (length(btrim(confirmed_by)) > 0),
 confirmed_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, shipment_id, plan_no),
 FOREIGN KEY (tenant_id, shipment_id, plan_no) REFERENCES cargo_ful.shipment_plan_version (tenant_id, shipment_id, plan_no)
);
CREATE TABLE cargo_ful.command_receipt (
 tenant_id text NOT NULL,
 command_id text NOT NULL,
 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 result_type text NOT NULL,
 result_id text NOT NULL,
 result_revision bigint NOT NULL CHECK (result_revision > 0),
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, command_id)
);

-- Only the status, the revision, updated_at and the cancellation of a shipment change, and a cancelled
-- shipment never changes again. Shipments are never deleted.
CREATE FUNCTION cargo_ful.reject_shipment_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['status','revision','updated_at','cancelled_by','cancelled_at','cancel_reason'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_ful: shipment %/% cannot be deleted', OLD.tenant_id, OLD.shipment_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.status = 'CANCELLED' OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_ful: shipment %/% changes only its status, and never once cancelled', OLD.tenant_id, OLD.shipment_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER shipment_rules BEFORE UPDATE OR DELETE ON cargo_ful.shipment
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.reject_shipment_change();

CREATE FUNCTION cargo_ful.reject_row_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_ful: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER shipment_plan_version_immutable BEFORE UPDATE OR DELETE ON cargo_ful.shipment_plan_version
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.reject_row_change();
CREATE TRIGGER shipment_plan_confirmation_immutable BEFORE UPDATE OR DELETE ON cargo_ful.shipment_plan_confirmation
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.reject_row_change();

-- A planned obligation belongs to the shipment's customer and legal entity and joins a shipment that is
-- not cancelled; afterwards it can only be released, once. The shipment row is locked first.
CREATE FUNCTION cargo_ful.check_shipment_obligation() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_ful.shipment;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_ful: planned obligations are kept' USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF OLD.released_at IS NOT NULL OR NEW.released_at IS NULL
       OR to_jsonb(OLD) - ARRAY['released_by','released_at'] IS DISTINCT FROM to_jsonb(NEW) - ARRAY['released_by','released_at'] THEN
      RAISE EXCEPTION 'cargo_ful: planned obligation %/%/% can only be released, once', OLD.tenant_id, OLD.shipment_id, OLD.entry_no
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  SELECT * INTO s FROM cargo_ful.shipment WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id FOR NO KEY UPDATE;
  IF NOT FOUND OR s.status = 'CANCELLED' OR s.customer_party_id <> NEW.customer_party_id OR s.legal_entity_id <> NEW.legal_entity_id THEN
    RAISE EXCEPTION 'cargo_ful: obligation %/% does not belong in shipment %', NEW.tenant_id, NEW.obligation_id, NEW.shipment_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER shipment_obligation_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_ful.shipment_obligation
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_shipment_obligation();

-- Only the latest plan version of a shipment can be confirmed, and only if its route is complete.
CREATE FUNCTION cargo_ful.check_plan_confirmation() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  p cargo_ful.shipment_plan_version;
BEGIN
  PERFORM 1 FROM cargo_ful.shipment WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id FOR NO KEY UPDATE;
  SELECT * INTO p FROM cargo_ful.shipment_plan_version
    WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id ORDER BY plan_no DESC LIMIT 1;
  IF NOT FOUND OR p.plan_no <> NEW.plan_no OR p.transport_mode IS NULL OR p.origin_code IS NULL OR p.destination_code IS NULL
     OR p.departure_from IS NULL OR NEW.confirmed_at < p.written_at THEN
    RAISE EXCEPTION 'cargo_ful: plan version % of shipment %/% cannot be confirmed', NEW.plan_no, NEW.tenant_id, NEW.shipment_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER shipment_plan_confirmation_rules BEFORE INSERT ON cargo_ful.shipment_plan_confirmation
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_plan_confirmation();

-- Tenant isolation as in the other contexts: the tenant transactor sets app.tenant_id in every transaction.
DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['shipment','shipment_obligation','shipment_plan_version','shipment_plan_confirmation','command_receipt'] LOOP
    EXECUTE format('ALTER TABLE cargo_ful.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_ful.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_ful.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
