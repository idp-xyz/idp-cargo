-- IDP-Cargo FIN V0.2 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Billing, the second part of N1 step 8 (FIN candidate 26: FB5 and FB1's tax treatments; decisions N1-FIN OD-2 B,
-- OD-5 B): each legal entity's tax treatments of charge codes (immutable versions in force from a date) and number
-- series of debit and credit notes (MD-28; drawn in order without gaps); debtors' billing profiles (MD-21: invoice
-- title and taxpayer number frozen from ORG, the enterprise's invoice data); debit notes billing a job's sell sides to
-- one debtor on an issued house document, and credit notes reversing their lines. A billed side gets a new version,
-- INVOICED, naming its note line; a reversed one returns to ESTIMATED. Notes are written with their job; an issued note
-- changes only from ISSUED to CANCELLED_BY_CREDIT_NOTE, once every line is reversed.

CREATE TABLE cargo_fin.charge_tax_treatment (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 charge_code text NOT NULL CHECK (length(btrim(charge_code)) BETWEEN 1 AND 64),
 treatment text NOT NULL CHECK (treatment IN ('EXEMPT_INTL_FREIGHT_FORWARDING','TAXABLE','PASS_THROUGH_DISBURSEMENT')),
 rate_bp integer CHECK (rate_bp BETWEEN 1 AND 3000),
 effective_from date NOT NULL,
 set_by text NOT NULL CHECK (length(btrim(set_by)) > 0),
 set_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, version_no),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 CONSTRAINT charge_tax_treatment_once UNIQUE (tenant_id, legal_entity_id, charge_code, effective_from),
 CONSTRAINT charge_tax_treatment_rate CHECK ((treatment = 'TAXABLE') = (rate_bp IS NOT NULL))
);

CREATE TABLE cargo_fin.invoice_series (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('DEBIT_NOTE','CREDIT_NOTE')),
 prefix text NOT NULL CHECK (prefix ~ '^[0-9A-Z][0-9A-Z-]{0,9}$'),
 width integer NOT NULL CHECK (width BETWEEN 4 AND 10),
 start_seq bigint NOT NULL CHECK (start_seq >= 1),
 next_seq bigint NOT NULL,
 registered_by text NOT NULL CHECK (length(btrim(registered_by)) > 0),
 registered_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, kind),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 CONSTRAINT invoice_series_range CHECK (length(prefix) + width <= 20 AND next_seq >= start_seq AND length(start_seq::text) <= width
   AND length((next_seq - 1)::text) <= width)
);

CREATE TABLE cargo_fin.billing_profile_version (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 debtor_party_id text NOT NULL CHECK (length(btrim(debtor_party_id)) BETWEEN 1 AND 128),
 version_no integer NOT NULL CHECK (version_no > 0),
 buyer_kind text NOT NULL CHECK (buyer_kind IN ('CN_ENTERPRISE','FOREIGN')),
 title text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 300),
 taxpayer_number text,
 party_revision bigint NOT NULL CHECK (party_revision > 0),
 registered_address text CHECK (length(btrim(registered_address)) BETWEEN 1 AND 300),
 registered_phone text CHECK (registered_phone ~ '^[0-9+() -]{3,30}$'),
 bank_name text CHECK (length(btrim(bank_name)) BETWEEN 1 AND 200),
 bank_account text CHECK (bank_account ~ '^[0-9A-Z ]{4,40}$'),
 evidence_ref text NOT NULL CHECK (evidence_ref ~ '^[A-Za-z0-9][A-Za-z0-9 ._:/#-]{0,199}$'),
 set_by text NOT NULL CHECK (length(btrim(set_by)) > 0),
 set_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, debtor_party_id, version_no),
 -- A Chinese enterprise's taxpayer number is its unified social credit code (GB 32100 characters; ORG checked it).
 CONSTRAINT billing_profile_taxpayer CHECK ((buyer_kind = 'CN_ENTERPRISE' AND taxpayer_number IS NOT NULL AND taxpayer_number ~ '^[0-9A-HJ-NP-RTUW-Y]{18}$')
   OR (buyer_kind = 'FOREIGN' AND taxpayer_number IS NULL)),
 CONSTRAINT billing_profile_bank CHECK ((bank_name IS NULL) = (bank_account IS NULL)),
 -- A sole trader's (individual industrial and commercial household, code 92...) phone and account are a person's data.
 CONSTRAINT billing_profile_sole_trader CHECK (taxpayer_number IS NULL OR taxpayer_number NOT LIKE '92%'
   OR (registered_phone IS NULL AND bank_account IS NULL))
);

