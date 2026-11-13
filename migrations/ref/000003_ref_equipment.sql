-- IDP-Cargo reference data / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Read-only equipment reference data (backlog PB8, the F2 pattern; MD-26: the reference importer maintains it,
-- contexts read it): ISO 6346 container size-types with their TEU and default limits. PHY freezes a planned load
-- unit's limits from the current edition, with the edition. The limits are defaults: a container's own tare and
-- maximum gross are on its CSC plate, which SEA records when it assigns the container. Data is kept by edition and
-- an edition never changes; one edition is current. Not tenant data: no row level security; the application reads it.

CREATE TABLE cargo_ref.equipment_edition (
  edition text PRIMARY KEY CHECK (edition ~ '^[A-Z0-9][A-Z0-9._-]{0,63}$'),
  source text NOT NULL CHECK (length(btrim(source)) > 0),
  source_digest text NOT NULL CHECK (source_digest ~ '^sha256:[0-9a-f]{64}$'),
  published date NOT NULL,
  note text,
  is_current boolean NOT NULL DEFAULT false,
  loaded_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX equipment_edition_one_current ON cargo_ref.equipment_edition (is_current) WHERE is_current;

-- An ISO 6346 size-type code (two characters of size, two of type): its TEU, maximum gross, tare and internal
-- capacity (in cubic centimetres, as PHY measures volume in base units).
CREATE TABLE cargo_ref.container_size_type (
  edition text NOT NULL REFERENCES cargo_ref.equipment_edition (edition),
  code text NOT NULL CHECK (code ~ '^[0-9A-Z]{2}[A-Z][0-9A-Z]$'),
  description text NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 200),
  teu numeric(3,1) NOT NULL CHECK (teu > 0 AND teu <= 3),
  max_gross_kg integer NOT NULL CHECK (max_gross_kg > 0),
  tare_kg integer NOT NULL CHECK (tare_kg > 0),
  capacity_cm3 bigint NOT NULL CHECK (capacity_cm3 > 0),
  PRIMARY KEY (edition, code),
  CHECK (tare_kg < max_gross_kg)
);

-- An edition never changes once loaded; only which edition is current may change.
CREATE FUNCTION cargo_ref.refuse_equipment_rewrite() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_TABLE_NAME = 'equipment_edition' AND TG_OP = 'UPDATE'
     AND (NEW.edition, NEW.source, NEW.source_digest, NEW.published, NEW.note, NEW.loaded_at)
         IS NOT DISTINCT FROM (OLD.edition, OLD.source, OLD.source_digest, OLD.published, OLD.note, OLD.loaded_at) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'cargo_ref: % rows of edition % cannot be %d', TG_TABLE_NAME, OLD.edition, lower(TG_OP)
    USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER equipment_edition_kept BEFORE UPDATE OR DELETE ON cargo_ref.equipment_edition
  FOR EACH ROW EXECUTE FUNCTION cargo_ref.refuse_equipment_rewrite();
CREATE TRIGGER container_size_type_kept BEFORE UPDATE OR DELETE ON cargo_ref.container_size_type
  FOR EACH ROW EXECUTE FUNCTION cargo_ref.refuse_equipment_rewrite();
