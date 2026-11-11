-- IDP-Cargo PHY V0.1 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Consolidations and their houses (N1, decisions N1-PHY OD-1, OD-5, OD-6, OD-8, OD-10): a consolidation has an
-- identity of its own and immutable versions; each FUL shipment has one membership ledger per carriage stage
-- with immutable entries, of which at most one is live. Shipments are FUL's: there are no cross-schema
-- foreign keys, only ids with the FUL revision and plan version PHY verified.
CREATE SCHEMA IF NOT EXISTS cargo_phy;

CREATE TABLE cargo_phy.consolidation (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 consolidation_id text NOT NULL CHECK (length(btrim(consolidation_id)) BETWEEN 1 AND 128),
 transport_mode text NOT NULL CHECK (transport_mode IN ('AIR','SEA')),
 kind text NOT NULL CHECK (kind IN ('AIR_CONSOL','SEA_LCL_OWN','SEA_LCL_COLOAD')),
 operating_legal_entity_id text NOT NULL CHECK (length(btrim(operating_legal_entity_id)) BETWEEN 1 AND 128),
 state text NOT NULL CHECK (state IN ('OPEN','CLOSED','RECONCILED','CANCELLED')),
 current_version integer NOT NULL CHECK (current_version > 0),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, consolidation_id),
 CONSTRAINT consolidation_kind_mode CHECK ((transport_mode = 'AIR') = (kind = 'AIR_CONSOL'))
);
CREATE INDEX consolidation_open ON cargo_phy.consolidation (tenant_id, transport_mode) WHERE state = 'OPEN';

-- Every change writes a version: the route, the planned departure and house cut-off as instants, and the
-- state after the change, so the original plan stays visible after a rollover.
CREATE TABLE cargo_phy.consolidation_version (
 tenant_id text NOT NULL,
 consolidation_id text NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 change text NOT NULL CHECK (change IN ('CREATED','AMENDED','CLOSED','REOPENED','CANCELLED')),
 state text NOT NULL CHECK (state IN ('OPEN','CLOSED','RECONCILED','CANCELLED')),
 origin_code text NOT NULL CHECK (origin_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 destination_code text NOT NULL CHECK (destination_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 origin_iata char(3) CHECK (origin_iata ~ '^[A-Z]{3}$'),
 destination_iata char(3) CHECK (destination_iata ~ '^[A-Z]{3}$'),
 location_edition text NOT NULL CHECK (location_edition ~ '^[A-Z0-9][A-Z0-9._-]{0,63}$'),
 planned_departure timestamptz NOT NULL,
 house_cut_off timestamptz NOT NULL,
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 written_by text NOT NULL CHECK (length(btrim(written_by)) > 0),
 written_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, consolidation_id, version_no),
 FOREIGN KEY (tenant_id, consolidation_id) REFERENCES cargo_phy.consolidation (tenant_id, consolidation_id),
 CONSTRAINT consolidation_version_route CHECK (origin_code <> destination_code AND (origin_iata IS NULL) = (destination_iata IS NULL)),
 CONSTRAINT consolidation_version_schedule CHECK (house_cut_off <= planned_departure),
 CONSTRAINT consolidation_version_reason CHECK ((change IN ('AMENDED','REOPENED','CANCELLED')) = (reason IS NOT NULL)),
 CONSTRAINT consolidation_version_state CHECK (CASE change
   WHEN 'CREATED' THEN state = 'OPEN' WHEN 'REOPENED' THEN state = 'OPEN' WHEN 'CLOSED' THEN state = 'CLOSED'
   WHEN 'CANCELLED' THEN state = 'CANCELLED' ELSE state IN ('OPEN','CLOSED') END)
);

-- One ledger per shipment and carriage stage (MAIN in N1); its revision guards every house command.
CREATE TABLE cargo_phy.consol_membership (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 stage text NOT NULL CHECK (stage = 'MAIN'),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, shipment_id, stage)
);

