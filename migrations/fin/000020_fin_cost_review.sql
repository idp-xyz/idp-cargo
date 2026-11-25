-- IDP-Cargo FIN V2.99 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The cost review of a loss-making job: a person who sees a job's profit below zero asks finance to review its costs,
-- and another person resolves the request. Neither step changes an amount: the request records the figures the
-- requester saw, and the resolution says only whether finance corrected the costs through the ordinary commands
-- (COST_CORRECTED) or found them right (NO_CHANGE).
--
-- Added: two tables, each immutable (rows are only inserted), with row security enabled and forced and written at READ
-- COMMITTED like every table of cargo_fin: cost_review_request (a request, numbered per tenant from 1 as CRR-<n>) and
-- cost_review_resolution (at most one for a request). Changed: the result types of command_receipt gain
-- 'CostReviewRequest' and 'CostReviewResolution'. No row changes and nothing is dropped; the ALTER TABLE takes an
-- ACCESS EXCLUSIVE lock on command_receipt for its brief check of the existing rows (reasoned, not measured).
--
-- What the database refuses: a request whose number is not the tenant's next (taken under the tenant's numbering lock,
-- so that numbers have no gap and two requests cannot take one), and, as a check at commit, a tenant whose numbers skip
-- one; a request id that is not CRR-<number>; figures that do not add up (revenue less cost is the profit) or whose
-- profit is not below zero; a profit version that is not a SHA-256 in hex; a request whose job is unknown, or whose
-- shipment, legal entity or currency are not the job's; a second request for a job while another is unresolved,
-- checked at commit with the job's row locked, so that two requests for one job cannot both commit; a resolution by
-- the person who requested (the resolution carries the requester and the request's time, and a foreign key ties them
-- to the request), a resolution before its request, a second resolution of a request, an outcome other than
-- COST_CORRECTED and NO_CHANGE, and a reason or note of less than 1 or more than 500 characters.
--
-- What the schema does not know, and only the domain keeps: that the figures are the job's profit as FIN computes it
-- (the database cannot compute it: it needs PHY's basis and the settings in force), that the profit version the
-- requester saw is the current one, that the job was OPEN when the request was made, that the requester and the
-- resolver hold the permissions RequestCostReview and ResolveCostReview, and that the actors are the tokens' actors.

CREATE TABLE cargo_fin.cost_review_request (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 request_no integer NOT NULL CHECK (request_no > 0),
 request_id text NOT NULL,
 job_id text NOT NULL,
 shipment_id text NOT NULL,
 legal_entity_id text NOT NULL,
 requested_by text NOT NULL CHECK (length(btrim(requested_by)) BETWEEN 1 AND 128),
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 currency char(3) NOT NULL,
 revenue_minor bigint NOT NULL,
 cost_minor bigint NOT NULL,
 profit_minor bigint NOT NULL,
 profit_basis text NOT NULL CHECK (profit_basis IN ('SEPARATE','INCLUDED_MARKED')),
 settings_version integer NOT NULL CHECK (settings_version >= 0),
 profit_version char(64) NOT NULL CHECK (profit_version ~ '^[0-9a-f]{64}$'),
 requested_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, request_no),
 CONSTRAINT cost_review_request_id UNIQUE (tenant_id, request_id),
 CONSTRAINT cost_review_request_requester UNIQUE (tenant_id, request_no, requested_by, requested_at),
 FOREIGN KEY (tenant_id, job_id) REFERENCES cargo_fin.freight_job (tenant_id, job_id),
 CONSTRAINT cost_review_request_id_form CHECK (request_id = 'CRR-' || request_no::text),
 CONSTRAINT cost_review_request_figures CHECK (revenue_minor - cost_minor = profit_minor),
 CONSTRAINT cost_review_request_loss CHECK (profit_minor < 0)
);
CREATE INDEX cost_review_request_job_idx ON cargo_fin.cost_review_request (tenant_id, job_id, request_no);

