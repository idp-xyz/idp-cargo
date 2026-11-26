-- IDP-Cargo SEA V2.105 (a whole container rolled to a later vessel) / PostgreSQL 16.
-- Run only as the migrator role, never at service start. 000001 through 000013 are unchanged.
-- The owner's acceptance criteria and decisions of 2026-11-26 (验收标准_V2.105_整箱改到另一航次.md): a consolidation
-- sails in more than one movement when the carrier rolls one of its containers to a later vessel, under a new booking or
-- a new capacity line of the same one; the container moves to the other movement with its seals, its VGM and its houses.
--   ocean_movement.booking_line, booking_link.capacity_line: the capacity line of the booking a movement and each of its
--     links carry, 1 to 20; every earlier row reads 1 (a constant default: no table is rewritten).
--   ocean_movement_one_open (000005: one movement in force per consolidation) is replaced by
--     ocean_movement_one_per_booking_line: one movement in force per booking line. A consolidation may so have several
--     movements in force, one per line it sails on.
--   container_visit_roll: one immutable row per move of a container to another movement, with its reason. The visit's
--     movement_id changes only with its roll, written in the same transaction: a sealed container (STUFFED,
--     VGM_SUBMITTED or GATED_IN, never LOADED) moves to another movement of its consolidation in force, of a container
--     booking, keeping everything else.
--   house_bill: a house that sails with its rolled container ends its entry ROLLED_WITH_CONTAINER in one movement and
--     starts one ROLLED_WITH_CONTAINER in the other, with no predecessor or successor.
-- Objects replaced, each with every rule of its latest earlier definition:
--   cargo_sea.check_visit_change()      000011 (gains the roll)
--   house_bill start and end kind checks and house_bill_start   000001 (gain ROLLED_WITH_CONTAINER)
-- Only the domain keeps that the houses moved are those loaded in the container, each in it alone, and that the two
-- movements' entries agree.
-- Locks: ADD COLUMN with a constant default and the constraint swaps take ACCESS EXCLUSIVE on their tables for their own
-- duration; no table is rewritten; the new CHECKs scan their tables once; no row is written. The grants step gains
-- UPDATE (movement_id) on container_visit.

ALTER TABLE cargo_sea.ocean_movement ADD COLUMN booking_line integer NOT NULL DEFAULT 1
  CONSTRAINT ocean_movement_booking_line CHECK (booking_line BETWEEN 1 AND 20);
ALTER TABLE cargo_sea.booking_link ADD COLUMN capacity_line integer NOT NULL DEFAULT 1
  CONSTRAINT booking_link_capacity_line CHECK (capacity_line BETWEEN 1 AND 20);
DROP INDEX cargo_sea.ocean_movement_one_open;
CREATE UNIQUE INDEX ocean_movement_one_per_booking_line ON cargo_sea.ocean_movement (tenant_id, booking_id, booking_line) WHERE state <> 'CANCELLED';

-- A movement's links are of its own booking line.
CREATE FUNCTION cargo_sea.check_link_line() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_sea.ocean_movement m WHERE m.tenant_id = NEW.tenant_id AND m.movement_id = NEW.movement_id
       AND m.booking_line = NEW.capacity_line) THEN
    RAISE EXCEPTION 'cargo_sea: link % of movement %/% takes capacity line %, which is not the movement''s', NEW.link_no, NEW.tenant_id, NEW.movement_id,
      NEW.capacity_line USING ERRCODE = 'check_violation', CONSTRAINT = 'booking_link_movement_line';
  END IF;
  RETURN NULL;
END $fn$;
CREATE TRIGGER booking_link_movement_line AFTER INSERT ON cargo_sea.booking_link FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_link_line();

ALTER TABLE cargo_sea.house_bill DROP CONSTRAINT house_bill_start_kind_check,
  ADD CONSTRAINT house_bill_start_kind_check CHECK (start_kind IN ('ASSOCIATED','REFRESHED','ROLLED_WITH_CONTAINER')),
  DROP CONSTRAINT house_bill_end_kind_check,
  ADD CONSTRAINT house_bill_end_kind_check CHECK (end_kind IN ('REFRESHED','DETACHED','TRANSFERRED','SHIPMENT_CANCELLED','ROLLED_WITH_CONTAINER')),
  DROP CONSTRAINT house_bill_start,
  ADD CONSTRAINT house_bill_start CHECK ((start_kind IN ('ASSOCIATED','ROLLED_WITH_CONTAINER')) = (predecessor_no IS NULL)
    AND (predecessor_no IS NULL OR predecessor_no < entry_no));

