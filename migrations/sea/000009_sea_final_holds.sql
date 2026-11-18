-- IDP-Cargo SEA V0.2 (corrections after the review of 2026-11-18) / PostgreSQL 16.
-- Run only as the migrator role, never at service start.
-- What a write past SEA could still do, found by the independent review of V2.64 to V2.70 with direct writes on the
-- application role: leave a FINAL movement whose published facts no longer hold, and read a signatory's name without
-- a log. SEA's own commands could do neither. Also: a goods description containing the word "signatory" could not
-- be published. 000001 through 000008 are unchanged.

-- 1. A house is ended only while its movement is OPEN at commit: SEA's refresh and end return a FINAL movement to
-- OPEN in the same transaction. Before, nothing checked an end at commit, so the only live house of a FINAL movement
-- could be ended and the movement stayed FINAL, its published facts still listing the house. The movement is read
-- under a share lock, which a finalization waits for and which waits for a finalization.
CREATE FUNCTION cargo_sea.check_house_bill_end_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  movement_state text;
BEGIN
  SELECT m.state INTO movement_state FROM cargo_sea.ocean_movement m
    WHERE m.tenant_id = NEW.tenant_id AND m.movement_id = NEW.movement_id FOR SHARE;
  IF movement_state IS DISTINCT FROM 'OPEN' THEN
    RAISE EXCEPTION 'cargo_sea: house bill %/%/% is ended while its movement is open', NEW.tenant_id, NEW.movement_id, NEW.entry_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'house_bill_ended_on_open_movement';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER house_bill_ended_on_open_movement AFTER UPDATE ON cargo_sea.house_bill
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_house_bill_end_at_commit();

-- 2. A container is assigned, stuffed or released only while its movement is not FINAL at commit: each changes the
-- containers the published facts list, and SEA refuses all three on a FINAL movement. Before, the check of a visit
-- refused them on a cancelled movement only. The rest of the function is 000002's. visit_inconsistency holds the
-- movement's share lock, so the state read after it is the state at commit.
CREATE OR REPLACE FUNCTION cargo_sea.check_visit_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_sea.container_visit;
  broken text;
BEGIN
  SELECT * INTO v FROM cargo_sea.container_visit WHERE tenant_id = NEW.tenant_id AND visit_id = NEW.visit_id;
  broken := cargo_sea.visit_inconsistency(v);
  IF broken IS NULL AND TG_OP = 'UPDATE' AND OLD.status = 'ASSIGNED' AND NEW.status = 'STUFFED' AND NOT EXISTS (
       SELECT 1 FROM cargo_sea.house_bill e JOIN cargo_sea.house_bill_stuffing s
         ON s.tenant_id = e.tenant_id AND s.movement_id = e.movement_id AND s.entry_no = e.entry_no
       WHERE e.tenant_id = NEW.tenant_id AND e.movement_id = NEW.movement_id AND e.ended_at IS NULL
         AND s.execution_id = NEW.execution_id AND s.load_unit_no = NEW.load_unit_no AND s.sealed_at <= NEW.stuffed_at) THEN
    broken := 'a container is stuffed once a live house of its movement carries a sealed stuffing of its load unit';
  END IF;
  IF broken IS NULL AND (TG_OP = 'INSERT' OR (OLD.status IS DISTINCT FROM NEW.status AND NEW.status IN ('STUFFED','RELEASED')))
     AND (SELECT m.state FROM cargo_sea.ocean_movement m
          WHERE m.tenant_id = NEW.tenant_id AND m.movement_id = NEW.movement_id) IS NOT DISTINCT FROM 'FINAL' THEN
    broken := 'a container is assigned, stuffed or released while the movement is not final';
  END IF;
  IF broken IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_sea: container visit %/%: %', NEW.tenant_id, NEW.visit_id, broken
      USING ERRCODE = 'check_violation', CONSTRAINT = 'container_visit_consistent';
  END IF;
  RETURN NULL;
END $fn$;

-- 3. Every change of a movement is a step of its revision by one, as SEA writes it. Before, the revision could stay
-- or go back: a FINAL movement returned to OPEN, changed and made FINAL again at the revision its facts were
-- published at passed the check of a FINAL movement, on facts that no longer held. With the step, a movement made
-- FINAL again is at a later revision and needs a facts version written at it. The rest is 000005's.
CREATE OR REPLACE FUNCTION cargo_sea.reject_movement_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','cancel_reason','cancelled_by','cancelled_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.state = 'CANCELLED' OR (NEW.state = 'CANCELLED' AND OLD.state <> 'OPEN')
     OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_sea: movement %/% changes only its state, and never once cancelled', OLD.tenant_id, OLD.movement_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.revision IS DISTINCT FROM OLD.revision + 1 THEN
    RAISE EXCEPTION 'cargo_sea: movement %/% changes with a step of its revision by one', OLD.tenant_id, OLD.movement_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- 4. Published facts have no member named for a signatory, at any depth. Before, the check refused the word anywhere