CREATE TABLE cargo_fin.debtor_invoice (
 tenant_id text NOT NULL,
 invoice_id text NOT NULL CHECK (length(btrim(invoice_id)) BETWEEN 1 AND 128),
 kind text NOT NULL CHECK (kind IN ('DEBIT_NOTE','CREDIT_NOTE')),
 job_id text NOT NULL,
 legal_entity_id text NOT NULL,
 number text NOT NULL CHECK (length(number) BETWEEN 5 AND 20),
 number_seq bigint NOT NULL CHECK (number_seq >= 1),
 debtor_party_id text NOT NULL,
 debtor_revision bigint NOT NULL CHECK (debtor_revision > 0),
 profile_version integer NOT NULL,
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 invoice_date date NOT NULL,
 due_date date,
 payment_days integer CHECK (payment_days BETWEEN 0 AND 365),
 payment_basis text CHECK (payment_basis IN ('INVOICE_DATE','END_OF_MONTH')),
 terms_version integer CHECK (terms_version > 0),
 document_id text CHECK (length(btrim(document_id)) BETWEEN 1 AND 128),
 document_version integer CHECK (document_version > 0),
 document_number text CHECK (length(btrim(document_number)) BETWEEN 1 AND 128),
 master_number text CHECK (length(btrim(master_number)) BETWEEN 1 AND 128),
 document_issued_at timestamptz,
 credits_invoice_id text,
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 write_off boolean NOT NULL DEFAULT false,
 total_minor bigint NOT NULL CHECK (abs(total_minor) <= 1000000000000000),
 local_total_minor bigint NOT NULL CHECK (abs(local_total_minor) <= 1000000000000000),
 state text NOT NULL CHECK (state IN ('ISSUED','CANCELLED_BY_CREDIT_NOTE')),
 issued_by text NOT NULL CHECK (length(btrim(issued_by)) > 0),
 issued_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, invoice_id),
 FOREIGN KEY (tenant_id, job_id) REFERENCES cargo_fin.freight_job (tenant_id, job_id),
 FOREIGN KEY (tenant_id, legal_entity_id, kind) REFERENCES cargo_fin.invoice_series (tenant_id, legal_entity_id, kind),
 FOREIGN KEY (tenant_id, debtor_party_id, profile_version) REFERENCES cargo_fin.billing_profile_version (tenant_id, debtor_party_id, version_no),
 FOREIGN KEY (tenant_id, credits_invoice_id) REFERENCES cargo_fin.debtor_invoice (tenant_id, invoice_id),
 CONSTRAINT debtor_invoice_number UNIQUE (tenant_id, legal_entity_id, number),
 CONSTRAINT debtor_invoice_kind CHECK (CASE kind
   WHEN 'DEBIT_NOTE' THEN due_date IS NOT NULL AND payment_days IS NOT NULL AND payment_basis IS NOT NULL AND document_id IS NOT NULL
     AND document_version IS NOT NULL AND document_number IS NOT NULL AND document_issued_at IS NOT NULL AND issued_at >= document_issued_at
     AND credits_invoice_id IS NULL AND reason IS NULL AND NOT write_off AND total_minor > 0
   ELSE due_date IS NULL AND payment_days IS NULL AND payment_basis IS NULL AND terms_version IS NULL AND document_id IS NULL
     AND document_version IS NULL AND document_number IS NULL AND master_number IS NULL AND document_issued_at IS NULL
     AND credits_invoice_id IS NOT NULL AND reason IS NOT NULL AND total_minor < 0 AND state = 'ISSUED' END)
);
CREATE INDEX debtor_invoice_by_debtor ON cargo_fin.debtor_invoice (tenant_id, legal_entity_id, debtor_party_id);
CREATE INDEX debtor_invoice_by_job ON cargo_fin.debtor_invoice (tenant_id, job_id);

