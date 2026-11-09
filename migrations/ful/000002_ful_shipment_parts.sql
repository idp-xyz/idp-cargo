-- IDP-Cargo FUL V0.2 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Owner decision F1: a shipment plans a part of an obligation version (ShipmentPart), and the parts of one
-- obligation may be planned in several shipments. The live parts of an obligation never exceed what COM
-- committed; a carrier's shortload or a rollover is replanned by changing parts, with a reason, without a COM
-- amendment. A V0.1 entry planned the whole obligation, so its part is the whole and converts without loss.

ALTER TABLE cargo_ful.shipment_obligation
  ADD COLUMN part_packs bigint,
  ADD COLUMN part_gross_weight bigint,
  ADD COLUMN part_volume bigint;

-- Entries are immutable to the application (only a release is allowed), and the table forces row level
-- security, so the conversion runs with the rules trigger off and the owner exempt for its duration.
ALTER TABLE cargo_ful.shipment_obligation NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_ful.shipment_obligation DISABLE TRIGGER shipment_obligation_rules;
UPDATE cargo_ful.shipment_obligation SET part_packs = packs, part_gross_weight = gross_weight, part_volume = volume;
ALTER TABLE cargo_ful.shipment_obligation ENABLE TRIGGER shipment_obligation_rules;
ALTER TABLE cargo_ful.shipment_obligation FORCE ROW LEVEL SECURITY;

-- packs, gross weight and volume stay what COM committed for the whole obligation; part_* is this entry's part.
ALTER TABLE cargo_ful.shipment_obligation
  ALTER COLUMN part_packs SET NOT NULL,
  ALTER COLUMN part_gross_weight SET NOT NULL,
  ADD CONSTRAINT shipment_obligation_part CHECK (part_packs > 0 AND part_packs <= packs
    AND part_gross_weight > 0 AND part_gross_weight <= gross_weight
    AND (part_volume IS NULL) = (volume IS NULL) AND (part_volume IS NULL OR (part_volume > 0 AND part_volume <= volume)));

-- A shipment plans at most one live part of an obligation; several shipments may each plan one.
DROP INDEX cargo_ful.shipment_obligation_one_live_shipment;
CREATE UNIQUE INDEX shipment_obligation_one_live_part ON cargo_ful.shipment_obligation (tenant_id, shipment_id, obligation_id)
  WHERE released_at IS NULL;
CREATE INDEX shipment_obligation_live_by_obligation ON cargo_ful.shipment_obligation (tenant_id, obligation_id)
  WHERE released_at IS NULL;

-- A changed part is a plan version of its own, and says why.
ALTER TABLE cargo_ful.shipment_plan_version
  DROP CONSTRAINT shipment_plan_version_change_check,
  ADD CONSTRAINT shipment_plan_version_change_check
    CHECK (change IN ('CREATED','OBLIGATION_ADDED','AMENDED','OBLIGATION_RELEASED','PART_CHANGED')),
  DROP CONSTRAINT shipment_plan_version_reason,
  ADD CONSTRAINT shipment_plan_version_reason CHECK ((change IN ('AMENDED','PART_CHANGED')) = (reason IS NOT NULL));

-- A planned part belongs to the shipment's customer and legal entity and joins a shipment that is not
-- cancelled; afterwards it can only be released, once. The shipment row is locked first. A live part also
-- takes the obligation's lock, the one FUL takes before it reads what other shipments plan of the obligation,
-- and then must fit: every live part of the obligation is of the same version and COM commitment, and
-- together they do not exceed its packs, gross weight or volume.
CREATE OR REPLACE FUNCTION cargo_ful.check_shipment_obligation() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_ful.shipment;
  mismatched bigint;
  planned_packs bigint;
  planned_weight bigint;
  planned_volume bigint;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_ful: planned obligations are kept' USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF OLD.released_at IS NOT NULL OR NEW.released_at IS NULL
       OR to_jsonb(OLD) - ARRAY['released_by','released_at'] IS DISTINCT FROM to_jsonb(NEW) - ARRAY['released_by','released_at'] THEN
      RAISE EXCEPTION 'cargo_ful: planned obligation %/%/% can only be released, once', OLD.tenant_id, OLD.shipment_id, OLD.entry_no
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  SELECT * INTO s FROM cargo_ful.shipment WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id FOR NO KEY UPDATE;
  IF NOT FOUND OR s.status = 'CANCELLED' OR s.customer_party_id <> NEW.customer_party_id OR s.legal_entity_id <> NEW.legal_entity_id THEN
    RAISE EXCEPTION 'cargo_ful: obligation %/% does not belong in shipment %', NEW.tenant_id, NEW.obligation_id, NEW.shipment_id
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.released_at IS NULL THEN
    PERFORM pg_advisory_xact_lock(hashtextextended('cargo_ful.obligation:' || NEW.tenant_id || '/' || NEW.obligation_id, 0));
    SELECT count(*) FILTER (WHERE (o.obligation_version, o.packs, o.gross_weight_unit, o.gross_weight, o.volume_unit, o.volume)
             IS DISTINCT FROM (NEW.obligation_version, NEW.packs, NEW.gross_weight_unit, NEW.gross_weight, NEW.volume_unit, NEW.volume)),
           coalesce(sum(o.part_packs), 0), coalesce(sum(o.part_gross_weight), 0), coalesce(sum(o.part_volume), 0)
      INTO mismatched, planned_packs, planned_weight, planned_volume
      FROM cargo_ful.shipment_obligation o
      WHERE o.tenant_id = NEW.tenant_id AND o.obligation_id = NEW.obligation_id AND o.released_at IS NULL;
    IF mismatched > 0 OR planned_packs + NEW.part_packs > NEW.packs OR planned_weight + NEW.part_gross_weight > NEW.gross_weight
       OR planned_volume + coalesce(NEW.part_volume, 0) > coalesce(NEW.volume, 0) THEN
      RAISE EXCEPTION 'cargo_ful: the live parts of obligation %/% would exceed it', NEW.tenant_id, NEW.obligation_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_obligation_within_obligation';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;