-- in the text, so a house whose goods description contained it could never be finalized. The check is a backstop:
-- SEA's facts have no such member. The other conditions are 000005's.
CREATE FUNCTION cargo_sea.json_names_a_signatory(doc jsonb) RETURNS boolean LANGUAGE sql IMMUTABLE AS $fn$
  SELECT EXISTS (SELECT 1 FROM jsonb_path_query(doc, '$.**') AS node,
      LATERAL jsonb_object_keys(CASE WHEN jsonb_typeof(node) = 'object' THEN node ELSE '{}'::jsonb END) AS member
    WHERE member ILIKE '%signatory%')
$fn$;
ALTER TABLE cargo_sea.ocean_doc_facts_version DROP CONSTRAINT ocean_doc_facts_version_content;
ALTER TABLE cargo_sea.ocean_doc_facts_version ADD CONSTRAINT ocean_doc_facts_version_content
  CHECK (content_sha256 = encode(sha256(convert_to(content, 'UTF8')), 'hex')
   AND jsonb_typeof(content::jsonb) = 'object' AND (content::jsonb)->>'version' = version_no::text
   AND (content::jsonb)->>'movement_id' = movement_id
   AND jsonb_typeof((content::jsonb)->'houses') = 'array' AND jsonb_array_length((content::jsonb)->'houses') > 0
   AND NOT cargo_sea.json_names_a_signatory(content::jsonb));

-- 5. A signatory's name is returned only against a read that was logged and committed before. Before, one function
-- wrote the log and returned the name in the caller's transaction: a caller that rolled the transaction back, or a
-- savepoint within it, kept the name and left no log. Now the read is two steps in two transactions. The first logs
-- the read and returns the log entry's id. The second returns the name for an entry written by another transaction,
-- which is therefore committed, within a minute of it. An entry may be read against more than once within that
-- minute, and an entry whose second step fails stays in the log: the log never has fewer reads than there were.
ALTER TABLE cargo_sea.vgm_signatory_access ADD COLUMN written_xact xid8; -- null on the entries written before
CREATE OR REPLACE FUNCTION cargo_sea.stamp_vgm_signatory_access() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  NEW.recorded_at := now();
  NEW.written_xact := pg_current_xact_id();
  RETURN NEW;
END $fn$;

-- The first step. An unknown assertion returns NULL and writes no log. The caller cannot choose the tenant: it is
-- the transaction's app.tenant_id.
CREATE FUNCTION cargo_sea.log_vgm_signatory_read(visit text, version integer, actor text, purpose text) RETURNS uuid
  LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, cargo_sea AS $fn$
DECLARE
  tenant text := current_setting('app.tenant_id', true);
  logged uuid;
BEGIN
  IF tenant IS NULL OR btrim(tenant) = '' OR purpose IS NULL OR purpose NOT IN ('CUSTOMS_AUDIT','LEGAL_CLAIM','DATA_SUBJECT_REQUEST')
     OR actor IS NULL OR length(btrim(actor)) = 0 THEN
    RAISE EXCEPTION 'cargo_sea: a signatory is read for a customs audit, a legal claim or the person''s own request'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_signatory_access_purpose';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM cargo_sea.vgm_signatory s
       WHERE s.tenant_id = tenant AND s.visit_id = visit AND s.version = log_vgm_signatory_read.version) THEN
    RETURN NULL;
  END IF;
  INSERT INTO cargo_sea.vgm_signatory_access (tenant_id, visit_id, version, actor_id, purpose)
    VALUES (tenant, visit, log_vgm_signatory_read.version, actor, purpose) RETURNING access_id INTO logged;
  RETURN logged;
END $fn$;
REVOKE ALL ON FUNCTION cargo_sea.log_vgm_signatory_read(text, integer, text, text) FROM PUBLIC;

-- The second step: the name of the assertion a log entry names, or NULL. The entry is of the caller's tenant, was
-- written by another transaction (a row of another transaction is seen only once it is committed), and is less than
-- a minute old by the clock, not by this transaction's start.
CREATE FUNCTION cargo_sea.read_logged_vgm_signatory(access uuid) RETURNS text
  LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, cargo_sea AS $fn$
  SELECT s.name FROM cargo_sea.vgm_signatory_access a
    JOIN cargo_sea.vgm_signatory s ON s.tenant_id = a.tenant_id AND s.visit_id = a.visit_id AND s.version = a.version
    WHERE a.access_id = access AND a.tenant_id = current_setting('app.tenant_id', true)
      AND a.written_xact IS NOT NULL AND a.written_xact <> pg_current_xact_id()
      AND a.recorded_at > clock_timestamp() - interval '1 minute'
$fn$;
REVOKE ALL ON FUNCTION cargo_sea.read_logged_vgm_signatory(uuid) FROM PUBLIC;

DROP FUNCTION cargo_sea.read_vgm_signatory(text, integer, text, text);
