-- IDP-Cargo FIN V0.1 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Freight job economics, first part (FIN candidate 26: FB1 in part, FB2 without rating, FB8; decisions N1-FIN OD-1 A,
-- OD-5 B, OD-7 A, OD-8 A): each legal entity's finance policy (functional currency and book, fixed once; exchange
-- rate entries that never overlap per type and currency), the freight job of a house shipment (one per shipment and
-- legal entity that is not cancelled) with charge lines whose sell and cost sides are immutable versions of
-- estimates, the quotes applied to them (a quote and order on one job only), and debtors' credit accounts with
-- terms versions and holds released once. Money in minor units with its currency; rates as integers with a scale.
-- No foreign key leaves cargo_fin.
CREATE SCHEMA IF NOT EXISTS cargo_fin;

CREATE TABLE cargo_fin.finance_policy (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 functional_currency char(3) NOT NULL CHECK (functional_currency ~ '^[A-Z]{3}$'),
 book_code text NOT NULL CHECK (length(btrim(book_code)) BETWEEN 1 AND 32),
 opened_by text NOT NULL CHECK (length(btrim(opened_by)) > 0),
 opened_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 PRIMARY KEY (tenant_id, legal_entity_id)
);

CREATE TABLE cargo_fin.exchange_rate_entry (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no > 0),
 rate_type text NOT NULL CHECK (rate_type IN ('ESTIMATE','INVOICE','ACCOUNTING')),
 from_currency char(3) NOT NULL CHECK (from_currency ~ '^[A-Z]{3}$'),
 rate bigint NOT NULL CHECK (rate > 0),
 scale integer NOT NULL CHECK (scale BETWEEN 0 AND 12),
 from_digits integer NOT NULL CHECK (from_digits BETWEEN 0 AND 4),
 to_digits integer NOT NULL CHECK (to_digits BETWEEN 0 AND 4),
 valid_from timestamptz NOT NULL,
 valid_until timestamptz,
 source text NOT NULL CHECK (length(btrim(source)) BETWEEN 1 AND 500),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, entry_no),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 CONSTRAINT exchange_rate_entry_period CHECK (valid_until IS NULL OR valid_until > valid_from)
);

