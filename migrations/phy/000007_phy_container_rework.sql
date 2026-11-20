-- IDP-Cargo PHY V0.5 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A sealed container is opened for rework (V2.85; the first of the steps of "a house leaves a sealed container"). A
-- STUFFING in state CLOSED, its consolidation CLOSED and no carrier evidence recorded on it, goes to the new state
-- REWORK on the SAME execution (SEA keeps the execution id of the stuffing, so never a new execution on the unit): its
-- seal is broken and moves into a new table, cargo_phy.stuffing_rework, one row per rework of an execution, numbered 1,
-- 2, ..., which also is the history of the seals the execution carried: who asked for the rework, when (business and
-- entered time) and why, the broken seal (the whole close evidence, who sealed and when) and, once it ends, how:
-- sealed again (who, when, the new seal's reference) or the stuffing given up.
--
-- In REWORK the execution has no close facts, takes no line, hold, carrier evidence or discrepancy, and takes reversals
-- only of whole lines for the reason UNSTUFFED. A reversal written while the execution is in REWORK records the rework
-- it was written in (reversal_line.rework_no, set by a trigger from the execution's state at the insert, never from the
-- statement), and at commit nothing of its house is left loaded in the consolidation (a whole house, in one command),
-- whatever the state of the execution is by then: a reseal in the same transaction does not excuse it. A closed lot of
-- the unloaded house is opened again by the same transaction: cargo_phy.cargo_lot_reopen records the lot's closing that
-- was undone, who reopened it and when, the stuffing, its rework and the reversal that went with it, and the lot's
-- CLOSED state changes only together with such a row (and the reversal) written in the same transaction; no other
-- change of a closed lot is accepted. A REWORK execution is sealed again (CLOSED, with a seal that differs from every
-- seal it carried, and cargo in it) or, with no live packs, given up (CANCELLED), and goes nowhere else. Seals are
-- compared by their letters and digits in capitals, keeping what is ASCII, so seal-1, SEAL 1 and SEAL1 are one seal
-- (cargo_phy.seal_key; the domain's sealKey is the same function; it is exactly the ASCII part of SEA's sealKey,
-- sea/internal/domain/visit.go, so two seals SEA calls equal are equal here, and some that differ only in non-ASCII
-- letters are equal here too: it is the stricter rule, and the domain refuses first); a seal whose reference has a
-- Unicode white space at either end (what Go's strings.TrimSpace trims) is not accepted when a stuffing is sealed (rows
-- sealed before this migration are not looked at). A consolidation with a container in REWORK is not reopened; it is
-- not cancelled or reconciled either, which the rule of 000006 (every execution CLOSED or CANCELLED) already refuses. A
-- stuffing in REWORK counts as the unit's stuffing that is not cancelled (000006).
--
-- What the database does not keep is what the domain keeps: that a rework is asked for a reason, the dates against the
-- execution's own lines and holds (the database checks the rework's dates against the seal it broke and the seal
-- against the consolidation's versions), which house the warehouse confirmed, and that the lots of the house were read
-- and locked first. "The same transaction" is told by xmin = pg_current_xact_id(), the top-level transaction's id: a
-- row written inside a savepoint carries the subtransaction's id and is refused, though it is of the same transaction.
-- The test is never permissive, and the commands use no savepoints. No existing row changes.
--
-- Objects of earlier migrations replaced here, each with every rule of its latest definition (file of that definition):
--   cargo_phy.check_load_execution_change()   000006_phy_cancelled_stuffing (first: 000002_phy_cargo_lots_tender)
--   cargo_phy.check_execution_addition()      000002_phy_cargo_lots_tender (the only one)
--   cargo_phy.check_cargo_lot_change()        000002_phy_cargo_lots_tender (the only one)
--   cargo_phy.check_reopen_after_stuffing()   000006_phy_cancelled_stuffing (first: 000004_phy_stuffing)
-- Constraint replaced: load_execution_state_check (000006).
--
-- Not replaced, and checked: check_sealed_reversal (000004) still refuses every reversal on a CLOSED stuffing, and
-- check_stuffing_line (000004) is not reached by a line on a REWORK execution, which check_execution_addition refuses
-- first; check_consolidation_version (000006) already refuses a cancel or reconcile while an execution is neither
-- CLOSED nor CANCELLED; check_unit_not_in_use and load_execution_one_per_unit (000006) already count every state but
-- CANCELLED.
--
-- Locks: ALTER TABLE takes ACCESS EXCLUSIVE on cargo_phy.load_execution and cargo_phy.reversal_line for its own
-- duration; the replaced and new CHECK constraints scan load_execution once to validate it, and the new column of
-- reversal_line has no default, so that table is not rewritten. The new tables are empty, so their constraints and
-- indexes cost nothing. CREATE TRIGGER takes SHARE ROW EXCLUSIVE on the table it is created on (load_execution,
-- reversal_line and the two new tables; none is created on cargo_lot, which only has a foreign key from
-- cargo_lot_reopen, and that takes a row lock on the lots it checks). CREATE OR REPLACE FUNCTION locks no table. No
-- table is rewritten.

ALTER TABLE cargo_phy.load_execution
  DROP CONSTRAINT load_execution_state_check,
  ADD CONSTRAINT load_execution_state_check CHECK (state IN ('OPEN','LOADING','ON_HOLD','CLOSED','CANCELLED','REWORK')),
  -- A container in rework is a started stuffing; its close facts are NULL as for every state but CLOSED
  -- (load_execution_closed, which is unchanged) and its seal is in cargo_phy.stuffing_rework.
  ADD CONSTRAINT load_execution_rework CHECK (state <> 'REWORK' OR (kind = 'STUFFING' AND started_at IS NOT NULL));

-- The rework a reversal was written in, set by cargo_phy.set_reversal_rework() and never changed (the table is
-- immutable).
ALTER TABLE cargo_phy.reversal_line ADD COLUMN rework_no integer CHECK (rework_no > 0);

-- A seal's key: its letters and digits in capitals, keeping what is ASCII, so that the same seal keyed another way is
-- the same seal. It is the domain's sealKey (phy/internal/domain/execution.go) and does not depend on the database's
-- locale (collation "C", an explicit character range and a translate, not [:alnum:] and upper()). The two letters whose
-- capitals are ASCII but which are not, U+0131 (dotless i) and U+017F (long s), are mapped to I and S first. That makes
-- the key exactly the ASCII part of SEA's sealKey (sea/internal/domain/visit.go: every Unicode letter and digit, in
-- capitals): two references that SEA calls equal have the same key here (U+0345 COMBINING GREEK YPOGEGRAMMENI, which
-- SEA drops and a [:alnum:] key kept, is dropped), and two that SEA keeps apart only because of a non-ASCII letter (an
-- accented one) can have the same key here, so this is the stricter rule. The domain refuses a seal by the same key
-- before it reaches the database, so a command does not end in a database error.
CREATE FUNCTION cargo_phy.seal_key(reference text) RETURNS text LANGUAGE sql IMMUTABLE AS $fn$
  SELECT translate(regexp_replace(translate(reference, 'ıſ', 'IS') COLLATE "C", '[^A-Za-z0-9]', '', 'g'), 'abcdefghijklmnopqrstuvwxyz', 'ABCDEFGHIJKLMNOPQRSTUVWXYZ')
