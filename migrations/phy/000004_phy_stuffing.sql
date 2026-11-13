-- IDP-Cargo PHY V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Stuffing (sea freight part 1b-ii): a load execution of kind STUFFING fills one planned container of a CLOSED
-- own-container LCL consolidation (its houses frozen) at the forwarder's CFS, and closes with the container's seal
-- as evidence. A unit takes one stuffing; a unit with one is not removed. Whether a stuffed line takes the container
-- over its type's default limits is PHY's domain rule (it holds the execution, OVERWEIGHT); the database keeps the
-- structure. Existing executions are tenders and name no unit.

ALTER TABLE cargo_phy.load_execution
  DROP CONSTRAINT load_execution_kind_check,
  ADD CONSTRAINT load_execution_kind_check CHECK (kind IN ('TENDER','STUFFING')),
  ADD COLUMN load_unit_no integer CHECK (load_unit_no > 0),
  ADD CONSTRAINT load_execution_unit_fk FOREIGN KEY (tenant_id, consolidation_id, load_unit_no)
    REFERENCES cargo_phy.planned_load_unit (tenant_id, consolidation_id, unit_no),
  ADD CONSTRAINT load_execution_unit CHECK ((kind = 'STUFFING') = (load_unit_no IS NOT NULL)),
  ADD CONSTRAINT load_execution_seal CHECK (kind <> 'STUFFING' OR closed_at IS NULL OR close_evidence_source = 'SEAL');
CREATE UNIQUE INDEX load_execution_one_per_unit ON cargo_phy.load_execution (tenant_id, consolidation_id, load_unit_no)
  WHERE load_unit_no IS NOT NULL;

-- A load execution keeps its consolidation, kind, mode, facility and unit; its state moves OPEN, LOADING, ON_HOLD and
-- back, CLOSED; a closed execution changes only its revision. Executions are never deleted. A new tender tenders an
-- AIR_CONSOL or SEA_LCL_COLOAD consolidation that is OPEN or CLOSED; a new stuffing fills a PLANNED container of a
-- CLOSED SEA_LCL_OWN consolidation, opened not before the unit was planned. The consolidation is read under its FOR
-- SHARE lock, which a unit's removal (FOR NO KEY UPDATE) waits for.
CREATE OR REPLACE FUNCTION cargo_phy.check_load_execution_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  fixed text[] := ARRAY['tenant_id','execution_id','consolidation_id','kind','transport_mode','facility_location','facility_code',
    'load_unit_no','opened_by','opened_at','opened_entered_at','created_at'];
  k text;
  c cargo_phy.consolidation;
  u cargo_phy.planned_load_unit;
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
         OR NEW.state <> 'OPEN' OR u.status IS DISTINCT FROM 'PLANNED' OR NEW.opened_at < u.planned_at THEN
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
  IF (OLD.state = 'CLOSED' AND to_jsonb(OLD) - ARRAY['revision','updated_at'] IS DISTINCT FROM to_jsonb(NEW) - ARRAY['revision','updated_at'])
     OR (OLD.state <> 'OPEN' AND NEW.state = 'OPEN') OR (OLD.state = 'OPEN' AND NEW.state NOT IN ('OPEN','LOADING'))
     OR (OLD.state = 'ON_HOLD' AND NEW.state = 'CLOSED') THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% cannot go from % to %', OLD.tenant_id, OLD.execution_id, OLD.state, NEW.state
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- A stuffing's lines come while its consolidation is CLOSED: a house is stuffed only from a frozen house list. The
-- execution row is locked by load_line_rules, which runs first.
CREATE FUNCTION cargo_phy.check_stuffing_line() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s text;
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_phy.load_execution x WHERE x.tenant_id = NEW.tenant_id AND x.execution_id = NEW.execution_id
       AND x.kind = 'STUFFING') THEN
    SELECT state INTO s FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
    IF s IS DISTINCT FROM 'CLOSED' THEN
      RAISE EXCEPTION 'cargo_phy: consolidation %/% is %; its houses are stuffed once it is CLOSED', NEW.tenant_id, NEW.consolidation_id, s
        USING ERRCODE = 'check_violation', CONSTRAINT = 'load_line_stuffing_frozen';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER load_line_stuffing BEFORE INSERT ON cargo_phy.load_line FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_stuffing_line();

-- A unit with a stuffing opened on it is not removed.
CREATE FUNCTION cargo_phy.check_unit_not_in_use() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.status = 'REMOVED' AND EXISTS (SELECT 1 FROM cargo_phy.load_execution x WHERE x.tenant_id = NEW.tenant_id
       AND x.consolidation_id = NEW.consolidation_id AND x.load_unit_no = NEW.unit_no) THEN
    RAISE EXCEPTION 'cargo_phy: load unit %/%/% has its stuffing', NEW.tenant_id, NEW.consolidation_id, NEW.unit_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'planned_load_unit_in_use';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER planned_load_unit_use BEFORE UPDATE ON cargo_phy.planned_load_unit
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_unit_not_in_use();
