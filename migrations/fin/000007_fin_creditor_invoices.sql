-- IDP-Cargo FIN V0.5 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Creditor invoices (FB4; OD-9 B, OD-10 A): a supplier's invoice to a legal entity is recorded from its document and
-- matched in full to the accruals it costs, job cost sides and consol pool cost lines. A final match closes its
-- accrual and carries the variance; a match not final leaves the rest accrued for a later invoice. A final variance
-- beyond the legal entity's threshold needs approval by another person than the matcher (or one allowed to approve
-- their own). An invoice is disputed and resolved, or cancelled; a cancelled one consumes nothing. A person releases
-- what no invoice will consume of an accrual (a RELEASED version of amount zero). Accruals keep only their estimates
-- and releases: what is matched is the sum of the live matches of invoices not cancelled. A consol cost pool closes
-- once every line is matched final or released, by invoices neither disputed nor awaiting approval, and apportioned at
-- its final cost; it reopens with a reason. An apportionment now records the line cost it divided: what is matched
-- plus what is still accrued; once PHY cancelled the consolidation or no house is left on it, a person may keep a
-- line's cost on the consolidation instead (RETAINED, with a reason), as CargoWise leaves a consol charge on the consol
-- when it is not apportioned. Rows written before stay valid: an apportionment line of FIN V0.4 has no figures of its
-- own and divided its version's. Every write to cargo_fin runs at READ COMMITTED, which its commit checks need.

-- Cost variance thresholds of a legal entity (OD-9 B), versioned on its policy.
CREATE TABLE cargo_fin.cost_variance_threshold (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 threshold_no integer NOT NULL CHECK (threshold_no > 0),
 amount_minor bigint NOT NULL CHECK (amount_minor >= 0 AND amount_minor <= 1000000000000000),
 rate_bp integer NOT NULL CHECK (rate_bp BETWEEN 0 AND 10000),
 set_by text NOT NULL CHECK (length(btrim(set_by)) > 0),
 set_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, threshold_no),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id)
);

-- A cost side or pool cost line version is ACCRUED or, once released, RELEASED with amount zero and no conversion.
ALTER TABLE cargo_fin.charge_side_version DROP CONSTRAINT charge_side_version_status_check,
  ADD CONSTRAINT charge_side_version_status_check CHECK (status IN ('ESTIMATED','INVOICED','ACCRUED','RELEASED')),
  DROP CONSTRAINT charge_side_cost,
  ADD CONSTRAINT charge_side_cost CHECK (side <> 'COST' OR (amount_minor >= 0 AND status IN ('ACCRUED','RELEASED'))),
  ADD CONSTRAINT charge_side_released CHECK (status <> 'RELEASED' OR (version_no > 1 AND reason IS NOT NULL AND amount_minor = 0 AND local_minor = 0
    AND rate IS NULL));
ALTER TABLE cargo_fin.pool_cost_version DROP CONSTRAINT pool_cost_version_status_check,
  ADD CONSTRAINT pool_cost_version_status_check CHECK (status IN ('ACCRUED','RELEASED')),
  ADD CONSTRAINT pool_cost_version_released CHECK (status <> 'RELEASED' OR (version_no > 1 AND amount_minor = 0 AND local_minor = 0 AND rate IS NULL));
ALTER TABLE cargo_fin.consol_cost_pool DROP CONSTRAINT consol_cost_pool_state_check,
  ADD CONSTRAINT consol_cost_pool_state_check CHECK (state IN ('OPEN','CLOSED'));

-- The line cost an apportionment divided (FIN V0.5); NULL on lines of FIN V0.4, which divided their version's figures.
-- RETAINED keeps the line's cost on the consolidation, with a reason and no shares, once no house is left to bear it.
ALTER TABLE cargo_fin.pool_apportionment_line DROP CONSTRAINT pool_apportionment_line_method_check,
  ADD CONSTRAINT pool_apportionment_line_method_check CHECK (method IN ('CHARGEABLE','VOLUME','PIECES','MANUAL','RETAINED')),
  DROP CONSTRAINT pool_apportionment_line_reason,
  ADD CONSTRAINT pool_apportionment_line_reason CHECK ((method IN ('MANUAL','RETAINED')) = (reason IS NOT NULL));
ALTER TABLE cargo_fin.pool_apportionment_line ADD COLUMN amount_minor bigint, ADD COLUMN local_minor bigint,
  ADD CONSTRAINT pool_apportionment_line_cost CHECK ((amount_minor IS NULL) = (local_minor IS NULL) AND amount_minor >= 0 AND local_minor >= 0
    AND amount_minor <= 2000000000000000 AND local_minor <= 2000000000000000);

CREATE TABLE cargo_fin.consol_cost_pool_closure (
 tenant_id text NOT NULL,
 pool_id text NOT NULL,
 closure_no integer NOT NULL CHECK (closure_no > 0),
 closed_by text NOT NULL CHECK (length(btrim(closed_by)) > 0),
 closed_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 reopened_by text CHECK (length(btrim(reopened_by)) > 0),
 reopen_reason text CHECK (length(btrim(reopen_reason)) BETWEEN 1 AND 500),
 reopened_at timestamptz,
 reopen_entered_at timestamptz,
 PRIMARY KEY (tenant_id, pool_id, closure_no),
 FOREIGN KEY (tenant_id, pool_id) REFERENCES cargo_fin.consol_cost_pool (tenant_id, pool_id),
 CONSTRAINT consol_cost_pool_closure_reopened CHECK ((reopened_at IS NULL) = (reopened_by IS NULL) AND (reopened_at IS NULL) = (reopen_reason IS NULL)
   AND (reopened_at IS NULL) = (reopen_entered_at IS NULL) AND (reopened_at IS NULL OR reopened_at >= closed_at))
);

