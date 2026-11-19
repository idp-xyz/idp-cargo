-- IDP-Cargo PHY V0.4 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A stuffing given up (CancelLoadExecution; found by the review of V2.80). A stuffing opened on a container and left
-- empty could be neither sealed nor removed with its unit, so the unit stayed in use for ever. Now a STUFFING that is
-- OPEN, LOADING or ON_HOLD and holds no live packs (nothing loaded, or every loaded pack unloaded again) becomes
-- CANCELLED: terminal, kept with its lines, reversals and holds as history, with who gave it up, when (business time
-- and entered time) and why. Its container is planned again: the one-stuffing-per-unit index and the removal of a unit
-- look at the stuffings that are not cancelled only, so the unit takes a new stuffing (a new execution) or is removed.
-- A cancelled execution changes no more and takes no line, reversal, hold, release of a hold or evidence. The database
-- keeps "cancelled without live packs" as it keeps the other facts about an execution's lines (conservation, no
-- reversal in a sealed container). A cancellation dated not before the execution's own lines, reversals and holds is
-- the domain's, as it is for a seal: the database checks a seal against its consolidation's versions and against the
-- start (closed_at >= started_at), not against the execution's lines, and checks a cancellation against the opening
-- and the start the same way. A cancellation changes nothing of the row but the state, the four cancellation columns,
-- revision and updated_at, as a closed row changes only its revision. The
-- order of business times around a cancellation is kept as it is around the other changes of a consolidation: a new
-- stuffing of a unit, and the unit's removal, are dated not before the latest cancellation of a stuffing of the unit.
-- No existing row changes. Part A of V2.82 (a lot's measure over several containers) needs no migration.
--
-- Objects of earlier migrations replaced here, each with every rule of its latest definition:
--   cargo_phy.check_load_execution_change()   latest in 000004_phy_stuffing (first in 000002_phy_cargo_lots_tender)
--   cargo_phy.check_consolidation_version()   latest in 000002_phy_cargo_lots_tender
--   cargo_phy.check_reopen_after_stuffing()   latest in 000004_phy_stuffing
--   cargo_phy.check_unit_not_in_use()         latest in 000004_phy_stuffing
-- Constraints and the index replaced: load_execution_state_check (000002), load_execution_started (000002) and
-- load_execution_one_per_unit (000004).
--
-- Locks: the ALTER TABLE statements take ACCESS EXCLUSIVE on cargo_phy.load_execution for their own duration, and
-- the new and replaced CHECK constraints scan the table once to validate it; ADD COLUMN without a default rewrites no
-- table. DROP INDEX takes ACCESS EXCLUSIVE and CREATE UNIQUE INDEX SHARE (writes wait, reads do not) on the table, one
-- scan for the index; CREATE TRIGGER takes SHARE ROW EXCLUSIVE on cargo_phy.hold_entry. CREATE OR REPLACE FUNCTION locks
-- no table. No table is rewritten.

ALTER TABLE cargo_phy.load_execution
  DROP CONSTRAINT load_execution_state_check,
  ADD CONSTRAINT load_execution_state_check CHECK (state IN ('OPEN','LOADING','ON_HOLD','CLOSED','CANCELLED')),
  ADD COLUMN cancelled_by text CHECK (length(btrim(cancelled_by)) > 0),
  ADD COLUMN cancelled_at timestamptz,
  ADD COLUMN cancelled_entered_at timestamptz,
  ADD COLUMN cancel_reason text CHECK (length(btrim(cancel_reason)) BETWEEN 1 AND 500),
  -- A stuffing given up while OPEN never started; one given up while LOADING or ON_HOLD did.
  DROP CONSTRAINT load_execution_started,
  ADD CONSTRAINT load_execution_started CHECK ((state = 'CANCELLED' OR (state = 'OPEN') = (started_at IS NULL))
    AND (started_by IS NULL) = (started_at IS NULL) AND (started_entered_at IS NULL) = (started_at IS NULL)
    AND (started_at IS NULL OR started_at >= opened_at)),
  ADD CONSTRAINT load_execution_cancelled CHECK ((state = 'CANCELLED') = (cancelled_at IS NOT NULL)
    AND (cancelled_by IS NULL) = (cancelled_at IS NULL) AND (cancelled_entered_at IS NULL) = (cancelled_at IS NULL)
    AND (cancel_reason IS NULL) = (cancelled_at IS NULL) AND (state <> 'CANCELLED' OR kind = 'STUFFING')
    AND (cancelled_at IS NULL OR (cancelled_at >= opened_at AND (started_at IS NULL OR cancelled_at >= started_at))));

-- A unit has at most one stuffing that is not cancelled. The index keeps its name, which PHY's store reads.
DROP INDEX cargo_phy.load_execution_one_per_unit;
CREATE UNIQUE INDEX load_execution_one_per_unit ON cargo_phy.load_execution (tenant_id, consolidation_id, load_unit_no)
  WHERE load_unit_no IS NOT NULL AND state <> 'CANCELLED';

-- A load execution keeps its consolidation, kind, mode, facility and unit; its state moves OPEN, LOADING, ON_HOLD and
-- back, CLOSED, or, for a stuffing with no live packs, CANCELLED; a closed or cancelled execution changes only its
-- revision (a cancelled one not even that). Executions are never deleted. A new tender tenders an AIR_CONSOL or
-- SEA_LCL_COLOAD consolidation that is OPEN or CLOSED; a new stuffing fills a PLANNED container of a CLOSED
-- SEA_LCL_OWN consolidation, opened not before the consolidation's latest version or unit change, and is sealed while
-- the consolidation is CLOSED. The consolidation is read under its FOR SHARE lock, which a unit's removal (FOR NO KEY
-- UPDATE) and a reopen wait for. A cancellation takes the execution row's lock, as a line, a reversal and a hold do
-- (check_execution_addition), so a load and a cancellation of one stuffing wait for each other.
CREATE OR REPLACE FUNCTION cargo_phy.check_load_execution_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  fixed text[] := ARRAY['tenant_id','execution_id','consolidation_id','kind','transport_mode','facility_location','facility_code',
    'load_unit_no','opened_by','opened_at','opened_entered_at','created_at'];
  k text;
  c cargo_phy.consolidation;
  u cargo_phy.planned_load_unit;
  live bigint;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% cannot be deleted', OLD.tenant_id, OLD.execution_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'INSERT' THEN
    SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
    IF NEW.kind = 'STUFFING' THEN
      SELECT * INTO u FROM cargo_phy.planned_load_unit
        WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id AND unit_no = NEW.load_unit_no;
      IF c.consolidation_id IS NULL OR c.kind <> 'SEA_LCL_OWN' OR c.state <> 'CLOSED' OR c.transport_mode <> NEW.transport_mode
         OR NEW.state <> 'OPEN' OR u.status IS DISTINCT FROM 'PLANNED'
         OR NEW.opened_at < greatest((SELECT max(v.written_at) FROM cargo_phy.consolidation_version v
               WHERE v.tenant_id = NEW.tenant_id AND v.consolidation_id = NEW.consolidation_id),
             (SELECT max(greatest(p.planned_at, p.removed_at)) FROM cargo_phy.planned_load_unit p
               WHERE p.tenant_id = NEW.tenant_id AND p.consolidation_id = NEW.consolidation_id),
             (SELECT max(x.cancelled_at) FROM cargo_phy.load_execution x WHERE x.tenant_id = NEW.tenant_id
               AND x.consolidation_id = NEW.consolidation_id AND x.load_unit_no = NEW.load_unit_no)) THEN
        RAISE EXCEPTION 'cargo_phy: consolidation %/% takes no stuffing of unit % now', NEW.tenant_id, NEW.consolidation_id, NEW.load_unit_no
          USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_stuffing';
      END IF;
      RETURN NEW;
    END IF;
    IF c.consolidation_id IS NULL OR c.kind NOT IN ('AIR_CONSOL','SEA_LCL_COLOAD') OR c.state NOT IN ('OPEN','CLOSED')
       OR c.transport_mode <> NEW.transport_mode OR NEW.state <> 'OPEN' THEN
      RAISE EXCEPTION 'cargo_phy: consolidation %/% takes no tender now', NEW.tenant_id, NEW.consolidation_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_consolidation';
    END IF;
    RETURN NEW;
  END IF;
  FOREACH k IN ARRAY fixed LOOP
    IF to_jsonb(OLD)->k IS DISTINCT FROM to_jsonb(NEW)->k THEN
      RAISE EXCEPTION 'cargo_phy: load execution %/% keeps its %', OLD.tenant_id, OLD.execution_id, k USING ERRCODE = 'restrict_violation';
    END IF;
  END LOOP;
  IF OLD.state = 'CANCELLED' THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% is cancelled and changes no more', OLD.tenant_id, OLD.execution_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF (OLD.state = 'CLOSED' AND to_jsonb(OLD) - ARRAY['revision','updated_at'] IS DISTINCT FROM to_jsonb(NEW) - ARRAY['revision','updated_at'])
     OR (OLD.state <> 'OPEN' AND NEW.state = 'OPEN') OR (OLD.state = 'OPEN' AND NEW.state NOT IN ('OPEN','LOADING','CANCELLED'))
     OR (OLD.state = 'ON_HOLD' AND NEW.state = 'CLOSED') THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% cannot go from % to %', OLD.tenant_id, OLD.execution_id, OLD.state, NEW.state
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.state = 'CANCELLED' THEN
    IF to_jsonb(OLD) - ARRAY['state','cancelled_by','cancelled_at','cancelled_entered_at','cancel_reason','revision','updated_at']
       IS DISTINCT FROM to_jsonb(NEW) - ARRAY['state','cancelled_by','cancelled_at','cancelled_entered_at','cancel_reason','revision','updated_at'] THEN
      RAISE EXCEPTION 'cargo_phy: load execution %/% is cancelled and keeps the rest of its row', NEW.tenant_id, NEW.execution_id
        USING ERRCODE = 'restrict_violation';
    END IF;
    SELECT coalesce(sum(l.packs), 0) - coalesce((SELECT sum(r.packs) FROM cargo_phy.reversal_line r
        WHERE r.tenant_id = NEW.tenant_id AND r.execution_id = NEW.execution_id), 0) INTO live
      FROM cargo_phy.load_line l WHERE l.tenant_id = NEW.tenant_id AND l.execution_id = NEW.execution_id;
    IF live > 0 THEN
      RAISE EXCEPTION 'cargo_phy: load execution %/% has % live packs and is not given up', NEW.tenant_id, NEW.execution_id, live
        USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_cancelled_packs';
    END IF;
  END IF;
  IF NEW.kind = 'STUFFING' AND NEW.state = 'CLOSED' AND OLD.state <> 'CLOSED' THEN
    SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
    IF c.state IS DISTINCT FROM 'CLOSED' OR NEW.closed_at < (SELECT max(v.written_at) FROM cargo_phy.consolidation_version v
         WHERE v.tenant_id = NEW.tenant_id AND v.consolidation_id = NEW.consolidation_id) THEN
      RAISE EXCEPTION 'cargo_phy: consolidation %/% is %; its containers are sealed once it is CLOSED, after its latest version', NEW.tenant_id, NEW.consolidation_id,
        c.state USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_seal_frozen';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;

