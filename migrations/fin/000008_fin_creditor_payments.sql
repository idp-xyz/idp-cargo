-- IDP-Cargo FIN V0.6 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Creditor payments (FIN candidate 26: OD-2 B, the payable side of V0.3's settlement): FIN keeps what is paid of
-- creditor invoices from bank or ERP evidence; the cash book, payment runs, bank reconciliation and exchange
-- differences stay in the ERP. A creditor payment is money a legal entity paid a creditor, recorded with its evidence
-- reference; its allocations pay the creditor's matched invoices, in part or across several, and what is not allocated
-- stays on account (paid in advance). A payment changes only to be cancelled, once; an allocation only to be reversed,
-- once; neither is deleted. A creditor invoice's state stays about the invoice: what is paid of it is the sum of its
-- live allocations, and an invoice that is paid is not cancelled.

CREATE TABLE cargo_fin.creditor_payment (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 payment_id text NOT NULL CHECK (length(btrim(payment_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL,
 creditor_party_id text NOT NULL CHECK (length(btrim(creditor_party_id)) BETWEEN 1 AND 128),
 creditor_revision bigint NOT NULL CHECK (creditor_revision > 0),
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 amount_minor bigint NOT NULL CHECK (amount_minor > 0 AND amount_minor <= 1000000000000000),
 paid_on date NOT NULL,
 evidence_source text NOT NULL CHECK (evidence_source IN ('BANK','ERP')),
 -- no trailing space: 'ABC ' would pass as other evidence than 'ABC'
 evidence_ref text NOT NULL CHECK (evidence_ref ~ '^[A-Za-z0-9]([A-Za-z0-9 ._:/#-]{0,198}[A-Za-z0-9._:/#-])?$'),
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
 PRIMARY KEY (tenant_id, payment_id),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 -- as debtor receipts: the date paid is not after the day recorded and not more than 366 days before, in any time zone
 -- (UTC-12 to UTC+14), in absolute hours (8820 = 366 days and 36 hours)
 CONSTRAINT creditor_payment_paid CHECK (recorded_at >= (paid_on::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
   AND recorded_at < (paid_on::timestamp AT TIME ZONE 'UTC') + interval '8820 hours'),
 CONSTRAINT creditor_payment_cancelled CHECK ((state = 'CANCELLED') = (cancelled_at IS NOT NULL) AND (cancelled_at IS NULL) = (cancel_reason IS NULL)
   AND (cancelled_at IS NULL) = (cancelled_by IS NULL) AND (cancelled_at IS NULL) = (cancelled_on IS NULL)
   AND (cancelled_at IS NULL) = (cancel_entered_at IS NULL)
   AND (cancelled_at IS NULL OR (cancelled_on >= paid_on AND cancelled_at >= recorded_at
     AND cancelled_at >= (cancelled_on::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
     AND cancelled_at < (cancelled_on::timestamp AT TIME ZONE 'UTC') + interval '36 hours')))
);
-- One live payment per piece of evidence; a cancelled payment frees its reference to be entered again.
CREATE UNIQUE INDEX creditor_payment_evidence_once ON cargo_fin.creditor_payment (tenant_id, legal_entity_id, evidence_source, evidence_ref)
  WHERE state = 'RECORDED';
CREATE INDEX creditor_payment_by_creditor ON cargo_fin.creditor_payment (tenant_id, legal_entity_id, creditor_party_id);

CREATE TABLE cargo_fin.creditor_payment_allocation (
 tenant_id text NOT NULL,
 payment_id text NOT NULL,
 allocation_no integer NOT NULL CHECK (allocation_no > 0),
 invoice_id text NOT NULL,
 amount_minor bigint NOT NULL CHECK (amount_minor > 0 AND amount_minor <= 1000000000000000),
 allocated_on date NOT NULL,
 allocated_by text NOT NULL CHECK (length(btrim(allocated_by)) > 0),
 allocated_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 reversed_by text CHECK (length(btrim(reversed_by)) > 0),
 reverse_reason text CHECK (length(btrim(reverse_reason)) BETWEEN 1 AND 500),
 reversed_on date,
 reversed_at timestamptz,
 reverse_entered_at timestamptz,
 PRIMARY KEY (tenant_id, payment_id, allocation_no),
 FOREIGN KEY (tenant_id, payment_id) REFERENCES cargo_fin.creditor_payment (tenant_id, payment_id),
 FOREIGN KEY (tenant_id, invoice_id) REFERENCES cargo_fin.creditor_invoice (tenant_id, invoice_id),
 CONSTRAINT creditor_payment_allocation_date CHECK (allocated_at >= (allocated_on::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
   AND allocated_at < (allocated_on::timestamp AT TIME ZONE 'UTC') + interval '36 hours'),
 CONSTRAINT creditor_payment_allocation_reversal CHECK ((reversed_at IS NULL) = (reversed_by IS NULL) AND (reversed_at IS NULL) = (reverse_reason IS NULL)
   AND (reversed_at IS NULL) = (reversed_on IS NULL) AND (reversed_at IS NULL) = (reverse_entered_at IS NULL)
   AND (reversed_at IS NULL OR (reversed_on >= allocated_on AND reversed_at >= allocated_at
     AND reversed_at >= (reversed_on::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
     AND reversed_at < (reversed_on::timestamp AT TIME ZONE 'UTC') + interval '36 hours')))
);
CREATE INDEX creditor_payment_allocation_by_invoice ON cargo_fin.creditor_payment_allocation (tenant_id, invoice_id);

ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check
    CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile','DebtorReceipt','ConsolCostPool','CreditorInvoice',
      'CreditorPayment'));

-- A payment changes only to be cancelled, once, with its revision; an allocation only to be reversed, once.
CREATE FUNCTION cargo_fin.creditor_payment_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','cancel_reason','cancelled_by','cancelled_on','cancelled_at','cancel_entered_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_fin: % rows are never deleted', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
  END IF;
  IF to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable OR OLD.state <> 'RECORDED' OR NEW.revision <= OLD.revision THEN
    RAISE EXCEPTION 'cargo_fin: a creditor payment changes only to be cancelled, once, with its revision' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER creditor_payment_rules BEFORE UPDATE OR DELETE ON cargo_fin.creditor_payment
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.creditor_payment_rules();
CREATE TRIGGER creditor_payment_allocation_rules BEFORE UPDATE OR DELETE ON cargo_fin.creditor_payment_allocation
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.resolve_once('reversed_at','reversed_by','reverse_reason','reversed_on','reverse_entered_at');

-- What an invoice totals covers what its live allocations pay.
CREATE FUNCTION cargo_fin.paid_within(t text, invoice text) RETURNS boolean LANGUAGE sql STABLE AS $fn$
  SELECT (SELECT coalesce(sum(a.amount_minor), 0) FROM cargo_fin.creditor_payment_allocation a
            WHERE a.tenant_id = t AND a.invoice_id = invoice AND a.reversed_at IS NULL)
      <= (SELECT i.total_minor FROM cargo_fin.creditor_invoice i WHERE i.tenant_id = t AND i.invoice_id = invoice)
$fn$;

-- A payment is consistent when its allocations are numbered from 1 without gaps and not earlier than it, the live
-- ones total at most its amount, and a cancelled payment has none live nor reversed after its cancellation date. Both
-- commit triggers call this, so an allocation inserted after SET CONSTRAINTS ALL IMMEDIATE ran the payment's own
-- check cannot escape it.
CREATE FUNCTION cargo_fin.creditor_payment_consistent_check(t text, id text) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  p cargo_fin.creditor_payment;
  ok boolean;
BEGIN
  SELECT * INTO p FROM cargo_fin.creditor_payment WHERE tenant_id = t AND payment_id = id;
  SELECT count(*) = coalesce(max(a.allocation_no), 0)
      AND coalesce(sum(a.amount_minor) FILTER (WHERE a.reversed_at IS NULL), 0) <= p.amount_minor
      AND coalesce(bool_and(a.allocated_at >= p.recorded_at AND a.allocated_on >= p.paid_on), true)
      AND (p.state = 'RECORDED' OR coalesce(bool_and(a.reversed_at IS NOT NULL AND a.reversed_on <= p.cancelled_on AND a.reversed_at <= p.cancelled_at), true))
    INTO ok FROM cargo_fin.creditor_payment_allocation a WHERE a.tenant_id = p.tenant_id AND a.payment_id = p.payment_id;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_fin: creditor payment %/% is inconsistent at commit (state %)', p.tenant_id, p.payment_id, p.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_payment_consistent';
  END IF;
END $fn$;
CREATE FUNCTION cargo_fin.check_payment_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.creditor_payment_consistent_check(NEW.tenant_id, NEW.payment_id);
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER creditor_payment_consistent AFTER INSERT OR UPDATE ON cargo_fin.creditor_payment
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_payment_at_commit();

-- At commit, an allocation: written by a transaction that writes its payment; its payment consistent; its invoice
-- locked as FIN's invoice commands lock it (a no-op for FIN's own allocations, which lock it before reading it: a
-- transaction past FIN that raced another allocation to the same invoice, or the invoice's cancellation, then waits and
-- the next statement reads what the other committed), of the payment's legal entity, creditor and currency, dated not
-- after the allocation; when written, MATCHED and not awaiting a variance approval (an invoice disputed or not yet
-- checked is held from payment); and the invoice has no more paid than it totals.
CREATE FUNCTION cargo_fin.check_payment_allocation_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.creditor_payment p WHERE p.tenant_id = NEW.tenant_id AND p.payment_id = NEW.payment_id
       AND p.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its parent', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.require_read_committed();
  PERFORM cargo_fin.creditor_payment_consistent_check(NEW.tenant_id, NEW.payment_id);
  PERFORM 1 FROM cargo_fin.creditor_invoice i WHERE i.tenant_id = NEW.tenant_id AND i.invoice_id = NEW.invoice_id FOR NO KEY UPDATE;
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.creditor_payment p JOIN cargo_fin.creditor_invoice i ON i.tenant_id = p.tenant_id AND i.invoice_id = NEW.invoice_id
       WHERE p.tenant_id = NEW.tenant_id AND p.payment_id = NEW.payment_id AND i.legal_entity_id = p.legal_entity_id
         AND i.creditor_party_id = p.creditor_party_id AND i.currency = p.currency AND NEW.allocated_on >= i.invoice_date
         AND (TG_OP = 'UPDATE' OR (i.state = 'MATCHED' AND NOT (i.approval_required AND i.approved_at IS NULL)))) THEN
    RAISE EXCEPTION 'cargo_fin: allocation % of creditor payment %/% is not to a payable invoice of its legal entity, creditor and currency dated by then',
      NEW.allocation_no, NEW.tenant_id, NEW.payment_id USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_payment_consistent';
  END IF;
  IF NOT cargo_fin.paid_within(NEW.tenant_id, NEW.invoice_id) THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% has more paid than it totals', NEW.tenant_id, NEW.invoice_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_invoice_paid_within';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER creditor_payment_allocation_consistent AFTER INSERT OR UPDATE ON cargo_fin.creditor_payment_allocation
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_payment_allocation_at_commit();

-- At commit, a creditor invoice cancelled has nothing paid: an allocation locks the invoice row as the cancellation
-- does, so the two take turns.
CREATE FUNCTION cargo_fin.check_creditor_invoice_unpaid_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.state = 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_fin.creditor_payment_allocation a WHERE a.tenant_id = NEW.tenant_id
       AND a.invoice_id = NEW.invoice_id AND a.reversed_at IS NULL) THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% is cancelled while payments pay it', NEW.tenant_id, NEW.invoice_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_invoice_paid_within';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER creditor_invoice_unpaid_when_cancelled AFTER UPDATE ON cargo_fin.creditor_invoice
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.state = 'CANCELLED') EXECUTE FUNCTION cargo_fin.check_creditor_invoice_unpaid_at_commit();

DO $tables$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['creditor_payment','creditor_payment_allocation'] LOOP
    EXECUTE format('ALTER TABLE cargo_fin.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_fin.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_fin.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
    -- every cargo_fin write runs at READ COMMITTED (fin/000007)
    EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.%I FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed()',
      tbl || '_read_committed', tbl);
  END LOOP;
END $tables$;