$fn$;

-- One row per rework of a stuffing. The opening is written once and never changes; the ending is written once.
CREATE TABLE cargo_phy.stuffing_rework (
 tenant_id text NOT NULL,
 execution_id text NOT NULL,
 rework_no integer NOT NULL CHECK (rework_no > 0),
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 asked_by text NOT NULL CHECK (length(btrim(asked_by)) > 0),
 asked_at timestamptz NOT NULL,
 asked_entered_at timestamptz NOT NULL,
 broken_seal_source text NOT NULL CHECK (broken_seal_source = 'SEAL'),
 broken_seal_reference text NOT NULL CHECK (length(btrim(broken_seal_reference)) BETWEEN 1 AND 128),
 broken_seal_sha256 char(64) CHECK (broken_seal_sha256 ~ '^[0-9a-f]{64}$'),
 broken_seal_occurred_at timestamptz NOT NULL,
 broken_sealed_by text NOT NULL CHECK (length(btrim(broken_sealed_by)) > 0),
 broken_sealed_at timestamptz NOT NULL,
 broken_sealed_entered_at timestamptz NOT NULL,
 ended_kind text CHECK (ended_kind IN ('SEALED','GIVEN_UP')),
 ended_by text CHECK (length(btrim(ended_by)) > 0),
 ended_at timestamptz,
 ended_entered_at timestamptz,
 new_seal_reference text CHECK (length(btrim(new_seal_reference)) BETWEEN 1 AND 128),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, execution_id, rework_no),
 FOREIGN KEY (tenant_id, execution_id) REFERENCES cargo_phy.load_execution (tenant_id, execution_id),
 CONSTRAINT stuffing_rework_times CHECK (asked_at >= broken_sealed_at AND broken_sealed_at >= broken_seal_occurred_at),
 CONSTRAINT stuffing_rework_ended CHECK ((ended_kind IS NULL) = (ended_at IS NULL) AND (ended_by IS NULL) = (ended_at IS NULL)
   AND (ended_entered_at IS NULL) = (ended_at IS NULL) AND (new_seal_reference IS NOT NULL) = coalesce(ended_kind = 'SEALED', false)
   AND (ended_at IS NULL OR ended_at >= asked_at))
);
-- A stuffing has at most one rework that is not ended.
CREATE UNIQUE INDEX stuffing_rework_one_open ON cargo_phy.stuffing_rework (tenant_id, execution_id) WHERE ended_kind IS NULL;