CREATE TABLE cargo_fin.creditor_invoice (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 invoice_id text NOT NULL CHECK (length(btrim(invoice_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL,
 creditor_party_id text NOT NULL CHECK (length(btrim(creditor_party_id)) BETWEEN 1 AND 128),
 creditor_revision bigint NOT NULL CHECK (creditor_revision > 0),
 -- the supplier's own number: no leading or trailing space, so 'ABC ' cannot pass as another invoice than 'ABC'
 supplier_invoice_no text NOT NULL CHECK (supplier_invoice_no ~ '^[A-Za-z0-9]([A-Za-z0-9 ._:/#-]{0,62}[A-Za-z0-9._:/#-])?$'),
 invoice_date date NOT NULL,
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 tax_document_kind text CHECK (tax_document_kind IN ('SPECIAL_FAPIAO','ORDINARY_FAPIAO','FOREIGN_INVOICE')),
 tax_document_number text CHECK (tax_document_number ~ '^[A-Za-z0-9]([A-Za-z0-9 ._:/#-]{0,62}[A-Za-z0-9._:/#-])?$'),
 total_minor bigint NOT NULL CHECK (total_minor > 0 AND total_minor <= 1000000000000000),
 state text NOT NULL CHECK (state IN ('RECORDED','MATCHED','DISPUTED','CANCELLED')),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 matched_by text CHECK (length(btrim(matched_by)) > 0),
 matched_at timestamptz,
 match_entered_at timestamptz,
 approval_required boolean,
 threshold_no integer,
 approved_by text CHECK (length(btrim(approved_by)) > 0),
 approval_reason text CHECK (length(btrim(approval_reason)) BETWEEN 1 AND 500),
 self_approved boolean,
 approved_at timestamptz,
 approval_entered_at timestamptz,
 cancel_reason text CHECK (length(btrim(cancel_reason)) BETWEEN 1 AND 500),
 cancelled_by text CHECK (length(btrim(cancelled_by)) > 0),
 cancelled_at timestamptz,
 cancel_entered_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, invoice_id),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 FOREIGN KEY (tenant_id, legal_entity_id, threshold_no) REFERENCES cargo_fin.cost_variance_threshold (tenant_id, legal_entity_id, threshold_no),
 CONSTRAINT creditor_invoice_tax_document CHECK ((tax_document_kind IS NULL) = (tax_document_number IS NULL)),
 -- the invoice date is not after the day recorded, in any time zone (UTC-12 to UTC+14), and not more than 366 days
 -- before (8820 hours, absolute, as debtor receipts)
 CONSTRAINT creditor_invoice_dated CHECK (recorded_at >= (invoice_date::timestamp AT TIME ZONE 'UTC') - interval '14 hours'
   AND recorded_at < (invoice_date::timestamp AT TIME ZONE 'UTC') + interval '8820 hours'),
 CONSTRAINT creditor_invoice_matched CHECK ((matched_at IS NULL) = (matched_by IS NULL) AND (matched_at IS NULL) = (match_entered_at IS NULL)
   AND (matched_at IS NULL) = (approval_required IS NULL) AND (threshold_no IS NULL OR matched_at IS NOT NULL)
   AND (matched_at IS NULL OR matched_at >= recorded_at)),
 CONSTRAINT creditor_invoice_approved CHECK ((approved_at IS NULL) = (approved_by IS NULL) AND (approved_at IS NULL) = (approval_reason IS NULL)
   AND (approved_at IS NULL) = (self_approved IS NULL) AND (approved_at IS NULL) = (approval_entered_at IS NULL)
   AND (approved_at IS NULL OR (approval_required IS TRUE AND approved_at >= matched_at AND self_approved = (approved_by = matched_by)))),
 CONSTRAINT creditor_invoice_cancelled CHECK ((state = 'CANCELLED') = (cancelled_at IS NOT NULL) AND (cancelled_at IS NULL) = (cancel_reason IS NULL)
   AND (cancelled_at IS NULL) = (cancelled_by IS NULL) AND (cancelled_at IS NULL) = (cancel_entered_at IS NULL)),
 CONSTRAINT creditor_invoice_state CHECK (CASE state WHEN 'RECORDED' THEN matched_at IS NULL WHEN 'MATCHED' THEN matched_at IS NOT NULL ELSE true END)
);
-- One invoice not cancelled per supplier number of a creditor and legal entity (DDD-018); a cancelled invoice frees it.
CREATE UNIQUE INDEX creditor_invoice_number_once ON cargo_fin.creditor_invoice (tenant_id, legal_entity_id, creditor_party_id, supplier_invoice_no)
  WHERE state <> 'CANCELLED';
CREATE INDEX creditor_invoice_by_creditor ON cargo_fin.creditor_invoice (tenant_id, legal_entity_id, creditor_party_id);

CREATE TABLE cargo_fin.creditor_invoice_line (
 tenant_id text NOT NULL,
 invoice_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 charge_code text NOT NULL CHECK (length(btrim(charge_code)) BETWEEN 1 AND 64),
 description text CHECK (length(btrim(description)) BETWEEN 1 AND 500),
 supplier_ref text CHECK (length(btrim(supplier_ref)) BETWEEN 1 AND 500),
 amount_minor bigint NOT NULL CHECK (amount_minor > 0 AND amount_minor <= 1000000000000000),
 PRIMARY KEY (tenant_id, invoice_id, line_no),
 FOREIGN KEY (tenant_id, invoice_id) REFERENCES cargo_fin.creditor_invoice (tenant_id, invoice_id)
);

CREATE TABLE cargo_fin.creditor_invoice_match (
 tenant_id text NOT NULL,
 invoice_id text NOT NULL,
 match_no integer NOT NULL CHECK (match_no > 0),
 line_no integer NOT NULL,
 target_kind text NOT NULL CHECK (target_kind IN ('JOB_COST','POOL_COST')),
 job_id text,
 cost_side text CHECK (cost_side = 'COST'),
 pool_id text,
 target_line integer NOT NULL CHECK (target_line > 0),
 target_version integer NOT NULL CHECK (target_version > 0),
 amount_minor bigint NOT NULL CHECK (amount_minor > 0 AND amount_minor <= 1000000000000000),
 final boolean NOT NULL,
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 local_minor bigint NOT NULL CHECK (abs(local_minor) <= 1000000000000000),
 rate_entry_no integer,
 rate_type text CHECK (rate_type = 'ACCOUNTING'),
 rate bigint CHECK (rate > 0),
 scale integer,
 from_digits integer,
 to_digits integer,
 rate_source text,
 accrual_amount_minor bigint NOT NULL,
 accrual_local_minor bigint NOT NULL,
 prior_amount_minor bigint NOT NULL,
 prior_local_minor bigint NOT NULL,
 variance_minor bigint NOT NULL,
 variance_local_minor bigint NOT NULL,
 PRIMARY KEY (tenant_id, invoice_id, match_no),
 FOREIGN KEY (tenant_id, invoice_id, line_no) REFERENCES cargo_fin.creditor_invoice_line (tenant_id, invoice_id, line_no),
 FOREIGN KEY (tenant_id, job_id, target_line, cost_side, target_version)
   REFERENCES cargo_fin.charge_side_version (tenant_id, job_id, line_no, side, version_no),
 FOREIGN KEY (tenant_id, pool_id, target_line, target_version) REFERENCES cargo_fin.pool_cost_version (tenant_id, pool_id, line_no, version_no),
 CONSTRAINT creditor_match_target CHECK (CASE target_kind WHEN 'JOB_COST' THEN job_id IS NOT NULL AND cost_side IS NOT NULL AND pool_id IS NULL
   ELSE pool_id IS NOT NULL AND job_id IS NULL AND cost_side IS NULL END),
 CONSTRAINT creditor_match_rate CHECK ((rate_entry_no IS NULL) = (rate IS NULL) AND (rate IS NULL) = (rate_type IS NULL) AND (rate IS NULL) = (scale IS NULL)
   AND (rate IS NULL) = (from_digits IS NULL) AND (rate IS NULL) = (to_digits IS NULL) AND (rate IS NULL) = (rate_source IS NULL)),
 CONSTRAINT creditor_match_variance CHECK (final OR (variance_minor = 0 AND variance_local_minor = 0))
);
-- An invoice matches an accrual once.
CREATE UNIQUE INDEX creditor_match_accrual_once ON cargo_fin.creditor_invoice_match
  (tenant_id, invoice_id, target_kind, coalesce(job_id, pool_id), target_line);
CREATE INDEX creditor_match_by_job ON cargo_fin.creditor_invoice_match (tenant_id, job_id, target_line) WHERE job_id IS NOT NULL;
CREATE INDEX creditor_match_by_pool ON cargo_fin.creditor_invoice_match (tenant_id, pool_id, target_line) WHERE pool_id IS NOT NULL;
-- what is matched of an accrual is looked up on every match, version, apportionment and closing
CREATE INDEX creditor_match_by_accrual ON cargo_fin.creditor_invoice_match (tenant_id, target_kind, (coalesce(job_id, pool_id)), target_line);

CREATE TABLE cargo_fin.creditor_invoice_dispute (
 tenant_id text NOT NULL,
 invoice_id text NOT NULL,
 dispute_no integer NOT NULL CHECK (dispute_no > 0),
 from_state text NOT NULL CHECK (from_state IN ('RECORDED','MATCHED')),
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 disputed_by text NOT NULL CHECK (length(btrim(disputed_by)) > 0),
 disputed_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 resolved_by text CHECK (length(btrim(resolved_by)) > 0),
 resolve_reason text CHECK (length(btrim(resolve_reason)) BETWEEN 1 AND 500),
 resolved_at timestamptz,
 resolve_entered_at timestamptz,
 PRIMARY KEY (tenant_id, invoice_id, dispute_no),
 FOREIGN KEY (tenant_id, invoice_id) REFERENCES cargo_fin.creditor_invoice (tenant_id, invoice_id),
 CONSTRAINT creditor_dispute_resolved CHECK ((resolved_at IS NULL) = (resolved_by IS NULL) AND (resolved_at IS NULL) = (resolve_reason IS NULL)
   AND (resolved_at IS NULL) = (resolve_entered_at IS NULL) AND (resolved_at IS NULL OR resolved_at >= disputed_at))
);

ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check
    CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile','DebtorReceipt','ConsolCostPool','CreditorInvoice'));

-- Rows never deleted; lines, matches and thresholds never change; a closure changes only to be reopened, once; a
-- dispute only to be resolved, once.
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['cost_variance_threshold','creditor_invoice_line','creditor_invoice_match'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_fin.%I FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;
CREATE FUNCTION cargo_fin.resolve_once() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := TG_ARGV::text[];
  first text := TG_ARGV[0];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_fin: % rows are never deleted', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
  END IF;
  IF to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable OR to_jsonb(OLD)->>first IS NOT NULL OR to_jsonb(NEW)->>first IS NULL THEN
    RAISE EXCEPTION 'cargo_fin: % changes only %, once', TG_TABLE_NAME, changeable USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER consol_cost_pool_closure_rules BEFORE UPDATE OR DELETE ON cargo_fin.consol_cost_pool_closure
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.resolve_once('reopened_at','reopened_by','reopen_reason','reopen_entered_at');
CREATE TRIGGER creditor_invoice_dispute_rules BEFORE UPDATE OR DELETE ON cargo_fin.creditor_invoice_dispute
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.resolve_once('resolved_at','resolved_by','resolve_reason','resolve_entered_at');

-- A creditor invoice changes its state along RECORDED -> MATCHED, RECORDED or MATCHED -> DISPUTED -> the state it came
-- from, RECORDED or DISPUTED -> CANCELLED; it is matched once, approved once and cancelled once, each with its revision.
CREATE FUNCTION cargo_fin.creditor_invoice_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','matched_by','matched_at','match_entered_at','approval_required','threshold_no','approved_by','approval_reason',
    'self_approved','approved_at','approval_entered_at','cancel_reason','cancelled_by','cancelled_at','cancel_entered_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_fin: % rows are never deleted', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
  END IF;
  IF to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable OR NEW.revision <= OLD.revision
     OR (OLD.matched_at IS NOT NULL AND (NEW.matched_at, NEW.matched_by, NEW.match_entered_at, NEW.approval_required, NEW.threshold_no)
       IS DISTINCT FROM (OLD.matched_at, OLD.matched_by, OLD.match_entered_at, OLD.approval_required, OLD.threshold_no))
     OR (OLD.approved_at IS NOT NULL AND (NEW.approved_at, NEW.approved_by, NEW.approval_reason, NEW.self_approved, NEW.approval_entered_at)
       IS DISTINCT FROM (OLD.approved_at, OLD.approved_by, OLD.approval_reason, OLD.self_approved, OLD.approval_entered_at))
     OR OLD.state = 'CANCELLED'
     OR NOT (OLD.state = NEW.state OR (OLD.state, NEW.state) IN (('RECORDED','MATCHED'),('RECORDED','DISPUTED'),('MATCHED','DISPUTED'),
       ('DISPUTED','RECORDED'),('DISPUTED','MATCHED'),('RECORDED','CANCELLED'),('DISPUTED','CANCELLED')))
     OR (NEW.matched_at IS NOT NULL AND OLD.matched_at IS NULL AND (OLD.state, NEW.state) <> ('RECORDED','MATCHED')) THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% changes along its states only (% to %)', OLD.tenant_id, OLD.invoice_id, OLD.state, NEW.state
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER creditor_invoice_rules BEFORE UPDATE OR DELETE ON cargo_fin.creditor_invoice
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.creditor_invoice_rules();

-- The commit checks that lock a job, pool or policy row and then read what other transactions committed (matches,
-- states) are sound only at READ COMMITTED, where each statement takes a new snapshot. At REPEATABLE READ or
-- SERIALIZABLE the read after the lock would use the transaction's first snapshot, and locking a row another
-- transaction only locked raises no serialization failure, so two transactions could each miss the other's match.
-- FIN runs every command at READ COMMITTED; a write at another level that reaches these checks is refused.
CREATE FUNCTION cargo_fin.require_read_committed() RETURNS void LANGUAGE plpgsql AS $fn$
BEGIN
  IF current_setting('transaction_isolation') <> 'read committed' THEN
    RAISE EXCEPTION 'cargo_fin: FIN''s commit checks run at READ COMMITTED, not %', current_setting('transaction_isolation')
      USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_read_committed';
  END IF;
END $fn$;

-- What the live matches (of invoices not cancelled) consume of an accrual: the amount, the local amount, whether one is
-- final, and whether one comes from an invoice disputed or awaiting a variance approval. Matches of the invoice named
-- by except are left out.
CREATE FUNCTION cargo_fin.accrual_consumed(t text, kind text, id text, line integer, except_invoice text DEFAULT NULL,
    OUT amount bigint, OUT local bigint, OUT final boolean, OUT held boolean) LANGUAGE sql STABLE AS $fn$
  SELECT coalesce(sum(m.amount_minor), 0)::bigint, coalesce(sum(m.local_minor), 0)::bigint, coalesce(bool_or(m.final), false),
         coalesce(bool_or(i.state = 'DISPUTED' OR (i.approval_required AND i.approved_at IS NULL)), false)
  FROM cargo_fin.creditor_invoice_match m JOIN cargo_fin.creditor_invoice i ON i.tenant_id = m.tenant_id AND i.invoice_id = m.invoice_id
  WHERE m.tenant_id = t AND m.target_kind = kind AND coalesce(m.job_id, m.pool_id) = id AND m.target_line = line AND i.state <> 'CANCELLED'
    AND (except_invoice IS NULL OR m.invoice_id <> except_invoice)
$fn$;

-- The latest version of an accrual: its number, status, currency, creditor, amount and local amount.
CREATE FUNCTION cargo_fin.accrual_latest(t text, kind text, id text, line integer,
    OUT version_no integer, OUT status text, OUT currency text, OUT creditor text, OUT amount bigint, OUT local bigint) LANGUAGE sql STABLE AS $fn$
  SELECT v.version_no, v.status, v.currency::text, v.party_id, v.amount_minor, v.local_minor FROM cargo_fin.charge_side_version v
    WHERE kind = 'JOB_COST' AND v.tenant_id = t AND v.job_id = id AND v.line_no = line AND v.side = 'COST'
  UNION ALL
  SELECT v.version_no, v.status, v.currency::text, v.creditor_party_id, v.amount_minor, v.local_minor FROM cargo_fin.pool_cost_version v
    WHERE kind = 'POOL_COST' AND v.tenant_id = t AND v.pool_id = id AND v.line_no = line
  ORDER BY 1 DESC LIMIT 1
$fn$;

-- What an accrual costs now (FIN V0.5): once matched final or released, what is matched; otherwise what is matched plus
-- the open rest of the latest version at its conversion, half away from zero (as Go's proportion).
CREATE FUNCTION cargo_fin.accrual_cost(t text, kind text, id text, line integer, OUT amount bigint, OUT local bigint) LANGUAGE plpgsql STABLE AS $fn$
DECLARE
  v record;
  c record;
  open bigint;
BEGIN
  SELECT * INTO v FROM cargo_fin.accrual_latest(t, kind, id, line);
  SELECT * INTO c FROM cargo_fin.accrual_consumed(t, kind, id, line);
  IF v.status = 'RELEASED' OR c.final THEN
    amount := c.amount;
    local := c.local;
    RETURN;
  END IF;
  open := greatest(v.amount - c.amount, 0);
  amount := c.amount + open;
  -- local × open / amount, half up (all non-negative), in one integer division: dividing in numeric first would round
  -- twice for large amounts
  local := c.local + CASE WHEN v.amount = 0 OR open = 0 THEN 0 ELSE div(2 * v.local::numeric * open + v.amount, 2::numeric * v.amount)::bigint END;
END $fn$;

-- The checks of an invoice at commit: lines numbered from 1 without gaps totalling the invoice; matches numbered from
-- 1 without gaps, present exactly when the invoice was matched, then matching every line in full; disputes numbered
-- from 1 without gaps, every one resolved but the last, which is open exactly when the invoice is DISPUTED; each match
-- converted at the ACCOUNTING entry of the policy in force when matched (none in the functional currency); and the
-- approval required exactly when a final variance in the functional currency is beyond the threshold the invoice was
-- matched under (the greater of its amount and its rate of the accrual's local amount), or non-zero without one. The
-- threshold is the invoice's by number, so later thresholds never change what it required; that it was the one in
-- force when matched is checked when its matches are written.
CREATE FUNCTION cargo_fin.creditor_invoice_consistent_check(t text, id text) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  i cargo_fin.creditor_invoice;
  fc text;
  th cargo_fin.cost_variance_threshold;
  ok boolean;
BEGIN
  SELECT * INTO i FROM cargo_fin.creditor_invoice WHERE tenant_id = t AND invoice_id = id;
  SELECT p.functional_currency INTO fc FROM cargo_fin.finance_policy p WHERE p.tenant_id = i.tenant_id AND p.legal_entity_id = i.legal_entity_id;
  IF i.threshold_no IS NOT NULL THEN
    SELECT * INTO th FROM cargo_fin.cost_variance_threshold x WHERE x.tenant_id = i.tenant_id AND x.legal_entity_id = i.legal_entity_id
      AND x.threshold_no = i.threshold_no;
  END IF;
  SELECT (SELECT count(*) = coalesce(max(l.line_no), 0) AND count(*) >= 1 AND coalesce(sum(l.amount_minor), 0) = i.total_minor
          FROM cargo_fin.creditor_invoice_line l WHERE l.tenant_id = i.tenant_id AND l.invoice_id = i.invoice_id)
     AND (SELECT count(*) = coalesce(max(m.match_no), 0) AND (count(*) > 0) = (i.matched_at IS NOT NULL)
          FROM cargo_fin.creditor_invoice_match m WHERE m.tenant_id = i.tenant_id AND m.invoice_id = i.invoice_id)
     AND (i.matched_at IS NULL OR NOT EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice_line l WHERE l.tenant_id = i.tenant_id
          AND l.invoice_id = i.invoice_id AND l.amount_minor <> (SELECT coalesce(sum(m.amount_minor), 0) FROM cargo_fin.creditor_invoice_match m
            WHERE m.tenant_id = l.tenant_id AND m.invoice_id = l.invoice_id AND m.line_no = l.line_no)))
     AND (SELECT count(*) = coalesce(max(d.dispute_no), 0)
            AND coalesce(bool_and(d.resolved_at IS NOT NULL OR d.dispute_no = (SELECT max(x.dispute_no) FROM cargo_fin.creditor_invoice_dispute x
              WHERE x.tenant_id = d.tenant_id AND x.invoice_id = d.invoice_id)), true)
            AND (i.state = 'CANCELLED' OR (i.state = 'DISPUTED') = coalesce(bool_or(d.resolved_at IS NULL), false))
          FROM cargo_fin.creditor_invoice_dispute d WHERE d.tenant_id = i.tenant_id AND d.invoice_id = i.invoice_id)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice_match m WHERE m.tenant_id = i.tenant_id AND m.invoice_id = i.invoice_id
       AND CASE WHEN i.currency = fc THEN m.rate IS NOT NULL OR m.local_minor <> m.amount_minor
         ELSE NOT EXISTS (SELECT 1 FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = i.tenant_id AND e.legal_entity_id = i.legal_entity_id
           AND e.entry_no = m.rate_entry_no AND e.rate_type = 'ACCOUNTING' AND m.rate_type = 'ACCOUNTING' AND e.from_currency = i.currency
           AND e.rate = m.rate AND e.scale = m.scale AND e.from_digits = m.from_digits AND e.to_digits = m.to_digits AND e.source = m.rate_source
           AND e.valid_from <= i.matched_at AND i.matched_at < e.valid_until
           AND m.local_minor = round(m.amount_minor::numeric * e.rate * power(10::numeric, e.to_digits - e.from_digits - e.scale))) END)
     AND (i.matched_at IS NULL OR (i.approval_required = EXISTS (
       SELECT 1 FROM cargo_fin.creditor_invoice_match m WHERE m.tenant_id = i.tenant_id AND m.invoice_id = i.invoice_id AND m.final
         AND m.variance_local_minor <> 0 AND (th.threshold_no IS NULL
           OR abs(m.variance_local_minor) > greatest(th.amount_minor, div(abs(m.accrual_local_minor)::numeric * th.rate_bp, 10000))))))
    INTO ok;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% is inconsistent at commit (state %)', i.tenant_id, i.invoice_id, i.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_invoice_consistent';
  END IF;
END $fn$;

-- The accruals an invoice matched belong to jobs and pools that are open: checked when the invoice is disputed or
-- cancelled, after locking them (pools in id order, then jobs in id order, as FIN's commands lock them), so that a
-- pool closing at the same time waits and then sees the other's commit. An invoice is not cancelled while another
-- invoice holds a final match of an accrual it matched in part: that final match's variance counted this one.
CREATE FUNCTION cargo_fin.creditor_targets_open_check(t text, id text, cancelling boolean) RETURNS void LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.require_read_committed();
  PERFORM 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = t AND p.pool_id IN (SELECT m.pool_id FROM cargo_fin.creditor_invoice_match m
    WHERE m.tenant_id = t AND m.invoice_id = id) ORDER BY p.pool_id FOR NO KEY UPDATE;
  PERFORM 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = t AND j.job_id IN (SELECT m.job_id FROM cargo_fin.creditor_invoice_match m
    WHERE m.tenant_id = t AND m.invoice_id = id) ORDER BY j.job_id FOR NO KEY UPDATE;
  IF EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice_match m LEFT JOIN cargo_fin.consol_cost_pool p ON p.tenant_id = m.tenant_id AND p.pool_id = m.pool_id
       LEFT JOIN cargo_fin.freight_job j ON j.tenant_id = m.tenant_id AND j.job_id = m.job_id
       WHERE m.tenant_id = t AND m.invoice_id = id AND coalesce(p.state, j.state) <> 'OPEN') THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% matched an accrual of a pool or job that is not open', t, id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_invoice_targets_open';
  END IF;
  IF cancelling AND EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice_match m WHERE m.tenant_id = t AND m.invoice_id = id AND NOT m.final
       AND EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice_match f JOIN cargo_fin.creditor_invoice fi ON fi.tenant_id = f.tenant_id
           AND fi.invoice_id = f.invoice_id
         WHERE f.tenant_id = m.tenant_id AND f.target_kind = m.target_kind AND coalesce(f.job_id, f.pool_id) = coalesce(m.job_id, m.pool_id)
           AND f.target_line = m.target_line AND f.final AND f.invoice_id <> m.invoice_id AND fi.state <> 'CANCELLED')) THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% is cancelled behind another invoice''s final match of an accrual it matched', t, id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_cancel_behind_final';
  END IF;
END $fn$;

CREATE FUNCTION cargo_fin.check_creditor_invoice_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.creditor_invoice_consistent_check(NEW.tenant_id, NEW.invoice_id);
  IF TG_OP = 'UPDATE' AND NEW.state IN ('DISPUTED','CANCELLED') AND NEW.state IS DISTINCT FROM OLD.state THEN
    PERFORM cargo_fin.creditor_targets_open_check(NEW.tenant_id, NEW.invoice_id, NEW.state = 'CANCELLED');
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER creditor_invoice_consistent AFTER INSERT OR UPDATE ON cargo_fin.creditor_invoice
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_creditor_invoice_at_commit();

-- At commit, an invoice's line, dispute or match is written with its invoice (so concurrent writers took turns on the
-- invoice row) and the invoice is consistent.
CREATE FUNCTION cargo_fin.check_creditor_record_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice i WHERE i.tenant_id = NEW.tenant_id AND i.invoice_id = NEW.invoice_id
       AND i.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its invoice', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.creditor_invoice_consistent_check(NEW.tenant_id, NEW.invoice_id);
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER creditor_invoice_line_with_invoice AFTER INSERT ON cargo_fin.creditor_invoice_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_creditor_record_at_commit();
CREATE CONSTRAINT TRIGGER creditor_invoice_dispute_with_invoice AFTER INSERT OR UPDATE ON cargo_fin.creditor_invoice_dispute
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_creditor_record_at_commit();

