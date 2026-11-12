-- IDP-Cargo FIN V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Settlement (FIN candidate 26: OD-2 B and the second half of FB5): FIN keeps the settlement status of debit notes from
-- bank or ERP evidence; the cash book, bank reconciliation and exchange differences stay in the ERP. A debtor receipt
-- is money received from a debtor of a legal entity, recorded with its evidence reference; its allocations settle the
-- debtor's debit notes, in part or across several, and what is not allocated stays on account. A receipt changes only
-- to be cancelled, once; an allocation only to be reversed, once; neither is deleted. A debit note's state stays about
-- the document: what is settled of it is the sum of its live allocations.

CREATE TABLE cargo_fin.debtor_receipt (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 receipt_id text NOT NULL CHECK (length(btrim(receipt_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL,
 debtor_party_id text NOT NULL CHECK (length(btrim(debtor_party_id)) BETWEEN 1 AND 128),
 debtor_revision bigint NOT NULL CHECK (debtor_revision > 0),
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 amount_minor bigint NOT NULL CHECK (amount_minor > 0 AND amount_minor <= 1000000000000000),
 -- deducted by banks on the way and borne by the legal entity: the debtor is credited with amount + bank charges
 bank_charges_minor bigint NOT NULL CHECK (bank_charges_minor >= 0),
 received_on date NOT NULL,
 evidence_source text NOT NULL CHECK (evidence_source IN ('BANK','ERP')),
 evidence_ref text NOT NULL CHECK (evidence_ref ~ '^[A-Za-z0-9][A-Za-z0-9 ._:/#-]{0,199}$'),
 state text NOT NULL CHECK (state IN ('RECORDED','CANCELLED')),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 cancel_reason text CHECK (cancel_reason IN ('ENTERED_IN_ERROR','PAYMENT_RETURNED')),
 cancelled_by text CHECK (length(btrim(cancelled_by)) > 0),
 cancelled_on date,
 cancelled_at timestamptz,
 cancel_entered_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, receipt_id),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 CONSTRAINT debtor_receipt_charges CHECK (bank_charges_minor <= amount_minor),
 -- FIN keeps no time zone of the legal entity: a civil date is checked against the instant it was given at, in any
 -- time zone (UTC-12 to UTC+14). The date received is not after the day recorded and not more than 366 days before.
 CONSTRAINT debtor_receipt_received CHECK (recorded_at >= (received_on::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
   AND recorded_at < (received_on::timestamp AT TIME ZONE 'UTC') + interval '366 days 36 hours'),
 CONSTRAINT debtor_receipt_cancelled CHECK ((state = 'CANCELLED') = (cancelled_at IS NOT NULL) AND (cancelled_at IS NULL) = (cancel_reason IS NULL)
   AND (cancelled_at IS NULL) = (cancelled_by IS NULL) AND (cancelled_at IS NULL) = (cancelled_on IS NULL)
   AND (cancelled_at IS NULL) = (cancel_entered_at IS NULL)
   AND (cancelled_at IS NULL OR (cancelled_on >= received_on AND cancelled_at >= recorded_at
     AND cancelled_at >= (cancelled_on::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
     AND cancelled_at < (cancelled_on::timestamp AT TIME ZONE 'UTC') + interval '36 hours')))
);
-- One live receipt per piece of evidence; a cancelled receipt frees its reference to be entered again.
CREATE UNIQUE INDEX debtor_receipt_evidence_once ON cargo_fin.debtor_receipt (tenant_id, legal_entity_id, evidence_source, evidence_ref)
  WHERE state = 'RECORDED';
CREATE INDEX debtor_receipt_by_debtor ON cargo_fin.debtor_receipt (tenant_id, legal_entity_id, debtor_party_id);

CREATE TABLE cargo_fin.debtor_receipt_allocation (
 tenant_id text NOT NULL,
 receipt_id text NOT NULL,
 allocation_no integer NOT NULL CHECK (allocation_no > 0),
 invoice_id text NOT NULL,
 amount_minor bigint NOT NULL CHECK (amount_minor > 0 AND amount_minor <= 2000000000000000),
 allocated_on date NOT NULL,
 allocated_by text NOT NULL CHECK (length(btrim(allocated_by)) > 0),
 allocated_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 reversed_by text CHECK (length(btrim(reversed_by)) > 0),
 reverse_reason text CHECK (length(btrim(reverse_reason)) BETWEEN 1 AND 500),
 reversed_on date,
 reversed_at timestamptz,
 reverse_entered_at timestamptz,
 PRIMARY KEY (tenant_id, receipt_id, allocation_no),
 FOREIGN KEY (tenant_id, receipt_id) REFERENCES cargo_fin.debtor_receipt (tenant_id, receipt_id),
 FOREIGN KEY (tenant_id, invoice_id) REFERENCES cargo_fin.debtor_invoice (tenant_id, invoice_id),
 CONSTRAINT debtor_receipt_allocation_date CHECK (allocated_at >= (allocated_on::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
   AND allocated_at < (allocated_on::timestamp AT TIME ZONE 'UTC') + interval '36 hours'),
 CONSTRAINT debtor_receipt_allocation_reversal CHECK ((reversed_at IS NULL) = (reversed_by IS NULL) AND (reversed_at IS NULL) = (reverse_reason IS NULL)
   AND (reversed_at IS NULL) = (reversed_on IS NULL) AND (reversed_at IS NULL) = (reverse_entered_at IS NULL)
   AND (reversed_at IS NULL OR (reversed_on >= allocated_on AND reversed_at >= allocated_at
     AND reversed_at >= (reversed_on::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
     AND reversed_at < (reversed_on::timestamp AT TIME ZONE 'UTC') + interval '36 hours')))
);
CREATE INDEX debtor_receipt_allocation_by_invoice ON cargo_fin.debtor_receipt_allocation (tenant_id, invoice_id);

ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile','DebtorReceipt'));

-- A receipt changes only to be cancelled, once, with its revision; an allocation only to be reversed, once.
CREATE FUNCTION cargo_fin.debtor_receipt_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','cancel_reason','cancelled_by','cancelled_on','cancelled_at','cancel_entered_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_fin: % rows are never deleted', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
  END IF;
  IF to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable OR OLD.state <> 'RECORDED' OR NEW.revision <= OLD.revision THEN
    RAISE EXCEPTION 'cargo_fin: a debtor receipt changes only to be cancelled, once, with its revision' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER debtor_receipt_rules BEFORE UPDATE OR DELETE ON cargo_fin.debtor_receipt
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.debtor_receipt_rules();
CREATE FUNCTION cargo_fin.debtor_receipt_allocation_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['reversed_by','reverse_reason','reversed_on','reversed_at','reverse_entered_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_fin: % rows are never deleted', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
  END IF;
  IF to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable OR OLD.reversed_at IS NOT NULL OR NEW.reversed_at IS NULL THEN
    RAISE EXCEPTION 'cargo_fin: a receipt allocation changes only to be reversed, once' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER debtor_receipt_allocation_rules BEFORE UPDATE OR DELETE ON cargo_fin.debtor_receipt_allocation
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.debtor_receipt_allocation_rules();

-- What a debit note still bills (its total and its credit notes' totals) covers what its live allocations settle.
CREATE FUNCTION cargo_fin.settled_within(t text, invoice text) RETURNS boolean LANGUAGE sql STABLE AS $fn$
  SELECT (SELECT coalesce(sum(a.amount_minor), 0) FROM cargo_fin.debtor_receipt_allocation a
            WHERE a.tenant_id = t AND a.invoice_id = invoice AND a.reversed_at IS NULL)
      <= (SELECT coalesce(sum(n.total_minor), 0) FROM cargo_fin.debtor_invoice n
            WHERE n.tenant_id = t AND (n.invoice_id = invoice OR n.credits_invoice_id = invoice))
$fn$;

-- At commit, a receipt: its allocations are numbered from 1 without gaps and not earlier than it, the live ones
-- total at most the amount and the bank charges, and a cancelled receipt has none live nor reversed after its
-- cancellation date.
CREATE FUNCTION cargo_fin.check_receipt_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  r cargo_fin.debtor_receipt;
  ok boolean;
BEGIN
  SELECT * INTO r FROM cargo_fin.debtor_receipt WHERE tenant_id = NEW.tenant_id AND receipt_id = NEW.receipt_id;
  SELECT count(*) = coalesce(max(a.allocation_no), 0)
      AND coalesce(sum(a.amount_minor) FILTER (WHERE a.reversed_at IS NULL), 0) <= r.amount_minor + r.bank_charges_minor
      AND coalesce(bool_and(a.allocated_at >= r.recorded_at AND a.allocated_on >= r.received_on), true)
      AND (r.state = 'RECORDED' OR coalesce(bool_and(a.reversed_at IS NOT NULL AND a.reversed_on <= r.cancelled_on AND a.reversed_at <= r.cancelled_at), true))
    INTO ok FROM cargo_fin.debtor_receipt_allocation a WHERE a.tenant_id = r.tenant_id AND a.receipt_id = r.receipt_id;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_fin: debtor receipt %/% is inconsistent at commit (state %)', r.tenant_id, r.receipt_id, r.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'debtor_receipt_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER debtor_receipt_consistent AFTER INSERT OR UPDATE ON cargo_fin.debtor_receipt
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_receipt_at_commit();

-- At commit, an allocation: written by a transaction that writes its receipt; to a debit note of the receipt's legal
-- entity, debtor and currency, dated not before the note; and the note has no more settled than it still bills.
CREATE FUNCTION cargo_fin.check_allocation_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_receipt r WHERE r.tenant_id = NEW.tenant_id AND r.receipt_id = NEW.receipt_id
       AND r.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its parent', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_receipt r JOIN cargo_fin.debtor_invoice n ON n.tenant_id = r.tenant_id AND n.invoice_id = NEW.invoice_id
       WHERE r.tenant_id = NEW.tenant_id AND r.receipt_id = NEW.receipt_id AND n.kind = 'DEBIT_NOTE' AND n.legal_entity_id = r.legal_entity_id
         AND n.debtor_party_id = r.debtor_party_id AND n.currency = r.currency AND NEW.allocated_on >= n.invoice_date) THEN
    RAISE EXCEPTION 'cargo_fin: allocation % of debtor receipt %/% is not to a debit note of its legal entity, debtor and currency dated by then',
      NEW.allocation_no, NEW.tenant_id, NEW.receipt_id USING ERRCODE = 'check_violation', CONSTRAINT = 'debtor_receipt_consistent';
  END IF;
  IF NOT cargo_fin.settled_within(NEW.tenant_id, NEW.invoice_id) THEN
    RAISE EXCEPTION 'cargo_fin: debit note %/% has more settled than it bills', NEW.tenant_id, NEW.invoice_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'debtor_invoice_settlement_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER debtor_receipt_allocation_consistent AFTER INSERT OR UPDATE ON cargo_fin.debtor_receipt_allocation
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_allocation_at_commit();

-- At commit, a job: none of its debit notes has more settled than it still bills (a credit note written with the job
-- lowers that).
CREATE FUNCTION cargo_fin.check_job_settlement_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n WHERE n.tenant_id = NEW.tenant_id AND n.job_id = NEW.job_id AND n.kind = 'DEBIT_NOTE'
       AND NOT cargo_fin.settled_within(n.tenant_id, n.invoice_id)) THEN
    RAISE EXCEPTION 'cargo_fin: a debit note of freight job %/% has more settled than it bills', NEW.tenant_id, NEW.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'debtor_invoice_settlement_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER freight_job_settlement_consistent AFTER INSERT OR UPDATE ON cargo_fin.freight_job
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_job_settlement_at_commit();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['debtor_receipt','debtor_receipt_allocation'] LOOP
    EXECUTE format('ALTER TABLE cargo_fin.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_fin.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_fin.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