-- A membership entry freezes the FUL revision and plan version PHY verified and the scope it read; only its
-- end is written later, once. A transfer or refresh ends one entry and starts the next, linked to it.
CREATE TABLE cargo_phy.membership_entry (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 stage text NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no > 0),
 consolidation_id text NOT NULL,
 start_kind text NOT NULL CHECK (start_kind IN ('ATTACHED','TRANSFERRED','REFRESHED')),
 predecessor_no integer,
 ful_revision bigint NOT NULL CHECK (ful_revision > 0),
 plan_version integer NOT NULL CHECK (plan_version > 0),
 packs bigint NOT NULL CHECK (packs > 0),
 gross_weight_unit text NOT NULL CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint NOT NULL CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume > 0),
 attached_by text NOT NULL CHECK (length(btrim(attached_by)) > 0),
 attached_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 end_kind text CHECK (end_kind IN ('DETACHED','TRANSFERRED','REFRESHED','SHIPMENT_CANCELLED')),
 end_reason text CHECK (end_reason IN ('ROLLOVER','FLIGHT_OR_VESSEL_CHANGE','CARRIER_CHANGE','SHUT_OUT','SPACE_SHORTAGE',
   'CUSTOMER_REQUEST','WRONG_CONSOL','SHIPMENT_CANCELLED','OTHER')),
 end_text text CHECK (length(btrim(end_text)) BETWEEN 1 AND 500),
 ended_by text CHECK (length(btrim(ended_by)) > 0),
 ended_at timestamptz,
 end_entered_at timestamptz,
 successor_no integer,
 PRIMARY KEY (tenant_id, shipment_id, stage, entry_no),
 FOREIGN KEY (tenant_id, shipment_id, stage) REFERENCES cargo_phy.consol_membership (tenant_id, shipment_id, stage),
 FOREIGN KEY (tenant_id, consolidation_id) REFERENCES cargo_phy.consolidation (tenant_id, consolidation_id),
 CONSTRAINT membership_entry_volume CHECK ((volume_unit IS NULL) = (volume IS NULL)),
 CONSTRAINT membership_entry_start CHECK ((start_kind = 'ATTACHED') = (predecessor_no IS NULL) AND (predecessor_no IS NULL OR predecessor_no < entry_no)),
 CONSTRAINT membership_entry_end CHECK ((end_kind IS NULL) = (ended_at IS NULL) AND (ended_by IS NULL) = (ended_at IS NULL)
   AND (end_entered_at IS NULL) = (ended_at IS NULL) AND (ended_at IS NULL OR ended_at >= attached_at)),
 CONSTRAINT membership_entry_successor CHECK ((successor_no IS NOT NULL) = (end_kind IS NOT NULL AND end_kind IN ('TRANSFERRED','REFRESHED'))
   AND (successor_no IS NULL OR successor_no = entry_no + 1)),
 CONSTRAINT membership_entry_reason CHECK ((end_reason IS NOT NULL) = coalesce(end_kind <> 'REFRESHED', false)
   AND coalesce(end_reason = 'SHIPMENT_CANCELLED', false) = coalesce(end_kind = 'SHIPMENT_CANCELLED', false)
   AND (end_reason IS DISTINCT FROM 'OTHER' OR end_text IS NOT NULL) AND (end_text IS NULL OR end_reason IS NOT NULL))
);
-- A shipment is in at most one consolidation per stage at a time (I-02, G04-02).
CREATE UNIQUE INDEX membership_entry_one_live ON cargo_phy.membership_entry (tenant_id, shipment_id, stage) WHERE ended_at IS NULL;
CREATE INDEX membership_entry_live_by_consolidation ON cargo_phy.membership_entry (tenant_id, consolidation_id) WHERE ended_at IS NULL;