-- At commit, a match: written with its invoice, which is consistent; its accrual's job or pool locked as FIN's
-- commands lock it (a no-op for them: a write past FIN racing another match to the same accrual waits, and the next
-- statement reads what the other committed), then open and of the invoice's legal entity; the accrual's latest version
-- is the one the match consumed, not released, in the invoice's currency, naming no creditor or the invoice's, of the
-- line's charge code or with a reason; it records the accrual's figures and what other invoices' live matches
-- consume; no other invoice holds a live final match of the accrual; a match not final keeps everything consumed within
-- the accrual, and a final one carries the variance of all live matches against it. With the policy then held FOR
-- SHARE (as FIN's matching holds it, so thresholds and exchange rate entries changing at the same time take turns), the
-- invoice names the threshold in force when it was matched, or none when none was.
CREATE FUNCTION cargo_fin.check_creditor_match_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  i cargo_fin.creditor_invoice;
  target text := coalesce(NEW.job_id, NEW.pool_id);
  owner_open boolean;
  owner_entity text;
  code text;
  v record;
  c record;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice x WHERE x.tenant_id = NEW.tenant_id AND x.invoice_id = NEW.invoice_id
       AND x.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its invoice', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.require_read_committed();
  PERFORM cargo_fin.creditor_invoice_consistent_check(NEW.tenant_id, NEW.invoice_id);
  SELECT * INTO i FROM cargo_fin.creditor_invoice WHERE tenant_id = NEW.tenant_id AND invoice_id = NEW.invoice_id;
  IF NEW.target_kind = 'JOB_COST' THEN
    PERFORM 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id FOR NO KEY UPDATE;
    SELECT j.state = 'OPEN', j.legal_entity_id INTO owner_open, owner_entity FROM cargo_fin.freight_job j
      WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id;
    SELECT l.charge_code INTO code FROM cargo_fin.charge_line l WHERE l.tenant_id = NEW.tenant_id AND l.job_id = NEW.job_id AND l.line_no = NEW.target_line;
  ELSE
    PERFORM 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id FOR NO KEY UPDATE;
    SELECT p.state = 'OPEN', p.legal_entity_id INTO owner_open, owner_entity FROM cargo_fin.consol_cost_pool p
      WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id;
    SELECT l.charge_code INTO code FROM cargo_fin.pool_cost_line l WHERE l.tenant_id = NEW.tenant_id AND l.pool_id = NEW.pool_id
      AND l.line_no = NEW.target_line;
  END IF;
  PERFORM 1 FROM cargo_fin.finance_policy p WHERE p.tenant_id = NEW.tenant_id AND p.legal_entity_id = i.legal_entity_id FOR SHARE;
  IF i.threshold_no IS DISTINCT FROM (SELECT x.threshold_no FROM cargo_fin.cost_variance_threshold x WHERE x.tenant_id = i.tenant_id
       AND x.legal_entity_id = i.legal_entity_id AND x.set_at <= i.matched_at ORDER BY x.threshold_no DESC LIMIT 1) THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% names another threshold than the one in force when it was matched', NEW.tenant_id, NEW.invoice_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_invoice_consistent';
  END IF;
  SELECT * INTO v FROM cargo_fin.accrual_latest(NEW.tenant_id, NEW.target_kind, target, NEW.target_line);
  SELECT * INTO c FROM cargo_fin.accrual_consumed(NEW.tenant_id, NEW.target_kind, target, NEW.target_line, NEW.invoice_id);
  IF i.state = 'CANCELLED' OR owner_open IS DISTINCT FROM true OR owner_entity <> i.legal_entity_id OR v.version_no <> NEW.target_version
     OR v.status <> 'ACCRUED' OR v.currency <> i.currency OR (v.creditor IS NOT NULL AND v.creditor <> i.creditor_party_id)
     OR (code <> (SELECT l.charge_code FROM cargo_fin.creditor_invoice_line l WHERE l.tenant_id = NEW.tenant_id AND l.invoice_id = NEW.invoice_id
       AND l.line_no = NEW.line_no) AND NEW.reason IS NULL)
     OR NEW.accrual_amount_minor <> v.amount OR NEW.accrual_local_minor <> v.local
     OR NEW.prior_amount_minor <> c.amount OR NEW.prior_local_minor <> c.local OR c.final
     OR (CASE WHEN NEW.final THEN NEW.variance_minor <> c.amount + NEW.amount_minor - v.amount
         OR NEW.variance_local_minor <> c.local + NEW.local_minor - v.local
       ELSE c.amount + NEW.amount_minor > v.amount END) THEN
    RAISE EXCEPTION 'cargo_fin: match % of creditor invoice %/% does not consume % % line % as it stands', NEW.match_no, NEW.tenant_id, NEW.invoice_id,
      NEW.target_kind, target, NEW.target_line USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_match_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER creditor_invoice_match_consistent AFTER INSERT ON cargo_fin.creditor_invoice_match
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_creditor_match_at_commit();

-- What creditor invoices consume of an accrual still stands against its versions: with a live final match, the version
-- it consumed is the latest (nothing is revised or released after it); otherwise a released accrual takes nothing more,
-- and an accrued one covers what live matches consume; once matched, it stays in the currency of the matching invoices
-- and names none of other creditors. A RELEASED version follows an ACCRUED one of the same creditor and currency.
CREATE FUNCTION cargo_fin.accrual_versions_check(t text, kind text, id text, line integer) RETURNS boolean LANGUAGE plpgsql STABLE AS $fn$
DECLARE
  v record;
  c record;
  final_version integer;
  ok boolean;
BEGIN
  SELECT * INTO v FROM cargo_fin.accrual_latest(t, kind, id, line);
  IF v.version_no IS NULL THEN
    RETURN true;
  END IF;
  SELECT * INTO c FROM cargo_fin.accrual_consumed(t, kind, id, line);
  SELECT max(m.target_version) INTO final_version FROM cargo_fin.creditor_invoice_match m JOIN cargo_fin.creditor_invoice i
    ON i.tenant_id = m.tenant_id AND i.invoice_id = m.invoice_id
    WHERE m.tenant_id = t AND m.target_kind = kind AND coalesce(m.job_id, m.pool_id) = id AND m.target_line = line AND m.final AND i.state <> 'CANCELLED';
  IF c.final THEN
    RETURN final_version = v.version_no;
  END IF;
  IF (v.status = 'ACCRUED' AND c.amount > v.amount) OR (c.amount > 0 AND EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice_match m
       JOIN cargo_fin.creditor_invoice i ON i.tenant_id = m.tenant_id AND i.invoice_id = m.invoice_id
       WHERE m.tenant_id = t AND m.target_kind = kind AND coalesce(m.job_id, m.pool_id) = id AND m.target_line = line AND i.state <> 'CANCELLED'
         AND (i.currency <> v.currency OR i.creditor_party_id <> coalesce(v.creditor, i.creditor_party_id)))) THEN
    RETURN false;
  END IF;
  IF kind = 'JOB_COST' THEN
    SELECT bool_and(r.status <> 'RELEASED' OR (p.status = 'ACCRUED' AND p.currency = r.currency AND p.party_id IS NOT DISTINCT FROM r.party_id
        AND p.party_revision = r.party_revision AND r.version_no = (SELECT max(x.version_no) FROM cargo_fin.charge_side_version x
          WHERE x.tenant_id = r.tenant_id AND x.job_id = r.job_id AND x.line_no = r.line_no AND x.side = 'COST')))
      INTO ok FROM cargo_fin.charge_side_version r LEFT JOIN cargo_fin.charge_side_version p ON p.tenant_id = r.tenant_id AND p.job_id = r.job_id
        AND p.line_no = r.line_no AND p.side = r.side AND p.version_no = r.version_no - 1
      WHERE r.tenant_id = t AND r.job_id = id AND r.line_no = line AND r.side = 'COST';
  ELSE
    SELECT bool_and(r.status <> 'RELEASED' OR (p.status = 'ACCRUED' AND p.currency = r.currency
        AND p.creditor_party_id IS NOT DISTINCT FROM r.creditor_party_id AND p.creditor_revision = r.creditor_revision
        AND r.version_no = (SELECT max(x.version_no) FROM cargo_fin.pool_cost_version x
          WHERE x.tenant_id = r.tenant_id AND x.pool_id = r.pool_id AND x.line_no = r.line_no)))
      INTO ok FROM cargo_fin.pool_cost_version r LEFT JOIN cargo_fin.pool_cost_version p ON p.tenant_id = r.tenant_id AND p.pool_id = r.pool_id
        AND p.line_no = r.line_no AND p.version_no = r.version_no - 1
      WHERE r.tenant_id = t AND r.pool_id = id AND r.line_no = line;
  END IF;
  RETURN coalesce(ok, true);