-- A closed lot opened again by the unloading of its house from a container in rework. The first reversal of the lot's
-- packs in that unloading is named; the row is checked against it at commit (check_lot_reopen_reversal).
CREATE TABLE cargo_phy.cargo_lot_reopen (
 tenant_id text NOT NULL,
 lot_id text NOT NULL,
 reopen_no integer NOT NULL CHECK (reopen_no > 0),
 execution_id text NOT NULL,
 rework_no integer NOT NULL CHECK (rework_no > 0),
 reversal_no integer NOT NULL CHECK (reversal_no > 0),
 closed_by text NOT NULL CHECK (length(btrim(closed_by)) > 0),
 closed_at timestamptz NOT NULL,
 closed_entered_at timestamptz NOT NULL,
 reopened_by text NOT NULL CHECK (length(btrim(reopened_by)) > 0),
 reopened_at timestamptz NOT NULL,
 reopened_entered_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, lot_id, reopen_no),
 FOREIGN KEY (tenant_id, lot_id) REFERENCES cargo_phy.cargo_lot (tenant_id, lot_id),
 FOREIGN KEY (tenant_id, execution_id, rework_no) REFERENCES cargo_phy.stuffing_rework (tenant_id, execution_id, rework_no),
 FOREIGN KEY (tenant_id, execution_id, reversal_no) REFERENCES cargo_phy.reversal_line (tenant_id, execution_id, reversal_no)
   DEFERRABLE INITIALLY DEFERRED,
 CONSTRAINT cargo_lot_reopen_times CHECK (reopened_at >= closed_at)
);

CREATE TRIGGER stuffing_rework_immutable_delete BEFORE DELETE ON cargo_phy.stuffing_rework
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();
CREATE TRIGGER cargo_lot_reopen_immutable BEFORE UPDATE OR DELETE ON cargo_phy.cargo_lot_reopen
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();

-- A rework is opened on a CLOSED stuffing whose close facts it takes over: the execution row is locked first, then
-- its state read. The rework is numbered after the last one of the execution.
CREATE FUNCTION cargo_phy.check_stuffing_rework_insert() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  x cargo_phy.load_execution;
BEGIN
  SELECT * INTO x FROM cargo_phy.load_execution WHERE tenant_id = NEW.tenant_id AND execution_id = NEW.execution_id FOR NO KEY UPDATE;
  IF x.execution_id IS NULL OR x.kind <> 'STUFFING' OR x.state <> 'CLOSED' THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% is not a sealed stuffing and takes no rework', NEW.tenant_id, NEW.execution_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'stuffing_rework_state';
  END IF;
  IF (NEW.broken_seal_source, NEW.broken_seal_reference, NEW.broken_seal_sha256, NEW.broken_seal_occurred_at, NEW.broken_sealed_by,
      NEW.broken_sealed_at, NEW.broken_sealed_entered_at)
     IS DISTINCT FROM (x.close_evidence_source, x.close_evidence_reference, x.close_evidence_sha256, x.close_evidence_occurred_at, x.closed_by,
      x.closed_at, x.closed_entered_at) THEN
    RAISE EXCEPTION 'cargo_phy: rework of %/% breaks a seal other than the one that is on the container', NEW.tenant_id, NEW.execution_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'stuffing_rework_broken_seal';
  END IF;
  IF NEW.rework_no IS DISTINCT FROM (SELECT coalesce(max(r.rework_no), 0) + 1 FROM cargo_phy.stuffing_rework r
       WHERE r.tenant_id = NEW.tenant_id AND r.execution_id = NEW.execution_id) THEN
    RAISE EXCEPTION 'cargo_phy: rework % of %/% does not follow the last', NEW.rework_no, NEW.tenant_id, NEW.execution_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'stuffing_rework_number';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER stuffing_rework_insert BEFORE INSERT ON cargo_phy.stuffing_rework
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_stuffing_rework_insert();

-- Only the ending of an open rework is written, once; a seal is not put back.
CREATE FUNCTION cargo_phy.check_stuffing_rework_update() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  ending text[] := ARRAY['ended_kind','ended_by','ended_at','ended_entered_at','new_seal_reference'];
BEGIN
  PERFORM 1 FROM cargo_phy.load_execution WHERE tenant_id = OLD.tenant_id AND execution_id = OLD.execution_id FOR NO KEY UPDATE;
  IF to_jsonb(OLD) - ending IS DISTINCT FROM to_jsonb(NEW) - ending THEN
    RAISE EXCEPTION 'cargo_phy: rework % of %/% keeps its opening', OLD.rework_no, OLD.tenant_id, OLD.execution_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.ended_kind IS NOT NULL OR NEW.ended_kind IS NULL THEN
    RAISE EXCEPTION 'cargo_phy: rework % of %/% is ended once', OLD.rework_no, OLD.tenant_id, OLD.execution_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.ended_kind = 'SEALED' AND EXISTS (SELECT 1 FROM cargo_phy.stuffing_rework r WHERE r.tenant_id = NEW.tenant_id
       AND r.execution_id = NEW.execution_id AND cargo_phy.seal_key(r.broken_seal_reference) = cargo_phy.seal_key(NEW.new_seal_reference)) THEN
    RAISE EXCEPTION 'cargo_phy: seal % was on container %/% before and is not put back', NEW.new_seal_reference, NEW.tenant_id, NEW.execution_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'stuffing_rework_new_seal';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER stuffing_rework_update BEFORE UPDATE ON cargo_phy.stuffing_rework
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_stuffing_rework_update();

