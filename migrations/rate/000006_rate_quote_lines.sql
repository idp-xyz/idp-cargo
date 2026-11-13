-- IDP-Cargo RATE V0.5 part 3a / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Quote lines entered by hand or rated (R6): a quote's kind and scope (the lane and cargo it is priced for), the
-- threshold of RATE_CARD_DISCOUNT it was priced under, and per line its source (MANUAL, RATED, RATED_OVERRIDDEN), the
-- snapshot lines and rated sell of a rated line, its basis (a lump sum or a rate per unit) and the catalogue version of
-- its code. Codes of lines priced before the catalogue that it does not list are kept and marked unlisted.

-- The characters Go's strings.TrimSpace removes (Unicode White_Space), for checks that agree with RATE's.
CREATE FUNCTION cargo_rate.white_space() RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE
  RETURN E' \t\n\u000B\f\r\u0085                 　';

ALTER TABLE cargo_rate.quote ADD COLUMN kind text NOT NULL DEFAULT 'ONE_OFF' CHECK (kind IN ('ONE_OFF'));
ALTER TABLE cargo_rate.quote ADD COLUMN discount_threshold_bp integer NOT NULL DEFAULT 0 CHECK (discount_threshold_bp BETWEEN 0 AND 10000);
ALTER TABLE cargo_rate.quote ADD COLUMN scope_mode text, ADD COLUMN scope_container_mode text, ADD COLUMN scope_origin text,
  ADD COLUMN scope_destination text, ADD COLUMN scope_via text, ADD COLUMN scope_carrier text, ADD COLUMN scope_pieces bigint,
  ADD COLUMN scope_gross_grams bigint, ADD COLUMN scope_volume_cm3 bigint, ADD COLUMN scope_documents bigint;
-- the bounds are RATE's (domain.MaxCount, MaxQuantity, MaxVolume); a carrier is an identifier without surrounding blanks
ALTER TABLE cargo_rate.quote ADD CONSTRAINT quote_scope CHECK (
  (scope_mode IS NULL) = (scope_container_mode IS NULL) AND (scope_mode IS NULL) = (scope_origin IS NULL)
  AND (scope_mode IS NULL) = (scope_destination IS NULL) AND (scope_mode IS NULL) = (scope_pieces IS NULL)
  AND (scope_mode IS NULL) = (scope_gross_grams IS NULL) AND (scope_mode IS NULL) = (scope_volume_cm3 IS NULL)
  AND (scope_mode IS NULL) = (scope_documents IS NULL) AND (scope_mode IS NOT NULL OR (scope_via IS NULL AND scope_carrier IS NULL))
  AND (scope_mode IS NULL OR ((scope_mode, scope_container_mode) IN (('AIR','LOOSE'),('SEA','LCL'))
    AND scope_origin ~ '^[A-Z]{2}[A-Z2-9]{3}$' AND scope_destination ~ '^[A-Z]{2}[A-Z2-9]{3}$' AND scope_origin <> scope_destination
    AND (scope_via IS NULL OR scope_via ~ '^[A-Z]{2}[A-Z2-9]{3}$')
    AND (scope_carrier IS NULL OR (length(scope_carrier) BETWEEN 1 AND 128 AND scope_carrier = btrim(scope_carrier, cargo_rate.white_space())))
    AND scope_pieces BETWEEN 0 AND 1000000000 AND scope_gross_grams BETWEEN 0 AND 1000000000000
    AND scope_volume_cm3 BETWEEN 0 AND 10000000000000 AND scope_documents BETWEEN 0 AND 1000000000))) NOT VALID;
ALTER TABLE cargo_rate.quote VALIDATE CONSTRAINT quote_scope;

ALTER TABLE cargo_rate.quote_line ADD COLUMN source text NOT NULL DEFAULT 'MANUAL' CHECK (source IN ('MANUAL','RATED','RATED_OVERRIDDEN'));
-- an override reason has 1 to 500 characters and is not blank, blank meaning Go's strings.TrimSpace (Unicode White_Space)
ALTER TABLE cargo_rate.quote_line ADD COLUMN snapshot_id text, ADD COLUMN sell_snapshot_line_no integer, ADD COLUMN cost_snapshot_line_no integer,
  ADD COLUMN rated_sell_currency char(3), ADD COLUMN rated_sell_minor bigint CHECK (rated_sell_minor >= 0),
  ADD COLUMN override_reason text CHECK (length(override_reason) <= 500 AND btrim(override_reason, cargo_rate.white_space()) <> '');