-- A hold is not released on an execution that is already cancelled: the hold ended with the execution, and releasing
-- the last hold would resume loading. The execution row is read under a share lock, so a release and a cancellation
-- wait for each other: a release that committed first is history (released, then cancelled); one behind a cancellation
-- is refused.
CREATE FUNCTION cargo_phy.check_hold_release_not_cancelled() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s text;
BEGIN
  SELECT x.state INTO s FROM cargo_phy.load_execution x WHERE x.tenant_id = NEW.tenant_id AND x.execution_id = NEW.execution_id FOR SHARE;
  IF s = 'CANCELLED' THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% is cancelled; its holds are not released', NEW.tenant_id, NEW.execution_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'hold_entry_cancelled';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER hold_entry_cancelled BEFORE UPDATE ON cargo_phy.hold_entry
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_hold_release_not_cancelled();

-- A consolidation version follows the previous one (as in V0.1), and: a reopen comes before any tendered pack, a
-- cancellation and a reconciliation leave no load execution open: a closed one is done, and so is a cancelled one.
CREATE OR REPLACE FUNCTION cargo_phy.check_consolidation_version() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
          WHEN 'CANCELLED' THEN prev.state = 'OPEN' WHEN 'REOPENED' THEN prev.state = 'CLOSED'
          WHEN 'RECONCILED' THEN prev.state = 'CLOSED' ELSE false END))
     OR (c.transport_mode = 'AIR') <> (NEW.origin_iata IS NOT NULL) THEN
    RAISE EXCEPTION 'cargo_phy: version % does not follow consolidation %/%', NEW.version_no, NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.change IN ('CANCELLED','RECONCILED') AND EXISTS (SELECT 1 FROM cargo_phy.load_execution x
       WHERE x.tenant_id = NEW.tenant_id AND x.consolidation_id = NEW.consolidation_id AND x.state NOT IN ('CLOSED','CANCELLED')) THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% has a load execution open', NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_executions_closed';
  END IF;
  IF NEW.change = 'REOPENED' AND EXISTS (SELECT 1 FROM cargo_phy.load_execution x JOIN cargo_phy.load_line l
       ON l.tenant_id = x.tenant_id AND l.execution_id = x.execution_id
       WHERE x.tenant_id = NEW.tenant_id AND x.consolidation_id = NEW.consolidation_id AND x.state = 'CLOSED'
         AND l.packs > coalesce((SELECT sum(r.packs) FROM cargo_phy.reversal_line r
           WHERE r.tenant_id = l.tenant_id AND r.execution_id = l.execution_id AND r.line_no = l.line_no), 0)) THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% has tendered cargo; it cannot reopen', NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_reopen_before_tender';
  END IF;
  RETURN NEW;
