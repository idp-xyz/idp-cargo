-- IDP-Cargo AIR V0.3.1 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- HAWB numbers from idp-platform-go's numbering (MD-28): a legal entity's HAWB series is now a numbering template and
-- its first number, fixed once; the platform's sequence table cargo_air.numbering_sequence (created by cargo-migrate from
-- the platform's asset just before AIR's migrations) draws the numbers in the association's transaction. Cargo owns that
-- table's data and permissions: rows are scoped by tenant (row-level security); a sequence starts at 1 or at its series'
-- seed, only for a registered series, moves one number at a time and is never deleted. HAWB templates do not reset yet
-- (AIR does not know the issuing office's time zone). An existing series becomes the continuous template
-- <prefix>{seq:<width>}, its sequence after the last number drawn and its first number the lowest it drew, so issued
-- numbers stay and new ones continue.

INSERT INTO cargo_air.numbering_sequence (scope, doc_type, period, next_val)
  SELECT tenant_id, 'HAWB/' || legal_entity_id, '', next_seq - 1 FROM cargo_air.hawb_series WHERE next_seq > 1;

ALTER TABLE cargo_air.numbering_sequence ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_air.numbering_sequence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_air.numbering_sequence USING (scope = current_setting('app.tenant_id', true))
  WITH CHECK (scope = current_setting('app.tenant_id', true));
CREATE FUNCTION cargo_air.numbering_forward_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_air.hawb_series;
BEGIN
  IF TG_OP = 'INSERT' THEN
    SELECT * INTO s FROM cargo_air.hawb_series x WHERE x.tenant_id = NEW.scope AND 'HAWB/' || x.legal_entity_id = NEW.doc_type;
    IF s.tenant_id IS NULL OR NOT (NEW.next_val = 1 OR (NEW.period = s.seed_period AND NEW.next_val = s.first_number - 1)) THEN
      RAISE EXCEPTION 'cargo_air: a numbering sequence starts at 1 or at its series'' seed' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' OR NEW.scope <> OLD.scope OR NEW.doc_type <> OLD.doc_type OR NEW.period <> OLD.period OR NEW.next_val <> OLD.next_val + 1 THEN
    RAISE EXCEPTION 'cargo_air: a numbering sequence moves one number at a time' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

DROP TRIGGER hawb_series_rules ON cargo_air.hawb_series;
DROP FUNCTION cargo_air.reject_series_change();
ALTER TABLE cargo_air.hawb_series ADD COLUMN template text, ADD COLUMN first_number bigint, ADD COLUMN seed_period text NOT NULL DEFAULT '';
UPDATE cargo_air.hawb_series s SET template = s.prefix || '{seq:' || s.width || '}', first_number = coalesce((SELECT min(substr(h.hawb,
    length(s.prefix) + 1)::bigint) FROM cargo_air.hawb_number h WHERE h.tenant_id = s.tenant_id AND h.legal_entity_id = s.legal_entity_id AND h.drawn
    AND length(h.hawb) = length(s.prefix) + s.width AND starts_with(h.hawb, s.prefix) AND substr(h.hawb, length(s.prefix) + 1) ~ '^[0-9]+$'), s.next_seq);
ALTER TABLE cargo_air.hawb_series DROP CONSTRAINT hawb_series_length, DROP COLUMN prefix, DROP COLUMN width, DROP COLUMN next_seq;
ALTER TABLE cargo_air.hawb_series ALTER COLUMN template SET NOT NULL, ALTER COLUMN first_number SET NOT NULL,
  ADD CONSTRAINT hawb_series_template CHECK (template ~ '^[0-9A-Z]+\{seq:([4-9]|10)\}[0-9A-Z]*$' AND length(template) <= 40 AND first_number >= 1
    AND seed_period = '');
CREATE TRIGGER hawb_series_immutable BEFORE UPDATE OR DELETE ON cargo_air.hawb_series FOR EACH ROW EXECUTE FUNCTION cargo_air.reject_row_change();
-- Rows inserted from here on start at 1 or at their series' seed, and move one number at a time.
CREATE TRIGGER numbering_sequence_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_air.numbering_sequence
  FOR EACH ROW EXECUTE FUNCTION cargo_air.numbering_forward_only();