-- At commit an execution and its reworks agree: a REWORK execution has its last rework open and no other is open; one
-- that is CLOSED after a rework was sealed again with the seal it carries, by the one who sealed it, and one that is
-- CANCELLED after a rework was given up, by the one who gave it up, at the same business and entered times; an
-- execution in no other state has no rework. The execution's row is locked by the statement that changed either, in
-- this transaction.
CREATE FUNCTION cargo_phy.check_stuffing_rework_agrees() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  x cargo_phy.load_execution;
  r cargo_phy.stuffing_rework;
  ok boolean;
BEGIN
  SELECT * INTO x FROM cargo_phy.load_execution WHERE tenant_id = NEW.tenant_id AND execution_id = NEW.execution_id;
  SELECT * INTO r FROM cargo_phy.stuffing_rework WHERE tenant_id = NEW.tenant_id AND execution_id = NEW.execution_id ORDER BY rework_no DESC LIMIT 1;
  ok := CASE
    WHEN x.state = 'REWORK' THEN r.rework_no IS NOT NULL AND r.ended_kind IS NULL
    WHEN r.rework_no IS NULL THEN true
    WHEN r.ended_kind IS NULL THEN false
    WHEN x.state = 'CLOSED' THEN r.ended_kind = 'SEALED' AND r.new_seal_reference = x.close_evidence_reference AND r.ended_by = x.closed_by
      AND r.ended_at = x.closed_at AND r.ended_entered_at = x.closed_entered_at
    WHEN x.state = 'CANCELLED' THEN r.ended_kind = 'GIVEN_UP' AND r.ended_by = x.cancelled_by AND r.ended_at = x.cancelled_at
      AND r.ended_entered_at = x.cancelled_entered_at
    ELSE false END;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% is % and its reworks do not agree', NEW.tenant_id, NEW.execution_id, x.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'stuffing_rework_agrees';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER stuffing_rework_agrees AFTER INSERT OR UPDATE ON cargo_phy.stuffing_rework
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_stuffing_rework_agrees();
CREATE CONSTRAINT TRIGGER load_execution_rework_agrees AFTER UPDATE ON cargo_phy.load_execution
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_stuffing_rework_agrees();