ALTER TABLE cargo_rate.quote_line ADD COLUMN basis text NOT NULL DEFAULT 'LUMP_SUM' CHECK (basis IN ('LUMP_SUM','PER_UNIT')),
  ADD COLUMN basis_unit text CHECK (basis_unit IN ('KG_CHARGEABLE','KG_GROSS','CBM','FREIGHT_TONNE','PIECE','DOCUMENT')),
  ADD COLUMN basis_rate_minor bigint CHECK (basis_rate_minor >= 0), ADD COLUMN basis_rate_scale smallint CHECK (basis_rate_scale BETWEEN 0 AND 6),
  ADD COLUMN basis_quantity bigint CHECK (basis_quantity > 0);
ALTER TABLE cargo_rate.quote_line ADD COLUMN code_version integer CHECK (code_version >= 0), ADD COLUMN code_unlisted boolean NOT NULL DEFAULT false;
-- A MANUAL line names no snapshot and is a lump sum. A RATED line sells what was rated; only a RATED line can be priced
-- per unit (its snapshot line's rate on its charged quantity, which reproduces the sell). An overridden line sells another
-- amount in the rated currency, not below zero, as a lump sum, with a reason.
ALTER TABLE cargo_rate.quote_line ADD CONSTRAINT quote_line_source CHECK (CASE source
  WHEN 'MANUAL' THEN snapshot_id IS NULL AND sell_snapshot_line_no IS NULL AND cost_snapshot_line_no IS NULL AND rated_sell_minor IS NULL
    AND rated_sell_currency IS NULL AND override_reason IS NULL AND basis = 'LUMP_SUM'
  ELSE snapshot_id IS NOT NULL AND sell_snapshot_line_no IS NOT NULL AND rated_sell_minor IS NOT NULL AND rated_sell_currency IS NOT NULL
    AND (source = 'RATED') = (override_reason IS NULL)
    AND CASE source WHEN 'RATED' THEN sell_currency = rated_sell_currency AND sell_minor = rated_sell_minor
      ELSE sell_currency = rated_sell_currency AND sell_minor <> rated_sell_minor AND sell_minor >= 0 AND basis = 'LUMP_SUM' END END
  AND (basis = 'PER_UNIT') = (basis_unit IS NOT NULL) AND (basis_unit IS NULL) = (basis_rate_minor IS NULL)
  AND (basis_unit IS NULL) = (basis_rate_scale IS NULL) AND (basis_unit IS NULL) = (basis_quantity IS NULL)
  AND NOT (code_unlisted AND code_version IS NOT NULL)) NOT VALID;
ALTER TABLE cargo_rate.quote_line VALIDATE CONSTRAINT quote_line_source;
-- a snapshot's SELL line prices one line of a quote
CREATE UNIQUE INDEX quote_line_snapshot_line_once ON cargo_rate.quote_line (tenant_id, quote_id, snapshot_id, sell_snapshot_line_no)
  WHERE snapshot_id IS NOT NULL;

-- Lines priced before the catalogue keep their codes; a code the tenant's catalogue (the starter list and its own
-- versions) does not list is marked unlisted. Quote lines are immutable, so the marking runs with the trigger off. The
-- migrator need not bypass row-level security: quote_line is not forced for its owner while its tenants are listed, and
-- the marking runs bound to each tenant in turn, so that the tenant's code versions are visible and the tenant guard,
-- run at once, sees the tenant it checks.
ALTER TABLE cargo_rate.quote_line NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_rate.quote_line DISABLE TRIGGER quote_line_immutable;
SET CONSTRAINTS ALL IMMEDIATE;
DO $mark$
DECLARE
  t text;
  bound text := current_setting('app.tenant_id', true);