-- The parts of COM obligation versions the house planned when PHY verified it (F1).
CREATE TABLE cargo_phy.membership_entry_part (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 stage text NOT NULL,
 entry_no integer NOT NULL,
 part_no integer NOT NULL CHECK (part_no > 0),
 obligation_id text NOT NULL CHECK (length(btrim(obligation_id)) BETWEEN 1 AND 128),
 obligation_version bigint NOT NULL CHECK (obligation_version > 0),
 order_id text NOT NULL CHECK (length(btrim(order_id)) BETWEEN 1 AND 128),
 packs bigint NOT NULL CHECK (packs > 0),
 package_type text NOT NULL CHECK (package_type ~ '^[A-Z0-9]{2}$'),
 gross_weight bigint NOT NULL CHECK (gross_weight > 0),
 volume bigint CHECK (volume > 0),
 PRIMARY KEY (tenant_id, shipment_id, stage, entry_no, part_no),
 FOREIGN KEY (tenant_id, shipment_id, stage, entry_no) REFERENCES cargo_phy.membership_entry (tenant_id, shipment_id, stage, entry_no)
);

CREATE TABLE cargo_phy.command_receipt (
 tenant_id text NOT NULL,
 command_id text NOT NULL,
 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 result_type text NOT NULL CHECK (result_type IN ('Consolidation','ConsolMembership')),
 result_id text NOT NULL,
 result_revision bigint NOT NULL CHECK (result_revision > 0),
 result_consolidation_id text NOT NULL,
 result_entry_no integer CHECK (result_entry_no > 0),
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, command_id),
 CONSTRAINT command_receipt_entry CHECK ((result_type = 'ConsolMembership') = (result_entry_no IS NOT NULL))
);

-- Only the state, the current version, the revision and updated_at of a consolidation change, and a cancelled
-- or reconciled one never changes again. Consolidations are never deleted.
CREATE FUNCTION cargo_phy.reject_consolidation_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','current_version','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% cannot be deleted', OLD.tenant_id, OLD.consolidation_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.state IN ('CANCELLED','RECONCILED') OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% changes only its state, and never once cancelled or reconciled', OLD.tenant_id, OLD.consolidation_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER consolidation_rules BEFORE UPDATE OR DELETE ON cargo_phy.consolidation
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_consolidation_change();

CREATE FUNCTION cargo_phy.reject_row_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_phy: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER consolidation_version_immutable BEFORE UPDATE OR DELETE ON cargo_phy.consolidation_version
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();
CREATE TRIGGER membership_entry_part_immutable BEFORE UPDATE OR DELETE ON cargo_phy.membership_entry_part
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();

-- A consolidation version follows the previous one and is the root's current version; the mode decides whether
-- the route carries IATA airport codes. The root row is locked first.
CREATE FUNCTION cargo_phy.check_consolidation_version() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  c cargo_phy.consolidation;
  prev cargo_phy.consolidation_version;
BEGIN
  SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR NO KEY UPDATE;
  SELECT * INTO prev FROM cargo_phy.consolidation_version
    WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id ORDER BY version_no DESC LIMIT 1;
  IF NOT FOUND THEN
    prev.version_no := 0;
  END IF;
  IF c.consolidation_id IS NULL OR NEW.version_no <> prev.version_no + 1 OR (NEW.version_no = 1) <> (NEW.change = 'CREATED')
     OR (prev.version_no > 0 AND (NEW.written_at < prev.written_at OR NOT CASE NEW.change
          WHEN 'AMENDED' THEN prev.state IN ('OPEN','CLOSED') WHEN 'CLOSED' THEN prev.state = 'OPEN'
          WHEN 'CANCELLED' THEN prev.state = 'OPEN' WHEN 'REOPENED' THEN prev.state = 'CLOSED' ELSE false END))
     OR (c.transport_mode = 'AIR') <> (NEW.origin_iata IS NOT NULL) THEN
    RAISE EXCEPTION 'cargo_phy: version % does not follow consolidation %/%', NEW.version_no, NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER consolidation_version_rules BEFORE INSERT ON cargo_phy.consolidation_version
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_consolidation_version();

