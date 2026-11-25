-- IDP-Cargo FIN V2.96 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The settings mechanism and FIN's settings (step C1a of the configuration of the rules). A change set proposes one or
-- more values of FIN's settings, for the company or for a legal entity, with a reason, and takes effect at a time not
-- before it was proposed and not more than 366 days after. One that only tightens is approved by its proposer when it
-- is proposed; one that loosens a value waits for another person, who approves it or refuses it. A change set not yet
-- in force can be cancelled. FIN's rules do not read the settings yet: this step stores, changes and answers them.
--
-- Added: four tables, each immutable (rows are only inserted), with row security enabled and forced and written at READ
-- COMMITTED like every table of cargo_fin: settings_change (a proposal), settings_change_entry (its values, one for
-- each scope and item), settings_approval (at most one for a change set) and settings_cancellation (at most one; of
-- kind CANCELLED, or REFUSED when an approver refused a proposal that was never approved; it records the role the canceller
-- acted under, CONFIGURER or APPROVER, and only an approver refuses). The approvals are numbered
-- per tenant from 1 without a gap: that number is the settings version. Changed: the result types of command_receipt
-- gain 'FinSettings', and the table gains result_notice (what a command says besides its receipt). No row changes and
-- nothing is dropped; the ALTER TABLE takes an ACCESS EXCLUSIVE lock on command_receipt for its brief check of the
-- existing rows (reasoned, not measured).
--
-- The value order. The catalogue's items and values are listed here once, in the functions settings_rank (a value's
-- position among its item's, from strict to loose, NULL for a value the item does not have), settings_default_rank (the
-- standard profile's value) and settings_looser_when_higher (F1's threshold lets more through when higher). A value or
-- item added later comes with a migration that replaces them. The check on an entry's value, and the database's own
-- answer to "does this change set loosen" (settings_loosening), read them.
--
-- What the database refuses: the approver is not the proposer (an approval by the proposer is the automatic one of a
-- change set that does not loosen, written with it); an automatic approval of a change set that the database finds
-- loosening, and a stored loosening flag that is not the database's answer, so that one person cannot write a loosening
-- alone: an entry loosens when its value is looser than the value in force at its scope at the effective time (its own,
-- else the company's, else the default, from the approved change sets not cancelled that are in force by then), or
-- looser than the company's value for a legal entity (the floor, the company's entries of the same change set counted),
-- or it raises a threshold; an approval is not before the proposal and takes effect at the later of its time and the
-- effective time; an effective time is not before the proposal nor more than 366 days after it; one approval and one
-- cancellation for a change set, no approval after a cancellation and no cancellation of a change set in force; a
-- refusal only of a change set not approved; an entry's scope (the company has no ID, a legal entity has one), its item
-- and value (the ranks above), its parameter (only F1's threshold has one, in 1 to 10^15 minor units, and only for a
-- legal entity, whose functional currency it is in) and a change set's entries (1 to 64, numbered without a gap,
-- written with it).
--
-- The recorded times. A time written by a proposal, an approval or a cancellation must be within one minute of the
-- database's own clock at the moment of the write (clock_timestamp(), not the transaction's start, because a command
-- may wait for the tenant's lock after its transaction began): wide enough for the clock of FIN's host to differ from
-- the database's by what a time service leaves, narrow enough that a row cannot be back-dated by more than that; and it
-- must not be before the latest time recorded for the tenant in any of the three tables, so that history is written
-- forwards and what was answered for a moment in the past does not change. The tests run on one machine, where FIN's
-- clock and the database's are the same.
--
-- The tenant's lock. Every insert into the three tables takes the advisory lock that FIN's commands take first
-- (hashtextextended('cargo_fin.settings/' || tenant, 0)): the checks above read what other transactions committed, and
-- the application role may not update these rows, so it cannot lock them as FIN's other commit checks lock theirs.
--
-- What the schema does not know, and only the domain keeps: that the configuration a change set makes is valid and
-- keeps F8's rule (F8 DEFAULT_TO_REMAINING only with F3 ONLY_WHEN_AMOUNT_MOVES), for each scope it touches and each
-- later change set; that nothing approved alone comes to loosen when an earlier change set is cancelled; that change
-- sets take effect in the order of their versions; that a legal entity has a finance policy; that an amount is in the
-- functional currency; that the approver holds the permission ApproveSettings and is a different person (the database
-- compares two strings); and that an item may be set at a scope (every FIN item is at both).