CREATE TABLE cargo_fin.debtor_invoice_line (
 tenant_id text NOT NULL,
 invoice_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 job_id text NOT NULL,
 job_line_no integer NOT NULL,
 side_version integer CHECK (side_version > 1),
 credits_invoice_id text,
 credits_line_no integer,
 charge_code text NOT NULL CHECK (length(btrim(charge_code)) BETWEEN 1 AND 64),
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 amount_minor bigint NOT NULL CHECK (amount_minor <> 0 AND abs(amount_minor) <= 1000000000000000),
 invoice_minor bigint NOT NULL CHECK (abs(invoice_minor) <= 1000000000000000),
 local_minor bigint NOT NULL CHECK (abs(local_minor) <= 1000000000000000),
 rate_entry_no integer,
 rate_type text CHECK (rate_type IN ('INVOICE','ACCOUNTING')),
 rate bigint CHECK (rate > 0),
 scale integer,
 from_digits integer,
 to_digits integer,
 rate_source text,
 tax_version integer NOT NULL CHECK (tax_version > 0),
 treatment text NOT NULL CHECK (treatment IN ('EXEMPT_INTL_FREIGHT_FORWARDING','TAXABLE','PASS_THROUGH_DISBURSEMENT')),
 rate_bp integer CHECK (rate_bp BETWEEN 1 AND 3000),
 PRIMARY KEY (tenant_id, invoice_id, line_no),
 FOREIGN KEY (tenant_id, invoice_id) REFERENCES cargo_fin.debtor_invoice (tenant_id, invoice_id),
 FOREIGN KEY (tenant_id, job_id, job_line_no) REFERENCES cargo_fin.charge_line (tenant_id, job_id, line_no),
 FOREIGN KEY (tenant_id, credits_invoice_id, credits_line_no) REFERENCES cargo_fin.debtor_invoice_line (tenant_id, invoice_id, line_no),
 CONSTRAINT debtor_invoice_line_ref CHECK ((side_version IS NULL) <> (credits_line_no IS NULL) AND (credits_line_no IS NULL) = (credits_invoice_id IS NULL)),
 CONSTRAINT debtor_invoice_line_rate CHECK ((rate_entry_no IS NULL) = (rate IS NULL) AND (rate IS NULL) = (rate_type IS NULL) AND (rate IS NULL) = (scale IS NULL)
   AND (rate IS NULL) = (from_digits IS NULL) AND (rate IS NULL) = (to_digits IS NULL) AND (rate IS NULL) = (rate_source IS NULL)),
 CONSTRAINT debtor_invoice_line_tax CHECK ((treatment = 'TAXABLE') = (rate_bp IS NOT NULL))
);
-- A sell side version is billed on one debit note line; a debit note line is reversed by one credit note line.
CREATE UNIQUE INDEX debtor_invoice_line_side_once ON cargo_fin.debtor_invoice_line (tenant_id, job_id, job_line_no, side_version) WHERE side_version IS NOT NULL;
CREATE UNIQUE INDEX debtor_invoice_line_reversed_once ON cargo_fin.debtor_invoice_line (tenant_id, credits_invoice_id, credits_line_no)
  WHERE credits_invoice_id IS NOT NULL;

-- A sell side is ESTIMATED or INVOICED; an INVOICED version names the debit note line that billed it.
ALTER TABLE cargo_fin.charge_side_version ADD COLUMN invoice_id text, ADD COLUMN invoice_line_no integer;
ALTER TABLE cargo_fin.charge_side_version DROP CONSTRAINT charge_side_version_status_check,
  ADD CONSTRAINT charge_side_version_status_check CHECK (status IN ('ESTIMATED','INVOICED','ACCRUED'));
ALTER TABLE cargo_fin.charge_side_version DROP CONSTRAINT charge_side_sell,
  ADD CONSTRAINT charge_side_sell CHECK (side <> 'SELL' OR (party_id IS NOT NULL AND status IN ('ESTIMATED','INVOICED')));
-- An estimate converts at the ESTIMATE rate, an invoiced side at its debit note's INVOICE or ACCOUNTING rate.
ALTER TABLE cargo_fin.charge_side_version ADD CONSTRAINT charge_side_invoice CHECK ((status = 'INVOICED') = (invoice_id IS NOT NULL)
  AND (invoice_id IS NULL) = (invoice_line_no IS NULL)),
  ADD CONSTRAINT charge_side_rate_type CHECK (rate_type IS NULL OR (status = 'INVOICED') = (rate_type IN ('INVOICE','ACCOUNTING'))),
  ADD CONSTRAINT charge_side_invoice_line FOREIGN KEY (tenant_id, invoice_id, invoice_line_no) REFERENCES cargo_fin.debtor_invoice_line (tenant_id, invoice_id, line_no);