-- At commit, the root's state and current version are its latest version's (checked for every consolidation a
-- transaction writes).
CREATE FUNCTION cargo_phy.check_consolidation_current() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  c cargo_phy.consolidation;
  v cargo_phy.consolidation_version;
BEGIN
  SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id;
  SELECT * INTO v FROM cargo_phy.consolidation_version
    WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id ORDER BY version_no DESC LIMIT 1;
  IF NOT FOUND OR v.version_no <> c.current_version OR v.state <> c.state THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% is not its latest version', NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_current_version';
  END IF;
  IF c.state = 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_phy.membership_entry e
       WHERE e.tenant_id = c.tenant_id AND e.consolidation_id = c.consolidation_id AND e.ended_at IS NULL) THEN
    RAISE EXCEPTION 'cargo_phy: cancelled consolidation %/% keeps houses', c.tenant_id, c.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_cancelled_without_houses';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER consolidation_current_version AFTER INSERT OR UPDATE ON cargo_phy.consolidation
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_consolidation_current();

-- Only the ledger's revision and updated_at change; ledgers are never deleted.
CREATE FUNCTION cargo_phy.reject_membership_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_phy: membership %/% changes only its revision', OLD.tenant_id, OLD.shipment_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER consol_membership_rules BEFORE UPDATE OR DELETE ON cargo_phy.consol_membership
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_membership_change();

-- An entry is only ended, once. While its consolidation is CLOSED (the house cut-off), only a cancelled
-- shipment's entry may end; the consolidation row is locked FOR SHARE, as house commands do.
CREATE FUNCTION cargo_phy.check_membership_entry() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  ends text[] := ARRAY['end_kind','end_reason','end_text','ended_by','ended_at','end_entered_at','successor_no'];
  s text;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_phy: membership entries are kept' USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.ended_at IS NOT NULL OR NEW.ended_at IS NULL OR to_jsonb(OLD) - ends IS DISTINCT FROM to_jsonb(NEW) - ends THEN
    RAISE EXCEPTION 'cargo_phy: membership entry %/%/% can only be ended, once', OLD.tenant_id, OLD.shipment_id, OLD.entry_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT state INTO s FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
  IF s IS DISTINCT FROM 'OPEN' AND NOT (s = 'CLOSED' AND NEW.end_kind = 'SHIPMENT_CANCELLED') THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% is %; its houses are frozen', NEW.tenant_id, NEW.consolidation_id, s
      USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_frozen';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER membership_entry_rules BEFORE UPDATE OR DELETE ON cargo_phy.membership_entry
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_membership_entry();

-- At commit, an entry inserted or still live has its consolidation OPEN, read under the consolidation's FOR
-- SHARE lock, which CloseConsolidation's FOR UPDATE waits for: eight attaches racing a close either commit
-- before it or are refused.
CREATE FUNCTION cargo_phy.check_live_entry_open() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  live boolean;
  s text;
BEGIN
  SELECT ended_at IS NULL INTO live FROM cargo_phy.membership_entry
    WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id AND stage = NEW.stage AND entry_no = NEW.entry_no;
  IF NOT live THEN
    RETURN NULL;
  END IF;
  SELECT state INTO s FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
  IF s IS DISTINCT FROM 'OPEN' THEN
    RAISE EXCEPTION 'cargo_phy: house %/% cannot join consolidation % while it is %', NEW.tenant_id, NEW.shipment_id, NEW.consolidation_id, s
      USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_consolidation_open';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER membership_entry_consolidation_open AFTER INSERT ON cargo_phy.membership_entry
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_live_entry_open();

-- Tenant isolation as in the other contexts: the tenant transactor sets app.tenant_id in every transaction.
DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['consolidation','consolidation_version','consol_membership','membership_entry','membership_entry_part','command_receipt'] LOOP
    EXECUTE format('ALTER TABLE cargo_phy.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_phy.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_phy.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