CREATE TABLE cargo_sea.container_visit_roll (
 tenant_id text NOT NULL,
 visit_id text NOT NULL,
 roll_no integer NOT NULL CHECK (roll_no > 0),
 from_movement_id text NOT NULL,
 to_movement_id text NOT NULL,
 reason text NOT NULL CHECK (length(btrim(reason)) > 0 AND length(reason) <= 500),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, visit_id, roll_no),
 FOREIGN KEY (tenant_id, visit_id) REFERENCES cargo_sea.container_visit (tenant_id, visit_id),
 FOREIGN KEY (tenant_id, from_movement_id) REFERENCES cargo_sea.ocean_movement (tenant_id, movement_id),
 FOREIGN KEY (tenant_id, to_movement_id) REFERENCES cargo_sea.ocean_movement (tenant_id, movement_id),
 CONSTRAINT container_visit_roll_elsewhere CHECK (from_movement_id <> to_movement_id)
);
-- A roll is immutable; it is the next of its visit, from the movement the visit is in.
CREATE FUNCTION cargo_sea.check_visit_roll() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP <> 'INSERT' THEN
    RAISE EXCEPTION 'cargo_sea: a container roll is never changed' USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.roll_no <> 1 + coalesce((SELECT max(r.roll_no) FROM cargo_sea.container_visit_roll r WHERE r.tenant_id = NEW.tenant_id AND r.visit_id = NEW.visit_id), 0)
     OR NOT EXISTS (SELECT 1 FROM cargo_sea.container_visit v WHERE v.tenant_id = NEW.tenant_id AND v.visit_id = NEW.visit_id
       AND v.movement_id = NEW.from_movement_id) THEN
    RAISE EXCEPTION 'cargo_sea: roll % of container visit %/% is its next and starts from the movement the visit is in', NEW.roll_no, NEW.tenant_id, NEW.visit_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'container_visit_roll_next';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER container_visit_roll_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_sea.container_visit_roll FOR EACH ROW
  EXECUTE FUNCTION cargo_sea.check_visit_roll();
ALTER TABLE cargo_sea.container_visit_roll ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_sea.container_visit_roll FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_sea.container_visit_roll
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION cargo_sea.check_visit_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  step text[] := ARRAY['revision','updated_at','written_xact'];
  stuffing text[] := ARRAY['status','execution_id','difference_reason','stuffed_by','stuffed_at','stuffed_phy_seal','stuffed_phy_sealed_at','revision','updated_at','written_xact'];
  release text[] := ARRAY['status','release_reason','released_by','released_at','revision','updated_at','written_xact'];
  move text[] := ARRAY['status','revision','updated_at','written_xact'];
  roll text[] := ARRAY['movement_id','revision','updated_at','written_xact'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_sea: container visit %/% is never deleted', OLD.tenant_id, OLD.visit_id USING ERRCODE = 'restrict_violation';
  END IF;
  NEW.written_xact := pg_current_xact_id();
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'ASSIGNED' OR NEW.revision <> 1 THEN
      RAISE EXCEPTION 'cargo_sea: container visit %/% starts ASSIGNED at revision 1', NEW.tenant_id, NEW.visit_id
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.revision = OLD.revision + 1 AND (
       (OLD.status = 'ASSIGNED' AND NEW.status = 'STUFFED' AND to_jsonb(OLD) - stuffing = to_jsonb(NEW) - stuffing)
    OR (OLD.status = 'ASSIGNED' AND NEW.status = 'RELEASED' AND to_jsonb(OLD) - release = to_jsonb(NEW) - release)
    OR (OLD.status = 'STUFFED' AND NEW.status = 'VGM_SUBMITTED' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status = 'VGM_SUBMITTED' AND NEW.status = 'GATED_IN' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status = 'GATED_IN' AND NEW.status = 'LOADED' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status = 'GATED_IN' AND NEW.status = 'VGM_SUBMITTED' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status <> 'RELEASED' AND NEW.status = OLD.status AND to_jsonb(OLD) - step = to_jsonb(NEW) - step)
    OR (OLD.status IN ('STUFFED','VGM_SUBMITTED','GATED_IN') AND NEW.status = OLD.status AND NEW.movement_id <> OLD.movement_id
        AND to_jsonb(OLD) - roll = to_jsonb(NEW) - roll
        AND EXISTS (SELECT 1 FROM cargo_sea.container_visit_roll r WHERE r.tenant_id = NEW.tenant_id AND r.visit_id = NEW.visit_id
             AND r.from_movement_id = OLD.movement_id AND r.to_movement_id = NEW.movement_id AND r.xmin = pg_current_xact_id()::xid)
        AND EXISTS (SELECT 1 FROM cargo_sea.ocean_movement m WHERE m.tenant_id = NEW.tenant_id AND m.movement_id = NEW.movement_id
             AND m.consolidation_id = NEW.consolidation_id AND m.kind = 'SEA_LCL_OWN' AND m.state <> 'CANCELLED'))) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'cargo_sea: container visit %/% leaves ASSIGNED once, for STUFFED or RELEASED; STUFFED only for VGM_SUBMITTED, that only for GATED_IN, GATED_IN only for LOADED or back to VGM_SUBMITTED; a sealed container not loaded moves to another movement of its consolidation only with its roll; otherwise only its revision moves, by one, and never once released',
    OLD.tenant_id, OLD.visit_id USING ERRCODE = 'restrict_violation';
END $fn$;