CREATE FUNCTION cargo_fin.settings_rank(item text, value text) RETURNS integer LANGUAGE sql IMMUTABLE AS $fn$
  SELECT v.r FROM (VALUES
    ('fin.F1', 'NOT_ALLOWED', 0), ('fin.F1', 'NOT_ABOVE_THRESHOLD', 1), ('fin.F1', 'UNRESTRICTED', 2),
    ('fin.F2', 'SUBMITTER_AND_PREVIEWER', 0), ('fin.F2', 'SUBMITTER_ONLY', 1),
    ('fin.F3', 'EVERY_ATTRIBUTION', 0), ('fin.F3', 'ONLY_WHEN_AMOUNT_MOVES', 1),
    ('fin.F4', 'ALL_PREVIEWED', 0), ('fin.F4', 'ONE_STEP_WITHOUT_OPENED_OR_LEFT', 1),
    ('fin.F6', 'SEPARATE', 0), ('fin.F6', 'INCLUDED_MARKED', 1),
    ('fin.F7', 'BLOCKS', 0), ('fin.F7', 'DOES_NOT_BLOCK', 1),
    ('fin.F8', 'REJECT_WITH_DEFAULT', 0), ('fin.F8', 'DEFAULT_TO_REMAINING', 1),
    ('fin.F9', 'RECONFIRM', 0), ('fin.F9', 'KEEP_RATIO', 1), ('fin.F9', 'KEEP_AMOUNT', 2)) AS v(i, x, r)
  WHERE v.i = item AND v.x = value
$fn$;

CREATE FUNCTION cargo_fin.settings_default_rank(item text) RETURNS integer LANGUAGE sql IMMUTABLE AS $fn$
  SELECT v.r FROM (VALUES ('fin.F1', 0), ('fin.F2', 1), ('fin.F3', 0), ('fin.F4', 0), ('fin.F6', 0), ('fin.F7', 1), ('fin.F8', 0), ('fin.F9', 2))
    AS v(i, r) WHERE v.i = item
$fn$;

CREATE FUNCTION cargo_fin.settings_looser_when_higher(item text, value text) RETURNS boolean LANGUAGE sql IMMUTABLE AS $fn$
  SELECT item = 'fin.F1' AND value = 'NOT_ABOVE_THRESHOLD'
$fn$;

CREATE TABLE cargo_fin.settings_change (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 change_id text NOT NULL CHECK (change_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$'),
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 proposed_by text NOT NULL CHECK (length(btrim(proposed_by)) BETWEEN 1 AND 128),
 proposed_at timestamptz NOT NULL,
 effective_at timestamptz NOT NULL,
 loosening boolean NOT NULL,
 PRIMARY KEY (tenant_id, change_id),
 UNIQUE (tenant_id, change_id, proposed_by, proposed_at, effective_at, loosening),
 CONSTRAINT settings_change_effective CHECK (effective_at >= proposed_at),
 CONSTRAINT settings_change_effective_limit CHECK (effective_at <= proposed_at + interval '366 days')
);

CREATE TABLE cargo_fin.settings_change_entry (
 tenant_id text NOT NULL,
 change_id text NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no BETWEEN 1 AND 64),
 scope_kind text NOT NULL,
 scope_id text NOT NULL,
 item_key text NOT NULL,
 value_id text NOT NULL,
 param_minor bigint,
 PRIMARY KEY (tenant_id, change_id, entry_no),
 CONSTRAINT settings_entry_once UNIQUE (tenant_id, change_id, scope_kind, scope_id, item_key),
 FOREIGN KEY (tenant_id, change_id) REFERENCES cargo_fin.settings_change (tenant_id, change_id),
 CONSTRAINT settings_entry_scope CHECK ((scope_kind = 'TENANT' AND scope_id = '')
   OR (scope_kind = 'LEGAL_ENTITY' AND length(btrim(scope_id)) BETWEEN 1 AND 128)),
 CONSTRAINT settings_entry_value_known CHECK (cargo_fin.settings_rank(item_key, value_id) IS NOT NULL),
 CONSTRAINT settings_entry_param CHECK ((param_minor IS NOT NULL) = (item_key = 'fin.F1' AND value_id = 'NOT_ABOVE_THRESHOLD')),
 CONSTRAINT settings_entry_param_range CHECK (param_minor IS NULL OR (param_minor BETWEEN 1 AND 1000000000000000 AND scope_kind = 'LEGAL_ENTITY'))
);

