-- IDP-Cargo SEA V2.100 (batch 2) / PostgreSQL 16. Run only as the migrator role, never at service start. SEA's settings
-- (the configuration of the rules, design sections 3 and 6) and the setting S1, a carrier report that contradicts SEA's
-- records. A change set proposes one or more values of SEA's settings, for the company or for a legal entity, with a
-- reason, and takes effect at a time not before it was proposed and not more than 366 days after. One that only
-- tightens is approved by its proposer when it is proposed; one that loosens a value waits for another person, who
-- approves it or refuses it. A change set not yet in force can be cancelled. The mechanism is fin/000017's, copied into
-- SEA's schema, so that SEA's settings stand apart from FIN's: SEA has its own ledger, its own versions and its own
-- lock. 000001 through 000011 are unchanged.
--
-- Added: four tables, each immutable (rows are only inserted), with row security enabled and forced and written at READ
-- COMMITTED: settings_change (a proposal), settings_change_entry (its values, one for each scope and item),
-- settings_approval (at most one for a change set) and settings_cancellation (at most one; of kind CANCELLED, or
-- REFUSED when an approver refused a proposal that was never approved; it records the role the canceller acted under,
-- CONFIGURER or APPROVER, and only an approver refuses). The approvals are numbered per tenant from 1 without a gap:
-- that number is the settings version. Changed: the result types of command_receipt gain 'SeaSettings', and the table
-- gains result_settings_version (the version an approval made, for a replay) and result_notice (what a command says
-- besides its receipt). carrier_conflict_release gains settings_version and s1_value: the version of SEA's settings and
-- the value of S1 the release was decided under, NULL both for a release written before this migration. No row changes
-- and nothing is dropped; each ALTER TABLE takes an ACCESS EXCLUSIVE lock on its table for its own duration, and the
-- new checks scan command_receipt and carrier_conflict_release once (reasoned, not measured); no column has a default,
-- so no table is rewritten.
--
-- The value order. The catalogue's items and values are listed here once, in the functions settings_rank (a value's
-- position among its item's, from strict to loose, NULL for a value the item does not have) and settings_default_rank
-- (the standard profile's value). A value or item added later comes with a migration that replaces them. The check on
-- an entry's value, and the database's own answer to "does this change set loosen" (settings_loosening), read them. No
-- item of SEA has a parameter yet, so the entry has no parameter column; an item that has one comes with a migration
-- that adds it (fin/000017's settings_entry_param and settings_looser_when_higher are the pattern).
--
-- What the database refuses: the approver is not the proposer (an approval by the proposer is the automatic one of a
-- change set that does not loosen, written with it); an automatic approval of a change set that the database finds
-- loosening, and a stored loosening flag that is not the database's answer, so that one person cannot write a loosening
-- alone: an entry loosens when its value is looser than the value in force at its scope at the effective time (its own,
-- else the company's, else the default, from the approved change sets not cancelled that are in force by then), or
-- looser than the company's value for a legal entity (the floor, the company's entries of the same change set counted);
-- an approval is not before the proposal and takes effect at the later of its time and the effective time; an effective
-- time is not before the proposal nor more than 366 days after it; one approval and one cancellation for a change set,
-- no approval after a cancellation and no cancellation of a change set in force; a refusal only of a change set not
-- approved; an entry's scope (the company has no ID, a legal entity has one), its item and value (the ranks above) and
-- a change set's entries (1 to 64, numbered without a gap, written with it). A release records its settings version (0
-- or more) and its S1 value (one of the two) together or neither.
--
-- The recorded times. A time written by a proposal, an approval or a cancellation must be within one minute of the
-- database's own clock at the moment of the write (clock_timestamp(), not the transaction's start, because a command
-- may wait for the tenant's lock after its transaction began): wide enough for the clock of SEA's host to differ from
-- the database's by what a time service leaves, narrow enough that a row cannot be back-dated by more than that; and it
-- must not be before the latest time recorded for the tenant in any of the three tables, so that history is written
-- forwards and what was answered for a moment in the past does not change. The tests run on one machine, where SEA's
-- clock and the database's are the same.
--
-- The tenant's lock. Every insert into the three tables takes the advisory lock that SEA's settings commands take first
-- (hashtextextended('cargo_sea.settings/' || tenant, 0)): the checks above read what other transactions committed, and
-- the application role may not update these rows, so it cannot lock them as SEA's other commit checks lock theirs. The
-- release of a carrier conflict takes the same lock in shared mode before it reads the settings; the database does not
-- check that.
--
-- What the schema does not know, and only the domain keeps: that the configuration a change set makes is valid (S1 has
-- no rule across items yet); that nothing approved alone comes to loosen when an earlier change set is cancelled; that
-- change sets take effect in the order of their versions; that a legal entity is one SEA knows (it has an HBL series or
-- a movement); that the approver holds the permission ApproveSettings and is a different person (the database compares
-- two strings); that an item may be set at a scope (S1 is at both); that the version and the value of S1 a release
-- records are the ones in force at the time it is entered, and that the release of a conflict under SUPERVISOR_ONLY was
-- made by a holder of the permission SuperviseCarrierConflicts (the database cannot know either).

CREATE FUNCTION cargo_sea.require_read_committed() RETURNS void LANGUAGE plpgsql AS $fn$
BEGIN
  IF current_setting('transaction_isolation') <> 'read committed' THEN
    RAISE EXCEPTION 'cargo_sea: SEA''s settings checks run at READ COMMITTED, not %', current_setting('transaction_isolation')
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sea_read_committed';
  END IF;
END $fn$;

CREATE FUNCTION cargo_sea.writes_read_committed() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_sea.require_read_committed();
  RETURN NULL;
END $fn$;

CREATE FUNCTION cargo_sea.settings_rank(item text, value text) RETURNS integer LANGUAGE sql IMMUTABLE AS $fn$
  SELECT v.r FROM (VALUES
    ('sea.S1', 'SUPERVISOR_ONLY', 0), ('sea.S1', 'WITH_REASON', 1)) AS v(i, x, r)
  WHERE v.i = item AND v.x = value
$fn$;

CREATE FUNCTION cargo_sea.settings_default_rank(item text) RETURNS integer LANGUAGE sql IMMUTABLE AS $fn$
  SELECT v.r FROM (VALUES ('sea.S1', 1)) AS v(i, r) WHERE v.i = item
$fn$;

CREATE TABLE cargo_sea.settings_change (
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

CREATE TABLE cargo_sea.settings_change_entry (
 tenant_id text NOT NULL,
 change_id text NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no BETWEEN 1 AND 64),
 scope_kind text NOT NULL,
 scope_id text NOT NULL,
 item_key text NOT NULL,
 value_id text NOT NULL,
 PRIMARY KEY (tenant_id, change_id, entry_no),
 CONSTRAINT settings_entry_once UNIQUE (tenant_id, change_id, scope_kind, scope_id, item_key),
 FOREIGN KEY (tenant_id, change_id) REFERENCES cargo_sea.settings_change (tenant_id, change_id),
 CONSTRAINT settings_entry_scope CHECK ((scope_kind = 'TENANT' AND scope_id = '')
   OR (scope_kind = 'LEGAL_ENTITY' AND length(btrim(scope_id)) BETWEEN 1 AND 128)),
 CONSTRAINT settings_entry_value_known CHECK (cargo_sea.settings_rank(item_key, value_id) IS NOT NULL)
);

CREATE TABLE cargo_sea.settings_approval (
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
   REFERENCES cargo_sea.settings_change (tenant_id, change_id, proposed_by, proposed_at, effective_at, loosening),
 CONSTRAINT settings_approval_other_actor CHECK (auto = (approved_by = proposed_by)),
 CONSTRAINT settings_approval_tightening_only CHECK (NOT (auto AND loosening)),
 CONSTRAINT settings_approval_automatic_at_proposal CHECK (NOT auto OR approved_at = proposed_at),
 CONSTRAINT settings_approval_after_proposal CHECK (approved_at >= proposed_at),
 CONSTRAINT settings_approval_in_force CHECK (in_force_at = greatest(effective_at, approved_at))
);

CREATE TABLE cargo_sea.settings_cancellation (
 tenant_id text NOT NULL,
 change_id text NOT NULL,
 cancelled_by text NOT NULL CHECK (length(btrim(cancelled_by)) BETWEEN 1 AND 128),
 cancelled_at timestamptz NOT NULL,
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 kind text NOT NULL DEFAULT 'CANCELLED' CHECK (kind IN ('CANCELLED','REFUSED')),
 -- the permission the canceller acted under: an approver may cancel a change set approved but not yet in force, and
 -- only an approver refuses
 cancelled_role text NOT NULL DEFAULT 'CONFIGURER' CONSTRAINT settings_cancellation_role CHECK (cancelled_role IN ('CONFIGURER','APPROVER')),
 CONSTRAINT settings_refusal_by_approver CHECK (kind <> 'REFUSED' OR cancelled_role = 'APPROVER'),
 PRIMARY KEY (tenant_id, change_id),
 FOREIGN KEY (tenant_id, change_id) REFERENCES cargo_sea.settings_change (tenant_id, change_id)
);

ALTER TABLE cargo_sea.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check
    CHECK (result_type IN ('OceanMovement','HblSeries','ContainerVisit','SeaSettings')),
  ADD COLUMN result_settings_version integer CONSTRAINT command_receipt_settings_version CHECK (result_settings_version > 0),
  ADD COLUMN result_notice text CONSTRAINT command_receipt_notice CHECK (length(result_notice) BETWEEN 1 AND 4000),
  ADD CONSTRAINT command_receipt_settings_of_settings
    CHECK ((result_settings_version IS NULL AND result_notice IS NULL) OR result_type = 'SeaSettings');

-- The settings a release was decided under: the version in force when it was entered (0 before any change set) and the
-- value of S1 that version gives for the movement's legal entity.
ALTER TABLE cargo_sea.carrier_conflict_release
  ADD COLUMN settings_version integer CONSTRAINT carrier_conflict_release_settings_version CHECK (settings_version >= 0),
  ADD COLUMN s1_value text CONSTRAINT carrier_conflict_release_s1_value CHECK (s1_value IN ('SUPERVISOR_ONLY','WITH_REASON')),
  ADD CONSTRAINT carrier_conflict_release_settings_together CHECK ((settings_version IS NULL) = (s1_value IS NULL));

DO $tables$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['settings_change','settings_change_entry','settings_approval','settings_cancellation'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_sea.%I FOR EACH ROW EXECUTE FUNCTION cargo_sea.reject_row_change()',
      tbl || '_immutable', tbl);
    EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON cargo_sea.%I FOR EACH STATEMENT EXECUTE FUNCTION cargo_sea.writes_read_committed()',
      tbl || '_read_committed', tbl);
    EXECUTE format('ALTER TABLE cargo_sea.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_sea.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_sea.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $tables$;

-- The entry of a scope in force at a time: the latest, by the time in force and then the version, of the entries of
-- approved change sets that are not cancelled and in force by then, leaving out one change set (the one asked about).
CREATE FUNCTION cargo_sea.settings_entry_in_force(t text, at timestamptz, except_id text, kind text, sid text, key text,
    OUT value_id text) LANGUAGE sql STABLE AS $fn$
  SELECT e.value_id FROM cargo_sea.settings_change_entry e
    JOIN cargo_sea.settings_approval a ON a.tenant_id = e.tenant_id AND a.change_id = e.change_id
    WHERE e.tenant_id = t AND e.change_id <> except_id AND e.scope_kind = kind AND e.scope_id = sid AND e.item_key = key
      AND a.in_force_at <= at
      AND NOT EXISTS (SELECT 1 FROM cargo_sea.settings_cancellation x WHERE x.tenant_id = e.tenant_id AND x.change_id = e.change_id)
    ORDER BY a.in_force_at DESC, a.version_no DESC LIMIT 1
$fn$;

-- Whether a change set loosens, as the domain decides it: some entry is looser than the value in force at its scope at
-- the effective time (its own, else the company's, else the default), or looser than the company's value for a legal
-- entity (the company's entry of the same change set, if it has one, else the company's value in force).
CREATE FUNCTION cargo_sea.settings_loosening(t text, cid text) RETURNS boolean LANGUAGE plpgsql STABLE AS $fn$
DECLARE
  c cargo_sea.settings_change;
  e cargo_sea.settings_change_entry;
  cur_value text;
  floor_value text;
  new_rank integer;
  cur_rank integer;
  floor_rank integer;
BEGIN
  SELECT * INTO c FROM cargo_sea.settings_change WHERE tenant_id = t AND change_id = cid;
  FOR e IN SELECT * FROM cargo_sea.settings_change_entry WHERE tenant_id = t AND change_id = cid LOOP
    new_rank := cargo_sea.settings_rank(e.item_key, e.value_id);
    SELECT f.value_id INTO cur_value FROM cargo_sea.settings_entry_in_force(t, c.effective_at, cid, e.scope_kind, e.scope_id, e.item_key) f;
    IF cur_value IS NULL AND e.scope_kind <> 'TENANT' THEN
      SELECT f.value_id INTO cur_value FROM cargo_sea.settings_entry_in_force(t, c.effective_at, cid, 'TENANT', '', e.item_key) f;
    END IF;
    cur_rank := coalesce(cargo_sea.settings_rank(e.item_key, cur_value), cargo_sea.settings_default_rank(e.item_key));
    IF new_rank > cur_rank THEN
      RETURN true;
    END IF;
    IF e.scope_kind <> 'TENANT' THEN
      SELECT x.value_id INTO floor_value FROM cargo_sea.settings_change_entry x
        WHERE x.tenant_id = t AND x.change_id = cid AND x.scope_kind = 'TENANT' AND x.item_key = e.item_key;
      IF floor_value IS NULL THEN
        SELECT f.value_id INTO floor_value FROM cargo_sea.settings_entry_in_force(t, c.effective_at, cid, 'TENANT', '', e.item_key) f;
      END IF;
      floor_rank := coalesce(cargo_sea.settings_rank(e.item_key, floor_value), cargo_sea.settings_default_rank(e.item_key));
      IF new_rank > floor_rank THEN
        RETURN true;
      END IF;
    END IF;
  END LOOP;
  RETURN false;
END $fn$;

-- At commit, a change set has 1 to 64 entries numbered from 1 without a gap, and an entry is written with its change
-- set.
CREATE FUNCTION cargo_sea.check_settings_change_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF (SELECT count(*) < 1 OR count(*) <> max(e.entry_no) FROM cargo_sea.settings_change_entry e
        WHERE e.tenant_id = NEW.tenant_id AND e.change_id = NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_sea: settings change set %/% has no entries or skips an entry number', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_change_entries';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER settings_change_entries AFTER INSERT ON cargo_sea.settings_change
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_settings_change_at_commit();

-- At commit, the stored loosening flag is the database's own answer.
CREATE FUNCTION cargo_sea.check_settings_flag_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.loosening IS DISTINCT FROM cargo_sea.settings_loosening(NEW.tenant_id, NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_sea: settings change set %/% is stored as %loosening, and the values in force say otherwise', NEW.tenant_id, NEW.change_id,
      CASE WHEN NEW.loosening THEN '' ELSE 'not ' END USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_change_loosening_flag';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER settings_change_loosening_flag AFTER INSERT ON cargo_sea.settings_change
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_settings_flag_at_commit();

CREATE FUNCTION cargo_sea.check_settings_entry_with_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_sea.settings_change c WHERE c.tenant_id = NEW.tenant_id AND c.change_id = NEW.change_id
       AND c.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_sea: settings entries are written with their change set' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'settings_entry_with_change';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER settings_entry_with_change AFTER INSERT ON cargo_sea.settings_change_entry
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_settings_entry_with_change();

-- A proposal takes the tenant's lock.
CREATE FUNCTION cargo_sea.lock_settings() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_sea.require_read_committed();
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_sea.settings/' || NEW.tenant_id, 0));
  RETURN NEW;
END $fn$;
CREATE TRIGGER settings_change_lock BEFORE INSERT ON cargo_sea.settings_change
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.lock_settings();

-- An approval: the tenant is locked (so that an approval and a cancellation of one change set take turns), the change
-- set is not cancelled, an automatic approval is written with its change set, and the database does not find that
-- change set loosening.
CREATE FUNCTION cargo_sea.check_settings_approval() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_sea.require_read_committed();
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_sea.settings/' || NEW.tenant_id, 0));
  IF EXISTS (SELECT 1 FROM cargo_sea.settings_cancellation x WHERE x.tenant_id = NEW.tenant_id AND x.change_id = NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_sea: settings change set %/% was cancelled; it is not approved', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_cancelled';
  END IF;
  IF NEW.auto AND NOT EXISTS (SELECT 1 FROM cargo_sea.settings_change c WHERE c.tenant_id = NEW.tenant_id AND c.change_id = NEW.change_id
       AND c.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_sea: an automatic approval is written with its change set' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'settings_approval_with_change';
  END IF;
  IF NEW.auto AND cargo_sea.settings_loosening(NEW.tenant_id, NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_sea: settings change set %/% loosens a value; another person approves it', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_loosening';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER settings_approval_check BEFORE INSERT ON cargo_sea.settings_approval
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_settings_approval();

-- A cancellation: the tenant is locked as an approval locks it, the cancellation is not before the proposal, a refusal
-- is of a change set not approved, and a cancellation is of a change set not in force (an approval takes effect at its
-- in_force_at).
CREATE FUNCTION cargo_sea.check_settings_cancellation() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_sea.require_read_committed();
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_sea.settings/' || NEW.tenant_id, 0));
  IF EXISTS (SELECT 1 FROM cargo_sea.settings_change c WHERE c.tenant_id = NEW.tenant_id AND c.change_id = NEW.change_id
       AND c.proposed_at > NEW.cancelled_at) THEN
    RAISE EXCEPTION 'cargo_sea: settings change set %/% is cancelled before it was proposed', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_cancellation_after_proposal';
  END IF;
  IF NEW.kind = 'REFUSED' AND EXISTS (SELECT 1 FROM cargo_sea.settings_approval a WHERE a.tenant_id = NEW.tenant_id AND a.change_id = NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_sea: settings change set %/% is approved; it is cancelled, not refused', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_refusal_approved';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_sea.settings_approval a WHERE a.tenant_id = NEW.tenant_id AND a.change_id = NEW.change_id
       AND a.in_force_at <= NEW.cancelled_at) THEN
    RAISE EXCEPTION 'cargo_sea: settings change set %/% is in force; a rollback is a new change set', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_cancellation_in_force';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER settings_cancellation_check BEFORE INSERT ON cargo_sea.settings_cancellation
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_settings_cancellation();

-- After each insert, the time written is within a minute of the database's clock and not before the latest time
-- recorded for the tenant in the three tables (the row itself counts, which only equals it).
CREATE FUNCTION cargo_sea.check_settings_time() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  at timestamptz;
  latest timestamptz;
BEGIN
  at := (to_jsonb(NEW) ->> CASE TG_TABLE_NAME WHEN 'settings_change' THEN 'proposed_at' WHEN 'settings_approval' THEN 'approved_at' ELSE 'cancelled_at' END)::timestamptz;
  IF at < clock_timestamp() - interval '1 minute' OR at > clock_timestamp() + interval '1 minute' THEN
    RAISE EXCEPTION 'cargo_sea: settings time % is not within a minute of the database''s clock', at
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_time_window';
  END IF;
  SELECT max(x.t) INTO latest FROM (
    SELECT proposed_at AS t FROM cargo_sea.settings_change WHERE tenant_id = NEW.tenant_id
    UNION ALL SELECT approved_at FROM cargo_sea.settings_approval WHERE tenant_id = NEW.tenant_id
    UNION ALL SELECT cancelled_at FROM cargo_sea.settings_cancellation WHERE tenant_id = NEW.tenant_id) x;
  IF at < latest THEN
    RAISE EXCEPTION 'cargo_sea: settings time % is before the latest recorded for tenant % (%)', at, NEW.tenant_id, latest
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_time_monotone';
  END IF;
  RETURN NULL;
END $fn$;
CREATE TRIGGER settings_change_time AFTER INSERT ON cargo_sea.settings_change FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_settings_time();
CREATE TRIGGER settings_approval_time AFTER INSERT ON cargo_sea.settings_approval FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_settings_time();
CREATE TRIGGER settings_cancellation_time AFTER INSERT ON cargo_sea.settings_cancellation FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_settings_time();

-- At commit, a tenant's approvals are numbered from 1 without a gap.
CREATE FUNCTION cargo_sea.check_settings_versions_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF (SELECT count(*) <> max(a.version_no) FROM cargo_sea.settings_approval a WHERE a.tenant_id = NEW.tenant_id) THEN
    RAISE EXCEPTION 'cargo_sea: settings versions of tenant % skip a number', NEW.tenant_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_versions_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER settings_versions_consistent AFTER INSERT ON cargo_sea.settings_approval
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_settings_versions_at_commit();
