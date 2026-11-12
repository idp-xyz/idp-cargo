-- IDP-Cargo AIR V0.3.1 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- HAWB numbers from idp-platform-go's numbering (MD-28): a legal entity's HAWB series is now a numbering template and
-- its first number, fixed once; the platform's sequence table cargo_air.numbering_sequence (created by cargo-migrate from
-- the platform's asset just before AIR's migrations) draws the numbers in the association's transaction. Cargo owns that
-- table's data and permissions: rows are scoped by tenant (row-level security), a sequence only moves forward and is
-- never deleted. An existing series becomes the continuous template <prefix>{seq:<width>}, its sequence starting after
-- the last number drawn, so issued numbers stay and new ones continue.

INSERT INTO cargo_air.numbering_sequence (scope, doc_type, period, next_val)
  SELECT tenant_id, 'HAWB/' || legal_entity_id, '', next_seq - 1 FROM cargo_air.hawb_series WHERE next_seq > 1;

ALTER TABLE cargo_air.numbering_sequence ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_air.numbering_sequence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_air.numbering_sequence USING (scope = current_setting('app.tenant_id', true))
  WITH CHECK (scope = current_setting('app.tenant_id', true));
CREATE FUNCTION cargo_air.numbering_forward_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR NEW.scope <> OLD.scope OR NEW.doc_type <> OLD.doc_type OR NEW.period <> OLD.period OR NEW.next_val <= OLD.next_val THEN
    RAISE EXCEPTION 'cargo_air: a numbering sequence only moves forward' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER numbering_sequence_forward BEFORE UPDATE OR DELETE ON cargo_air.numbering_sequence
  FOR EACH ROW EXECUTE FUNCTION cargo_air.numbering_forward_only();

DROP TRIGGER hawb_series_rules ON cargo_air.hawb_series;
DROP FUNCTION cargo_air.reject_series_change();
ALTER TABLE cargo_air.hawb_series ADD COLUMN template text, ADD COLUMN first_number bigint, ADD COLUMN seed_period text NOT NULL DEFAULT '';
UPDATE cargo_air.hawb_series SET template = prefix || '{seq:' || width || '}', first_number = next_seq;
ALTER TABLE cargo_air.hawb_series DROP CONSTRAINT hawb_series_length, DROP COLUMN prefix, DROP COLUMN width, DROP COLUMN next_seq;
ALTER TABLE cargo_air.hawb_series ALTER COLUMN template SET NOT NULL, ALTER COLUMN first_number SET NOT NULL,
  ADD CONSTRAINT hawb_series_template CHECK (template ~ '^[0-9A-Z]+\{' AND length(template) <= 40 AND first_number >= 1
    AND length(seed_period) <= 8);
CREATE TRIGGER hawb_series_immutable BEFORE UPDATE OR DELETE ON cargo_air.hawb_series FOR EACH ROW EXECUTE FUNCTION cargo_air.reject_row_change();
