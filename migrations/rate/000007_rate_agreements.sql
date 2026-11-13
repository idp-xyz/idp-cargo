-- IDP-Cargo RATE V0.5 part 3b / PostgreSQL 16. Run only as the migrator role, never at service start.
-- AGREEMENT quotes (R6, C1 option 3, N1-RATE OD-4 A, OD-5 A): a quote that prices by lane with a DRAFT version of the
-- customer's CUSTOMER SELL card, valid over the quote's validity, instead of fixed lines; the margin check of each of
-- its lanes at a reference quantity against the valid buy rates (AGREEMENT_BELOW_FLOOR); and the version published
-- only by accepting the quote, in the acceptance's transaction.

ALTER TABLE cargo_rate.quote DROP CONSTRAINT quote_kind_check;
ALTER TABLE cargo_rate.quote ADD CONSTRAINT quote_kind_check CHECK (kind IN ('ONE_OFF','AGREEMENT'));
ALTER TABLE cargo_rate.quote ADD COLUMN agreement_rate_card_id text, ADD COLUMN agreement_version_no integer;
ALTER TABLE cargo_rate.quote ADD CONSTRAINT quote_agreement CHECK ((kind = 'AGREEMENT') = (agreement_rate_card_id IS NOT NULL)
  AND (agreement_rate_card_id IS NULL) = (agreement_version_no IS NULL) AND (agreement_version_no IS NULL OR agreement_version_no > 0)) NOT VALID;
ALTER TABLE cargo_rate.quote VALIDATE CONSTRAINT quote_agreement;
-- An AGREEMENT's scope is its mode (its lanes are its card's entries); a ONE_OFF's is as in rate/000006.
ALTER TABLE cargo_rate.quote DROP CONSTRAINT quote_scope;
ALTER TABLE cargo_rate.quote ADD CONSTRAINT quote_scope CHECK (CASE kind
  WHEN 'AGREEMENT' THEN (scope_mode, scope_container_mode) IN (('AIR','LOOSE'),('SEA','LCL')) AND scope_origin IS NULL AND scope_destination IS NULL
    AND scope_via IS NULL AND scope_carrier IS NULL AND scope_pieces IS NULL AND scope_gross_grams IS NULL AND scope_volume_cm3 IS NULL
    AND scope_documents IS NULL
  ELSE (scope_mode IS NULL) = (scope_container_mode IS NULL) AND (scope_mode IS NULL) = (scope_origin IS NULL)
  AND (scope_mode IS NULL) = (scope_destination IS NULL) AND (scope_mode IS NULL) = (scope_pieces IS NULL)
  AND (scope_mode IS NULL) = (scope_gross_grams IS NULL) AND (scope_mode IS NULL) = (scope_volume_cm3 IS NULL)
  AND (scope_mode IS NULL) = (scope_documents IS NULL) AND (scope_mode IS NOT NULL OR (scope_via IS NULL AND scope_carrier IS NULL))
  AND (scope_mode IS NULL OR ((scope_mode, scope_container_mode) IN (('AIR','LOOSE'),('SEA','LCL'))
    AND scope_origin ~ '^[A-Z]{2}[A-Z2-9]{3}$' AND scope_destination ~ '^[A-Z]{2}[A-Z2-9]{3}$' AND scope_origin <> scope_destination
    AND (scope_via IS NULL OR scope_via ~ '^[A-Z]{2}[A-Z2-9]{3}$')
    AND (scope_carrier IS NULL OR (length(scope_carrier) BETWEEN 1 AND 128 AND scope_carrier = btrim(scope_carrier, cargo_rate.white_space())))
    AND scope_pieces BETWEEN 0 AND 1000000000 AND scope_gross_grams BETWEEN 0 AND 1000000000000
    AND scope_volume_cm3 BETWEEN 0 AND 10000000000000 AND scope_documents BETWEEN 0 AND 1000000000)) END) NOT VALID;
ALTER TABLE cargo_rate.quote VALIDATE CONSTRAINT quote_scope;
-- A card version is the agreement of one quote that has not expired; once it expired, the draft may change and be
-- quoted again.
CREATE UNIQUE INDEX quote_agreement_version_once ON cargo_rate.quote (tenant_id, agreement_rate_card_id, agreement_version_no)
  WHERE kind = 'AGREEMENT' AND status <> 'EXPIRED';

