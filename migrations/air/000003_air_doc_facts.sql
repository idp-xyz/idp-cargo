-- IDP-Cargo AIR V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The movement's facts finalized for DOC (AirDocFacts v1, relationship 11): each finalization stores the next
-- immutable version of the master and its houses, as the exact JSON published with its SHA-256 and the movement
-- revision it was written at, and the movement becomes FINAL; any later change returns it to OPEN and the next
-- finalization stores the next version. A house entry keeps the
-- goods description of each part, as FUL relays it from COM, and the lots' screening evidence FUL relays from PHY.
-- Existing movements stay OPEN; existing parts have no goods description and existing entries no screenings.

ALTER TABLE cargo_air.air_movement
  DROP CONSTRAINT air_movement_state_check,
  ADD CONSTRAINT air_movement_state_check CHECK (state IN ('OPEN','FINAL','CANCELLED'));
-- One movement in force (OPEN or FINAL) per air consolidation.
DROP INDEX cargo_air.air_movement_one_open;
CREATE UNIQUE INDEX air_movement_one_open ON cargo_air.air_movement (tenant_id, consolidation_id) WHERE state <> 'CANCELLED';

-- A finalization's receipt names the version it published.
ALTER TABLE cargo_air.command_receipt
  ADD COLUMN result_facts_version integer CHECK (result_facts_version > 0);

ALTER TABLE cargo_air.house_waybill_part
  ADD COLUMN goods_description text CHECK (length(btrim(goods_description)) BETWEEN 1 AND 500);

CREATE TABLE cargo_air.house_waybill_screening (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 entry_no integer NOT NULL,
 screening_no integer NOT NULL CHECK (screening_no > 0),
 lot_id text NOT NULL CHECK (length(btrim(lot_id)) BETWEEN 1 AND 128),
 outcome text NOT NULL CHECK (outcome IN ('PASSED','FAILED','PENDING','NOT_REQUIRED')),
 screener text NOT NULL CHECK (length(btrim(screener)) BETWEEN 1 AND 500),
 method text NOT NULL CHECK (length(btrim(method)) BETWEEN 1 AND 128),
 reference text NOT NULL CHECK (length(btrim(reference)) BETWEEN 1 AND 500),
 screened_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, movement_id, entry_no, screening_no),
 FOREIGN KEY (tenant_id, movement_id, entry_no) REFERENCES cargo_air.house_waybill (tenant_id, movement_id, entry_no)
);

CREATE TABLE cargo_air.air_doc_facts_version (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 content text NOT NULL,
 content_sha256 char(64) NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
 movement_revision bigint NOT NULL CHECK (movement_revision > 0),
 finalized_by text NOT NULL CHECK (length(btrim(finalized_by)) > 0),
 finalized_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, movement_id, version_no),
 FOREIGN KEY (tenant_id, movement_id) REFERENCES cargo_air.air_movement (tenant_id, movement_id),
 CONSTRAINT air_doc_facts_version_content CHECK (content_sha256 = encode(sha256(convert_to(content, 'UTF8')), 'hex')
   AND jsonb_typeof(content::jsonb) = 'object' AND (content::jsonb)->>'version' = version_no::text AND (content::jsonb)->>'movement_id' = movement_id
   AND jsonb_typeof((content::jsonb)->'houses') = 'array' AND jsonb_array_length((content::jsonb)->'houses') > 0),
 UNIQUE (tenant_id, movement_id, movement_revision)
);

DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['house_waybill_screening','air_doc_facts_version'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_air.%I FOR EACH ROW EXECUTE FUNCTION cargo_air.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- Only the state, cancellation, MAWB status, revision and updated_at of a movement change; only an OPEN movement
-- is cancelled, and a cancelled one never changes again.
CREATE OR REPLACE FUNCTION cargo_air.reject_movement_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','mawb_status','cancel_reason','cancelled_by','cancelled_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.state = 'CANCELLED' OR (NEW.state = 'CANCELLED' AND OLD.state <> 'OPEN')
     OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_air: movement %/% changes only its state, and never once cancelled', OLD.tenant_id, OLD.movement_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- At commit, a cancelled movement keeps no live house waybill; a movement's MAWB comes from its first link; its
-- doc facts versions are numbered from one; and a FINAL movement has a live house and is at the revision its latest
-- version was written at (every command moves the revision, so a FINAL movement has not changed since). The row
-- is read as it stands at commit.
CREATE OR REPLACE FUNCTION cargo_air.check_movement_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  m cargo_air.air_movement;
  versions integer;
  last_version integer;
  last_revision bigint;
BEGIN
  SELECT * INTO m FROM cargo_air.air_movement WHERE tenant_id = NEW.tenant_id AND movement_id = NEW.movement_id;
  SELECT count(*), coalesce(max(version_no), 0) INTO versions, last_version
    FROM cargo_air.air_doc_facts_version v WHERE v.tenant_id = m.tenant_id AND v.movement_id = m.movement_id;
  SELECT v.movement_revision INTO last_revision FROM cargo_air.air_doc_facts_version v
    WHERE v.tenant_id = m.tenant_id AND v.movement_id = m.movement_id AND v.version_no = last_version;
  IF m.state = 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_air.house_waybill e
       WHERE e.tenant_id = m.tenant_id AND e.movement_id = m.movement_id AND e.ended_at IS NULL)
     OR NOT EXISTS (SELECT 1 FROM cargo_air.booking_link l WHERE l.tenant_id = m.tenant_id AND l.movement_id = m.movement_id
       AND l.link_no = 1 AND l.booking_id = m.mawb_booking_id AND l.capacity_version = m.mawb_capacity_version)
     OR versions <> last_version
     OR m.state = 'FINAL' AND (versions = 0 OR last_revision IS DISTINCT FROM m.revision
       OR NOT EXISTS (SELECT 1 FROM cargo_air.house_waybill e WHERE e.tenant_id = m.tenant_id AND e.movement_id = m.movement_id
         AND e.ended_at IS NULL)) THEN
    RAISE EXCEPTION 'cargo_air: movement %/% is inconsistent at commit', m.tenant_id, m.movement_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'air_movement_consistent';
  END IF;
  RETURN NULL;
END $fn$;

-- At commit, a doc facts version a transaction writes is the latest of the movement, numbered next, and written
-- at the revision of the FINAL movement as it stands.
CREATE FUNCTION cargo_air.check_doc_facts_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_air.air_movement m WHERE m.tenant_id = NEW.tenant_id AND m.movement_id = NEW.movement_id AND m.state = 'FINAL'
       AND m.revision = NEW.movement_revision)
     OR (SELECT count(*) FROM cargo_air.air_doc_facts_version v WHERE v.tenant_id = NEW.tenant_id AND v.movement_id = NEW.movement_id) <> NEW.version_no
     OR EXISTS (SELECT 1 FROM cargo_air.air_doc_facts_version v WHERE v.tenant_id = NEW.tenant_id AND v.movement_id = NEW.movement_id
       AND v.version_no > NEW.version_no) THEN
    RAISE EXCEPTION 'cargo_air: doc facts version %/%/% is not the latest of a FINAL movement', NEW.tenant_id, NEW.movement_id, NEW.version_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'air_doc_facts_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER air_doc_facts_consistent AFTER INSERT ON cargo_air.air_doc_facts_version
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_air.check_doc_facts_at_commit();

-- At commit, a house's screening evidence and parts are written by the transaction that writes the entry: an
-- entry's facts never grow later.
CREATE FUNCTION cargo_air.check_entry_facts_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_air.house_waybill e WHERE e.tenant_id = NEW.tenant_id AND e.movement_id = NEW.movement_id
       AND e.entry_no = NEW.entry_no AND e.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_air: % of house waybill %/%/% are written with the entry', TG_TABLE_NAME, NEW.tenant_id, NEW.movement_id, NEW.entry_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'house_waybill_facts_with_entry';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER house_waybill_screening_with_entry AFTER INSERT ON cargo_air.house_waybill_screening
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_air.check_entry_facts_at_commit();
CREATE CONSTRAINT TRIGGER house_waybill_part_with_entry AFTER INSERT ON cargo_air.house_waybill_part
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_air.check_entry_facts_at_commit();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['house_waybill_screening','air_doc_facts_version'] LOOP
    EXECUTE format('ALTER TABLE cargo_air.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_air.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_air.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