CREATE TABLE cargo_fin.settings_approval (
 tenant_id text NOT NULL,
 change_id text NOT NULL,
 -- the proposal's own columns, so that the rules about the pair of rows hold here as checks
 proposed_by text NOT NULL,
 proposed_at timestamptz NOT NULL,
 effective_at timestamptz NOT NULL,
 loosening boolean NOT NULL,
 approved_by text NOT NULL CHECK (length(btrim(approved_by)) BETWEEN 1 AND 128),
 approved_at timestamptz NOT NULL,
 -- the settings version this approval makes
 version_no integer NOT NULL CHECK (version_no > 0),
 in_force_at timestamptz NOT NULL,
 auto boolean NOT NULL,
 PRIMARY KEY (tenant_id, change_id),
 CONSTRAINT settings_approval_version_once UNIQUE (tenant_id, version_no),
 CONSTRAINT settings_approval_matches_change FOREIGN KEY (tenant_id, change_id, proposed_by, proposed_at, effective_at, loosening)
   REFERENCES cargo_fin.settings_change (tenant_id, change_id, proposed_by, proposed_at, effective_at, loosening),
 CONSTRAINT settings_approval_other_actor CHECK (auto = (approved_by = proposed_by)),
 CONSTRAINT settings_approval_tightening_only CHECK (NOT (auto AND loosening)),
 CONSTRAINT settings_approval_automatic_at_proposal CHECK (NOT auto OR approved_at = proposed_at),
 CONSTRAINT settings_approval_after_proposal CHECK (approved_at >= proposed_at),
 CONSTRAINT settings_approval_in_force CHECK (in_force_at = greatest(effective_at, approved_at))
);

CREATE TABLE cargo_fin.settings_cancellation (
 tenant_id text NOT NULL,
 change_id text NOT NULL,
 cancelled_by text NOT NULL CHECK (length(btrim(cancelled_by)) BETWEEN 1 AND 128),
 cancelled_at timestamptz NOT NULL,
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 kind text NOT NULL DEFAULT 'CANCELLED' CHECK (kind IN ('CANCELLED','REFUSED')),
 -- the permission the canceller acted under: an approver may cancel a change set approved but not yet in force, and only an
 -- approver refuses
 cancelled_role text NOT NULL DEFAULT 'CONFIGURER' CONSTRAINT settings_cancellation_role CHECK (cancelled_role IN ('CONFIGURER','APPROVER')),
 CONSTRAINT settings_refusal_by_approver CHECK (kind <> 'REFUSED' OR cancelled_role = 'APPROVER'),
 PRIMARY KEY (tenant_id, change_id),
 FOREIGN KEY (tenant_id, change_id) REFERENCES cargo_fin.settings_change (tenant_id, change_id)
);

ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check
    CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile','DebtorReceipt','ConsolCostPool','CreditorInvoice',
      'CreditorPayment','FapiaoRecord','FinSettings')),
  ADD COLUMN result_notice text CONSTRAINT command_receipt_notice CHECK (length(result_notice) BETWEEN 1 AND 4000);

DO $tables$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['settings_change','settings_change_entry','settings_approval','settings_cancellation'] LOOP
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

-- The entry of a scope in force at a time: the latest, by the time in force and then the version, of the entries of
-- approved change sets that are not cancelled and in force by then, leaving out one change set (the one asked about).
CREATE FUNCTION cargo_fin.settings_entry_in_force(t text, at timestamptz, except_id text, kind text, sid text, key text,
    OUT value_id text, OUT param_minor bigint) LANGUAGE sql STABLE AS $fn$
  SELECT e.value_id, e.param_minor FROM cargo_fin.settings_change_entry e
    JOIN cargo_fin.settings_approval a ON a.tenant_id = e.tenant_id AND a.change_id = e.change_id
    WHERE e.tenant_id = t AND e.change_id <> except_id AND e.scope_kind = kind AND e.scope_id = sid AND e.item_key = key
      AND a.in_force_at <= at
      AND NOT EXISTS (SELECT 1 FROM cargo_fin.settings_cancellation x WHERE x.tenant_id = e.tenant_id AND x.change_id = e.change_id)
    ORDER BY a.in_force_at DESC, a.version_no DESC LIMIT 1
$fn$;