-- The margin check of each lane of an AGREEMENT (OD-4): at checked_at, on the reference cargo, the sell of the entry
-- and the cost of the valid buy rates, in the quote currency; cost_minor is NULL when no supplier card rated the lane in
-- full. Written with the quote, immutable.
CREATE TABLE cargo_rate.quote_agreement_check (
 tenant_id text NOT NULL,
 quote_id text NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no BETWEEN 1 AND 500),
 checked_at timestamptz NOT NULL,
 sell_minor bigint NOT NULL CHECK (sell_minor >= 0),
 cost_minor bigint CHECK (cost_minor >= 0),
 PRIMARY KEY (tenant_id, quote_id, entry_no),
 FOREIGN KEY (tenant_id, quote_id) REFERENCES cargo_rate.quote (tenant_id, quote_id)
);
CREATE TRIGGER quote_agreement_check_immutable BEFORE UPDATE OR DELETE ON cargo_rate.quote_agreement_check
  FOR EACH ROW EXECUTE FUNCTION cargo_rate.reject_row_change();
ALTER TABLE cargo_rate.quote_agreement_check ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_rate.quote_agreement_check FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_rate.quote_agreement_check
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE CONSTRAINT TRIGGER _tenant_scope AFTER INSERT OR UPDATE ON cargo_rate.quote_agreement_check
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.tenant_scope_at_commit();
CREATE CONSTRAINT TRIGGER quote_agreement_check_written_with_quote AFTER INSERT ON cargo_rate.quote_agreement_check
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_written_with_quote_at_commit();

-- At commit, as a quote is written: a ONE_OFF quote has no lane checks; an AGREEMENT has no lines, names a DRAFT version
-- of a CUSTOMER SELL card of its legal entity and customer valid exactly over its validity, and has one check per
-- entry of that version.
CREATE FUNCTION cargo_rate.check_quote_kind_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.kind = 'ONE_OFF' THEN
    IF EXISTS (SELECT 1 FROM cargo_rate.quote_agreement_check k WHERE k.tenant_id = NEW.tenant_id AND k.quote_id = NEW.quote_id) THEN
      RAISE EXCEPTION 'cargo_rate: ONE_OFF quote %/% has no lane checks', NEW.tenant_id, NEW.quote_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'quote_agreement_consistent';
    END IF;
    RETURN NULL;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM cargo_rate.rate_card c JOIN cargo_rate.rate_card_version v ON v.tenant_id = c.tenant_id AND v.rate_card_id = c.rate_card_id
       WHERE c.tenant_id = NEW.tenant_id AND c.rate_card_id = NEW.agreement_rate_card_id AND v.version_no = NEW.agreement_version_no
         AND c.scope = 'CUSTOMER' AND c.side = 'SELL' AND c.legal_entity_id = NEW.legal_entity_id AND c.party_id = NEW.customer_party_id
         AND v.status = 'DRAFT' AND v.valid_from = NEW.valid_from AND v.valid_until = NEW.valid_until)
     OR EXISTS (SELECT 1 FROM cargo_rate.quote_line l WHERE l.tenant_id = NEW.tenant_id AND l.quote_id = NEW.quote_id)
     OR NOT EXISTS (SELECT 1 FROM cargo_rate.rate_entry e WHERE e.tenant_id = NEW.tenant_id AND e.rate_card_id = NEW.agreement_rate_card_id
       AND e.version_no = NEW.agreement_version_no)
     OR EXISTS (SELECT 1 FROM cargo_rate.rate_entry e WHERE e.tenant_id = NEW.tenant_id AND e.rate_card_id = NEW.agreement_rate_card_id
       AND e.version_no = NEW.agreement_version_no AND NOT EXISTS (SELECT 1 FROM cargo_rate.quote_agreement_check k
         WHERE k.tenant_id = NEW.tenant_id AND k.quote_id = NEW.quote_id AND k.entry_no = e.entry_no))
     OR EXISTS (SELECT 1 FROM cargo_rate.quote_agreement_check k WHERE k.tenant_id = NEW.tenant_id AND k.quote_id = NEW.quote_id
       AND NOT EXISTS (SELECT 1 FROM cargo_rate.rate_entry e WHERE e.tenant_id = NEW.tenant_id AND e.rate_card_id = NEW.agreement_rate_card_id
         AND e.version_no = NEW.agreement_version_no AND e.entry_no = k.entry_no)) THEN
    RAISE EXCEPTION 'cargo_rate: AGREEMENT quote %/% names a DRAFT customer version over its validity, has no lines and checks each lane once',
      NEW.tenant_id, NEW.quote_id USING ERRCODE = 'check_violation', CONSTRAINT = 'quote_agreement_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER quote_agreement_consistent AFTER INSERT ON cargo_rate.quote
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_quote_kind_at_commit();