END $fn$;

-- At commit, a cost side version (written with its job, which FIN's job checks see to) belongs to an OPEN job and
-- stands against what creditor invoices consume of it; a CANCELLED job has no cost side matched or released.
CREATE FUNCTION cargo_fin.check_job_accrual_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.require_read_committed();
  IF TG_TABLE_NAME = 'charge_side_version' THEN
    IF NOT EXISTS (SELECT 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id AND j.state = 'OPEN')
       OR NOT cargo_fin.accrual_versions_check(NEW.tenant_id, 'JOB_COST', NEW.job_id, NEW.line_no) THEN
      RAISE EXCEPTION 'cargo_fin: the cost side of line % of freight job %/% is written on a job not open or does not stand against its creditor invoices',
        NEW.line_no, NEW.tenant_id, NEW.job_id USING ERRCODE = 'check_violation', CONSTRAINT = 'accrual_consumption_consistent';
    END IF;
  ELSIF NEW.state = 'CANCELLED' AND (EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v WHERE v.tenant_id = NEW.tenant_id AND v.job_id = NEW.job_id
       AND v.status = 'RELEASED') OR EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice_match m JOIN cargo_fin.creditor_invoice i
         ON i.tenant_id = m.tenant_id AND i.invoice_id = m.invoice_id WHERE m.tenant_id = NEW.tenant_id AND m.job_id = NEW.job_id
         AND i.state <> 'CANCELLED')) THEN
    RAISE EXCEPTION 'cargo_fin: freight job %/% is cancelled with cost sides matched or released', NEW.tenant_id, NEW.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'accrual_consumption_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER charge_side_accrual_consistent AFTER INSERT ON cargo_fin.charge_side_version
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.side = 'COST') EXECUTE FUNCTION cargo_fin.check_job_accrual_at_commit();
CREATE CONSTRAINT TRIGGER freight_job_accruals_consistent AFTER UPDATE ON cargo_fin.freight_job
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_job_accrual_at_commit();

