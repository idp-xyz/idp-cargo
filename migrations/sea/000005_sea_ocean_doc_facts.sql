-- IDP-Cargo SEA V0.2 (sea freight part 1e-iv) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The movement's facts finalized for DOC (OceanDocFacts v1, relationship 12). Each finalization stores the next
-- immutable version, the exact JSON published with its SHA-256 and the movement revision it was written at, and the
-- movement becomes FINAL. A later link, refresh or end returns it to OPEN; the next finalization stores the next
-- version. The published JSON carries a VGM assertion's identity and never the signatory's name.

ALTER TABLE cargo_sea.ocean_movement DROP CONSTRAINT ocean_movement_state_check;
ALTER TABLE cargo_sea.ocean_movement ADD CONSTRAINT ocean_movement_state_check
  CHECK (state IN ('OPEN','FINAL','CANCELLED'));
-- One movement in force (OPEN or FINAL) per sea consolidation.
DROP INDEX cargo_sea.ocean_movement_one_open;
CREATE UNIQUE INDEX ocean_movement_one_open ON cargo_sea.ocean_movement (tenant_id, consolidation_id) WHERE state <> 'CANCELLED';

ALTER TABLE cargo_sea.command_receipt ADD COLUMN result_facts_version integer CHECK (result_facts_version > 0);
ALTER TABLE cargo_sea.command_receipt ADD CONSTRAINT command_receipt_facts_of_movement
  CHECK (result_facts_version IS NULL OR result_type = 'OceanMovement');

CREATE TABLE cargo_sea.ocean_doc_facts_version (
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
 FOREIGN KEY (tenant_id, movement_id) REFERENCES cargo_sea.ocean_movement (tenant_id, movement_id),
 CONSTRAINT ocean_doc_facts_version_content CHECK (content_sha256 = encode(sha256(convert_to(content, 'UTF8')), 'hex')
   AND jsonb_typeof(content::jsonb) = 'object' AND (content::jsonb)->>'version' = version_no::text
   AND (content::jsonb)->>'movement_id' = movement_id
   AND jsonb_typeof((content::jsonb)->'houses') = 'array' AND jsonb_array_length((content::jsonb)->'houses') > 0
   AND NOT (content LIKE '%signatory%')),
 UNIQUE (tenant_id, movement_id, movement_revision)
);
CREATE TRIGGER ocean_doc_facts_version_immutable BEFORE UPDATE OR DELETE ON cargo_sea.ocean_doc_facts_version
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.reject_row_change();

-- Only an OPEN movement is cancelled, and a cancelled one never changes again.
CREATE OR REPLACE FUNCTION cargo_sea.reject_movement_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','cancel_reason','cancelled_by','cancelled_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.state = 'CANCELLED' OR (NEW.state = 'CANCELLED' AND OLD.state <> 'OPEN')
     OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_sea: movement %/% changes only its state, and never once cancelled', OLD.tenant_id, OLD.movement_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- A FINAL movement has its latest facts version written at its current revision, and a live house. Versions are
-- numbered from one without gaps.
CREATE OR REPLACE FUNCTION cargo_sea.check_movement_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  m cargo_sea.ocean_movement;
  versions integer;
  last_version integer;
  last_revision bigint;
BEGIN
  IF NOT cargo_sea.movement_consistent(NEW.tenant_id, NEW.movement_id) THEN
    RAISE EXCEPTION 'cargo_sea: movement %/% is inconsistent at commit', NEW.tenant_id, NEW.movement_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_movement_consistent';
  END IF;
  SELECT * INTO m FROM cargo_sea.ocean_movement WHERE tenant_id = NEW.tenant_id AND movement_id = NEW.movement_id;
  SELECT count(*), coalesce(max(version_no), 0) INTO versions, last_version
    FROM cargo_sea.ocean_doc_facts_version v WHERE v.tenant_id = m.tenant_id AND v.movement_id = m.movement_id;
  SELECT v.movement_revision INTO last_revision FROM cargo_sea.ocean_doc_facts_version v
    WHERE v.tenant_id = m.tenant_id AND v.movement_id = m.movement_id AND v.version_no = last_version;
  IF versions <> last_version OR m.state = 'FINAL' AND (versions = 0 OR last_revision IS DISTINCT FROM m.revision
       OR NOT EXISTS (SELECT 1 FROM cargo_sea.house_bill e WHERE e.tenant_id = m.tenant_id AND e.movement_id = m.movement_id
         AND e.ended_at IS NULL)) THEN
    RAISE EXCEPTION 'cargo_sea: movement %/% facts are not the latest of a FINAL movement with a house', m.tenant_id, m.movement_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_movement_consistent';
  END IF;
  RETURN NULL;
END $fn$;

CREATE FUNCTION cargo_sea.check_ocean_doc_facts_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_sea.ocean_movement m WHERE m.tenant_id = NEW.tenant_id AND m.movement_id = NEW.movement_id
       AND m.state = 'FINAL' AND m.revision = NEW.movement_revision)
     OR (SELECT count(*) FROM cargo_sea.ocean_doc_facts_version v WHERE v.tenant_id = NEW.tenant_id AND v.movement_id = NEW.movement_id) <> NEW.version_no
     OR EXISTS (SELECT 1 FROM cargo_sea.ocean_doc_facts_version v WHERE v.tenant_id = NEW.tenant_id AND v.movement_id = NEW.movement_id
       AND v.version_no > NEW.version_no) THEN
    RAISE EXCEPTION 'cargo_sea: ocean doc facts %/%/% is not the latest of a FINAL movement', NEW.tenant_id, NEW.movement_id, NEW.version_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_doc_facts_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER ocean_doc_facts_consistent AFTER INSERT ON cargo_sea.ocean_doc_facts_version
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_ocean_doc_facts_at_commit();

ALTER TABLE cargo_sea.ocean_doc_facts_version ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_sea.ocean_doc_facts_version FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_sea.ocean_doc_facts_version
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