-- The rates of a draft an AGREEMENT quote names do not change until the quote expired.
CREATE OR REPLACE FUNCTION cargo_rate.rate_content_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE r record; st text; quoted text;
BEGIN
  IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION 'cargo_rate: % rows are replaced, not changed', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT v.status INTO st FROM cargo_rate.rate_card_version v
    WHERE v.tenant_id = r.tenant_id AND v.rate_card_id = r.rate_card_id AND v.version_no = r.version_no FOR SHARE;
  IF st IS DISTINCT FROM 'DRAFT' THEN
    RAISE EXCEPTION 'cargo_rate: version %/%/% is %; its rates change only by a new version', r.tenant_id, r.rate_card_id, r.version_no, st
      USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT q.quote_id INTO quoted FROM cargo_rate.quote q WHERE q.tenant_id = r.tenant_id AND q.kind = 'AGREEMENT'
    AND q.agreement_rate_card_id = r.rate_card_id AND q.agreement_version_no = r.version_no AND q.status <> 'EXPIRED';
  IF quoted IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_rate: version %/%/% is quoted by agreement %; its rates change once the quote expired', r.tenant_id, r.rate_card_id,
      r.version_no, quoted USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN r;
END $fn$;

-- As rate/000004, but a CUSTOMER version is published only together with the acceptance of the AGREEMENT quote that
-- names it, in the same transaction, valid over the quote's validity.
CREATE OR REPLACE FUNCTION cargo_rate.check_rate_card_version_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE c record;
BEGIN
  IF current_setting('transaction_isolation') <> 'read committed' THEN
    RAISE EXCEPTION 'cargo_rate: RATE''s commit checks run at READ COMMITTED, not %', current_setting('transaction_isolation')
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rate_read_committed';
  END IF;
  SELECT k.scope INTO c FROM cargo_rate.rate_card k WHERE k.tenant_id = NEW.tenant_id AND k.rate_card_id = NEW.rate_card_id FOR NO KEY UPDATE;
  IF NOT FOUND OR EXISTS (SELECT 1 FROM cargo_rate.rate_card_version v WHERE v.tenant_id = NEW.tenant_id AND v.rate_card_id = NEW.rate_card_id
       AND v.status <> 'DRAFT' AND EXISTS (SELECT 1 FROM cargo_rate.rate_card_version o WHERE o.tenant_id = v.tenant_id
         AND o.rate_card_id = v.rate_card_id AND o.version_no <> v.version_no AND o.status <> 'DRAFT'
         AND cargo_rate.rates_in(o) && cargo_rate.rates_in(v))) THEN
    RAISE EXCEPTION 'cargo_rate: published versions of rate card %/% overlap', NEW.tenant_id, NEW.rate_card_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rate_version_overlap';
  END IF;
  IF NEW.status = 'PUBLISHED' AND ((c.scope = 'TARIFF') <> (NEW.approved_by IS NOT NULL)
       OR (c.scope = 'CUSTOMER' AND (TG_OP = 'INSERT' OR OLD.status = 'DRAFT') AND NOT EXISTS (SELECT 1 FROM cargo_rate.quote q
         WHERE q.tenant_id = NEW.tenant_id AND q.kind = 'AGREEMENT' AND q.agreement_rate_card_id = NEW.rate_card_id
           AND q.agreement_version_no = NEW.version_no AND q.status = 'ACCEPTED' AND q.xmin = pg_current_xact_id()::xid
           AND q.valid_from = NEW.valid_from AND q.valid_until IS NOT DISTINCT FROM NEW.valid_until))
       OR NOT EXISTS (SELECT 1 FROM cargo_rate.rate_entry e WHERE e.tenant_id = NEW.tenant_id AND e.rate_card_id = NEW.rate_card_id
         AND e.version_no = NEW.version_no)
       OR EXISTS (SELECT 1 FROM cargo_rate.rate_entry e WHERE e.tenant_id = NEW.tenant_id AND e.rate_card_id = NEW.rate_card_id
         AND e.version_no = NEW.version_no AND NOT EXISTS (SELECT 1 FROM cargo_rate.rate_line l WHERE l.tenant_id = e.tenant_id
           AND l.rate_card_id = e.rate_card_id AND l.version_no = e.version_no AND l.entry_no = e.entry_no))) THEN
    RAISE EXCEPTION 'cargo_rate: version %/%/% is published without entries, lines, the approval its card needs or its accepted agreement',
      NEW.tenant_id, NEW.rate_card_id, NEW.version_no USING ERRCODE = 'check_violation', CONSTRAINT = 'rate_version_publishable';
  END IF;
  RETURN NULL;
END $fn$;