ALTER TABLE cargo_fin.command_receipt ADD COLUMN result_number text CHECK (length(result_number) BETWEEN 5 AND 20);
ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile'));

-- Rows change only where a column says so; V0.2 adds a debit note's state (ISSUED to CANCELLED_BY_CREDIT_NOTE, once)
-- and a series' next number (forward only).
CREATE OR REPLACE FUNCTION cargo_fin.change_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := TG_ARGV::text[];
  o jsonb := to_jsonb(OLD);
  n jsonb;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_fin: % rows are never deleted', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
  END IF;
  n := to_jsonb(NEW);
  IF o - changeable IS DISTINCT FROM n - changeable
     OR (TG_TABLE_NAME = 'freight_job' AND o->>'state' = 'CANCELLED')
     OR (TG_TABLE_NAME = 'job_basis' AND (NOT (o->>'live')::boolean OR (n->>'live')::boolean))
     OR (TG_TABLE_NAME = 'credit_hold' AND (o->>'released_at' IS NOT NULL OR n->>'released_at' IS NULL))
     OR (TG_TABLE_NAME = 'exchange_rate_entry' AND (o->>'ended_at' IS NOT NULL OR n->>'ended_at' IS NULL
       OR (n->>'valid_until')::timestamptz >= (o->>'valid_until')::timestamptz))
     OR (TG_TABLE_NAME = 'debtor_invoice' AND (o->>'state' <> 'ISSUED' OR n->>'state' <> 'CANCELLED_BY_CREDIT_NOTE'))
     OR (TG_TABLE_NAME = 'invoice_series' AND (n->>'next_seq')::bigint <= (o->>'next_seq')::bigint) THEN
    RAISE EXCEPTION 'cargo_fin: % changes only %', TG_TABLE_NAME, changeable USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER debtor_invoice_rules BEFORE UPDATE OR DELETE ON cargo_fin.debtor_invoice
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.change_only('state');
CREATE TRIGGER invoice_series_rules BEFORE UPDATE OR DELETE ON cargo_fin.invoice_series
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.change_only('next_seq');
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['charge_tax_treatment','billing_profile_version','debtor_invoice_line'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_fin.%I FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- At commit, a policy also numbers its tax treatments from 1 without gaps, a charge code's later versions start later,
-- and its two series' prefixes never start one another.
CREATE OR REPLACE FUNCTION cargo_fin.check_policy_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  p cargo_fin.finance_policy;
  ok boolean;
BEGIN
  SELECT * INTO p FROM cargo_fin.finance_policy WHERE tenant_id = NEW.tenant_id AND legal_entity_id = NEW.legal_entity_id;
  SELECT (SELECT count(*) FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = p.tenant_id AND e.legal_entity_id = p.legal_entity_id)
       = (SELECT coalesce(max(entry_no), 0) FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = p.tenant_id AND e.legal_entity_id = p.legal_entity_id)
     AND (SELECT count(DISTINCT to_digits) <= 1 FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = p.tenant_id AND e.legal_entity_id = p.legal_entity_id)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = p.tenant_id AND e.legal_entity_id = p.legal_entity_id
       AND e.from_currency = p.functional_currency)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.exchange_rate_entry a JOIN cargo_fin.exchange_rate_entry b ON b.tenant_id = a.tenant_id
       AND b.legal_entity_id = a.legal_entity_id AND b.rate_type = a.rate_type AND b.from_currency = a.from_currency AND b.entry_no > a.entry_no
       WHERE a.tenant_id = p.tenant_id AND a.legal_entity_id = p.legal_entity_id
         AND tstzrange(a.valid_from, a.valid_until) && tstzrange(b.valid_from, b.valid_until))
     AND (SELECT count(*) = coalesce(max(version_no), 0) FROM cargo_fin.charge_tax_treatment t WHERE t.tenant_id = p.tenant_id
       AND t.legal_entity_id = p.legal_entity_id)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_tax_treatment a JOIN cargo_fin.charge_tax_treatment b ON b.tenant_id = a.tenant_id
       AND b.legal_entity_id = a.legal_entity_id AND b.charge_code = a.charge_code AND b.version_no > a.version_no
       WHERE a.tenant_id = p.tenant_id AND a.legal_entity_id = p.legal_entity_id AND b.effective_from <= a.effective_from)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.invoice_series a JOIN cargo_fin.invoice_series b ON b.tenant_id = a.tenant_id
       AND b.legal_entity_id = a.legal_entity_id AND b.kind > a.kind
       WHERE a.tenant_id = p.tenant_id AND a.legal_entity_id = p.legal_entity_id AND (starts_with(a.prefix, b.prefix) OR starts_with(b.prefix, a.prefix)))
    INTO ok;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_fin: finance policy %/% is inconsistent at commit', p.tenant_id, p.legal_entity_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'finance_policy_consistent';
  END IF;
  RETURN NULL;
