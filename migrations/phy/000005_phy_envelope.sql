-- IDP-Cargo PHY V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The physical envelope at the close (sea freight part 1b-iii; PHY design: CloseConsolidation needs "planned totals
-- within the physical envelope or an accepted overrun reason"). With load units planned, the live houses' planned
-- gross weight and volume are checked against the live units' default maximum gross less tare and their capacity;
-- a plan over it, or in a unit PHY does not convert, closes only with an overrun reason, which the CLOSED version
-- keeps; a plan within it, or a consolidation without units, takes none. The reason records the plan against the
-- units live at the close: a unit planned or removed while CLOSED (with its own removal reason) is not checked
-- again until the next close. Versions written before keep no reason on their close.

ALTER TABLE cargo_phy.consolidation_version
  DROP CONSTRAINT consolidation_version_reason,
  ADD CONSTRAINT consolidation_version_reason CHECK (CASE WHEN change IN ('AMENDED','REOPENED','CANCELLED') THEN reason IS NOT NULL
    WHEN change = 'CLOSED' THEN true ELSE reason IS NULL END);

-- A weight in grams and a volume in cubic centimetres, rounded up, as PHY converts them (UN/ECE Recommendation 20:
-- a pound is 453.59237 g, a cubic foot 28,316.846592 cm3); NULL for a unit PHY does not convert.
CREATE FUNCTION cargo_phy.grams(unit text, amount bigint) RETURNS numeric LANGUAGE sql IMMUTABLE AS $fn$
  SELECT ceil(amount * CASE unit WHEN 'GRM' THEN 1 WHEN 'KGM' THEN 1000 WHEN 'TNE' THEN 1000000 WHEN 'LBR' THEN 453.59237 END)
$fn$;
CREATE FUNCTION cargo_phy.cubic_cm(unit text, amount bigint) RETURNS numeric LANGUAGE sql IMMUTABLE AS $fn$
  SELECT ceil(amount * CASE unit WHEN 'CMQ' THEN 1 WHEN 'DMQ' THEN 1000 WHEN 'LTR' THEN 1000 WHEN 'MTQ' THEN 1000000
    WHEN 'FTQ' THEN 28316.846592 END)
$fn$;

-- A CLOSED version is written with an overrun reason exactly when the live houses' plan exceeds the live units'
-- envelope at that moment. It runs after consolidation_version_rules, which locks the consolidation row first, as its
-- commands lock it, so no house or unit changes meanwhile; the lock is taken here too.
CREATE FUNCTION cargo_phy.check_close_envelope() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  unit_count bigint;
  unit_payload numeric;
  unit_capacity numeric;
  planned_weight numeric;
  planned_volume numeric;
  unchecked boolean;
  over boolean;
BEGIN
  IF NEW.change <> 'CLOSED' THEN
    RETURN NEW;
  END IF;
  PERFORM 1 FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR NO KEY UPDATE;
  SELECT count(*), coalesce(sum((max_gross_kg - tare_kg) * 1000), 0), coalesce(sum(capacity_cm3), 0) INTO unit_count, unit_payload, unit_capacity
    FROM cargo_phy.planned_load_unit WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id AND status = 'PLANNED';
  SELECT coalesce(sum(cargo_phy.grams(gross_weight_unit, gross_weight)), 0), coalesce(sum(cargo_phy.cubic_cm(volume_unit, volume)), 0),
      coalesce(bool_or(cargo_phy.grams(gross_weight_unit, gross_weight) IS NULL
        OR (volume IS NOT NULL AND cargo_phy.cubic_cm(volume_unit, volume) IS NULL)), false)
    INTO planned_weight, planned_volume, unchecked
    FROM cargo_phy.membership_entry WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id AND ended_at IS NULL;
  over := unit_count > 0 AND (unchecked OR planned_weight > unit_payload OR planned_volume > unit_capacity);
  IF over <> (NEW.reason IS NOT NULL) THEN
    RAISE EXCEPTION 'cargo_phy: close of consolidation %/% %', NEW.tenant_id, NEW.consolidation_id,
      CASE WHEN over THEN 'exceeds its load units'' envelope without an overrun reason' ELSE 'gives an overrun reason within its envelope' END
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_close_envelope';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER consolidation_version_rules_envelope BEFORE INSERT ON cargo_phy.consolidation_version
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_close_envelope();