-- The conservation rule, as fin/000006, against the line cost the apportionment divided (its version's figures for an
-- apportionment line of FIN V0.4); a RETAINED line keeps its cost on the consolidation and has no share. That no house
-- was left when it was retained is checked by FIN with PHY's answer: PHY's facts are not in this schema.
CREATE OR REPLACE FUNCTION cargo_fin.pool_line_shares_check(t text, pool text, ano integer, line integer) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  al cargo_fin.pool_apportionment_line;
  v cargo_fin.pool_cost_version;
  ok boolean;
BEGIN
  SELECT * INTO al FROM cargo_fin.pool_apportionment_line WHERE tenant_id = t AND pool_id = pool AND apportionment_no = ano AND line_no = line;
  SELECT * INTO v FROM cargo_fin.pool_cost_version
    WHERE tenant_id = t AND pool_id = pool AND line_no = al.line_no AND version_no = al.line_version;
  SELECT CASE WHEN al.method = 'RETAINED' THEN count(*) = 0 AND al.amount_minor IS NOT NULL
      ELSE count(*) = coalesce(max(s.share_no), 0) AND count(*) >= 1
        AND coalesce(sum(s.amount_minor), -1) = coalesce(al.amount_minor, v.amount_minor)
        AND coalesce(sum(s.local_minor), -1) = coalesce(al.local_minor, v.local_minor) END
    INTO ok FROM cargo_fin.pool_apportionment_share s
    WHERE s.tenant_id = t AND s.pool_id = pool AND s.apportionment_no = ano AND s.line_no = line;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: shares of pool %/% apportionment % line % do not conserve its amount', t, pool, ano, line
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_apportionment_conserves';
  END IF;