END $fn$;

-- A consolidation does not reopen once a container of it is sealed, nor with a reopen dated before its stuffings'
-- changes, a cancellation of one included. It runs after consolidation_version_rules, which locks the consolidation row.
CREATE OR REPLACE FUNCTION cargo_phy.check_reopen_after_stuffing() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  latest timestamptz;
BEGIN
  IF NEW.change <> 'REOPENED' THEN
    RETURN NEW;
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_phy.load_execution x WHERE x.tenant_id = NEW.tenant_id AND x.consolidation_id = NEW.consolidation_id
       AND x.kind = 'STUFFING' AND x.state = 'CLOSED') THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% has a sealed container; it cannot reopen', NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_reopen_before_seal';
  END IF;
  SELECT greatest(max(x.opened_at), max(x.started_at), max(x.cancelled_at),
      max((SELECT max(l.loaded_at) FROM cargo_phy.load_line l WHERE l.tenant_id = x.tenant_id AND l.execution_id = x.execution_id)),
      max((SELECT max(r.reversed_at) FROM cargo_phy.reversal_line r WHERE r.tenant_id = x.tenant_id AND r.execution_id = x.execution_id)),
      max((SELECT max(greatest(h.placed_at, h.released_at)) FROM cargo_phy.hold_entry h
        WHERE h.tenant_id = x.tenant_id AND h.execution_id = x.execution_id)))
    INTO latest FROM cargo_phy.load_execution x
    WHERE x.tenant_id = NEW.tenant_id AND x.consolidation_id = NEW.consolidation_id AND x.kind = 'STUFFING';
  IF NEW.written_at < latest THEN
    RAISE EXCEPTION 'cargo_phy: version % of consolidation %/% is dated before its stuffing', NEW.version_no, NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_reopen_after_stuffing';
  END IF;
  RETURN NEW;