CREATE TABLE cargo_fin.freight_job (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 job_id text NOT NULL CHECK (length(btrim(job_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL,
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 kind text NOT NULL CHECK (kind IN ('AIR_EXPORT_HOUSE','SEA_EXPORT_LCL_HOUSE')),
 functional_currency char(3) NOT NULL,
 book_code text NOT NULL,
 customer_party_id text NOT NULL CHECK (length(btrim(customer_party_id)) BETWEEN 1 AND 128),
 plan_version integer NOT NULL CHECK (plan_version > 0),
 state text NOT NULL CHECK (state IN ('OPEN','CANCELLED')),
 opened_by text NOT NULL CHECK (length(btrim(opened_by)) > 0),
 opened_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 cancel_reason text CHECK (length(btrim(cancel_reason)) BETWEEN 1 AND 500),
 cancelled_by text CHECK (length(btrim(cancelled_by)) > 0),
 cancelled_at timestamptz,
 cancel_entered_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, job_id),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 CONSTRAINT freight_job_cancellation CHECK ((state = 'CANCELLED') = (cancelled_at IS NOT NULL) AND (cancelled_at IS NULL) = (cancelled_by IS NULL)
   AND (cancelled_at IS NULL) = (cancel_reason IS NULL) AND (cancelled_at IS NULL) = (cancel_entered_at IS NULL))
);
-- OD-1 A: one job per shipment and legal entity that is not cancelled.
CREATE UNIQUE INDEX freight_job_one_per_shipment ON cargo_fin.freight_job (tenant_id, shipment_id, legal_entity_id) WHERE state <> 'CANCELLED';
CREATE INDEX freight_job_open_by_legal_entity ON cargo_fin.freight_job (tenant_id, legal_entity_id) WHERE state = 'OPEN';

CREATE TABLE cargo_fin.job_basis (
 tenant_id text NOT NULL,
 job_id text NOT NULL,
 basis_no integer NOT NULL CHECK (basis_no > 0),
 basis_id text NOT NULL CHECK (length(btrim(basis_id)) BETWEEN 1 AND 128),
 quote_id text NOT NULL CHECK (length(btrim(quote_id)) BETWEEN 1 AND 128),
 quote_revision bigint NOT NULL CHECK (quote_revision > 0),
 order_id text NOT NULL CHECK (length(btrim(order_id)) BETWEEN 1 AND 128),
 applied_by text NOT NULL CHECK (length(btrim(applied_by)) > 0),
 applied_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 live boolean NOT NULL DEFAULT true,
 PRIMARY KEY (tenant_id, job_id, basis_no),
 FOREIGN KEY (tenant_id, job_id) REFERENCES cargo_fin.freight_job (tenant_id, job_id)
);
-- A quote and order are priced on one job that is not cancelled (cancelling a job frees its bases).
CREATE UNIQUE INDEX job_basis_once ON cargo_fin.job_basis (tenant_id, quote_id, order_id) WHERE live;

CREATE TABLE cargo_fin.charge_line (
 tenant_id text NOT NULL,
 job_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 charge_code text NOT NULL CHECK (length(btrim(charge_code)) BETWEEN 1 AND 64),
 description text CHECK (length(btrim(description)) BETWEEN 1 AND 500),
 source text NOT NULL CHECK (source IN ('QUOTE','MANUAL')),
 basis_no integer,
 quote_line_no integer CHECK (quote_line_no > 0),
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, job_id, line_no),
 FOREIGN KEY (tenant_id, job_id) REFERENCES cargo_fin.freight_job (tenant_id, job_id),
 FOREIGN KEY (tenant_id, job_id, basis_no) REFERENCES cargo_fin.job_basis (tenant_id, job_id, basis_no),
 CONSTRAINT charge_line_source CHECK (CASE source WHEN 'QUOTE' THEN basis_no IS NOT NULL AND quote_line_no IS NOT NULL AND reason IS NULL
   ELSE basis_no IS NULL AND quote_line_no IS NULL AND reason IS NOT NULL END)
);

CREATE TABLE cargo_fin.charge_side_version (
 tenant_id text NOT NULL,
 job_id text NOT NULL,
 line_no integer NOT NULL,
 side text NOT NULL CHECK (side IN ('SELL','COST')),
 version_no integer NOT NULL CHECK (version_no > 0),
 party_id text CHECK (length(btrim(party_id)) BETWEEN 1 AND 128),
 party_revision bigint NOT NULL CHECK (party_revision >= 0),
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 amount_minor bigint NOT NULL,
 local_minor bigint NOT NULL,
 rate_entry_no integer,
 rate_type text CHECK (rate_type IN ('ESTIMATE','INVOICE','ACCOUNTING')),
 rate bigint CHECK (rate > 0),
 scale integer,
 from_digits integer,
 to_digits integer,
 rate_source text,
 status text NOT NULL CHECK (status IN ('ESTIMATED','ACCRUED')),
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, job_id, line_no, side, version_no),
 FOREIGN KEY (tenant_id, job_id, line_no) REFERENCES cargo_fin.charge_line (tenant_id, job_id, line_no),
 CONSTRAINT charge_side_sell CHECK (side <> 'SELL' OR (party_id IS NOT NULL AND status = 'ESTIMATED')),
 CONSTRAINT charge_side_cost CHECK (side <> 'COST' OR (amount_minor >= 0 AND status = 'ACCRUED')),
 CONSTRAINT charge_side_reason CHECK (version_no = 1 OR reason IS NOT NULL),
 CONSTRAINT charge_side_rate CHECK ((rate_entry_no IS NULL) = (rate IS NULL) AND (rate IS NULL) = (rate_type IS NULL) AND (rate IS NULL) = (scale IS NULL)
   AND (rate IS NULL) = (from_digits IS NULL) AND (rate IS NULL) = (to_digits IS NULL) AND (rate IS NULL) = (rate_source IS NULL))
);

