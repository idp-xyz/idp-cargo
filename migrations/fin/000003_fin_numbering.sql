-- IDP-Cargo FIN V0.2.1 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Note numbers from idp-platform-go's numbering (MD-28): a legal entity's series of a note kind is now a numbering
-- template, its literal prefix and its first number, fixed once; the platform's sequence table
-- cargo_fin.numbering_sequence (created by cargo-migrate from the platform's asset just before FIN's migrations) draws
-- the numbers in the issuing transaction, per period of the template (none, year, quarter or month). Cargo owns that
-- table's data and permissions: rows are scoped by tenant (row-level security); a sequence starts at 1 or at its
-- series' seed, only for a registered series, moves one number at a time, is never deleted, and at commit holds exactly
-- the numbers of its notes. A note keeps its period, and its number is its template rendered at its sequence value and
-- invoice date (the database renders Cargo's template subset itself). An existing series becomes the continuous template
-- <prefix>{seq:<width>} with its sequence's last value the last number drawn, so issued numbers stay and new ones
-- continue.

INSERT INTO cargo_fin.numbering_sequence (scope, doc_type, period, next_val)
  SELECT tenant_id, kind || '/' || legal_entity_id, '', next_seq - 1 FROM cargo_fin.invoice_series WHERE next_seq > 1;

ALTER TABLE cargo_fin.numbering_sequence ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.numbering_sequence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.numbering_sequence USING (scope = current_setting('app.tenant_id', true))
  WITH CHECK (scope = current_setting('app.tenant_id', true));
CREATE FUNCTION cargo_fin.numbering_forward_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_fin.invoice_series;
BEGIN
  IF TG_OP = 'INSERT' THEN
    SELECT * INTO s FROM cargo_fin.invoice_series x WHERE x.tenant_id = NEW.scope AND x.kind || '/' || x.legal_entity_id = NEW.doc_type;
    IF s.tenant_id IS NULL OR NOT (NEW.next_val = 1 OR (NEW.period = s.seed_period AND NEW.next_val = s.first_number - 1)) THEN
      RAISE EXCEPTION 'cargo_fin: a numbering sequence starts at 1 or at its series'' seed' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' OR NEW.scope <> OLD.scope OR NEW.doc_type <> OLD.doc_type OR NEW.period <> OLD.period OR NEW.next_val <> OLD.next_val + 1 THEN
    RAISE EXCEPTION 'cargo_fin: a numbering sequence moves one number at a time' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- Cargo's template subset rendered as the platform renders it: {yyyy} {yy} {mm} {q} from the date, {seq:N} zero-padded;
-- the period is that of the finest date token (month, quarter, year, or none).
CREATE FUNCTION cargo_fin.render_number(template text, seq bigint, d date) RETURNS text LANGUAGE sql IMMUTABLE AS $fn$
  SELECT regexp_replace(replace(replace(replace(replace(template, '{yyyy}', to_char(d, 'YYYY')), '{yy}', to_char(d, 'YY')), '{mm}', to_char(d, 'MM')),
      '{q}', extract(quarter FROM d)::int::text), '\{seq:[0-9]+\}', lpad(seq::text, substring(template FROM '\{seq:([0-9]+)\}')::int, '0'))
$fn$;
CREATE FUNCTION cargo_fin.number_period(template text, d date) RETURNS text LANGUAGE sql IMMUTABLE AS $fn$
  SELECT CASE WHEN strpos(template, '{mm}') > 0 THEN to_char(d, 'YYYYMM')
    WHEN strpos(template, '{q}') > 0 THEN to_char(d, 'YYYY') || 'Q' || extract(quarter FROM d)::int
    WHEN strpos(template, '{yyyy}') > 0 OR strpos(template, '{yy}') > 0 THEN to_char(d, 'YYYY') ELSE '' END
$fn$;

DROP TRIGGER invoice_series_rules ON cargo_fin.invoice_series;
DROP TRIGGER invoice_series_consistent ON cargo_fin.invoice_series;
DROP FUNCTION cargo_fin.check_series_at_commit();
ALTER TABLE cargo_fin.invoice_series ADD COLUMN template text, ADD COLUMN first_number bigint, ADD COLUMN seed_period text NOT NULL DEFAULT '';
UPDATE cargo_fin.invoice_series SET template = prefix || '{seq:' || width || '}', first_number = start_seq;
ALTER TABLE cargo_fin.invoice_series DROP CONSTRAINT invoice_series_range, DROP COLUMN width, DROP COLUMN start_seq, DROP COLUMN next_seq;
ALTER TABLE cargo_fin.invoice_series ALTER COLUMN template SET NOT NULL, ALTER COLUMN first_number SET NOT NULL,
  ADD CONSTRAINT invoice_series_template CHECK (starts_with(template, prefix) AND substr(template, length(prefix) + 1, 1) = '{' AND length(template) <= 60
    AND template ~ '^[0-9A-Z][0-9A-Z-]*(\{(yyyy|yy|mm|q)\}|[0-9A-Z-])*\{seq:([4-9]|10)\}(\{(yyyy|yy|mm|q)\}|[0-9A-Z-])*$'
    AND (template !~ '\{(mm|q)\}' OR template ~ '\{(yyyy|yy)\}') AND first_number >= 1 AND length(seed_period) <= 8);
CREATE TRIGGER invoice_series_immutable BEFORE UPDATE OR DELETE ON cargo_fin.invoice_series FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
-- Rows inserted from here on start at 1 or at their series' seed, and move one number at a time.
CREATE TRIGGER numbering_sequence_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.numbering_sequence
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.numbering_forward_only();

ALTER TABLE cargo_fin.debtor_invoice ADD COLUMN number_period text NOT NULL DEFAULT '' CHECK (length(number_period) <= 8);
CREATE UNIQUE INDEX debtor_invoice_number_seq ON cargo_fin.debtor_invoice (tenant_id, legal_entity_id, kind, number_period, number_seq);

-- At commit, a note kind's sequence of a period has drawn exactly the numbers of its notes: from the seed of the
-- registration period (the first number) or from 1, up to its last value.
CREATE FUNCTION cargo_fin.check_numbering_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_fin.invoice_series;
  v_kind text := split_part(NEW.doc_type, '/', 1);
  v_le text := substr(NEW.doc_type, length(split_part(NEW.doc_type, '/', 1)) + 2);
  v_seed bigint := 0;
  v_last bigint;
BEGIN
  SELECT * INTO s FROM cargo_fin.invoice_series x WHERE x.tenant_id = NEW.scope AND x.legal_entity_id = v_le AND x.kind = v_kind;
  IF FOUND AND NEW.period = s.seed_period THEN
    v_seed := s.first_number - 1;
  END IF;
  SELECT q.next_val INTO v_last FROM cargo_fin.numbering_sequence q WHERE q.scope = NEW.scope AND q.doc_type = NEW.doc_type AND q.period = NEW.period;
  IF s.tenant_id IS NULL OR v_last - v_seed <> (SELECT count(*) FROM cargo_fin.debtor_invoice n WHERE n.tenant_id = NEW.scope
       AND n.legal_entity_id = v_le AND n.kind = v_kind AND n.number_period = NEW.period) THEN
    RAISE EXCEPTION 'cargo_fin: sequence %/%/% drew numbers no note holds', NEW.scope, NEW.doc_type, NEW.period
      USING ERRCODE = 'check_violation', CONSTRAINT = 'invoice_series_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER invoice_series_consistent AFTER INSERT OR UPDATE ON cargo_fin.numbering_sequence
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_numbering_at_commit();

-- At commit, a job's notes are consistent:
-- (a) a note is of the job's legal entity, its lines are numbered from 1 without gaps and its totals are their sums;
-- (b) its number is its series' template rendered at its sequence value and invoice date, its period the template's
-- period of that date, and its sequence value one the platform's sequence of the period drew (after the first number's
-- seed in the seed period); (c) its invoice date is the issue's date in some time zone (UTC-12
-- to UTC+14);
-- (d) a debit note's due date follows its payment terms, those of the debtor's credit account version it names or none
-- (due on the invoice date), and it is CANCELLED_BY_CREDIT_NOTE exactly when every line is reversed; a cancelled job has
-- no ISSUED debit note;
-- (e) a debit note line bills the sell side version it names, which was written with it as INVOICED for the note's
-- debtor at the line's amount and conversion and follows an ESTIMATED version of that debtor and amount; it converts as
-- the note's currency asks (none in the functional currency; the INVOICE rate into the functional currency; the
-- ACCOUNTING rate from a foreign note currency) at an entry of the policy in force at the issue, and bills under the
-- charge code's tax treatment in force at the invoice date among the versions entered by then;
-- (f) a credit note reverses lines of a debit note of the job to the same debtor, profile and currency, dated on or
-- after it, each line negating its debit note line under the same conversion and tax;
-- (g) every INVOICED side version is billed by the debit note line it names;
-- (h) the version after an INVOICED one exists exactly when a credit note reverses its debit note line, and is the
-- ESTIMATED version of the same debtor and amount (zero when the credit note writes the charges off) that credit note
-- wrote.
CREATE OR REPLACE FUNCTION cargo_fin.check_job_invoices_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  j cargo_fin.freight_job;
  ok boolean;