END $fn$;

-- A closed pool takes no record: every line, version, apportionment and share is written while its pool is OPEN. With
-- the pool row written in the same transaction (fin_records_with_parent), a closing at the same time takes turns.
CREATE FUNCTION cargo_fin.pool_open_check(t text, id text) RETURNS void LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = t AND p.pool_id = id AND p.state = 'OPEN') THEN
    RAISE EXCEPTION 'cargo_fin: consol cost pool %/% is closed; reopen it first', t, id USING ERRCODE = 'check_violation', CONSTRAINT = 'consol_cost_pool_open';
  END IF;
END $fn$;

-- As fin/000006, and the pool OPEN.
CREATE OR REPLACE FUNCTION cargo_fin.check_pool_record_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id
       AND p.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its pool', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.pool_open_check(NEW.tenant_id, NEW.pool_id);
  PERFORM cargo_fin.consol_cost_pool_consistent_check(NEW.tenant_id, NEW.pool_id);
  RETURN NULL;
END $fn$;

-- As fin/000006 (a share written with its pool, conserving its line, its job locked as FIN's job commands lock it and
-- then an OPEN job of the pool's legal entity for the share's shipment), at READ COMMITTED and with the pool OPEN.
CREATE OR REPLACE FUNCTION cargo_fin.check_share_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id
       AND p.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its pool', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.require_read_committed();
  PERFORM cargo_fin.pool_open_check(NEW.tenant_id, NEW.pool_id);
  PERFORM cargo_fin.pool_line_shares_check(NEW.tenant_id, NEW.pool_id, NEW.apportionment_no, NEW.line_no);
  PERFORM 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id FOR NO KEY UPDATE;
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.freight_job j JOIN cargo_fin.consol_cost_pool p
       ON p.tenant_id = j.tenant_id AND p.pool_id = NEW.pool_id
       WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id AND j.state = 'OPEN'
         AND j.legal_entity_id = p.legal_entity_id AND j.shipment_id = NEW.shipment_id) THEN
    RAISE EXCEPTION 'cargo_fin: share % of pool %/% apportionment % needs an OPEN job of the pool''s legal entity for shipment %',
      NEW.share_no, NEW.tenant_id, NEW.pool_id, NEW.apportionment_no, NEW.shipment_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_share_job_open';
  END IF;
  RETURN NULL;