-- A load execution keeps its consolidation, kind, mode, facility and unit; its state moves OPEN, LOADING, ON_HOLD and
-- back, CLOSED, or, for a stuffing with no live packs, CANCELLED. A closed or cancelled execution changes only its
-- revision (a cancelled one not even that), except that a sealed STUFFING goes to REWORK: its consolidation CLOSED, no
-- carrier evidence on it, its close facts cleared in the same change and the rework that took them over inserted in
-- this transaction. A REWORK execution changes only its revision, or goes to CLOSED (the rules of a seal, cargo in the
-- container and a seal that differs from every broken one) or to CANCELLED (no live packs, as for any other); to no
-- other state. A new tender tenders an AIR_CONSOL or SEA_LCL_COLOAD consolidation that is OPEN or CLOSED; a new
-- stuffing fills a PLANNED container of a CLOSED SEA_LCL_OWN consolidation, opened not before the consolidation's
-- latest version or unit change, and is sealed while the consolidation is CLOSED. The consolidation is read under its
-- FOR SHARE lock, which a unit's removal (FOR NO KEY UPDATE) and a reopen wait for. A cancellation takes the execution
-- row's lock, as a line, a reversal and a hold do (check_execution_addition), so a load and a cancellation of one
-- stuffing wait for each other, and so do a carrier evidence and the opening of a rework: whichever comes second sees
-- the other.
CREATE OR REPLACE FUNCTION cargo_phy.check_load_execution_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  fixed text[] := ARRAY['tenant_id','execution_id','consolidation_id','kind','transport_mode','facility_location','facility_code',
    'load_unit_no','opened_by','opened_at','opened_entered_at','created_at'];
  seal_facts text[] := ARRAY['state','closed_by','closed_at','closed_entered_at','close_evidence_source','close_evidence_reference',
    'close_evidence_sha256','close_evidence_occurred_at','revision','updated_at'];
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
  IF (OLD.state = 'CLOSED' AND NEW.state <> 'REWORK' AND to_jsonb(OLD) - ARRAY['revision','updated_at'] IS DISTINCT FROM to_jsonb(NEW) - ARRAY['revision','updated_at'])
     OR (OLD.state = 'CLOSED' AND NEW.state = 'REWORK' AND (NEW.kind <> 'STUFFING' OR to_jsonb(OLD) - seal_facts IS DISTINCT FROM to_jsonb(NEW) - seal_facts))
     OR (OLD.state NOT IN ('CLOSED','REWORK') AND NEW.state = 'REWORK')
     OR (OLD.state = 'REWORK' AND NEW.state NOT IN ('REWORK','CLOSED','CANCELLED'))
     OR (OLD.state = 'REWORK' AND NEW.state = 'REWORK' AND to_jsonb(OLD) - ARRAY['revision','updated_at'] IS DISTINCT FROM to_jsonb(NEW) - ARRAY['revision','updated_at'])
     OR (OLD.state <> 'OPEN' AND NEW.state = 'OPEN') OR (OLD.state = 'OPEN' AND NEW.state NOT IN ('OPEN','LOADING','CANCELLED'))
     OR (OLD.state = 'ON_HOLD' AND NEW.state = 'CLOSED') THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% cannot go from % to %', OLD.tenant_id, OLD.execution_id, OLD.state, NEW.state
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.state = 'CLOSED' AND NEW.state = 'REWORK' THEN
    SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
    IF c.state IS DISTINCT FROM 'CLOSED' THEN
      RAISE EXCEPTION 'cargo_phy: consolidation %/% is %; only a CLOSED one has a container opened for rework', NEW.tenant_id, NEW.consolidation_id, c.state
        USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_rework_consolidation';
    END IF;
    IF EXISTS (SELECT 1 FROM cargo_phy.carrier_load_evidence e WHERE e.tenant_id = NEW.tenant_id AND e.execution_id = NEW.execution_id) THEN
      RAISE EXCEPTION 'cargo_phy: load execution %/% has carrier evidence and is not opened for rework', NEW.tenant_id, NEW.execution_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_rework_carrier';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM cargo_phy.stuffing_rework r WHERE r.tenant_id = NEW.tenant_id AND r.execution_id = NEW.execution_id
         AND r.ended_kind IS NULL AND r.xmin = pg_current_xact_id()::xid) THEN
      RAISE EXCEPTION 'cargo_phy: load execution %/% is opened for rework with no rework recorded in this transaction', NEW.tenant_id, NEW.execution_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_rework_record';
    END IF;
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
  IF NEW.kind = 'STUFFING' AND NEW.state = 'CLOSED' AND OLD.state <> 'CLOSED' AND NEW.close_evidence_reference ~ '(^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]|[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]$)' THEN
    RAISE EXCEPTION 'cargo_phy: the seal of container %/% has a blank at an end of its reference', NEW.tenant_id, NEW.execution_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_seal_blank';
  END IF;
  IF NEW.kind = 'STUFFING' AND NEW.state = 'CLOSED' AND OLD.state <> 'CLOSED' THEN
    SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
    IF c.state IS DISTINCT FROM 'CLOSED' OR NEW.closed_at < (SELECT max(v.written_at) FROM cargo_phy.consolidation_version v
         WHERE v.tenant_id = NEW.tenant_id AND v.consolidation_id = NEW.consolidation_id) THEN
      RAISE EXCEPTION 'cargo_phy: consolidation %/% is %; its containers are sealed once it is CLOSED, after its latest version', NEW.tenant_id, NEW.consolidation_id,
        c.state USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_seal_frozen';
    END IF;
  END IF;
  IF OLD.state = 'REWORK' AND NEW.state = 'CLOSED' THEN
    SELECT coalesce(sum(l.packs), 0) - coalesce((SELECT sum(r.packs) FROM cargo_phy.reversal_line r
        WHERE r.tenant_id = NEW.tenant_id AND r.execution_id = NEW.execution_id), 0) INTO live
      FROM cargo_phy.load_line l WHERE l.tenant_id = NEW.tenant_id AND l.execution_id = NEW.execution_id;
    IF live < 1 THEN
      RAISE EXCEPTION 'cargo_phy: load execution %/% holds no live packs and is not sealed again', NEW.tenant_id, NEW.execution_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_reseal_cargo';
    END IF;
    IF EXISTS (SELECT 1 FROM cargo_phy.stuffing_rework r WHERE r.tenant_id = NEW.tenant_id AND r.execution_id = NEW.execution_id
         AND cargo_phy.seal_key(r.broken_seal_reference) = cargo_phy.seal_key(NEW.close_evidence_reference)) THEN
      RAISE EXCEPTION 'cargo_phy: seal % was on container %/% before and is not put back', NEW.close_evidence_reference, NEW.tenant_id, NEW.execution_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_reseal_new';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;

