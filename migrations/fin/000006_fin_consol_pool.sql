-- IDP-Cargo FIN V0.4 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Consol cost pools (FB3, OD-6 A): carrier and co-loader costs of a whole consolidation are recorded on the pool of
-- its operating legal entity (one pool per consolidation and legal entity) and apportioned to the house jobs by
-- chargeable weight (air: 6000 cm³ per kg rounded up to 500 g), W/M freight tonnes (LCL), volume, pieces, or by hand
-- with a reason. Cost lines are versioned accruals; an apportionment is immutable and supersedes earlier ones line by
-- line; its shares conserve the line's amount and local amount exactly. Matching creditor invoices and closing the
-- pool come with FB4.

CREATE TABLE cargo_fin.consol_cost_pool (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 pool_id text NOT NULL CHECK (length(btrim(pool_id)) BETWEEN 1 AND 128),
 consolidation_id text NOT NULL CHECK (length(btrim(consolidation_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL,
 mode text NOT NULL CHECK (mode IN ('AIR','SEA_LCL')),
 functional_currency char(3) NOT NULL CHECK (functional_currency ~ '^[A-Z]{3}$'),
 book_code text NOT NULL CHECK (length(btrim(book_code)) BETWEEN 1 AND 64),
 state text NOT NULL CHECK (state IN ('OPEN')), -- CLOSED comes with FB4
 opened_by text NOT NULL CHECK (length(btrim(opened_by)) > 0),
 opened_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, pool_id),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 CONSTRAINT consol_cost_pool_one_per_consol UNIQUE (tenant_id, consolidation_id, legal_entity_id)
);

CREATE TABLE cargo_fin.pool_cost_line (
 tenant_id text NOT NULL,
 pool_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 charge_code text NOT NULL CHECK (length(btrim(charge_code)) BETWEEN 1 AND 64),
 description text CHECK (length(btrim(description)) BETWEEN 1 AND 500),
 supplier_ref text CHECK (length(btrim(supplier_ref)) BETWEEN 1 AND 500),
 method text NOT NULL CHECK (method IN ('CHARGEABLE','VOLUME','PIECES','MANUAL')),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, pool_id, line_no),
 FOREIGN KEY (tenant_id, pool_id) REFERENCES cargo_fin.consol_cost_pool (tenant_id, pool_id)
);

CREATE TABLE cargo_fin.pool_cost_version (
 tenant_id text NOT NULL,
 pool_id text NOT NULL,
 line_no integer NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 creditor_party_id text CHECK (length(btrim(creditor_party_id)) BETWEEN 1 AND 128),
 creditor_revision bigint NOT NULL CHECK (creditor_revision >= 0),
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 amount_minor bigint NOT NULL CHECK (amount_minor >= 0 AND amount_minor <= 1000000000000000),
 local_minor bigint NOT NULL CHECK (local_minor >= 0 AND local_minor <= 1000000000000000),
 rate_entry_no integer,
 rate_type text,
 rate bigint,
 scale integer,
 from_digits integer,
 to_digits integer,
 rate_source text,
 status text NOT NULL CHECK (status IN ('ACCRUED')), -- matching comes with FB4
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, pool_id, line_no, version_no),
 FOREIGN KEY (tenant_id, pool_id, line_no) REFERENCES cargo_fin.pool_cost_line (tenant_id, pool_id, line_no),
 CONSTRAINT pool_cost_version_creditor CHECK ((creditor_party_id IS NULL) = (creditor_revision = 0)),
 CONSTRAINT pool_cost_version_reason CHECK ((version_no = 1) = (reason IS NULL)),
 CONSTRAINT pool_cost_version_fx CHECK ((rate IS NULL) = (rate_entry_no IS NULL) AND (rate IS NULL) = (rate_type IS NULL)
   AND (rate IS NULL) = (scale IS NULL) AND (rate IS NULL) = (from_digits IS NULL) AND (rate IS NULL) = (to_digits IS NULL)
   AND (rate IS NULL) = (rate_source IS NULL))
);

CREATE TABLE cargo_fin.pool_apportionment (
 tenant_id text NOT NULL,
 pool_id text NOT NULL,
 apportionment_no integer NOT NULL CHECK (apportionment_no > 0),
 basis_revision bigint NOT NULL CHECK (basis_revision > 0),
 stage text NOT NULL CHECK (stage IN ('ACTUAL','PLANNED')),
 made_by text NOT NULL CHECK (length(btrim(made_by)) > 0),
 made_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, pool_id, apportionment_no),
 FOREIGN KEY (tenant_id, pool_id) REFERENCES cargo_fin.consol_cost_pool (tenant_id, pool_id)
);

