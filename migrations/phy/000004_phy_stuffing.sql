-- IDP-Cargo PHY V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Stuffing (sea freight part 1b-ii): a load execution of kind STUFFING fills one planned container of a CLOSED
-- own-container LCL consolidation (its houses frozen) at the forwarder's CFS, and is sealed, the seal its closing
-- evidence, while the consolidation is still CLOSED. A unit takes one stuffing; a unit with one is not removed; a
-- sealed container is not unstuffed, and its consolidation does not reopen. Whether the cargo takes the container
-- over its type's default limits is PHY's domain rule (it holds the execution, OVERWEIGHT); the database keeps the
-- structure and the order of business times. Existing executions are tenders and name no unit.

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
-- CLOSED SEA_LCL_OWN consolidation, opened not before the consolidation's latest version or unit change, and is
-- sealed while the consolidation is CLOSED. The consolidation is read under its FOR SHARE lock, which a unit's
-- removal (FOR NO KEY UPDATE) and a reopen wait for.
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
         OR NEW.state <> 'OPEN' OR u.status IS DISTINCT FROM 'PLANNED'
         OR NEW.opened_at < greatest((SELECT max(v.written_at) FROM cargo_phy.consolidation_version v
               WHERE v.tenant_id = NEW.tenant_id AND v.consolidation_id = NEW.consolidation_id),
             (SELECT max(greatest(p.planned_at, p.removed_at)) FROM cargo_phy.planned_load_unit p
               WHERE p.tenant_id = NEW.tenant_id AND p.consolidation_id = NEW.consolidation_id)) THEN
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
  IF NEW.kind = 'STUFFING' AND NEW.state = 'CLOSED' AND OLD.state <> 'CLOSED' THEN
    SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
    IF c.state IS DISTINCT FROM 'CLOSED' THEN
      RAISE EXCEPTION 'cargo_phy: consolidation %/% is %; its containers are sealed once it is CLOSED', NEW.tenant_id, NEW.consolidation_id,
        c.state USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_seal_frozen';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;

-- A sealed container is not unstuffed: it would need a new seal, which comes later. The execution row is locked by
-- reversal_line_rules, which runs first.
CREATE FUNCTION cargo_phy.check_sealed_reversal() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_phy.load_execution x WHERE x.tenant_id = NEW.tenant_id AND x.execution_id = NEW.execution_id
       AND x.kind = 'STUFFING' AND x.state = 'CLOSED') THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% is a sealed container', NEW.tenant_id, NEW.execution_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'reversal_line_sealed';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER reversal_line_sealed BEFORE INSERT ON cargo_phy.reversal_line FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_sealed_reversal();

-- A consolidation does not reopen once a container of it is sealed, nor with a reopen dated before its stuffings'
-- changes. It runs after consolidation_version_rules, which locks the consolidation row.
CREATE FUNCTION cargo_phy.check_reopen_after_stuffing() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
  SELECT greatest(max(x.opened_at), max(x.started_at),
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
CREATE TRIGGER consolidation_version_stuffing BEFORE INSERT ON cargo_phy.consolidation_version
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_reopen_after_stuffing();

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
