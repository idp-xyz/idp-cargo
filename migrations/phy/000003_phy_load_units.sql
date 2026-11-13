-- IDP-Cargo PHY V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Planned load units (sea freight part 1b-i, backlog PB8): a consolidation plans the units its cargo is to be loaded
-- into, with the limits of each unit's type frozen from the equipment reference data with the edition. V0.3 plans
-- containers on own-container LCL consolidations (SEA_LCL_OWN); a co-loader's consolidation has none, and ULD and
-- loose units come with ULD build-up. A unit is only planned or removed; whether it is in use or closed is a fact of
-- its load executions, which come with stuffing.

-- Receipts of PLAN_LOAD_UNIT name the consolidation and the unit they planned.
ALTER TABLE cargo_phy.command_receipt
  DROP CONSTRAINT command_receipt_entry,
  ADD CONSTRAINT command_receipt_entry CHECK (result_type <> 'ConsolMembership' OR result_entry_no IS NOT NULL);

CREATE TABLE cargo_phy.planned_load_unit (
 tenant_id text NOT NULL,
 consolidation_id text NOT NULL,
 unit_no integer NOT NULL CHECK (unit_no > 0),
 kind text NOT NULL CHECK (kind = 'CONTAINER'),
 type_code text NOT NULL CHECK (type_code ~ '^[0-9A-Z]{2}[A-Z][0-9A-Z]$'),
 equipment_edition text NOT NULL,
 teu_tenths integer NOT NULL CHECK (teu_tenths BETWEEN 1 AND 30),
 max_gross_kg bigint NOT NULL,
 tare_kg bigint NOT NULL CHECK (tare_kg > 0),
 capacity_cm3 bigint NOT NULL CHECK (capacity_cm3 > 0),
 status text NOT NULL CHECK (status IN ('PLANNED','REMOVED')),
 planned_by text NOT NULL CHECK (length(btrim(planned_by)) > 0),
 planned_at timestamptz NOT NULL,
 planned_entered_at timestamptz NOT NULL,
 remove_reason text CHECK (length(btrim(remove_reason)) BETWEEN 1 AND 500),
 removed_by text CHECK (length(btrim(removed_by)) > 0),
 removed_at timestamptz,
 remove_entered_at timestamptz,
 PRIMARY KEY (tenant_id, consolidation_id, unit_no),
 FOREIGN KEY (tenant_id, consolidation_id) REFERENCES cargo_phy.consolidation (tenant_id, consolidation_id),
 FOREIGN KEY (equipment_edition, type_code) REFERENCES cargo_ref.container_size_type (edition, code),
 CONSTRAINT planned_load_unit_weights CHECK (max_gross_kg > tare_kg),
 CONSTRAINT planned_load_unit_removed CHECK ((status = 'REMOVED') = (removed_at IS NOT NULL) AND (removed_by IS NULL) = (removed_at IS NULL)
   AND (remove_reason IS NULL) = (removed_at IS NULL) AND (remove_entered_at IS NULL) = (removed_at IS NULL)
   AND (removed_at IS NULL OR removed_at >= planned_at))
);

-- A unit follows the consolidation's previous one, on an own-container LCL consolidation that is OPEN or CLOSED, with
-- the limits its type has in the edition named. Only its removal is written later, once, while the consolidation is
-- OPEN or CLOSED. Units are never deleted. The consolidation row is locked first, as its commands lock it.
CREATE FUNCTION cargo_phy.check_planned_load_unit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  removal text[] := ARRAY['status','remove_reason','removed_by','removed_at','remove_entered_at'];
  c cargo_phy.consolidation;
  t cargo_ref.container_size_type;
  last integer;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_phy: planned load units are kept' USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'UPDATE' AND (OLD.status <> 'PLANNED' OR NEW.status <> 'REMOVED' OR to_jsonb(OLD) - removal IS DISTINCT FROM to_jsonb(NEW) - removal) THEN
    RAISE EXCEPTION 'cargo_phy: planned load unit %/%/% can only be removed, once', OLD.tenant_id, OLD.consolidation_id, OLD.unit_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR NO KEY UPDATE;
  IF c.state IS NULL OR c.state NOT IN ('OPEN','CLOSED') THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% is %; its load units are frozen', NEW.tenant_id, NEW.consolidation_id, c.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'planned_load_unit_frozen';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    RETURN NEW;
  END IF;
  SELECT max(unit_no) INTO last FROM cargo_phy.planned_load_unit WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id;
  SELECT * INTO t FROM cargo_ref.container_size_type WHERE edition = NEW.equipment_edition AND code = NEW.type_code;
  IF NEW.unit_no <> coalesce(last, 0) + 1 OR c.kind <> 'SEA_LCL_OWN' OR NEW.status <> 'PLANNED' OR t.code IS NULL
     OR NEW.teu_tenths <> t.teu * 10 OR NEW.max_gross_kg <> t.max_gross_kg OR NEW.tare_kg <> t.tare_kg OR NEW.capacity_cm3 <> t.capacity_cm3 THEN
    RAISE EXCEPTION 'cargo_phy: load unit % does not follow consolidation %/% or its type''s reference limits', NEW.unit_no, NEW.tenant_id,
      NEW.consolidation_id USING ERRCODE = 'check_violation', CONSTRAINT = 'planned_load_unit_follows';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER planned_load_unit_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_phy.planned_load_unit
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_planned_load_unit();

-- Tenant isolation as in the other PHY tables.
ALTER TABLE cargo_phy.planned_load_unit ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_phy.planned_load_unit FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_phy.planned_load_unit
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
