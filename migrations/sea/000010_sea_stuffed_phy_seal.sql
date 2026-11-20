-- IDP-Cargo SEA V2.88 (SEA follows a container's rework) / PostgreSQL 16.
-- Run only as the migrator role, never at service start. 000001 through 000009 are unchanged.
-- What SEA took when it took the container as stuffed: PHY's seal of the stuffing as FUL relayed it, and its sealed-at.
-- A container is resealed when PHY's seal now differs from this, whatever business times and difference reason say.
-- NULL on every visit stuffed before this version and on every visit not yet stuffed (SEA then judges a reseal by the
-- movement's entries). Set together, and only at the step ASSIGNED to STUFFED, with the other stuffing columns.
-- Deployment: SEA writes the two columns in every visit UPDATE, and the application role's UPDATE grant on them lives in
-- the grants step (.github/workflows/org-pg16-contract.yml, `localpg.sh grants`), as for every earlier column, not in this
-- migration: apply the migration, then the grants, before SEA runs.
-- Objects replaced, each with every rule of its latest earlier definition:
--   cargo_sea.check_visit_change()    000004 (the stuffing columns of the step gain the two)
--   cargo_sea.check_visit_at_commit() 000009 (at that step a live house carries the stuffing taken)
-- Locks: ALTER TABLE ... ADD COLUMN (nullable, no default) and ADD CONSTRAINT take ACCESS EXCLUSIVE on
-- cargo_sea.container_visit for their own duration; no table is rewritten, the CHECK scans the table once (every
-- existing row has both columns NULL, which it accepts) and no row is written. CREATE OR REPLACE FUNCTION takes no table
-- lock. What only the domain keeps: that the stored seal is the very text of the stuffing the container was taken with
-- (the commit check compares it with a live house's stuffing at the step; a later refresh changes the houses, not it).

ALTER TABLE cargo_sea.container_visit ADD COLUMN stuffed_phy_seal text, ADD COLUMN stuffed_phy_sealed_at timestamptz;
ALTER TABLE cargo_sea.container_visit ADD CONSTRAINT container_visit_stuffed_phy_seal CHECK (
  (stuffed_phy_seal IS NULL) = (stuffed_phy_sealed_at IS NULL)
  AND (stuffed_phy_seal IS NULL OR (status IN ('STUFFED','VGM_SUBMITTED','GATED_IN','LOADED')
       AND length(btrim(stuffed_phy_seal)) BETWEEN 1 AND 500 AND stuffed_phy_sealed_at <= stuffed_at)));

CREATE OR REPLACE FUNCTION cargo_sea.check_visit_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  step text[] := ARRAY['revision','updated_at','written_xact'];
  stuffing text[] := ARRAY['status','execution_id','difference_reason','stuffed_by','stuffed_at','stuffed_phy_seal','stuffed_phy_sealed_at','revision','updated_at','written_xact'];
  release text[] := ARRAY['status','release_reason','released_by','released_at','revision','updated_at','written_xact'];
  move text[] := ARRAY['status','revision','updated_at','written_xact'];
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
    OR (OLD.status <> 'RELEASED' AND NEW.status = OLD.status AND to_jsonb(OLD) - step = to_jsonb(NEW) - step)) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'cargo_sea: container visit %/% leaves ASSIGNED once, for STUFFED or RELEASED; STUFFED only for VGM_SUBMITTED, that only for GATED_IN, and that only for LOADED; otherwise only its revision moves, by one, and never once released',
    OLD.tenant_id, OLD.visit_id USING ERRCODE = 'restrict_violation';
END $fn$;

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
         AND s.execution_id = NEW.execution_id AND s.load_unit_no = NEW.load_unit_no AND s.sealed_at <= NEW.stuffed_at
         AND (NEW.stuffed_phy_seal IS NULL OR (s.seal = NEW.stuffed_phy_seal AND s.sealed_at = NEW.stuffed_phy_sealed_at))) THEN
    broken := 'a container is stuffed once a live house of its movement carries a sealed stuffing of its load unit, with the seal and time it was taken with';
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