END $fn$;

-- A unit with a stuffing opened on it that is not cancelled is not removed; a unit whose stuffings are all cancelled is,
-- dated not before the latest of those cancellations.
CREATE OR REPLACE FUNCTION cargo_phy.check_unit_not_in_use() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.status = 'REMOVED' AND EXISTS (SELECT 1 FROM cargo_phy.load_execution x WHERE x.tenant_id = NEW.tenant_id
       AND x.consolidation_id = NEW.consolidation_id AND x.load_unit_no = NEW.unit_no AND x.state <> 'CANCELLED') THEN
    RAISE EXCEPTION 'cargo_phy: load unit %/%/% has its stuffing', NEW.tenant_id, NEW.consolidation_id, NEW.unit_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'planned_load_unit_in_use';
  END IF;
  IF NEW.status = 'REMOVED' AND NEW.removed_at < (SELECT max(x.cancelled_at) FROM cargo_phy.load_execution x WHERE x.tenant_id = NEW.tenant_id
       AND x.consolidation_id = NEW.consolidation_id AND x.load_unit_no = NEW.unit_no) THEN
    RAISE EXCEPTION 'cargo_phy: load unit %/%/% is removed before the cancellation of its stuffing', NEW.tenant_id, NEW.consolidation_id, NEW.unit_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'planned_load_unit_cancelled_time';
  END IF;
  RETURN NEW;
END $fn$;