END $fn$;

-- At commit, an apportionment line (as fin/000006: written with its pool and conserving its line) of FIN V0.5 divides
-- the latest version of its line at the line's cost as it stands, while the pool is OPEN.
CREATE OR REPLACE FUNCTION cargo_fin.check_apportioned_line_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  cost record;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id
       AND p.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its pool', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.require_read_committed();
  PERFORM cargo_fin.pool_open_check(NEW.tenant_id, NEW.pool_id);
  PERFORM cargo_fin.pool_line_shares_check(NEW.tenant_id, NEW.pool_id, NEW.apportionment_no, NEW.line_no);
  SELECT * INTO cost FROM cargo_fin.accrual_cost(NEW.tenant_id, 'POOL_COST', NEW.pool_id, NEW.line_no);
  IF NEW.amount_minor IS DISTINCT FROM cost.amount OR NEW.local_minor IS DISTINCT FROM cost.local
     OR NEW.line_version <> (SELECT v.version_no FROM cargo_fin.accrual_latest(NEW.tenant_id, 'POOL_COST', NEW.pool_id, NEW.line_no) v) THEN
    RAISE EXCEPTION 'cargo_fin: apportionment % of pool %/% divides line % at another cost than it stands at', NEW.apportionment_no, NEW.tenant_id,
      NEW.pool_id, NEW.line_no USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_apportionment_cost';
  END IF;
  RETURN NULL;
END $fn$;

-- At commit, a pool cost version stands against what creditor invoices consume of its line (as for job cost sides).
CREATE FUNCTION cargo_fin.check_pool_accrual_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.require_read_committed();
  IF NOT cargo_fin.accrual_versions_check(NEW.tenant_id, 'POOL_COST', NEW.pool_id, NEW.line_no) THEN
    RAISE EXCEPTION 'cargo_fin: cost line % of pool %/% does not stand against its creditor invoices', NEW.line_no, NEW.tenant_id, NEW.pool_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'accrual_consumption_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER pool_cost_version_accrual_consistent AFTER INSERT ON cargo_fin.pool_cost_version
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_pool_accrual_at_commit();

-- At commit, a pool's closures are numbered from 1 without gaps, each reopened but the last, which is open exactly when
-- the pool is CLOSED; a CLOSED pool's every line is matched final or released, by invoices neither disputed nor
-- awaiting approval, and its latest apportionment (if any) divided the line's final cost, on the ACTUAL basis unless
-- that cost is zero (a line of zero cost needs none). Whether the apportionment names the houses and quantities PHY
-- reports now is checked by FIN before it closes the pool: PHY's facts are not in this schema.
CREATE FUNCTION cargo_fin.consol_cost_pool_closure_check(t text, id text) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  p cargo_fin.consol_cost_pool;
  ok boolean;
BEGIN
  SELECT * INTO p FROM cargo_fin.consol_cost_pool WHERE tenant_id = t AND pool_id = id;
  IF p.state = 'CLOSED' THEN
    PERFORM cargo_fin.require_read_committed();
  END IF;
  SELECT count(*) = coalesce(max(c.closure_no), 0)
      AND coalesce(bool_and(c.reopened_at IS NOT NULL OR c.closure_no = (SELECT max(x.closure_no) FROM cargo_fin.consol_cost_pool_closure x
        WHERE x.tenant_id = c.tenant_id AND x.pool_id = c.pool_id)), true)
      AND (p.state = 'CLOSED') = coalesce(bool_or(c.reopened_at IS NULL), false)
    INTO ok FROM cargo_fin.consol_cost_pool_closure c WHERE c.tenant_id = p.tenant_id AND c.pool_id = p.pool_id;
  IF ok AND p.state = 'CLOSED' THEN
    SELECT NOT EXISTS (SELECT 1 FROM cargo_fin.pool_cost_line l
        CROSS JOIN LATERAL cargo_fin.accrual_latest(l.tenant_id, 'POOL_COST', l.pool_id, l.line_no) v
        CROSS JOIN LATERAL cargo_fin.accrual_consumed(l.tenant_id, 'POOL_COST', l.pool_id, l.line_no) c
        LEFT JOIN LATERAL (SELECT al.*, a.stage FROM cargo_fin.pool_apportionment_line al JOIN cargo_fin.pool_apportionment a
            ON a.tenant_id = al.tenant_id AND a.pool_id = al.pool_id AND a.apportionment_no = al.apportionment_no
            JOIN cargo_fin.pool_cost_version av ON av.tenant_id = al.tenant_id AND av.pool_id = al.pool_id AND av.line_no = al.line_no
              AND av.version_no = al.line_version
            WHERE al.tenant_id = l.tenant_id AND al.pool_id = l.pool_id AND al.line_no = l.line_no
            ORDER BY al.apportionment_no DESC LIMIT 1) la ON true
        WHERE l.tenant_id = p.tenant_id AND l.pool_id = p.pool_id
          AND ((v.status <> 'RELEASED' AND NOT c.final) OR c.held
            OR CASE WHEN la.apportionment_no IS NULL THEN c.amount <> 0 OR c.local <> 0
              ELSE coalesce(la.amount_minor, (SELECT av.amount_minor FROM cargo_fin.pool_cost_version av WHERE av.tenant_id = la.tenant_id
                  AND av.pool_id = la.pool_id AND av.line_no = la.line_no AND av.version_no = la.line_version)) <> c.amount
                OR coalesce(la.local_minor, (SELECT av.local_minor FROM cargo_fin.pool_cost_version av WHERE av.tenant_id = la.tenant_id
                  AND av.pool_id = la.pool_id AND av.line_no = la.line_no AND av.version_no = la.line_version)) <> c.local
                OR ((c.amount <> 0 OR c.local <> 0) AND la.stage <> 'ACTUAL') END))
      INTO ok;
  END IF;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: consol cost pool %/% is % without its costs final', p.tenant_id, p.pool_id, p.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consol_cost_pool_closable';
  END IF;