CREATE TABLE cargo_fin.credit_account (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL,
 debtor_party_id text NOT NULL CHECK (length(btrim(debtor_party_id)) BETWEEN 1 AND 128),
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 state text NOT NULL CHECK (state IN ('ACTIVE','ON_HOLD')),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, legal_entity_id, debtor_party_id),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id)
);
CREATE TABLE cargo_fin.credit_terms_version (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 debtor_party_id text NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 limit_minor bigint NOT NULL CHECK (limit_minor >= 0),
 currency char(3) NOT NULL,
 payment_days integer NOT NULL CHECK (payment_days BETWEEN 0 AND 365),
 payment_basis text NOT NULL CHECK (payment_basis IN ('INVOICE_DATE','END_OF_MONTH')),
 set_by text NOT NULL CHECK (length(btrim(set_by)) > 0),
 set_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, debtor_party_id, version_no),
 FOREIGN KEY (tenant_id, legal_entity_id, debtor_party_id) REFERENCES cargo_fin.credit_account (tenant_id, legal_entity_id, debtor_party_id)
);
CREATE TABLE cargo_fin.credit_hold (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 debtor_party_id text NOT NULL,
 hold_no integer NOT NULL CHECK (hold_no > 0),
 code text NOT NULL CHECK (code IN ('MANUAL','OVER_LIMIT','OVERDUE')),
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 placed_by text NOT NULL CHECK (length(btrim(placed_by)) > 0),
 placed_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 released_by text CHECK (length(btrim(released_by)) > 0),
 released_at timestamptz,
 release_reason text CHECK (length(btrim(release_reason)) BETWEEN 1 AND 500),
 release_entered_at timestamptz,
 PRIMARY KEY (tenant_id, legal_entity_id, debtor_party_id, hold_no),
 FOREIGN KEY (tenant_id, legal_entity_id, debtor_party_id) REFERENCES cargo_fin.credit_account (tenant_id, legal_entity_id, debtor_party_id),
 CONSTRAINT credit_hold_release CHECK ((released_at IS NULL) = (released_by IS NULL) AND (released_at IS NULL) = (release_reason IS NULL)
   AND (released_at IS NULL) = (release_entered_at IS NULL) AND (released_at IS NULL OR released_at >= placed_at))
);

CREATE TABLE cargo_fin.command_receipt (
 tenant_id text NOT NULL,
 command_id text NOT NULL,
 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 result_type text NOT NULL CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount')),
 result_id text NOT NULL,
 result_revision bigint NOT NULL CHECK (result_revision > 0),
 result_version integer CHECK (result_version >= 0),
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, command_id)
);

-- Rows change only where a column says so: a policy its revision; a job its state, cancellation, revision and
-- updated_at, and never once cancelled; a basis only stops being live, once, with its job's cancellation; an account
-- its state, revision and updated_at; a hold is only released, once. Everything else is immutable.
CREATE FUNCTION cargo_fin.reject_row_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_fin: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE FUNCTION cargo_fin.change_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
     OR (TG_TABLE_NAME = 'credit_hold' AND (o->>'released_at' IS NOT NULL OR n->>'released_at' IS NULL)) THEN
    RAISE EXCEPTION 'cargo_fin: % changes only %', TG_TABLE_NAME, changeable USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER finance_policy_rules BEFORE UPDATE OR DELETE ON cargo_fin.finance_policy
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.change_only('revision');
CREATE TRIGGER freight_job_rules BEFORE UPDATE OR DELETE ON cargo_fin.freight_job
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.change_only('state','cancel_reason','cancelled_by','cancelled_at','cancel_entered_at','revision','updated_at');
CREATE TRIGGER job_basis_rules BEFORE UPDATE OR DELETE ON cargo_fin.job_basis
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.change_only('live');
CREATE TRIGGER credit_account_rules BEFORE UPDATE OR DELETE ON cargo_fin.credit_account
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.change_only('state','revision','updated_at');
CREATE TRIGGER credit_hold_rules BEFORE UPDATE OR DELETE ON cargo_fin.credit_hold
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.change_only('released_by','released_at','release_reason','release_entered_at');
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['exchange_rate_entry','charge_line','charge_side_version','credit_terms_version'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_fin.%I FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- At commit: a policy's rate entries are numbered from 1 without gaps, agree on the functional currency's digits,
-- never convert the functional currency and never overlap per type and currency.
CREATE FUNCTION cargo_fin.check_policy_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
    INTO ok;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_fin: finance policy %/% is inconsistent at commit', p.tenant_id, p.legal_entity_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'finance_policy_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER finance_policy_consistent AFTER INSERT OR UPDATE ON cargo_fin.finance_policy
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_policy_at_commit();

-- At commit: a job's lines, bases and side versions are numbered from 1 without gaps; every line has a side; a QUOTE
-- line's basis is the job's; a side's local amount is its amount when in the functional currency, and otherwise
-- converted at an entry of the job's policy of that type, rate and digits; the job's functional currency and book
-- are its policy's; a cancelled job has no live basis and an open one only live bases.
CREATE FUNCTION cargo_fin.check_job_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.charge_side_version v WHERE v.tenant_id = j.tenant_id AND v.job_id = j.job_id
       AND CASE WHEN v.currency = j.functional_currency THEN v.rate IS NOT NULL OR v.local_minor <> v.amount_minor
         ELSE NOT EXISTS (SELECT 1 FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = j.tenant_id AND e.legal_entity_id = j.legal_entity_id
           AND e.entry_no = v.rate_entry_no AND e.rate_type = v.rate_type AND e.from_currency = v.currency AND e.rate = v.rate AND e.scale = v.scale
           AND e.from_digits = v.from_digits AND e.to_digits = v.to_digits AND e.valid_from <= v.recorded_at
           AND (e.valid_until IS NULL OR v.recorded_at < e.valid_until)) END)
    INTO ok;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_fin: freight job %/% is inconsistent at commit (state %)', j.tenant_id, j.job_id, j.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'freight_job_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER freight_job_consistent AFTER INSERT OR UPDATE ON cargo_fin.freight_job
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_job_at_commit();