-- Whether a change set loosens, as the domain decides it: some entry is looser than the value in force at its scope at
-- the effective time (its own, else the company's, else the default), or the same value with a higher threshold, or
-- looser than the company's value for a legal entity (the company's entry of the same change set, if it has one, else
-- the company's value in force).
CREATE FUNCTION cargo_fin.settings_loosening(t text, cid text) RETURNS boolean LANGUAGE plpgsql STABLE AS $fn$
DECLARE
  c cargo_fin.settings_change;
  e cargo_fin.settings_change_entry;
  cur record;
  floor_value text;
  new_rank integer;
  cur_rank integer;
  floor_rank integer;
BEGIN
  SELECT * INTO c FROM cargo_fin.settings_change WHERE tenant_id = t AND change_id = cid;
  FOR e IN SELECT * FROM cargo_fin.settings_change_entry WHERE tenant_id = t AND change_id = cid LOOP
    new_rank := cargo_fin.settings_rank(e.item_key, e.value_id);
    SELECT * INTO cur FROM cargo_fin.settings_entry_in_force(t, c.effective_at, cid, e.scope_kind, e.scope_id, e.item_key);
    IF cur.value_id IS NULL AND e.scope_kind <> 'TENANT' THEN
      SELECT * INTO cur FROM cargo_fin.settings_entry_in_force(t, c.effective_at, cid, 'TENANT', '', e.item_key);
    END IF;
    cur_rank := coalesce(cargo_fin.settings_rank(e.item_key, cur.value_id), cargo_fin.settings_default_rank(e.item_key));
    IF new_rank > cur_rank OR (new_rank = cur_rank AND cargo_fin.settings_looser_when_higher(e.item_key, e.value_id)
        AND e.param_minor IS NOT NULL AND cur.param_minor IS NOT NULL AND e.param_minor > cur.param_minor) THEN
      RETURN true;
    END IF;
    IF e.scope_kind <> 'TENANT' THEN
      SELECT x.value_id INTO floor_value FROM cargo_fin.settings_change_entry x
        WHERE x.tenant_id = t AND x.change_id = cid AND x.scope_kind = 'TENANT' AND x.item_key = e.item_key;
      IF floor_value IS NULL THEN
        SELECT f.value_id INTO floor_value FROM cargo_fin.settings_entry_in_force(t, c.effective_at, cid, 'TENANT', '', e.item_key) f;
      END IF;
      floor_rank := coalesce(cargo_fin.settings_rank(e.item_key, floor_value), cargo_fin.settings_default_rank(e.item_key));
      IF new_rank > floor_rank THEN
        RETURN true;
      END IF;
    END IF;
  END LOOP;
  RETURN false;
END $fn$;

-- At commit, a change set has 1 to 64 entries numbered from 1 without a gap, and an entry is written with its change
-- set.
CREATE FUNCTION cargo_fin.check_settings_change_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF (SELECT count(*) < 1 OR count(*) <> max(e.entry_no) FROM cargo_fin.settings_change_entry e
        WHERE e.tenant_id = NEW.tenant_id AND e.change_id = NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% has no entries or skips an entry number', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_change_entries';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER settings_change_entries AFTER INSERT ON cargo_fin.settings_change
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_change_at_commit();

-- At commit, the stored loosening flag is the database's own answer.
CREATE FUNCTION cargo_fin.check_settings_flag_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.loosening IS DISTINCT FROM cargo_fin.settings_loosening(NEW.tenant_id, NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% is stored as %loosening, and the values in force say otherwise', NEW.tenant_id, NEW.change_id,
      CASE WHEN NEW.loosening THEN '' ELSE 'not ' END USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_change_loosening_flag';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER settings_change_loosening_flag AFTER INSERT ON cargo_fin.settings_change
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_flag_at_commit();

CREATE FUNCTION cargo_fin.check_settings_entry_with_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.settings_change c WHERE c.tenant_id = NEW.tenant_id AND c.change_id = NEW.change_id
       AND c.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: settings entries are written with their change set' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'settings_entry_with_change';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER settings_entry_with_change AFTER INSERT ON cargo_fin.settings_change_entry
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_entry_with_change();

-- A proposal takes the tenant's lock.
CREATE FUNCTION cargo_fin.lock_settings() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.require_read_committed();
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_fin.settings/' || NEW.tenant_id, 0));
  RETURN NEW;
END $fn$;
CREATE TRIGGER settings_change_lock BEFORE INSERT ON cargo_fin.settings_change
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.lock_settings();

-- An approval: the tenant is locked (so that an approval and a cancellation of one change set take turns), the change
-- set is not cancelled, an automatic approval is written with its change set, and the database does not find that
-- change set loosening.
CREATE FUNCTION cargo_fin.check_settings_approval() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.require_read_committed();
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_fin.settings/' || NEW.tenant_id, 0));
  IF EXISTS (SELECT 1 FROM cargo_fin.settings_cancellation x WHERE x.tenant_id = NEW.tenant_id AND x.change_id = NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% was cancelled; it is not approved', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_cancelled';
  END IF;
  IF NEW.auto AND NOT EXISTS (SELECT 1 FROM cargo_fin.settings_change c WHERE c.tenant_id = NEW.tenant_id AND c.change_id = NEW.change_id
       AND c.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: an automatic approval is written with its change set' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'settings_approval_with_change';
  END IF;
  IF NEW.auto AND cargo_fin.settings_loosening(NEW.tenant_id, NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% loosens a value; another person approves it', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_loosening';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER settings_approval_check BEFORE INSERT ON cargo_fin.settings_approval
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_approval();

-- A cancellation: the tenant is locked as an approval locks it, the cancellation is not before the proposal, a refusal
-- is of a change set not approved, and a cancellation is of a change set not in force (an approval takes effect at its
-- in_force_at).
CREATE FUNCTION cargo_fin.check_settings_cancellation() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.require_read_committed();
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_fin.settings/' || NEW.tenant_id, 0));
  IF EXISTS (SELECT 1 FROM cargo_fin.settings_change c WHERE c.tenant_id = NEW.tenant_id AND c.change_id = NEW.change_id
       AND c.proposed_at > NEW.cancelled_at) THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% is cancelled before it was proposed', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_cancellation_after_proposal';
  END IF;
  IF NEW.kind = 'REFUSED' AND EXISTS (SELECT 1 FROM cargo_fin.settings_approval a WHERE a.tenant_id = NEW.tenant_id AND a.change_id = NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% is approved; it is cancelled, not refused', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_refusal_approved';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_fin.settings_approval a WHERE a.tenant_id = NEW.tenant_id AND a.change_id = NEW.change_id
       AND a.in_force_at <= NEW.cancelled_at) THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% is in force; a rollback is a new change set', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_cancellation_in_force';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER settings_cancellation_check BEFORE INSERT ON cargo_fin.settings_cancellation
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_cancellation();

-- After each insert, the time written is within a minute of the database's clock and not before the latest time
-- recorded for the tenant in the three tables (the row itself counts, which only equals it).
CREATE FUNCTION cargo_fin.check_settings_time() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  at timestamptz;
  latest timestamptz;
BEGIN
  at := (to_jsonb(NEW) ->> CASE TG_TABLE_NAME WHEN 'settings_change' THEN 'proposed_at' WHEN 'settings_approval' THEN 'approved_at' ELSE 'cancelled_at' END)::timestamptz;
  IF at < clock_timestamp() - interval '1 minute' OR at > clock_timestamp() + interval '1 minute' THEN
    RAISE EXCEPTION 'cargo_fin: settings time % is not within a minute of the database''s clock', at
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_time_window';
  END IF;
  SELECT max(x.t) INTO latest FROM (
    SELECT proposed_at AS t FROM cargo_fin.settings_change WHERE tenant_id = NEW.tenant_id
    UNION ALL SELECT approved_at FROM cargo_fin.settings_approval WHERE tenant_id = NEW.tenant_id
    UNION ALL SELECT cancelled_at FROM cargo_fin.settings_cancellation WHERE tenant_id = NEW.tenant_id) x;
  IF at < latest THEN
    RAISE EXCEPTION 'cargo_fin: settings time % is before the latest recorded for tenant % (%)', at, NEW.tenant_id, latest
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_time_monotone';
  END IF;
  RETURN NULL;
END $fn$;
CREATE TRIGGER settings_change_time AFTER INSERT ON cargo_fin.settings_change FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_time();
CREATE TRIGGER settings_approval_time AFTER INSERT ON cargo_fin.settings_approval FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_time();
CREATE TRIGGER settings_cancellation_time AFTER INSERT ON cargo_fin.settings_cancellation FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_time();

-- At commit, a tenant's approvals are numbered from 1 without a gap.
CREATE FUNCTION cargo_fin.check_settings_versions_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF (SELECT count(*) <> max(a.version_no) FROM cargo_fin.settings_approval a WHERE a.tenant_id = NEW.tenant_id) THEN
    RAISE EXCEPTION 'cargo_fin: settings versions of tenant % skip a number', NEW.tenant_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_versions_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER settings_versions_consistent AFTER INSERT ON cargo_fin.settings_approval
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_versions_at_commit();
