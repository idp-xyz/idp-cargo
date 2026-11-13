-- IDP-Cargo RATE V0.5 part 3a / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Quote lines entered by hand or rated (R6): a quote's kind and scope (the lane and cargo it is priced for), the
-- threshold of RATE_CARD_DISCOUNT it was priced under, and per line its source (MANUAL, RATED, RATED_OVERRIDDEN), the
-- snapshot lines and rated sell of a rated line, its basis (a lump sum or a rate per unit) and the catalogue version of
-- its code. Codes of lines priced before the catalogue that it does not list are kept and marked unlisted.

ALTER TABLE cargo_rate.quote ADD COLUMN kind text NOT NULL DEFAULT 'ONE_OFF' CHECK (kind IN ('ONE_OFF'));
ALTER TABLE cargo_rate.quote ADD COLUMN discount_threshold_bp integer NOT NULL DEFAULT 0 CHECK (discount_threshold_bp BETWEEN 0 AND 10000);
ALTER TABLE cargo_rate.quote ADD COLUMN scope_mode text, ADD COLUMN scope_container_mode text, ADD COLUMN scope_origin text,
  ADD COLUMN scope_destination text, ADD COLUMN scope_via text, ADD COLUMN scope_carrier text, ADD COLUMN scope_pieces bigint,
  ADD COLUMN scope_gross_grams bigint, ADD COLUMN scope_volume_cm3 bigint, ADD COLUMN scope_documents bigint;
ALTER TABLE cargo_rate.quote ADD CONSTRAINT quote_scope CHECK (
  (scope_mode IS NULL) = (scope_container_mode IS NULL) AND (scope_mode IS NULL) = (scope_origin IS NULL)
  AND (scope_mode IS NULL) = (scope_destination IS NULL) AND (scope_mode IS NULL) = (scope_pieces IS NULL)
  AND (scope_mode IS NULL) = (scope_gross_grams IS NULL) AND (scope_mode IS NULL) = (scope_volume_cm3 IS NULL)
  AND (scope_mode IS NULL) = (scope_documents IS NULL) AND (scope_mode IS NOT NULL OR (scope_via IS NULL AND scope_carrier IS NULL))
  AND (scope_mode IS NULL OR ((scope_mode, scope_container_mode) IN (('AIR','LOOSE'),('SEA','LCL'))
    AND scope_origin ~ '^[A-Z]{2}[A-Z2-9]{3}$' AND scope_destination ~ '^[A-Z]{2}[A-Z2-9]{3}$' AND scope_origin <> scope_destination
    AND (scope_via IS NULL OR scope_via ~ '^[A-Z]{2}[A-Z2-9]{3}$') AND scope_pieces >= 0 AND scope_gross_grams >= 0 AND scope_volume_cm3 >= 0
    AND scope_documents >= 0))) NOT VALID;
ALTER TABLE cargo_rate.quote VALIDATE CONSTRAINT quote_scope;

ALTER TABLE cargo_rate.quote_line ADD COLUMN source text NOT NULL DEFAULT 'MANUAL' CHECK (source IN ('MANUAL','RATED','RATED_OVERRIDDEN'));
ALTER TABLE cargo_rate.quote_line ADD COLUMN snapshot_id text, ADD COLUMN sell_snapshot_line_no integer, ADD COLUMN cost_snapshot_line_no integer,
  ADD COLUMN rated_sell_currency char(3), ADD COLUMN rated_sell_minor bigint CHECK (rated_sell_minor >= 0),
  ADD COLUMN override_reason text CHECK (length(btrim(override_reason)) BETWEEN 1 AND 500);
ALTER TABLE cargo_rate.quote_line ADD COLUMN basis text NOT NULL DEFAULT 'LUMP_SUM' CHECK (basis IN ('LUMP_SUM','PER_UNIT')),
  ADD COLUMN basis_unit text CHECK (basis_unit IN ('KG_CHARGEABLE','KG_GROSS','CBM','FREIGHT_TONNE','PIECE','DOCUMENT')),
  ADD COLUMN basis_rate_minor bigint CHECK (basis_rate_minor >= 0), ADD COLUMN basis_rate_scale smallint CHECK (basis_rate_scale BETWEEN 0 AND 6),
  ADD COLUMN basis_quantity bigint CHECK (basis_quantity >= 0);