CREATE TABLE cargo_fin.pool_apportionment_line (
 tenant_id text NOT NULL,
 pool_id text NOT NULL,
 apportionment_no integer NOT NULL,
 line_no integer NOT NULL,
 line_version integer NOT NULL,
 method text NOT NULL CHECK (method IN ('CHARGEABLE','VOLUME','PIECES','MANUAL')),
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 PRIMARY KEY (tenant_id, pool_id, apportionment_no, line_no),
 FOREIGN KEY (tenant_id, pool_id, apportionment_no) REFERENCES cargo_fin.pool_apportionment (tenant_id, pool_id, apportionment_no),
 FOREIGN KEY (tenant_id, pool_id, line_no, line_version) REFERENCES cargo_fin.pool_cost_version (tenant_id, pool_id, line_no, version_no),
 CONSTRAINT pool_apportionment_line_reason CHECK ((method = 'MANUAL') = (reason IS NOT NULL))
);

CREATE TABLE cargo_fin.pool_apportionment_share (
 tenant_id text NOT NULL,
 pool_id text NOT NULL,
 apportionment_no integer NOT NULL,
 line_no integer NOT NULL,
 share_no integer NOT NULL CHECK (share_no > 0),
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 job_id text NOT NULL,
 quantity bigint NOT NULL CHECK (quantity >= 0),
 -- chargeable grams, cubic centimetres, pieces or milli freight tonnes; NULL on a MANUAL share
 unit text CHECK (unit IN ('GRM','CMQ','PCS','MWM')),
 amount_minor bigint NOT NULL CHECK (amount_minor >= 0),
 local_minor bigint NOT NULL CHECK (local_minor >= 0),
 PRIMARY KEY (tenant_id, pool_id, apportionment_no, line_no, share_no),
 FOREIGN KEY (tenant_id, pool_id, apportionment_no, line_no) REFERENCES cargo_fin.pool_apportionment_line (tenant_id, pool_id, apportionment_no, line_no),
 FOREIGN KEY (tenant_id, job_id) REFERENCES cargo_fin.freight_job (tenant_id, job_id),
 CONSTRAINT pool_apportionment_share_house_once UNIQUE (tenant_id, pool_id, apportionment_no, line_no, shipment_id)
);
CREATE INDEX pool_apportionment_share_by_job ON cargo_fin.pool_apportionment_share (tenant_id, job_id);

ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check
    CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile','DebtorReceipt','ConsolCostPool'));

-- A pool changes only its state, revision and updated_at; everything under it is append-only and immutable.
CREATE TRIGGER consol_cost_pool_rules BEFORE UPDATE OR DELETE ON cargo_fin.consol_cost_pool
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.change_only('state','revision','updated_at');
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['pool_cost_line','pool_cost_version','pool_apportionment','pool_apportionment_line','pool_apportionment_share'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_fin.%I FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- At commit, a pool: its functional currency and book are its policy's; its lines, each line's versions and its
-- apportionments are numbered from 1 without gaps and every line has a version; a version's local amount is its
-- amount when in the functional currency, and otherwise its amount converted (halves away from zero) at an ESTIMATE
-- entry of the pool's policy in force when the version was recorded.
CREATE FUNCTION cargo_fin.consol_cost_pool_consistent_check(t text, id text) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  p cargo_fin.consol_cost_pool;
  ok boolean;
BEGIN
  SELECT * INTO p FROM cargo_fin.consol_cost_pool WHERE tenant_id = t AND pool_id = id;
  SELECT EXISTS (SELECT 1 FROM cargo_fin.finance_policy fp WHERE fp.tenant_id = p.tenant_id AND fp.legal_entity_id = p.legal_entity_id
       AND fp.functional_currency = p.functional_currency AND fp.book_code = p.book_code)
     AND (SELECT count(*) = coalesce(max(line_no), 0) FROM cargo_fin.pool_cost_line l WHERE l.tenant_id = p.tenant_id AND l.pool_id = p.pool_id)
     AND (SELECT count(*) = coalesce(max(apportionment_no), 0) FROM cargo_fin.pool_apportionment a
       WHERE a.tenant_id = p.tenant_id AND a.pool_id = p.pool_id)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.pool_cost_line l WHERE l.tenant_id = p.tenant_id AND l.pool_id = p.pool_id
       AND (SELECT count(*) <> coalesce(max(version_no), 0) OR count(*) = 0 FROM cargo_fin.pool_cost_version v
         WHERE v.tenant_id = l.tenant_id AND v.pool_id = l.pool_id AND v.line_no = l.line_no))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.pool_cost_version v WHERE v.tenant_id = p.tenant_id AND v.pool_id = p.pool_id
       AND CASE WHEN v.currency = p.functional_currency OR v.rate IS NULL
           THEN v.rate IS NOT NULL OR v.local_minor <> v.amount_minor OR (v.currency <> p.functional_currency AND v.amount_minor <> 0)
         ELSE NOT EXISTS (SELECT 1 FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = p.tenant_id AND e.legal_entity_id = p.legal_entity_id
           AND e.entry_no = v.rate_entry_no AND e.rate_type = v.rate_type AND e.rate_type = 'ESTIMATE' AND e.from_currency = v.currency AND e.rate = v.rate
           AND e.scale = v.scale AND e.from_digits = v.from_digits AND e.to_digits = v.to_digits AND e.valid_from <= v.recorded_at
           AND v.recorded_at < e.valid_until
           AND v.local_minor = round(v.amount_minor::numeric * e.rate * power(10::numeric, e.to_digits - e.from_digits - e.scale))) END)
    INTO ok;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: consol cost pool %/% is inconsistent at commit', p.tenant_id, p.pool_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consol_cost_pool_consistent';
  END IF;