-- What an execution gains it gains in its state: lines while loading (or held by the line itself), holds while loading
-- or on hold, reversals then too, once closed only of packs taken back from the carrier, and in REWORK only of a whole
-- line, for the reason UNSTUFFED; carrier evidence and its discrepancies once closed. The execution row is locked
-- first. A line names its execution's consolidation and a lot of its shipment and mode; a reversal never takes packs
-- out of a closed lot (the lot of a house unloaded in a rework is opened again first). A sealed STUFFING takes no
-- reversal at all (check_sealed_reversal).
CREATE OR REPLACE FUNCTION cargo_phy.check_execution_addition() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  x cargo_phy.load_execution;
  l cargo_phy.cargo_lot;
  live bigint;
  allowed text[] := CASE TG_TABLE_NAME WHEN 'carrier_load_evidence' THEN ARRAY['CLOSED'] WHEN 'loading_discrepancy' THEN ARRAY['CLOSED']
    WHEN 'reversal_line' THEN ARRAY['LOADING','ON_HOLD','CLOSED','REWORK'] ELSE ARRAY['LOADING','ON_HOLD'] END;
BEGIN
  SELECT * INTO x FROM cargo_phy.load_execution WHERE tenant_id = NEW.tenant_id AND execution_id = NEW.execution_id FOR NO KEY UPDATE;
  IF x.execution_id IS NULL OR NOT x.state = ANY (allowed) THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% takes no % while %', NEW.tenant_id, NEW.execution_id, TG_TABLE_NAME, x.state
      USING ERRCODE = 'check_violation';
  END IF;
  IF TG_TABLE_NAME = 'reversal_line' THEN
    SELECT c.* INTO l FROM cargo_phy.load_line ll JOIN cargo_phy.cargo_lot c ON c.tenant_id = ll.tenant_id AND c.lot_id = ll.lot_id
      WHERE ll.tenant_id = NEW.tenant_id AND ll.execution_id = NEW.execution_id AND ll.line_no = (to_jsonb(NEW)->>'line_no')::int;
    IF l.state = 'CLOSED' OR (x.state = 'CLOSED' AND (to_jsonb(NEW)->>'reason') <> 'OFFLOADED_BY_FORWARDER') THEN
      RAISE EXCEPTION 'cargo_phy: reversal %/% takes no packs out of a closed lot, nor after the tender but packs taken back from the carrier',
        NEW.tenant_id, NEW.execution_id USING ERRCODE = 'check_violation';
    END IF;
    IF x.state = 'REWORK' THEN
      SELECT ll.packs - coalesce((SELECT sum(r.packs) FROM cargo_phy.reversal_line r WHERE r.tenant_id = ll.tenant_id
          AND r.execution_id = ll.execution_id AND r.line_no = ll.line_no), 0) INTO live
        FROM cargo_phy.load_line ll WHERE ll.tenant_id = NEW.tenant_id AND ll.execution_id = NEW.execution_id AND ll.line_no = (to_jsonb(NEW)->>'line_no')::int;
      IF (to_jsonb(NEW)->>'reason') <> 'UNSTUFFED' OR (to_jsonb(NEW)->>'packs')::bigint IS DISTINCT FROM live THEN
        RAISE EXCEPTION 'cargo_phy: reversal %/% in a rework takes the whole line, for the reason UNSTUFFED', NEW.tenant_id, NEW.execution_id
          USING ERRCODE = 'check_violation', CONSTRAINT = 'reversal_line_rework';
      END IF;
    END IF;
  END IF;
  IF TG_TABLE_NAME = 'load_line' THEN
    SELECT * INTO l FROM cargo_phy.cargo_lot WHERE tenant_id = NEW.tenant_id AND lot_id = (to_jsonb(NEW)->>'lot_id');
    IF (to_jsonb(NEW)->>'consolidation_id') IS DISTINCT FROM x.consolidation_id OR l.shipment_id IS DISTINCT FROM (to_jsonb(NEW)->>'shipment_id')
       OR l.transport_mode <> x.transport_mode OR l.state = 'CLOSED' THEN
      RAISE EXCEPTION 'cargo_phy: load line %/%/% does not fit its execution and lot', NEW.tenant_id, NEW.execution_id, to_jsonb(NEW)->>'line_no'
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;

-- A reversal records the rework it was written in: the execution's state is read at the insert, under the execution's
-- row lock, and the number of its open rework is set, whatever the statement gave. The reversal never changes
-- afterwards.
CREATE FUNCTION cargo_phy.set_reversal_rework() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  x cargo_phy.load_execution;
BEGIN
  SELECT * INTO x FROM cargo_phy.load_execution WHERE tenant_id = NEW.tenant_id AND execution_id = NEW.execution_id FOR NO KEY UPDATE;
  NEW.rework_no := CASE WHEN x.state = 'REWORK' THEN (SELECT max(r.rework_no) FROM cargo_phy.stuffing_rework r
    WHERE r.tenant_id = NEW.tenant_id AND r.execution_id = NEW.execution_id AND r.ended_kind IS NULL) END;
  RETURN NEW;
END $fn$;
-- The order of the BEFORE triggers does not matter to this one: it locks the execution row and reads the state itself,
-- and if reversal_line_rules (check_execution_addition) then refuses the reversal, the transaction fails anyway.
CREATE TRIGGER reversal_line_rules_rework BEFORE INSERT ON cargo_phy.reversal_line
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.set_reversal_rework();

