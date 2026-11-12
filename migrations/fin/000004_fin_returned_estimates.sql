-- IDP-Cargo FIN V0.2.2 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A credit note returns a reversed sell side to the estimate it had before it was billed, with that estimate's
-- conversion, instead of converting again at the ESTIMATE rate in force: such a version keeps when its rate was applied
-- (converted_at). The job's checks recompute its local amount at the entry in force at that time, an exchange rate ended
-- earlier counts that time as its use, and the version must copy the estimate it returns to (or be zero for a
-- write-off).

ALTER TABLE cargo_fin.charge_side_version ADD COLUMN converted_at timestamptz,
  ADD CONSTRAINT charge_side_converted CHECK (converted_at IS NULL OR (rate IS NOT NULL AND status = 'ESTIMATED' AND converted_at <= recorded_at));

-- At commit, a job's side versions: as fin/000001, converted at their entry in force when their rate was applied; only
-- a version after an INVOICED one (a credit note's return, checked by check_job_invoices_at_commit) was converted earlier
-- than it was recorded.
CREATE OR REPLACE FUNCTION cargo_fin.check_job_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  j cargo_fin.freight_job;
  ok boolean;
BEGIN
  SELECT * INTO j FROM cargo_fin.freight_job WHERE tenant_id = NEW.tenant_id AND job_id = NEW.job_id;
  SELECT EXISTS (SELECT 1 FROM cargo_fin.finance_policy p WHERE p.tenant_id = j.tenant_id AND p.legal_entity_id = j.legal_entity_id
       AND p.functional_currency = j.functional_currency AND p.book_code = j.book_code)
     AND (SELECT count(*) = coalesce(max(line_no), 0) FROM cargo_fin.charge_line l WHERE l.tenant_id = j.tenant_id AND l.job_id = j.job_id)
     AND (SELECT count(*) = coalesce(max(basis_no), 0) FROM cargo_fin.job_basis b WHERE b.tenant_id = j.tenant_id AND b.job_id = j.job_id)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.job_basis b WHERE b.tenant_id = j.tenant_id AND b.job_id = j.job_id AND b.live = (j.state = 'CANCELLED'))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_line l WHERE l.tenant_id = j.tenant_id AND l.job_id = j.job_id
       AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v WHERE v.tenant_id = l.tenant_id AND v.job_id = l.job_id AND v.line_no = l.line_no))
     AND NOT EXISTS (SELECT 1 FROM (SELECT line_no, side, count(*) AS n, max(version_no) AS m FROM cargo_fin.charge_side_version v
       WHERE v.tenant_id = j.tenant_id AND v.job_id = j.job_id GROUP BY line_no, side) s WHERE s.n <> s.m)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v WHERE v.tenant_id = j.tenant_id AND v.job_id = j.job_id AND v.converted_at IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_side_version p WHERE p.tenant_id = v.tenant_id AND p.job_id = v.job_id AND p.line_no = v.line_no
         AND p.side = v.side AND p.version_no = v.version_no - 1 AND p.status = 'INVOICED'))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v WHERE v.tenant_id = j.tenant_id AND v.job_id = j.job_id
       AND CASE WHEN v.currency = j.functional_currency OR v.rate IS NULL
           THEN v.rate IS NOT NULL OR v.local_minor <> v.amount_minor OR (v.currency <> j.functional_currency AND v.amount_minor <> 0)
         ELSE NOT EXISTS (SELECT 1 FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = j.tenant_id AND e.legal_entity_id = j.legal_entity_id
           AND e.entry_no = v.rate_entry_no AND e.rate_type = v.rate_type AND e.from_currency = v.currency AND e.rate = v.rate AND e.scale = v.scale
           AND e.from_digits = v.from_digits AND e.to_digits = v.to_digits AND e.valid_from <= coalesce(v.converted_at, v.recorded_at)
           AND coalesce(v.converted_at, v.recorded_at) < e.valid_until
           AND v.local_minor = round(v.amount_minor::numeric * e.rate * power(10::numeric, e.to_digits - e.from_digits - e.scale))) END)
    INTO ok;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_fin: freight job %/% is inconsistent at commit (state %)', j.tenant_id, j.job_id, j.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'freight_job_consistent';
  END IF;
  RETURN NULL;
END $fn$;

-- At commit, an entry ended earlier was used by no charge (when its rate was applied) or debit note at or after its new end.
CREATE OR REPLACE FUNCTION cargo_fin.check_rate_end_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v JOIN cargo_fin.freight_job j ON j.tenant_id = v.tenant_id AND j.job_id = v.job_id
       WHERE v.tenant_id = NEW.tenant_id AND j.legal_entity_id = NEW.legal_entity_id AND v.rate_entry_no = NEW.entry_no
         AND coalesce(v.converted_at, v.recorded_at) >= NEW.valid_until)
     OR EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line l JOIN cargo_fin.debtor_invoice n ON n.tenant_id = l.tenant_id AND n.invoice_id = l.invoice_id
       WHERE n.tenant_id = NEW.tenant_id AND n.legal_entity_id = NEW.legal_entity_id AND l.rate_entry_no = NEW.entry_no AND l.side_version IS NOT NULL
         AND n.issued_at >= NEW.valid_until) THEN
    RAISE EXCEPTION 'cargo_fin: exchange rate entry %/%/% converted a charge at or after its new end', NEW.tenant_id, NEW.legal_entity_id, NEW.entry_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'exchange_rate_entry_in_use';
  END IF;
  RETURN NULL;
END $fn$;

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
-- ESTIMATED version that credit note wrote for the same debtor: the estimate billed (the version before the INVOICED
-- one) with its amount, local amount and conversion, converted when that estimate was, or zero when the credit note
-- writes the charges off.
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
           AND CASE WHEN cn.write_off THEN w.amount_minor = 0 AND w.local_minor = 0 AND w.rate IS NULL AND w.converted_at IS NULL
             ELSE EXISTS (SELECT 1 FROM cargo_fin.charge_side_version p WHERE p.tenant_id = v.tenant_id AND p.job_id = v.job_id AND p.line_no = v.line_no
               AND p.side = 'SELL' AND p.version_no = v.version_no - 1 AND p.status = 'ESTIMATED' AND w.amount_minor = p.amount_minor
               AND w.local_minor = p.local_minor AND w.rate_entry_no IS NOT DISTINCT FROM p.rate_entry_no AND w.rate_type IS NOT DISTINCT FROM p.rate_type
               AND w.rate IS NOT DISTINCT FROM p.rate AND w.scale IS NOT DISTINCT FROM p.scale AND w.from_digits IS NOT DISTINCT FROM p.from_digits
               AND w.to_digits IS NOT DISTINCT FROM p.to_digits AND w.rate_source IS NOT DISTINCT FROM p.rate_source
               AND (p.rate IS NULL OR w.converted_at = coalesce(p.converted_at, p.recorded_at))) END))
    INTO ok;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_fin: the notes of freight job %/% are inconsistent at commit', j.tenant_id, j.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'freight_job_invoices_consistent';
  END IF;
  RETURN NULL;
END $fn$;