END $fn$;

-- The conservation rule (DDD-020/021): an apportioned line's shares are numbered from 1 without gaps, there is at
-- least one, and they sum exactly to the amount and the local amount of the line version they divide.
CREATE FUNCTION cargo_fin.pool_line_shares_check(t text, pool text, ano integer, line integer) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  al cargo_fin.pool_apportionment_line;
  v cargo_fin.pool_cost_version;
  ok boolean;
BEGIN
  SELECT * INTO al FROM cargo_fin.pool_apportionment_line WHERE tenant_id = t AND pool_id = pool AND apportionment_no = ano AND line_no = line;
  SELECT * INTO v FROM cargo_fin.pool_cost_version
    WHERE tenant_id = t AND pool_id = pool AND line_no = al.line_no AND version_no = al.line_version;
  SELECT count(*) = coalesce(max(s.share_no), 0) AND count(*) >= 1
      AND coalesce(sum(s.amount_minor), -1) = v.amount_minor AND coalesce(sum(s.local_minor), -1) = v.local_minor
    INTO ok FROM cargo_fin.pool_apportionment_share s
    WHERE s.tenant_id = t AND s.pool_id = pool AND s.apportionment_no = ano AND s.line_no = line;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: shares of pool %/% apportionment % line % do not conserve its amount', t, pool, ano, line
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_apportionment_conserves';
  END IF;
END $fn$;

CREATE FUNCTION cargo_fin.check_pool_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.consol_cost_pool_consistent_check(NEW.tenant_id, NEW.pool_id);
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER consol_cost_pool_consistent AFTER INSERT OR UPDATE ON cargo_fin.consol_cost_pool
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_pool_at_commit();

-- At commit, every pool record: written by a transaction that also wrote its pool row (so the pool's revision moved
-- and concurrent writers took turns on the pool lock), and the pool consistent. The line and share paths then check
-- conservation each on their own, so a row inserted after SET CONSTRAINTS ALL IMMEDIATE ran the other path's check
-- cannot escape it.
CREATE FUNCTION cargo_fin.check_pool_record_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id
       AND p.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its pool', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.consol_cost_pool_consistent_check(NEW.tenant_id, NEW.pool_id);
  RETURN NULL;
END $fn$;
DO $withpool$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['pool_cost_line','pool_cost_version','pool_apportionment'] LOOP
    EXECUTE format('CREATE CONSTRAINT TRIGGER %I AFTER INSERT ON cargo_fin.%I
        DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_pool_record_at_commit()', tbl || '_with_pool', tbl);
  END LOOP;
END $withpool$;

CREATE FUNCTION cargo_fin.check_apportioned_line_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id
       AND p.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its pool', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.pool_line_shares_check(NEW.tenant_id, NEW.pool_id, NEW.apportionment_no, NEW.line_no);
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER pool_apportionment_line_conserves AFTER INSERT ON cargo_fin.pool_apportionment_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_apportioned_line_at_commit();

-- At commit, a share: written with its pool; conserving its line; and its job, locked as FIN's job commands lock it
-- (so a cancellation racing an apportionment waits and then sees the other's commit), an OPEN job of the pool's legal
-- entity for the share's shipment.
CREATE FUNCTION cargo_fin.check_share_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id
       AND p.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its pool', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
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
CREATE CONSTRAINT TRIGGER pool_apportionment_share_consistent AFTER INSERT ON cargo_fin.pool_apportionment_share
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_share_at_commit();

-- At commit, a job: once CANCELLED it holds no current pool share (one under the latest apportionment covering its
-- line). The cancelling command and an apportionment lock the job row, so they take turns; re-apportion the pool on a
-- basis without the house (PHY detached it) before cancelling its job.
CREATE FUNCTION cargo_fin.check_job_pool_shares_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.state = 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_fin.pool_apportionment_share sh
       WHERE sh.tenant_id = NEW.tenant_id AND sh.job_id = NEW.job_id
         AND sh.apportionment_no = (SELECT max(l2.apportionment_no) FROM cargo_fin.pool_apportionment_line l2
           WHERE l2.tenant_id = sh.tenant_id AND l2.pool_id = sh.pool_id AND l2.line_no = sh.line_no)) THEN
    RAISE EXCEPTION 'cargo_fin: freight job %/% holds current pool shares; apportion the pool without it first', NEW.tenant_id, NEW.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_share_job_open';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER freight_job_pool_shares_consistent AFTER UPDATE ON cargo_fin.freight_job
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_job_pool_shares_at_commit();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['consol_cost_pool','pool_cost_line','pool_cost_version','pool_apportionment','pool_apportionment_line',
      'pool_apportionment_share'] LOOP
    EXECUTE format('ALTER TABLE cargo_fin.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_fin.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_fin.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