BEGIN
  FOR t IN SELECT DISTINCT tenant_id FROM cargo_rate.quote_line ORDER BY 1 LOOP
    PERFORM set_config('app.tenant_id', t, true);
    UPDATE cargo_rate.quote_line l SET code_unlisted = true
      WHERE l.tenant_id = t AND NOT EXISTS (SELECT 1 FROM cargo_rate.charge_code_starter s WHERE s.code = l.charge_code)
        AND NOT EXISTS (SELECT 1 FROM cargo_rate.charge_code_version v WHERE v.tenant_id = l.tenant_id AND v.code = l.charge_code);
  END LOOP;
  PERFORM set_config('app.tenant_id', coalesce(bound, ''), true);
END $mark$;
SET CONSTRAINTS ALL DEFERRED;
ALTER TABLE cargo_rate.quote_line ENABLE TRIGGER quote_line_immutable;
ALTER TABLE cargo_rate.quote_line FORCE ROW LEVEL SECURITY;
COMMENT ON COLUMN cargo_rate.quote_line.code_unlisted IS
  'The charge code was recorded as free text before the catalogue existed and the catalogue does not list it (RB12); kept as recorded.';
COMMENT ON COLUMN cargo_rate.quote_line.code_version IS
  'The catalogue version of the code the line was priced with: 0 for a starter code the tenant never changed; NULL for a line priced before the catalogue.';

-- A quote keeps what it was priced on. Only what the store writes as the quote moves on (its approval, acceptance and
-- expiry, with its revision and update time) changes; the rest of a row, columns added later included, never does.
-- A migration that converts quotes disables this trigger as it does quote_accepted_immutable.
CREATE FUNCTION cargo_rate.reject_priced_quote_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  moves text[] := ARRAY['status','approved_by','approved_at','approval_reason_ref','self_approved','accepted_by','accepted_at',
    'accepted_recorded_at','acceptance_evidence_kind','acceptance_evidence_ref','accepted_customer_revision','expired_at','revision','updated_at'];
BEGIN
  IF to_jsonb(NEW) - moves IS DISTINCT FROM to_jsonb(OLD) - moves THEN
    RAISE EXCEPTION 'cargo_rate: quote %/% keeps what it was priced on', OLD.tenant_id, OLD.quote_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER quote_priced_frozen BEFORE UPDATE ON cargo_rate.quote FOR EACH ROW EXECUTE FUNCTION cargo_rate.reject_priced_quote_change();

-- At commit: a quote's lines and exchange rates are written with the quote, in the transaction that created it, so that
-- no line is added to a quote once priced, approved or accepted.
CREATE FUNCTION cargo_rate.check_written_with_quote_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_rate.quote q WHERE q.tenant_id = NEW.tenant_id AND q.quote_id = NEW.quote_id
       AND q.xmin = pg_current_xact_id()::xid AND q.created_at = now() AND q.revision = 1 AND q.status IN ('AWAITING_APPROVAL','PRICED')) THEN
    RAISE EXCEPTION 'cargo_rate: % of quote %/% is written with the quote', TG_TABLE_NAME, NEW.tenant_id, NEW.quote_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'quote_written_once';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER quote_line_written_with_quote AFTER INSERT ON cargo_rate.quote_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_written_with_quote_at_commit();
CREATE CONSTRAINT TRIGGER quote_exchange_rate_written_with_quote AFTER INSERT ON cargo_rate.quote_exchange_rate
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_written_with_quote_at_commit();

-- At commit: a new line takes a code of the catalogue, at a version it lists: 0 for a code of the starter list, else an
-- active version of the tenant's.
CREATE FUNCTION cargo_rate.check_quote_line_code_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.code_unlisted OR NEW.code_version IS NULL
     OR NOT (CASE WHEN NEW.code_version = 0 THEN EXISTS (SELECT 1 FROM cargo_rate.charge_code_starter s WHERE s.code = NEW.charge_code)
       ELSE EXISTS (SELECT 1 FROM cargo_rate.charge_code_version v WHERE v.tenant_id = NEW.tenant_id AND v.code = NEW.charge_code
         AND v.version = NEW.code_version AND v.active) END) THEN
    RAISE EXCEPTION 'cargo_rate: line % of quote %/% takes code % of the catalogue at a version it lists', NEW.line_no, NEW.tenant_id,
      NEW.quote_id, NEW.charge_code USING ERRCODE = 'check_violation', CONSTRAINT = 'quote_line_catalogued';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER quote_line_catalogued AFTER INSERT ON cargo_rate.quote_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_quote_line_code_at_commit();