-- At commit, a reversal written in a rework left nothing of its house loaded in the consolidation: one whole house is
-- unloaded by a command, and the house's packs in another container are partial unloading, which is not built. It does
-- not matter what state the execution is in by then: the reversal says it was written in a rework. The house's
-- membership ledger is locked as a load locks it, so a load of the house elsewhere and this wait for each other: the
-- one that commits first is seen by the other.
CREATE FUNCTION cargo_phy.check_rework_house_whole() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  x cargo_phy.load_execution;
  shipment text;
BEGIN
  IF NEW.rework_no IS NOT NULL THEN
    SELECT * INTO x FROM cargo_phy.load_execution WHERE tenant_id = NEW.tenant_id AND execution_id = NEW.execution_id FOR NO KEY UPDATE;
    SELECT l.shipment_id INTO shipment FROM cargo_phy.load_line l WHERE l.tenant_id = NEW.tenant_id AND l.execution_id = NEW.execution_id AND l.line_no = NEW.line_no;
    PERFORM 1 FROM cargo_phy.consol_membership WHERE tenant_id = NEW.tenant_id AND shipment_id = shipment AND stage = 'MAIN' FOR NO KEY UPDATE;
    IF cargo_phy.live_packs_of_house(NEW.tenant_id, x.consolidation_id, shipment) > 0 THEN
      RAISE EXCEPTION 'cargo_phy: house %/% is not unloaded whole from container %: part of it is still loaded in consolidation %', NEW.tenant_id,
        shipment, NEW.execution_id, x.consolidation_id USING ERRCODE = 'check_violation', CONSTRAINT = 'reversal_line_rework_house';
    END IF;
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER reversal_line_rework_house AFTER INSERT ON cargo_phy.reversal_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_rework_house_whole();