END $fn$;
CREATE FUNCTION cargo_fin.check_pool_closure_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_TABLE_NAME = 'consol_cost_pool_closure' AND NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id
       AND p.pool_id = NEW.pool_id AND p.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its pool', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.consol_cost_pool_closure_check(NEW.tenant_id, NEW.pool_id);
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER consol_cost_pool_closable AFTER INSERT OR UPDATE ON cargo_fin.consol_cost_pool
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_pool_closure_at_commit();
CREATE CONSTRAINT TRIGGER consol_cost_pool_closure_with_pool AFTER INSERT OR UPDATE ON cargo_fin.consol_cost_pool_closure
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_pool_closure_at_commit();

-- At commit, thresholds are written with their policy, numbered from 1 without gaps and set in order, and not set at or
-- before the time any invoice of the legal entity was matched: an invoice keeps the threshold in force when it was
-- matched, and history does not change behind it. The policy row is written in the same transaction, and matching holds
-- it FOR SHARE, so the two take turns.
CREATE OR REPLACE FUNCTION cargo_fin.check_written_with_parent() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  found boolean;
BEGIN
  CASE TG_TABLE_NAME
    WHEN 'exchange_rate_entry', 'charge_tax_treatment', 'invoice_series', 'cost_variance_threshold' THEN
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
  IF TG_TABLE_NAME = 'cost_variance_threshold' THEN -- nested: NEW of the other tables has no threshold_no
    PERFORM cargo_fin.require_read_committed();
    IF NEW.threshold_no <> 1 + (SELECT count(*) FROM cargo_fin.cost_variance_threshold x
         WHERE x.tenant_id = NEW.tenant_id AND x.legal_entity_id = NEW.legal_entity_id AND x.threshold_no < NEW.threshold_no)
       OR EXISTS (SELECT 1 FROM cargo_fin.cost_variance_threshold x WHERE x.tenant_id = NEW.tenant_id AND x.legal_entity_id = NEW.legal_entity_id
         AND x.threshold_no < NEW.threshold_no AND x.set_at > NEW.set_at)
       OR EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice i WHERE i.tenant_id = NEW.tenant_id AND i.legal_entity_id = NEW.legal_entity_id
         AND i.matched_at >= NEW.set_at) THEN
      RAISE EXCEPTION 'cargo_fin: variance threshold %/%/% is not the next in number and time', NEW.tenant_id, NEW.legal_entity_id, NEW.threshold_no
        USING ERRCODE = 'check_violation', CONSTRAINT = 'finance_policy_consistent';
    END IF;
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER cost_variance_threshold_with_parent AFTER INSERT ON cargo_fin.cost_variance_threshold DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_written_with_parent();

-- At commit, an entry ended earlier was used by no charge, debit note, pool cost or creditor match at or after its new
-- end (pool costs were not checked before: fin/000006 left them out).
CREATE OR REPLACE FUNCTION cargo_fin.check_rate_end_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v JOIN cargo_fin.freight_job j ON j.tenant_id = v.tenant_id AND j.job_id = v.job_id
       WHERE v.tenant_id = NEW.tenant_id AND j.legal_entity_id = NEW.legal_entity_id AND v.rate_entry_no = NEW.entry_no
         AND coalesce(v.converted_at, v.recorded_at) >= NEW.valid_until)
     OR EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice_line l JOIN cargo_fin.debtor_invoice n ON n.tenant_id = l.tenant_id AND n.invoice_id = l.invoice_id
       WHERE n.tenant_id = NEW.tenant_id AND n.legal_entity_id = NEW.legal_entity_id AND l.rate_entry_no = NEW.entry_no AND l.side_version IS NOT NULL
         AND n.issued_at >= NEW.valid_until)
     OR EXISTS (SELECT 1 FROM cargo_fin.pool_cost_version v JOIN cargo_fin.consol_cost_pool p ON p.tenant_id = v.tenant_id AND p.pool_id = v.pool_id
       WHERE v.tenant_id = NEW.tenant_id AND p.legal_entity_id = NEW.legal_entity_id AND v.rate_entry_no = NEW.entry_no AND v.recorded_at >= NEW.valid_until)
     OR EXISTS (SELECT 1 FROM cargo_fin.creditor_invoice_match m JOIN cargo_fin.creditor_invoice i ON i.tenant_id = m.tenant_id AND i.invoice_id = m.invoice_id
       WHERE i.tenant_id = NEW.tenant_id AND i.legal_entity_id = NEW.legal_entity_id AND m.rate_entry_no = NEW.entry_no
         AND i.matched_at >= NEW.valid_until) THEN
    RAISE EXCEPTION 'cargo_fin: exchange rate entry %/%/% converted a charge at or after its new end', NEW.tenant_id, NEW.legal_entity_id, NEW.entry_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'exchange_rate_entry_in_use';
  END IF;
  RETURN NULL;
END $fn$;

-- Every write to cargo_fin runs at READ COMMITTED: FIN's commit checks lock the rows other transactions write (jobs,
-- pools, receipts, the policy) and then read what those committed, which only READ COMMITTED's per-statement snapshot
-- sees. This covers the checks of earlier versions too (settlements, pool shares, tax treatments, rate ends). A
-- statement-level trigger on every table refuses an INSERT, UPDATE or DELETE at another level before it runs; a table
-- added later gets one in its own migration.
CREATE FUNCTION cargo_fin.writes_read_committed() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.require_read_committed();
  RETURN NULL;
END $fn$;
DO $rc$
DECLARE tbl text;
BEGIN
  FOR tbl IN SELECT c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
      WHERE n.nspname = 'cargo_fin' AND c.relkind IN ('r','p') ORDER BY c.relname LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.%I FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed()',
      tbl || '_read_committed', tbl);
  END LOOP;
END $rc$;

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['cost_variance_threshold','consol_cost_pool_closure','creditor_invoice','creditor_invoice_line','creditor_invoice_match',
      'creditor_invoice_dispute'] LOOP
    EXECUTE format('ALTER TABLE cargo_fin.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_fin.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_fin.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