ALTER TABLE cargo_rate.quote_line ADD COLUMN code_version integer CHECK (code_version >= 0), ADD COLUMN code_unlisted boolean NOT NULL DEFAULT false;
ALTER TABLE cargo_rate.quote_line ADD CONSTRAINT quote_line_source CHECK (CASE source
  WHEN 'MANUAL' THEN snapshot_id IS NULL AND sell_snapshot_line_no IS NULL AND cost_snapshot_line_no IS NULL AND rated_sell_minor IS NULL
    AND rated_sell_currency IS NULL AND override_reason IS NULL
  ELSE snapshot_id IS NOT NULL AND sell_snapshot_line_no IS NOT NULL AND rated_sell_minor IS NOT NULL AND rated_sell_currency IS NOT NULL
    AND (source = 'RATED') = (override_reason IS NULL)
    AND CASE source WHEN 'RATED' THEN sell_currency = rated_sell_currency AND sell_minor = rated_sell_minor
      ELSE sell_currency = rated_sell_currency AND sell_minor <> rated_sell_minor END END
  AND (basis = 'PER_UNIT') = (basis_unit IS NOT NULL) AND (basis_unit IS NULL) = (basis_rate_minor IS NULL)
  AND (basis_unit IS NULL) = (basis_rate_scale IS NULL) AND (basis_unit IS NULL) = (basis_quantity IS NULL)
  AND NOT (code_unlisted AND code_version IS NOT NULL)) NOT VALID;
ALTER TABLE cargo_rate.quote_line VALIDATE CONSTRAINT quote_line_source;

-- Lines priced before the catalogue keep their codes; a code the tenant's catalogue (the starter list and its own
-- versions) does not list is marked unlisted. Quote lines are immutable, so the marking runs with the trigger off and
-- the owner exempt from row-level security for its duration (as rate/000003 did).
ALTER TABLE cargo_rate.quote_line NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_rate.quote_line DISABLE TRIGGER quote_line_immutable;
-- the row checks the update queues (the tenant guard, which the migrator is exempt from) run at once, so that the table
-- can be altered again in this transaction
SET CONSTRAINTS ALL IMMEDIATE;
UPDATE cargo_rate.quote_line l SET code_unlisted = true
  WHERE NOT EXISTS (SELECT 1 FROM cargo_rate.charge_code_starter s WHERE s.code = l.charge_code)
    AND NOT EXISTS (SELECT 1 FROM cargo_rate.charge_code_version v WHERE v.tenant_id = l.tenant_id AND v.code = l.charge_code);
SET CONSTRAINTS ALL DEFERRED;
ALTER TABLE cargo_rate.quote_line ENABLE TRIGGER quote_line_immutable;
ALTER TABLE cargo_rate.quote_line FORCE ROW LEVEL SECURITY;
COMMENT ON COLUMN cargo_rate.quote_line.code_unlisted IS
  'The charge code was recorded as free text before the catalogue existed and the catalogue does not list it (RB12); kept as recorded.';

-- At commit: a RATED or RATED_OVERRIDDEN line equals its snapshot as rated: a snapshot of purpose QUOTE_LINE for the
-- quote's legal entity and customer, whose SELL line of the line's code (no gap) is the rated sell, and, when the line
-- names one, whose BUY line of the code is the cost.
CREATE FUNCTION cargo_rate.check_rated_quote_line_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.source = 'MANUAL' THEN
    RETURN NULL;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM cargo_rate.quote q JOIN cargo_rate.rate_snapshot s ON s.tenant_id = q.tenant_id AND s.snapshot_id = NEW.snapshot_id
       JOIN cargo_rate.rate_snapshot_line x ON x.tenant_id = s.tenant_id AND x.snapshot_id = s.snapshot_id AND x.line_no = NEW.sell_snapshot_line_no
       WHERE q.tenant_id = NEW.tenant_id AND q.quote_id = NEW.quote_id AND s.purpose = 'QUOTE_LINE' AND s.legal_entity_id = q.legal_entity_id
         AND s.customer_party_id IS NOT DISTINCT FROM q.customer_party_id AND s.mode = q.scope_mode AND s.origin = q.scope_origin
         AND s.destination = q.scope_destination AND x.side = 'SELL' AND x.gap IS NULL AND x.charge_code = NEW.charge_code
         AND x.currency = NEW.rated_sell_currency AND x.amount_minor = NEW.rated_sell_minor)
     OR (NEW.cost_snapshot_line_no IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_rate.rate_snapshot_line x WHERE x.tenant_id = NEW.tenant_id
       AND x.snapshot_id = NEW.snapshot_id AND x.line_no = NEW.cost_snapshot_line_no AND x.side = 'BUY' AND x.gap IS NULL
       AND x.charge_code = NEW.charge_code AND x.currency = NEW.cost_currency AND x.amount_minor = NEW.cost_minor)) THEN
    RAISE EXCEPTION 'cargo_rate: line % of quote %/% is not its snapshot as rated', NEW.line_no, NEW.tenant_id, NEW.quote_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'quote_line_rated';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER quote_line_rated AFTER INSERT ON cargo_rate.quote_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_rated_quote_line_at_commit();