-- A lot keeps its shipment, facility and mode; its state moves RECEIVING, RECEIVED, CLOSED, and a closed lot never
-- changes, but for one change: it is opened again, back to RECEIVED when its receipt was completed and RECEIVING when
-- not, with its closed_by, closed_at and closed_entered_at cleared, and nothing else of the row changed, together with
-- a cargo_lot_reopen row inserted in the same transaction before it that names the closing it undoes
-- (check_lot_reopen_insert, check_lot_reopen_reversal). Lots are never deleted.
CREATE OR REPLACE FUNCTION cargo_phy.check_cargo_lot_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  fixed text[] := ARRAY['tenant_id','lot_id','shipment_id','facility_location','facility_code','transport_mode','created_at'];
  closing text[] := ARRAY['state','closed_by','closed_at','closed_entered_at','revision','updated_at'];
  k text;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_phy: cargo lot %/% cannot be deleted', OLD.tenant_id, OLD.lot_id USING ERRCODE = 'restrict_violation';
  END IF;
  FOREACH k IN ARRAY fixed LOOP
    IF to_jsonb(OLD)->k IS DISTINCT FROM to_jsonb(NEW)->k THEN
      RAISE EXCEPTION 'cargo_phy: cargo lot %/% keeps its %', OLD.tenant_id, OLD.lot_id, k USING ERRCODE = 'restrict_violation';
    END IF;
  END LOOP;
  IF OLD.state = 'CLOSED' AND NEW.state <> 'CLOSED'
     AND NEW.state = (CASE WHEN OLD.completed_at IS NULL THEN 'RECEIVING' ELSE 'RECEIVED' END)
     AND NEW.closed_by IS NULL AND NEW.closed_at IS NULL AND NEW.closed_entered_at IS NULL
     AND to_jsonb(OLD) - closing IS NOT DISTINCT FROM to_jsonb(NEW) - closing
     AND EXISTS (SELECT 1 FROM cargo_phy.cargo_lot_reopen o WHERE o.tenant_id = OLD.tenant_id AND o.lot_id = OLD.lot_id
       AND o.reopen_no = (SELECT max(m.reopen_no) FROM cargo_phy.cargo_lot_reopen m WHERE m.tenant_id = OLD.tenant_id AND m.lot_id = OLD.lot_id)
       AND o.xmin = pg_current_xact_id()::xid AND o.closed_by = OLD.closed_by AND o.closed_at = OLD.closed_at AND o.closed_entered_at = OLD.closed_entered_at) THEN
    RETURN NEW;
  END IF;
  IF OLD.state = 'CLOSED' OR (OLD.state = 'RECEIVED' AND NEW.state = 'RECEIVING') THEN
    RAISE EXCEPTION 'cargo_phy: cargo lot %/% cannot go from % to %', OLD.tenant_id, OLD.lot_id, OLD.state, NEW.state
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- A reopen row is written for a lot that is CLOSED with exactly the facts it records, while the stuffing it names is in
-- the rework it names, not ended, and not before the rework was asked (the execution row locked first, then read, and
-- the lot's after it). It is numbered after the lot's last. That the lot has packs in the execution is the reversal's,
-- at commit.
CREATE FUNCTION cargo_phy.check_lot_reopen_insert() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  x cargo_phy.load_execution;
  l cargo_phy.cargo_lot;
  r cargo_phy.stuffing_rework;
BEGIN
  SELECT * INTO x FROM cargo_phy.load_execution WHERE tenant_id = NEW.tenant_id AND execution_id = NEW.execution_id FOR NO KEY UPDATE;
  SELECT * INTO l FROM cargo_phy.cargo_lot WHERE tenant_id = NEW.tenant_id AND lot_id = NEW.lot_id FOR NO KEY UPDATE;
  SELECT * INTO r FROM cargo_phy.stuffing_rework WHERE tenant_id = NEW.tenant_id AND execution_id = NEW.execution_id AND rework_no = NEW.rework_no;
  IF x.state IS DISTINCT FROM 'REWORK' OR r.ended_kind IS NOT NULL OR NEW.reopened_at < r.asked_at THEN
    RAISE EXCEPTION 'cargo_phy: lot %/% is not reopened by rework % of %: the container is not in that rework', NEW.tenant_id, NEW.lot_id,
      NEW.rework_no, NEW.execution_id USING ERRCODE = 'check_violation', CONSTRAINT = 'cargo_lot_reopen_rework';
  END IF;
  IF l.state IS DISTINCT FROM 'CLOSED' OR (l.closed_by, l.closed_at, l.closed_entered_at) IS DISTINCT FROM (NEW.closed_by, NEW.closed_at, NEW.closed_entered_at) THEN
    RAISE EXCEPTION 'cargo_phy: lot %/% is not closed with the facts the reopen records', NEW.tenant_id, NEW.lot_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cargo_lot_reopen_closed';
  END IF;
  IF NEW.reopen_no IS DISTINCT FROM (SELECT coalesce(max(o.reopen_no), 0) + 1 FROM cargo_phy.cargo_lot_reopen o WHERE o.tenant_id = NEW.tenant_id AND o.lot_id = NEW.lot_id) THEN
    RAISE EXCEPTION 'cargo_phy: reopen % of lot %/% does not follow the last', NEW.reopen_no, NEW.tenant_id, NEW.lot_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cargo_lot_reopen_number';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER cargo_lot_reopen_insert BEFORE INSERT ON cargo_phy.cargo_lot_reopen
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_lot_reopen_insert();

-- At commit a reopen has its reversal, written in the same transaction: a reversal of a line of this lot in the
-- execution, for the reason UNSTUFFED, with the execution still in the rework, and the lot is no longer CLOSED.
CREATE FUNCTION cargo_phy.check_lot_reopen_reversal() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  l cargo_phy.cargo_lot;
BEGIN
  SELECT * INTO l FROM cargo_phy.cargo_lot WHERE tenant_id = NEW.tenant_id AND lot_id = NEW.lot_id;
  IF l.state = 'CLOSED' OR NOT EXISTS (SELECT 1 FROM cargo_phy.reversal_line r JOIN cargo_phy.load_line ll
       ON ll.tenant_id = r.tenant_id AND ll.execution_id = r.execution_id AND ll.line_no = r.line_no
       JOIN cargo_phy.load_execution x ON x.tenant_id = r.tenant_id AND x.execution_id = r.execution_id
       WHERE r.tenant_id = NEW.tenant_id AND r.execution_id = NEW.execution_id AND r.reversal_no = NEW.reversal_no AND ll.lot_id = NEW.lot_id
         AND r.reason = 'UNSTUFFED' AND r.xmin = pg_current_xact_id()::xid AND x.state = 'REWORK') THEN
    RAISE EXCEPTION 'cargo_phy: lot %/% is reopened without a reversal of its packs from %, written in the same transaction', NEW.tenant_id, NEW.lot_id,
      NEW.execution_id USING ERRCODE = 'check_violation', CONSTRAINT = 'cargo_lot_reopen_reversal';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER cargo_lot_reopen_reversal AFTER INSERT ON cargo_phy.cargo_lot_reopen
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_lot_reopen_reversal();

-- A consolidation does not reopen once a container of it is sealed or opened for rework, nor with a reopen dated before
-- its stuffings' changes, a cancellation of one included. It runs after consolidation_version_rules, which locks the
-- consolidation row.
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
  IF EXISTS (SELECT 1 FROM cargo_phy.load_execution x WHERE x.tenant_id = NEW.tenant_id AND x.consolidation_id = NEW.consolidation_id
       AND x.kind = 'STUFFING' AND x.state = 'REWORK') THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% has a container opened for rework; it cannot reopen', NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_reopen_during_rework';
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

-- Tenant isolation as in the other tables.
DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['stuffing_rework','cargo_lot_reopen'] LOOP
    EXECUTE format('ALTER TABLE cargo_phy.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_phy.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_phy.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
