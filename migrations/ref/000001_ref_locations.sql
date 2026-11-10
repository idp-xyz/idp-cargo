-- IDP-Cargo reference data / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Read-only location reference data (repository structure baseline §11: internal/reference, schema cargo_ref;
-- MD-26: the reference importer maintains it, contexts read it). Decision F2, option 4: every transport mode
-- stores UN/LOCODEs; AIR needs an airport-level location with one IATA airport code; IATA codes come from
-- verified airport data, never from the last three letters of a UN/LOCODE; aliases carriers use resolve to the
-- current code. Data is kept by edition and an edition never changes; one edition is current.
-- Reference data is not tenant data: there is no row level security, and the application role only reads it.

CREATE SCHEMA IF NOT EXISTS cargo_ref;

CREATE TABLE cargo_ref.location_edition (
  edition text PRIMARY KEY CHECK (edition ~ '^[A-Z0-9][A-Z0-9._-]{0,63}$'),
  source text NOT NULL CHECK (length(btrim(source)) > 0),
  source_digest text NOT NULL CHECK (source_digest ~ '^sha256:[0-9a-f]{64}$'),
  published date NOT NULL,
  note text,
  is_current boolean NOT NULL DEFAULT false,
  loaded_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX location_edition_one_current ON cargo_ref.location_edition (is_current) WHERE is_current;

-- A UN/LOCODE entry. functions are the eight UN/LOCODE function classifiers: 1 port, 2 rail, 3 road, 4 airport,
-- 5 postal, 6 multimodal, 7 fixed transport, B border crossing, '-' where the function is absent. They are kept
-- as published: the release does not always place a classifier in its own position (AEDXB reads 1-345--6), so
-- only the characters are checked. The port (position 1) and airport (position 4) classifiers FUL relies on
-- are the ones it checks.
CREATE TABLE cargo_ref.location (
  edition text NOT NULL REFERENCES cargo_ref.location_edition (edition),
  unlocode text NOT NULL CHECK (unlocode ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
  name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
  country_code char(2) NOT NULL,
  functions char(8) NOT NULL CHECK (functions ~ '^[-0-9B]{8}$'),
  status char(2) NOT NULL CHECK (status ~ '^[A-Z]{2}$'),
  PRIMARY KEY (edition, unlocode),
  CHECK (country_code = left(unlocode, 2))
);

-- IATA airport codes of locations: one location per code and one code per location in an edition, and only on
-- locations with the airport function.
CREATE TABLE cargo_ref.airport_code (
  edition text NOT NULL,
  iata char(3) NOT NULL CHECK (iata ~ '^[A-Z]{3}$'),
  unlocode text NOT NULL,
  source text NOT NULL CHECK (length(btrim(source)) > 0),
  PRIMARY KEY (edition, iata),
  UNIQUE (edition, unlocode),
  FOREIGN KEY (edition, unlocode) REFERENCES cargo_ref.location (edition, unlocode)
);

-- Codes carriers use for a location in a mode, such as CNSHA for the port of Shanghai, which UN/LOCODE now
-- gives to Hongqiao airport. An alias resolves only where its code does not serve the mode itself.
CREATE TABLE cargo_ref.location_alias (
  edition text NOT NULL,
  mode text NOT NULL CHECK (mode IN ('AIR','SEA','ROAD','RAIL')),
  alias text NOT NULL CHECK (alias ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
  unlocode text NOT NULL,
  source text NOT NULL CHECK (length(btrim(source)) > 0),
  PRIMARY KEY (edition, mode, alias),
  FOREIGN KEY (edition, unlocode) REFERENCES cargo_ref.location (edition, unlocode),
  CHECK (alias <> unlocode)
);

CREATE FUNCTION cargo_ref.airport_needs_airport_function() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_ref.location l WHERE l.edition = NEW.edition AND l.unlocode = NEW.unlocode
                 AND substr(l.functions, 4, 1) = '4') THEN
    RAISE EXCEPTION 'cargo_ref: % of edition % has no airport function for IATA %', NEW.unlocode, NEW.edition, NEW.iata
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER airport_code_airport_function BEFORE INSERT ON cargo_ref.airport_code
  FOR EACH ROW EXECUTE FUNCTION cargo_ref.airport_needs_airport_function();

-- An edition never changes once loaded; only which edition is current may change.
CREATE FUNCTION cargo_ref.refuse_edition_rewrite() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_TABLE_NAME = 'location_edition' AND TG_OP = 'UPDATE'
     AND (NEW.edition, NEW.source, NEW.source_digest, NEW.published, NEW.note, NEW.loaded_at)
         IS NOT DISTINCT FROM (OLD.edition, OLD.source, OLD.source_digest, OLD.published, OLD.note, OLD.loaded_at) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'cargo_ref: % rows of edition % cannot be %d', TG_TABLE_NAME, OLD.edition, lower(TG_OP)
    USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER location_edition_kept BEFORE UPDATE OR DELETE ON cargo_ref.location_edition
  FOR EACH ROW EXECUTE FUNCTION cargo_ref.refuse_edition_rewrite();
CREATE TRIGGER location_kept BEFORE UPDATE OR DELETE ON cargo_ref.location
  FOR EACH ROW EXECUTE FUNCTION cargo_ref.refuse_edition_rewrite();
CREATE TRIGGER airport_code_kept BEFORE UPDATE OR DELETE ON cargo_ref.airport_code
  FOR EACH ROW EXECUTE FUNCTION cargo_ref.refuse_edition_rewrite();
CREATE TRIGGER location_alias_kept BEFORE UPDATE OR DELETE ON cargo_ref.location_alias
  FOR EACH ROW EXECUTE FUNCTION cargo_ref.refuse_edition_rewrite();