CREATE TABLE cargo_fin.cost_review_resolution (
 tenant_id text NOT NULL,
 request_no integer NOT NULL,
 -- the request's own columns, so that the rules about the pair of rows hold here as checks
 requested_by text NOT NULL,
 requested_at timestamptz NOT NULL,
 resolved_by text NOT NULL CHECK (length(btrim(resolved_by)) BETWEEN 1 AND 128),
 outcome text NOT NULL CHECK (outcome IN ('COST_CORRECTED','NO_CHANGE')),
 note text NOT NULL CHECK (length(btrim(note)) BETWEEN 1 AND 500),
 resolved_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, request_no),
 CONSTRAINT cost_review_resolution_matches_request FOREIGN KEY (tenant_id, request_no, requested_by, requested_at)
   REFERENCES cargo_fin.cost_review_request (tenant_id, request_no, requested_by, requested_at),
 CONSTRAINT cost_review_resolution_other_actor CHECK (resolved_by <> requested_by),
 CONSTRAINT cost_review_resolution_after_request CHECK (resolved_at >= requested_at)
);

ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check
    CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile','DebtorReceipt','ConsolCostPool','CreditorInvoice',
      'CreditorPayment','FapiaoRecord','FinSettings','CostReviewRequest','CostReviewResolution'));

DO $tables$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['cost_review_request','cost_review_resolution'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_fin.%I FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change()',
      tbl || '_immutable', tbl);
    EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.%I FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed()',
      tbl || '_read_committed', tbl);
    EXECUTE format('ALTER TABLE cargo_fin.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_fin.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_fin.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $tables$;

-- A request takes the tenant's numbering lock and is the next number: numbers have no gap, and two requests of a
-- tenant cannot take one.
CREATE FUNCTION cargo_fin.check_cost_review_number() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE last integer;
BEGIN
  PERFORM cargo_fin.require_read_committed();
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_fin.cost_review_numbers/' || NEW.tenant_id, 0));
  SELECT coalesce(max(r.request_no), 0) INTO last FROM cargo_fin.cost_review_request r WHERE r.tenant_id = NEW.tenant_id;
  IF NEW.request_no <> last + 1 THEN
    RAISE EXCEPTION 'cargo_fin: cost review request number % of tenant % is not the next, %', NEW.request_no, NEW.tenant_id, last + 1
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_review_request_next_number';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER cost_review_request_number BEFORE INSERT ON cargo_fin.cost_review_request
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_cost_review_number();

-- At commit, a request names its job's shipment, legal entity and currency, and its job has no other request that is
-- unresolved. The job's row is locked first (as an apportionment and a job change lock it), so that two requests for
-- one job take turns and the second sees the first.
CREATE FUNCTION cargo_fin.check_cost_review_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id FOR NO KEY UPDATE;
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id
       AND j.shipment_id = NEW.shipment_id AND j.legal_entity_id = NEW.legal_entity_id AND j.functional_currency = NEW.currency) THEN
    RAISE EXCEPTION 'cargo_fin: cost review request %/% does not name its job''s shipment, legal entity and currency', NEW.tenant_id, NEW.request_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_review_request_job';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_fin.cost_review_request o WHERE o.tenant_id = NEW.tenant_id AND o.job_id = NEW.job_id AND o.request_no <> NEW.request_no
       AND NOT EXISTS (SELECT 1 FROM cargo_fin.cost_review_resolution x WHERE x.tenant_id = o.tenant_id AND x.request_no = o.request_no)) THEN
    RAISE EXCEPTION 'cargo_fin: job %/% has a cost review request that is not resolved', NEW.tenant_id, NEW.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_review_one_open';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER cost_review_one_open AFTER INSERT ON cargo_fin.cost_review_request
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_cost_review_at_commit();

-- At commit, a tenant's requests are numbered from 1 without a gap.
CREATE FUNCTION cargo_fin.check_cost_review_numbers_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF (SELECT count(*) <> max(r.request_no) FROM cargo_fin.cost_review_request r WHERE r.tenant_id = NEW.tenant_id) THEN
    RAISE EXCEPTION 'cargo_fin: cost review request numbers of tenant % skip a number', NEW.tenant_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_review_numbers_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER cost_review_numbers_consistent AFTER INSERT ON cargo_fin.cost_review_request
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_cost_review_numbers_at_commit();