BEGIN
  SELECT * INTO j FROM cargo_fin.freight_job WHERE tenant_id = NEW.tenant_id AND job_id = NEW.job_id;
  SELECT NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n WHERE n.tenant_id = j.tenant_id AND n.job_id = j.job_id
       AND (n.legal_entity_id <> j.legal_entity_id
         OR (SELECT count(*) = 0 OR count(*) <> max(line_no) OR sum(invoice_minor) <> n.total_minor OR sum(local_minor) <> n.local_total_minor
             FROM cargo_fin.debtor_invoice_line l WHERE l.tenant_id = n.tenant_id AND l.invoice_id = n.invoice_id)
         OR NOT EXISTS (SELECT 1 FROM cargo_fin.invoice_series s JOIN cargo_fin.numbering_sequence q ON q.scope = s.tenant_id
             AND q.doc_type = s.kind || '/' || s.legal_entity_id AND q.period = n.number_period
           WHERE s.tenant_id = n.tenant_id AND s.legal_entity_id = n.legal_entity_id AND s.kind = n.kind
             AND n.number = cargo_fin.render_number(s.template, n.number_seq, n.invoice_date)
             AND n.number_period = cargo_fin.number_period(s.template, n.invoice_date)
             AND n.number_seq > CASE WHEN n.number_period = s.seed_period THEN s.first_number - 1 ELSE 0 END AND n.number_seq <= q.next_val)
         OR n.issued_at < (n.invoice_date::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
         OR n.issued_at >= (n.invoice_date::timestamp AT TIME ZONE 'UTC') + interval '36 hours'))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n WHERE n.tenant_id = j.tenant_id AND n.job_id = j.job_id AND n.kind = 'DEBIT_NOTE'
       AND (n.due_date <> CASE n.payment_basis WHEN 'INVOICE_DATE' THEN n.invoice_date
             ELSE (date_trunc('month', n.invoice_date::timestamp) + interval '1 month - 1 day')::date END + n.payment_days
         OR CASE WHEN n.terms_version IS NULL THEN n.payment_days <> 0 OR n.payment_basis <> 'INVOICE_DATE'
             ELSE NOT EXISTS (SELECT 1 FROM cargo_fin.credit_terms_version t WHERE t.tenant_id = n.tenant_id AND t.legal_entity_id = n.legal_entity_id
               AND t.debtor_party_id = n.debtor_party_id AND t.version_no = n.terms_version AND t.payment_days = n.payment_days
               AND t.payment_basis = n.payment_basis) END
         OR (j.state = 'CANCELLED' AND n.state = 'ISSUED')
         OR (n.state = 'CANCELLED_BY_CREDIT_NOTE') <> NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line l WHERE l.tenant_id = n.tenant_id
             AND l.invoice_id = n.invoice_id AND NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line c WHERE c.tenant_id = l.tenant_id
               AND c.credits_invoice_id = l.invoice_id AND c.credits_line_no = l.line_no))))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n JOIN cargo_fin.debtor_invoice_line l ON l.tenant_id = n.tenant_id AND l.invoice_id = n.invoice_id
       WHERE n.tenant_id = j.tenant_id AND n.job_id = j.job_id AND n.kind = 'DEBIT_NOTE' AND (l.side_version IS NULL OR l.job_id <> n.job_id
         OR NOT EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v JOIN cargo_fin.charge_line c ON c.tenant_id = v.tenant_id AND c.job_id = v.job_id
             AND c.line_no = v.line_no
           WHERE v.tenant_id = l.tenant_id AND v.job_id = l.job_id AND v.line_no = l.job_line_no AND v.side = 'SELL' AND v.version_no = l.side_version
             AND v.status = 'INVOICED' AND v.invoice_id = l.invoice_id AND v.invoice_line_no = l.line_no AND v.party_id = n.debtor_party_id
             AND v.party_revision = n.debtor_revision AND v.currency = l.currency AND v.amount_minor = l.amount_minor AND v.local_minor = l.local_minor
             AND v.rate_entry_no IS NOT DISTINCT FROM l.rate_entry_no AND v.rate_type IS NOT DISTINCT FROM l.rate_type AND v.rate IS NOT DISTINCT FROM l.rate
             AND v.scale IS NOT DISTINCT FROM l.scale AND v.from_digits IS NOT DISTINCT FROM l.from_digits AND v.to_digits IS NOT DISTINCT FROM l.to_digits
             AND v.rate_source IS NOT DISTINCT FROM l.rate_source AND v.recorded_at = n.issued_at AND c.charge_code = l.charge_code)
         OR (SELECT v.status <> 'ESTIMATED' OR v.party_id <> n.debtor_party_id OR v.currency <> l.currency OR v.amount_minor <> l.amount_minor
             FROM cargo_fin.charge_side_version v WHERE v.tenant_id = l.tenant_id AND v.job_id = l.job_id AND v.line_no = l.job_line_no AND v.side = 'SELL'
               AND v.version_no = l.side_version - 1) IS NOT FALSE
         OR CASE
           WHEN l.rate IS NULL THEN l.currency <> j.functional_currency OR n.currency <> j.functional_currency OR l.invoice_minor <> l.amount_minor
             OR l.local_minor <> l.amount_minor
           WHEN l.rate_type = 'INVOICE' THEN n.currency <> j.functional_currency OR l.currency = j.functional_currency OR l.local_minor <> l.invoice_minor
             OR NOT EXISTS (SELECT 1 FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = j.tenant_id AND e.legal_entity_id = j.legal_entity_id
               AND e.entry_no = l.rate_entry_no AND e.rate_type = 'INVOICE' AND e.from_currency = l.currency AND e.rate = l.rate AND e.scale = l.scale
               AND e.from_digits = l.from_digits AND e.to_digits = l.to_digits AND e.source = l.rate_source AND e.valid_from <= n.issued_at
               AND n.issued_at < e.valid_until
               AND l.invoice_minor = round(l.amount_minor::numeric * e.rate * power(10::numeric, e.to_digits - e.from_digits - e.scale)))
           ELSE n.currency = j.functional_currency OR l.currency <> n.currency OR l.invoice_minor <> l.amount_minor
             OR NOT EXISTS (SELECT 1 FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = j.tenant_id AND e.legal_entity_id = j.legal_entity_id
               AND e.entry_no = l.rate_entry_no AND e.rate_type = 'ACCOUNTING' AND e.from_currency = n.currency AND e.rate = l.rate AND e.scale = l.scale
               AND e.from_digits = l.from_digits AND e.to_digits = l.to_digits AND e.source = l.rate_source AND e.valid_from <= n.issued_at
               AND n.issued_at < e.valid_until
               AND l.local_minor = round(l.amount_minor::numeric * e.rate * power(10::numeric, e.to_digits - e.from_digits - e.scale))) END
         OR NOT EXISTS (SELECT 1 FROM cargo_fin.charge_tax_treatment t WHERE t.tenant_id = j.tenant_id AND t.legal_entity_id = j.legal_entity_id
             AND t.version_no = l.tax_version AND t.charge_code = l.charge_code AND t.treatment = l.treatment AND t.rate_bp IS NOT DISTINCT FROM l.rate_bp
             AND t.effective_from <= n.invoice_date
             AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_tax_treatment x WHERE x.tenant_id = t.tenant_id AND x.legal_entity_id = t.legal_entity_id
               AND x.charge_code = t.charge_code AND x.effective_from > t.effective_from AND x.effective_from <= n.invoice_date
               AND x.entered_at <= n.entered_at))))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n JOIN cargo_fin.debtor_invoice d ON d.tenant_id = n.tenant_id AND d.invoice_id = n.credits_invoice_id
       WHERE n.tenant_id = j.tenant_id AND n.job_id = j.job_id AND n.kind = 'CREDIT_NOTE'
         AND (d.kind <> 'DEBIT_NOTE' OR d.job_id <> n.job_id OR d.debtor_party_id <> n.debtor_party_id OR d.debtor_revision <> n.debtor_revision
           OR d.profile_version <> n.profile_version OR d.currency <> n.currency OR n.invoice_date < d.invoice_date OR n.issued_at < d.issued_at))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n JOIN cargo_fin.debtor_invoice_line l ON l.tenant_id = n.tenant_id AND l.invoice_id = n.invoice_id
       WHERE n.tenant_id = j.tenant_id AND n.job_id = j.job_id AND n.kind = 'CREDIT_NOTE' AND (l.credits_invoice_id IS DISTINCT FROM n.credits_invoice_id
         OR NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line d WHERE d.tenant_id = l.tenant_id AND d.invoice_id = l.credits_invoice_id
           AND d.line_no = l.credits_line_no AND d.job_id = l.job_id AND d.job_line_no = l.job_line_no AND d.charge_code = l.charge_code
           AND d.currency = l.currency AND l.amount_minor = -d.amount_minor AND l.invoice_minor = -d.invoice_minor AND l.local_minor = -d.local_minor
           AND l.rate_entry_no IS NOT DISTINCT FROM d.rate_entry_no AND l.rate_type IS NOT DISTINCT FROM d.rate_type AND l.rate IS NOT DISTINCT FROM d.rate
           AND l.scale IS NOT DISTINCT FROM d.scale AND l.from_digits IS NOT DISTINCT FROM d.from_digits AND l.to_digits IS NOT DISTINCT FROM d.to_digits
           AND l.rate_source IS NOT DISTINCT FROM d.rate_source AND l.tax_version = d.tax_version AND l.treatment = d.treatment
           AND l.rate_bp IS NOT DISTINCT FROM d.rate_bp)))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v WHERE v.tenant_id = j.tenant_id AND v.job_id = j.job_id AND v.status = 'INVOICED'
       AND NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line l WHERE l.tenant_id = v.tenant_id AND l.invoice_id = v.invoice_id
         AND l.line_no = v.invoice_line_no AND l.job_id = v.job_id AND l.job_line_no = v.line_no AND l.side_version = v.version_no))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v WHERE v.tenant_id = j.tenant_id AND v.job_id = j.job_id AND v.status = 'INVOICED'
       AND (EXISTS (SELECT 1 FROM cargo_fin.charge_side_version w WHERE w.tenant_id = v.tenant_id AND w.job_id = v.job_id AND w.line_no = v.line_no
             AND w.side = 'SELL' AND w.version_no = v.version_no + 1)
         OR EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line c WHERE c.tenant_id = v.tenant_id AND c.credits_invoice_id = v.invoice_id
             AND c.credits_line_no = v.invoice_line_no))
       AND NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line c JOIN cargo_fin.debtor_invoice cn ON cn.tenant_id = c.tenant_id AND cn.invoice_id = c.invoice_id
         JOIN cargo_fin.charge_side_version w ON w.tenant_id = v.tenant_id AND w.job_id = v.job_id AND w.line_no = v.line_no AND w.side = 'SELL'
           AND w.version_no = v.version_no + 1
         WHERE c.tenant_id = v.tenant_id AND c.credits_invoice_id = v.invoice_id AND c.credits_line_no = v.invoice_line_no AND w.status = 'ESTIMATED'
           AND w.recorded_at = cn.issued_at AND w.party_id = v.party_id AND w.currency = v.currency
           AND w.amount_minor = CASE WHEN cn.write_off THEN 0 ELSE v.amount_minor END))
    INTO ok;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_fin: the notes of freight job %/% are inconsistent at commit', j.tenant_id, j.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'freight_job_invoices_consistent';
  END IF;
  RETURN NULL;
END $fn$;
