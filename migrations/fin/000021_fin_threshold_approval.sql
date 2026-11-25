-- IDP-Cargo FIN V2.100 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The cost variance threshold (E1 of the configuration of the rules) under approval. A version of a legal entity's
-- cost variance threshold is stamped with FIN's clock. One that is not looser than the version in force (its amount
-- and its rate are not higher; the first version is always looser) is approved by its setter when it is set; a looser
-- one is proposed and takes effect, at the time of its approval, when another person approves it, or never when
-- another person refuses it with a reason. A final match reads the threshold in force at its own system time.
--
-- Added: two columns on cost_variance_threshold, proposed_at (FIN's clock at the proposal) and looser, both NULL for a
-- version written before this migration; and one table, cost_variance_threshold_decision, with at most one row for a
-- version: its approval (kind APPROVED, with the time it is in force from) or its refusal (kind REFUSED, with a
-- reason). The decision is a row of its own, not columns of the version, because the application role has no UPDATE on
-- cost_variance_threshold (the grants are the workflow's), and every FIN table the settings use is insert-only as
-- this one is; the rows are never updated or deleted, are written at READ COMMITTED, and have row security enabled and
-- forced. Each version written before this migration gets a decision row here, approved by its setter at the time
-- it was set (auto, in force from its set_at, which was its business time); that is the only insert the migration
-- makes. Changed: check_written_with_parent (the threshold's commit check, fin/000007: it keeps its checks and adds the
-- rules below), and check_creditor_match_at_commit (fin/000007: the invoice names the threshold in force at its
-- match_entered_at, which FIN now reads after the policy's lock, in place of the one set by its matched_at). The
-- invoice's matched_at stays the business time the rates are read at. No column is dropped, no grant changes. The
-- ALTER TABLE takes an ACCESS EXCLUSIVE lock on cost_variance_threshold briefly (reasoned, not measured).
--
-- What the database refuses, past FIN: a version of this kind of row that names no proposal time or no looser flag, or
-- a stored looser flag that is not the database's answer (looser than the latest version numbered before it that was
-- approved and in force at its proposal time, or any when none was); a version not looser without its automatic
-- approval written with it; a second version while an earlier one has no decision (one proposed at a time for a legal
-- entity); a decision of a version written before this migration; a looser version approved by its setter, a refusal by
-- the setter, and an automatic approval of a looser one or by another person; a decision before the proposal; proposal
-- and decision times that are not within a minute of the database's clock or are before the latest time recorded for
-- the legal entity (as in fin/000017); a decision not written with its policy; and an invoice that names another
-- threshold than the one in force at the time it was matched; and a match time (match_entered_at) that is not within a
-- minute of the database's clock or is earlier than the latest in-force time of an approved version of the legal
-- entity's policy written since this migration (a match holds the policy FOR SHARE and a decision takes it exclusively,
-- so a match that read its clock after its lock is never earlier than a decision that committed before it).
--
-- What the schema does not know, and only the domain keeps: that the approver holds the permission ApproveSettings (the
-- database compares two strings); that the time a version is in force from is the time FIN's clock read under the
-- policy's lock (the database holds it within a minute of its own clock and monotone); that a version is numbered in
-- the order it was set and the business time of its command is not before the previous version's (fin/000007's rule,
-- unchanged); that an approval or a refusal names the version that waits (the database knows the version's row); the
-- idempotency of the commands; and that a match read its clock after it held the policy FOR SHARE (the database takes
-- the same lock when the match is written, holds the recorded time within a minute of its own clock and not before the
-- latest approval in force, and compares the threshold named with the one in force at the recorded time; it cannot tell
-- the time FIN read from another within the minute that lies after the latest approval).

ALTER TABLE cargo_fin.cost_variance_threshold ADD COLUMN proposed_at timestamptz, ADD COLUMN looser boolean,
  ADD CONSTRAINT cost_variance_threshold_proposal CHECK ((proposed_at IS NULL) = (looser IS NULL));

CREATE TABLE cargo_fin.cost_variance_threshold_decision (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 threshold_no integer NOT NULL,
 kind text NOT NULL CHECK (kind IN ('APPROVED','REFUSED')),
 decided_by text NOT NULL CHECK (length(btrim(decided_by)) BETWEEN 1 AND 128),
 decided_at timestamptz NOT NULL,
 in_force_at timestamptz,
 auto boolean NOT NULL,
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 PRIMARY KEY (tenant_id, legal_entity_id, threshold_no),
 FOREIGN KEY (tenant_id, legal_entity_id, threshold_no) REFERENCES cargo_fin.cost_variance_threshold (tenant_id, legal_entity_id, threshold_no),
 CONSTRAINT cost_variance_decision_shape CHECK (CASE kind
   WHEN 'APPROVED' THEN in_force_at = decided_at AND reason IS NULL
   ELSE in_force_at IS NULL AND reason IS NOT NULL AND NOT auto END)
);

-- the versions written before this migration were approved by their setters when they were set
INSERT INTO cargo_fin.cost_variance_threshold_decision (tenant_id, legal_entity_id, threshold_no, kind, decided_by, decided_at, in_force_at, auto)
  SELECT tenant_id, legal_entity_id, threshold_no, 'APPROVED', set_by, set_at, set_at, true FROM cargo_fin.cost_variance_threshold;

CREATE TRIGGER cost_variance_threshold_decision_immutable BEFORE UPDATE OR DELETE ON cargo_fin.cost_variance_threshold_decision
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
CREATE TRIGGER cost_variance_threshold_decision_read_committed BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.cost_variance_threshold_decision
  FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed();
ALTER TABLE cargo_fin.cost_variance_threshold_decision ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.cost_variance_threshold_decision FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.cost_variance_threshold_decision USING (tenant_id = current_setting('app.tenant_id',true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id',true));

-- Whether a version is looser than the latest version numbered before it that was approved and in force when it was
-- proposed: a higher amount or a higher rate, and any version when none was.
CREATE FUNCTION cargo_fin.threshold_looser(t text, le text, no integer) RETURNS boolean LANGUAGE plpgsql STABLE AS $fn$
DECLARE
  v cargo_fin.cost_variance_threshold;
  cur cargo_fin.cost_variance_threshold;
BEGIN
  SELECT * INTO v FROM cargo_fin.cost_variance_threshold WHERE tenant_id = t AND legal_entity_id = le AND threshold_no = no;
  SELECT x.* INTO cur FROM cargo_fin.cost_variance_threshold x JOIN cargo_fin.cost_variance_threshold_decision d ON d.tenant_id = x.tenant_id
    AND d.legal_entity_id = x.legal_entity_id AND d.threshold_no = x.threshold_no
    WHERE x.tenant_id = t AND x.legal_entity_id = le AND x.threshold_no < no AND d.kind = 'APPROVED' AND d.in_force_at <= v.proposed_at
    ORDER BY x.threshold_no DESC LIMIT 1;
  RETURN NOT FOUND OR v.amount_minor > cur.amount_minor OR v.rate_bp > cur.rate_bp;
END $fn$;

-- A decision: the version is one written since this migration, the decision is not before its proposal, an automatic
-- approval is the setter's of a version not looser written with it, any other decision is by another person than the
-- setter, and an approval of a looser version is not automatic.
CREATE FUNCTION cargo_fin.check_threshold_decision() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_fin.cost_variance_threshold;
BEGIN
  PERFORM cargo_fin.require_read_committed();
  SELECT * INTO v FROM cargo_fin.cost_variance_threshold WHERE tenant_id = NEW.tenant_id AND legal_entity_id = NEW.legal_entity_id
    AND threshold_no = NEW.threshold_no;
  IF v.proposed_at IS NULL THEN
    RAISE EXCEPTION 'cargo_fin: threshold %/%/% was written before its approval existed and is decided', NEW.tenant_id, NEW.legal_entity_id, NEW.threshold_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_variance_decision_legacy';
  END IF;
  IF NEW.decided_at < v.proposed_at THEN
    RAISE EXCEPTION 'cargo_fin: threshold %/%/% is decided before it was proposed', NEW.tenant_id, NEW.legal_entity_id, NEW.threshold_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_variance_decision_after_proposal';
  END IF;
  IF NEW.auto <> (NEW.decided_by = v.set_by) OR (NEW.auto AND (v.looser OR NEW.kind <> 'APPROVED')) THEN
    RAISE EXCEPTION 'cargo_fin: threshold %/%/% is decided by its setter, or automatically though it is looser; another approver decides it',
      NEW.tenant_id, NEW.legal_entity_id, NEW.threshold_no USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_variance_decision_other_actor';
  END IF;
  IF NEW.auto AND NOT EXISTS (SELECT 1 FROM cargo_fin.cost_variance_threshold c WHERE c.tenant_id = NEW.tenant_id AND c.legal_entity_id = NEW.legal_entity_id
       AND c.threshold_no = NEW.threshold_no AND c.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: an automatic approval is written with its version' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'cost_variance_decision_with_version';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER cost_variance_threshold_decision_check BEFORE INSERT ON cargo_fin.cost_variance_threshold_decision
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_threshold_decision();

-- After each insert of a version or a decision, its time is within a minute of the database's clock and not before the
-- latest time recorded for the legal entity (versions written before this migration have no time and are not counted).
CREATE FUNCTION cargo_fin.check_threshold_time() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  at timestamptz;
  latest timestamptz;
BEGIN
  at := (to_jsonb(NEW) ->> CASE TG_TABLE_NAME WHEN 'cost_variance_threshold' THEN 'proposed_at' ELSE 'decided_at' END)::timestamptz;
  IF at IS NULL THEN
    RETURN NULL;
  END IF;
  IF at < clock_timestamp() - interval '1 minute' OR at > clock_timestamp() + interval '1 minute' THEN
    RAISE EXCEPTION 'cargo_fin: threshold time % is not within a minute of the database''s clock', at
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_variance_time_window';
  END IF;
  SELECT max(x.t) INTO latest FROM (
    SELECT proposed_at AS t FROM cargo_fin.cost_variance_threshold WHERE tenant_id = NEW.tenant_id AND legal_entity_id = NEW.legal_entity_id
    UNION ALL SELECT d.decided_at FROM cargo_fin.cost_variance_threshold_decision d JOIN cargo_fin.cost_variance_threshold v
      ON v.tenant_id = d.tenant_id AND v.legal_entity_id = d.legal_entity_id AND v.threshold_no = d.threshold_no
      WHERE d.tenant_id = NEW.tenant_id AND d.legal_entity_id = NEW.legal_entity_id AND v.proposed_at IS NOT NULL) x;
  IF at < latest THEN
    RAISE EXCEPTION 'cargo_fin: threshold time % is before the latest recorded for legal entity % (%)', at, NEW.legal_entity_id, latest
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_variance_time_monotone';
  END IF;
  RETURN NULL;
END $fn$;
CREATE TRIGGER cost_variance_threshold_time AFTER INSERT ON cargo_fin.cost_variance_threshold FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_threshold_time();
CREATE TRIGGER cost_variance_threshold_decision_time AFTER INSERT ON cargo_fin.cost_variance_threshold_decision
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_threshold_time();

-- At commit, a decision is written with its policy (the policy row is then locked, so a match that holds it FOR SHARE
-- and a decision take turns), and a version, besides the rules of fin/000007, names its proposal time and looser
-- flag, the flag is the database's own answer, a version not looser has its automatic approval, and no earlier
-- version is undecided.
CREATE OR REPLACE FUNCTION cargo_fin.check_written_with_parent() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  found boolean;
BEGIN
  CASE TG_TABLE_NAME
    WHEN 'exchange_rate_entry', 'charge_tax_treatment', 'invoice_series', 'cost_variance_threshold', 'cost_variance_threshold_decision' THEN
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
    IF NEW.proposed_at IS NULL THEN
      RAISE EXCEPTION 'cargo_fin: variance threshold %/%/% names no proposal time and no looser flag', NEW.tenant_id, NEW.legal_entity_id, NEW.threshold_no
        USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_variance_threshold_proposal';
    END IF;
    IF NEW.looser <> cargo_fin.threshold_looser(NEW.tenant_id, NEW.legal_entity_id, NEW.threshold_no) THEN
      RAISE EXCEPTION 'cargo_fin: variance threshold %/%/% is stored as %looser, and the versions in force say otherwise', NEW.tenant_id, NEW.legal_entity_id,
        NEW.threshold_no, CASE WHEN NEW.looser THEN '' ELSE 'not ' END USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_variance_threshold_looser';
    END IF;
    IF NOT NEW.looser AND NOT EXISTS (SELECT 1 FROM cargo_fin.cost_variance_threshold_decision d WHERE d.tenant_id = NEW.tenant_id
         AND d.legal_entity_id = NEW.legal_entity_id AND d.threshold_no = NEW.threshold_no AND d.kind = 'APPROVED' AND d.auto) THEN
      RAISE EXCEPTION 'cargo_fin: variance threshold %/%/% is not looser and is approved when it is set', NEW.tenant_id, NEW.legal_entity_id, NEW.threshold_no
        USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_variance_threshold_auto';
    END IF;
    IF EXISTS (SELECT 1 FROM cargo_fin.cost_variance_threshold x WHERE x.tenant_id = NEW.tenant_id AND x.legal_entity_id = NEW.legal_entity_id
         AND x.threshold_no < NEW.threshold_no AND NOT EXISTS (SELECT 1 FROM cargo_fin.cost_variance_threshold_decision d WHERE d.tenant_id = x.tenant_id
           AND d.legal_entity_id = x.legal_entity_id AND d.threshold_no = x.threshold_no)) THEN
      RAISE EXCEPTION 'cargo_fin: variance threshold %/%/% is set while an earlier version waits for its decision', NEW.tenant_id, NEW.legal_entity_id,
        NEW.threshold_no USING ERRCODE = 'check_violation', CONSTRAINT = 'cost_variance_threshold_one_proposed';
    END IF;
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER cost_variance_threshold_decision_with_parent AFTER INSERT ON cargo_fin.cost_variance_threshold_decision
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_written_with_parent();

-- A match names the threshold in force at its system time (match_entered_at): the latest version approved and in force
-- by then, or none. Otherwise as fin/000007.
CREATE OR REPLACE FUNCTION cargo_fin.check_creditor_match_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
  IF i.match_entered_at < clock_timestamp() - interval '1 minute' OR i.match_entered_at > clock_timestamp() + interval '1 minute' THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% was matched at % which is not within a minute of the database''s clock', NEW.tenant_id, NEW.invoice_id,
      i.match_entered_at USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_match_time_window';
  END IF;
  IF i.threshold_no IS DISTINCT FROM (SELECT x.threshold_no FROM cargo_fin.cost_variance_threshold x JOIN cargo_fin.cost_variance_threshold_decision d
       ON d.tenant_id = x.tenant_id AND d.legal_entity_id = x.legal_entity_id AND d.threshold_no = x.threshold_no
       WHERE x.tenant_id = i.tenant_id AND x.legal_entity_id = i.legal_entity_id AND d.kind = 'APPROVED' AND d.in_force_at <= i.match_entered_at
       ORDER BY x.threshold_no DESC LIMIT 1) THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% names another threshold than the one in force when it was matched', NEW.tenant_id, NEW.invoice_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_invoice_consistent';
  END IF;
  IF i.match_entered_at < (SELECT max(d.in_force_at) FROM cargo_fin.cost_variance_threshold_decision d JOIN cargo_fin.cost_variance_threshold x
       ON x.tenant_id = d.tenant_id AND x.legal_entity_id = d.legal_entity_id AND x.threshold_no = d.threshold_no
       WHERE d.tenant_id = i.tenant_id AND d.legal_entity_id = i.legal_entity_id AND d.kind = 'APPROVED' AND x.proposed_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cargo_fin: creditor invoice %/% was matched at % before a threshold version approved by then was in force', NEW.tenant_id, NEW.invoice_id,
      i.match_entered_at USING ERRCODE = 'check_violation', CONSTRAINT = 'creditor_match_time_monotone';
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