-- At commit: an account's terms and holds are numbered from 1, at most one hold is open, the last, and the state
-- says whether it is; terms are in the account's currency.
CREATE FUNCTION cargo_fin.check_account_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  a cargo_fin.credit_account;
  ok boolean;
BEGIN
  SELECT * INTO a FROM cargo_fin.credit_account WHERE tenant_id = NEW.tenant_id AND legal_entity_id = NEW.legal_entity_id
    AND debtor_party_id = NEW.debtor_party_id;
  SELECT EXISTS (SELECT 1 FROM cargo_fin.finance_policy p WHERE p.tenant_id = a.tenant_id AND p.legal_entity_id = a.legal_entity_id
       AND p.functional_currency = a.currency)
     AND (SELECT count(*) = coalesce(max(version_no), 0) AND count(*) > 0 AND bool_and(t.currency = a.currency) FROM cargo_fin.credit_terms_version t
       WHERE t.tenant_id = a.tenant_id AND t.legal_entity_id = a.legal_entity_id AND t.debtor_party_id = a.debtor_party_id)
     AND (SELECT count(*) = coalesce(max(hold_no), 0) FROM cargo_fin.credit_hold h WHERE h.tenant_id = a.tenant_id
       AND h.legal_entity_id = a.legal_entity_id AND h.debtor_party_id = a.debtor_party_id)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.credit_hold h WHERE h.tenant_id = a.tenant_id AND h.legal_entity_id = a.legal_entity_id
       AND h.debtor_party_id = a.debtor_party_id AND h.released_at IS NULL AND h.hold_no < (SELECT max(hold_no) FROM cargo_fin.credit_hold x
         WHERE x.tenant_id = a.tenant_id AND x.legal_entity_id = a.legal_entity_id AND x.debtor_party_id = a.debtor_party_id))
     AND (a.state = 'ON_HOLD') = EXISTS (SELECT 1 FROM cargo_fin.credit_hold h WHERE h.tenant_id = a.tenant_id AND h.legal_entity_id = a.legal_entity_id
       AND h.debtor_party_id = a.debtor_party_id AND h.released_at IS NULL)
    INTO ok;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_fin: credit account %/%/% is inconsistent at commit (state %)', a.tenant_id, a.legal_entity_id, a.debtor_party_id, a.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'credit_account_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER credit_account_consistent AFTER INSERT OR UPDATE ON cargo_fin.credit_account
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_account_at_commit();

-- At commit, a child row is written by a transaction that writes its parent row: nothing is added to a policy, job or
-- account behind its back.
CREATE FUNCTION cargo_fin.check_written_with_parent() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  found boolean;
BEGIN
  CASE TG_TABLE_NAME
    WHEN 'exchange_rate_entry' THEN
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
DO $with$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['exchange_rate_entry','job_basis','charge_line','charge_side_version','credit_terms_version','credit_hold'] LOOP
    EXECUTE format('CREATE CONSTRAINT TRIGGER %I AFTER INSERT OR UPDATE ON cargo_fin.%I DEFERRABLE INITIALLY DEFERRED FOR EACH ROW '
      'EXECUTE FUNCTION cargo_fin.check_written_with_parent()', tbl || '_with_parent', tbl);
  END LOOP;
END $with$;

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['finance_policy','exchange_rate_entry','freight_job','job_basis','charge_line','charge_side_version','credit_account',
      'credit_terms_version','credit_hold','command_receipt'] LOOP
    EXECUTE format('ALTER TABLE cargo_fin.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_fin.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_fin.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