-- At commit: a RATED or RATED_OVERRIDDEN line equals its snapshot as rated. The snapshot is of purpose QUOTE_LINE, for the
-- quote's legal entity and customer, for no other subject than the quote, and was rated for the quote's scope (mode,
-- container mode, lane, carrier and cargo) and service level. Its SELL line of the line's code (no gap) is the rated sell
-- and gives the basis: the line is priced per unit (the snapshot line's unit, rate and charged quantity) exactly when it
-- is RATED from a PER_UNIT line that no minimum or maximum bounded, and is a lump sum otherwise. When the snapshot priced
-- the code's BUY side, the line names that BUY line and its cost is the line's.
CREATE FUNCTION cargo_rate.check_rated_quote_line_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.source = 'MANUAL' THEN
    RETURN NULL;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM cargo_rate.quote q JOIN cargo_rate.rate_snapshot s ON s.tenant_id = q.tenant_id AND s.snapshot_id = NEW.snapshot_id
       JOIN cargo_rate.rate_snapshot_line x ON x.tenant_id = s.tenant_id AND x.snapshot_id = s.snapshot_id AND x.line_no = NEW.sell_snapshot_line_no
       WHERE q.tenant_id = NEW.tenant_id AND q.quote_id = NEW.quote_id AND s.purpose = 'QUOTE_LINE' AND s.legal_entity_id = q.legal_entity_id
         AND s.customer_party_id IS NOT DISTINCT FROM q.customer_party_id
         AND (s.subject_kind IS NULL OR (s.subject_kind = 'QUOTE' AND s.subject_id = q.quote_id))
         AND s.mode = q.scope_mode AND s.container_mode = q.scope_container_mode AND s.origin = q.scope_origin
         AND s.destination = q.scope_destination AND s.via IS NOT DISTINCT FROM q.scope_via AND s.carrier_party_id IS NOT DISTINCT FROM q.scope_carrier
         AND s.pieces = q.scope_pieces AND s.gross_grams = q.scope_gross_grams AND s.volume_cm3 = q.scope_volume_cm3
         AND s.documents = q.scope_documents AND s.service_level = q.service_level
         AND x.side = 'SELL' AND x.gap IS NULL AND x.charge_code = NEW.charge_code
         AND x.currency = NEW.rated_sell_currency AND x.amount_minor = NEW.rated_sell_minor
         AND CASE WHEN NEW.source = 'RATED' AND x.calculator = 'PER_UNIT' AND NOT x.minimum_applied AND NOT x.maximum_applied
           THEN NEW.basis = 'PER_UNIT' AND NEW.basis_unit = x.rate_unit AND NEW.basis_rate_minor = x.rate_minor
             AND NEW.basis_rate_scale = x.rate_scale AND NEW.basis_quantity = x.charged
           ELSE NEW.basis = 'LUMP_SUM' END)
     OR (CASE WHEN NEW.cost_snapshot_line_no IS NULL THEN EXISTS (SELECT 1 FROM cargo_rate.rate_snapshot_line x WHERE x.tenant_id = NEW.tenant_id
         AND x.snapshot_id = NEW.snapshot_id AND x.side = 'BUY' AND x.gap IS NULL AND x.charge_code = NEW.charge_code)
       ELSE NOT EXISTS (SELECT 1 FROM cargo_rate.rate_snapshot_line x WHERE x.tenant_id = NEW.tenant_id
         AND x.snapshot_id = NEW.snapshot_id AND x.line_no = NEW.cost_snapshot_line_no AND x.side = 'BUY' AND x.gap IS NULL
         AND x.charge_code = NEW.charge_code AND x.currency = NEW.cost_currency AND x.amount_minor = NEW.cost_minor) END) THEN
    RAISE EXCEPTION 'cargo_rate: line % of quote %/% is not its snapshot as rated', NEW.line_no, NEW.tenant_id, NEW.quote_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'quote_line_rated';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER quote_line_rated AFTER INSERT ON cargo_rate.quote_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_rated_quote_line_at_commit();