END $fn$;

-- At commit, an entry ended earlier was used by no charge or debit note at or after its new end.
CREATE OR REPLACE FUNCTION cargo_fin.check_rate_end_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v JOIN cargo_fin.freight_job j ON j.tenant_id = v.tenant_id AND j.job_id = v.job_id
       WHERE v.tenant_id = NEW.tenant_id AND j.legal_entity_id = NEW.legal_entity_id AND v.rate_entry_no = NEW.entry_no AND v.recorded_at >= NEW.valid_until)
     OR EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line l JOIN cargo_fin.debtor_invoice n ON n.tenant_id = l.tenant_id AND n.invoice_id = l.invoice_id
       WHERE n.tenant_id = NEW.tenant_id AND n.legal_entity_id = NEW.legal_entity_id AND l.rate_entry_no = NEW.entry_no AND l.side_version IS NOT NULL
         AND n.issued_at >= NEW.valid_until) THEN
    RAISE EXCEPTION 'cargo_fin: exchange rate entry %/%/% converted a charge at or after its new end', NEW.tenant_id, NEW.legal_entity_id, NEW.entry_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'exchange_rate_entry_in_use';
  END IF;
  RETURN NULL;
END $fn$;

-- At commit, a new tax treatment version starts after every debit note line of the legal entity billing its charge code
-- that no credit note reversed (a treatment is corrected by reversing the lines billed under the wrong one).
CREATE FUNCTION cargo_fin.check_tax_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line l JOIN cargo_fin.debtor_invoice n ON n.tenant_id = l.tenant_id AND n.invoice_id = l.invoice_id
       WHERE n.tenant_id = NEW.tenant_id AND n.legal_entity_id = NEW.legal_entity_id AND n.kind = 'DEBIT_NOTE' AND l.charge_code = NEW.charge_code
         AND n.invoice_date >= NEW.effective_from AND NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line c WHERE c.tenant_id = l.tenant_id
           AND c.credits_invoice_id = l.invoice_id AND c.credits_line_no = l.line_no)) THEN
    RAISE EXCEPTION 'cargo_fin: tax treatment %/%/% starts on or before a debit note billing %', NEW.tenant_id, NEW.legal_entity_id, NEW.version_no,
      NEW.charge_code USING ERRCODE = 'check_violation', CONSTRAINT = 'charge_tax_treatment_in_use';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER charge_tax_treatment_in_use AFTER INSERT ON cargo_fin.charge_tax_treatment
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_tax_at_commit();

-- At commit, a series has drawn exactly the numbers of its notes: every number from its start up to its next is a note's.
CREATE FUNCTION cargo_fin.check_series_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF (SELECT s.next_seq - s.start_seq FROM cargo_fin.invoice_series s WHERE s.tenant_id = NEW.tenant_id AND s.legal_entity_id = NEW.legal_entity_id
        AND s.kind = NEW.kind)
     <> (SELECT count(*) FROM cargo_fin.debtor_invoice n WHERE n.tenant_id = NEW.tenant_id AND n.legal_entity_id = NEW.legal_entity_id AND n.kind = NEW.kind) THEN
    RAISE EXCEPTION 'cargo_fin: series %/%/% drew numbers no note holds', NEW.tenant_id, NEW.legal_entity_id, NEW.kind
      USING ERRCODE = 'check_violation', CONSTRAINT = 'invoice_series_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER invoice_series_consistent AFTER INSERT OR UPDATE ON cargo_fin.invoice_series
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_series_at_commit();

-- At commit, a debtor's billing profile versions are numbered from 1 without gaps.
CREATE FUNCTION cargo_fin.check_profile_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF (SELECT count(*) <> max(version_no) FROM cargo_fin.billing_profile_version v WHERE v.tenant_id = NEW.tenant_id
        AND v.debtor_party_id = NEW.debtor_party_id) THEN
    RAISE EXCEPTION 'cargo_fin: billing profile %/% skips a version', NEW.tenant_id, NEW.debtor_party_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'billing_profile_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER billing_profile_consistent AFTER INSERT ON cargo_fin.billing_profile_version
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_profile_at_commit();

-- At commit, a job's notes are consistent:
-- (a) a note is of the job's legal entity, its lines are numbered from 1 without gaps and its totals are their sums;
-- (b) its number is its series' format of a sequence value drawn; (c) its invoice date is the issue's date in some time
-- zone (UTC-12 to UTC+14);
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
CREATE FUNCTION cargo_fin.check_job_invoices_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  j cargo_fin.freight_job;
  ok boolean;
BEGIN
  SELECT * INTO j FROM cargo_fin.freight_job WHERE tenant_id = NEW.tenant_id AND job_id = NEW.job_id;
  SELECT NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n WHERE n.tenant_id = j.tenant_id AND n.job_id = j.job_id
       AND (n.legal_entity_id <> j.legal_entity_id
         OR (SELECT count(*) = 0 OR count(*) <> max(line_no) OR sum(invoice_minor) <> n.total_minor OR sum(local_minor) <> n.local_total_minor
             FROM cargo_fin.debtor_invoice_line l WHERE l.tenant_id = n.tenant_id AND l.invoice_id = n.invoice_id)
         OR NOT EXISTS (SELECT 1 FROM cargo_fin.invoice_series s WHERE s.tenant_id = n.tenant_id AND s.legal_entity_id = n.legal_entity_id
             AND s.kind = n.kind AND n.number_seq >= s.start_seq AND n.number_seq < s.next_seq AND n.number = s.prefix || lpad(n.number_seq::text, s.width, '0'))
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
CREATE CONSTRAINT TRIGGER freight_job_invoices_consistent AFTER INSERT OR UPDATE ON cargo_fin.freight_job
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_job_invoices_at_commit();

-- At commit, tax treatments and series are written with their policy, notes and their lines with their job.
CREATE OR REPLACE FUNCTION cargo_fin.check_written_with_parent() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  found boolean;
BEGIN
  CASE TG_TABLE_NAME
    WHEN 'exchange_rate_entry', 'charge_tax_treatment', 'invoice_series' THEN
      SELECT EXISTS (SELECT 1 FROM cargo_fin.finance_policy p WHERE p.tenant_id = NEW.tenant_id AND p.legal_entity_id = NEW.legal_entity_id
        AND p.xmin = pg_current_xact_id()::xid) INTO found;
    WHEN 'credit_terms_version', 'credit_hold' THEN
      SELECT EXISTS (SELECT 1 FROM cargo_fin.credit_account a WHERE a.tenant_id = NEW.tenant_id AND a.legal_entity_id = NEW.legal_entity_id
        AND a.debtor_party_id = NEW.debtor_party_id AND a.xmin = pg_current_xact_id()::xid) INTO found;
    ELSE
      SELECT EXISTS (SELECT 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id
        AND j.xmin = pg_current_xact_id()::xid) INTO found;
  END CASE;
  IF NOT found THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its parent', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER charge_tax_treatment_with_parent AFTER INSERT ON cargo_fin.charge_tax_treatment DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_written_with_parent();
CREATE CONSTRAINT TRIGGER invoice_series_with_parent AFTER INSERT ON cargo_fin.invoice_series DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_written_with_parent();
CREATE CONSTRAINT TRIGGER debtor_invoice_with_parent AFTER INSERT OR UPDATE ON cargo_fin.debtor_invoice DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_written_with_parent();
CREATE CONSTRAINT TRIGGER debtor_invoice_line_with_parent AFTER INSERT ON cargo_fin.debtor_invoice_line DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_written_with_parent();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['charge_tax_treatment','invoice_series','billing_profile_version','debtor_invoice','debtor_invoice_line'] LOOP
    EXECUTE format('ALTER TABLE cargo_fin.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_fin.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_fin.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
