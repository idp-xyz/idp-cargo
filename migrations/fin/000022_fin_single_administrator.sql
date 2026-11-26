-- IDP-Cargo FIN V2.102 (batches A, D, E and F) / PostgreSQL 16. Run only as the migrator role, never at service start. A single
-- administrator (design section 9, item 3): a tenant whose onboarding declared that it has one administrator may have a loosening
-- of FIN's settings approved by its own proposer, but only with a written risk acknowledgement in the approval and the declaration
-- it relies on. A tenant has one single administrator in force: the one named by its latest declaration. The customer's
-- onboarding (batch F) holds its change sets until the owner confirms it, and the self-approvals are reviewed one by one.
-- 000001 through 000017 are unchanged.
--
-- Added: the table single_administrator, the declaration; the table settings_hold, which says that a change set is held for an
-- onboarding; the table settings_self_approval_review, the review of one self-approval; and on settings_approval the columns
-- risk_acknowledgement and basis_onboarding_id.
-- A declaration names the tenant, the onboarding that made it (ORG's record, which FIN does not read), the consultant
-- who proposed that onboarding and the customer's owner who confirmed it (two different people), the administrator the
-- owner named (one person, who is not the consultant; the owner may name themselves), and when FIN recorded it. It is only inserted (immutable, row security enabled and forced, written at READ COMMITTED) and is written by
-- FIN's command DeclareFinSingleAdministrator, which the platform's bootstrap calls once the owner has confirmed the
-- onboarding; a tenant has one declaration for each onboarding that made one, and the latest
-- names the single administrator in force: a later onboarding that declares another administrator replaces the earlier one from its
-- declaration's time, and the earlier administrator may no longer approve their own loosening; an onboarding that makes none changes
-- nothing. The order of a tenant's declarations is total because no two have the same time (the unique key
-- single_administrator_time_once on the tenant and declared_at; the command refuses a declaration not later than the tenant's latest
-- and the database refuses two at one instant). There is no way to withdraw one yet.
--
-- How the database knows the declaration. FIN's schema holds it: the approval's trigger reads single_administrator in
-- the same schema, so no read crosses a schema and the roles stay as they are. What the database cannot know is that
-- ORG's onboarding exists and was confirmed by the owner it names: FIN's command takes it on the word of its caller,
-- which holds a permission granted by name only (DeclareSingleAdministrator) that the bootstrap's onboarding alone is
-- given: the bootstrap's authorizer grants it only to a context that carries the platform's own mark, which only the principal
-- the onboarding builds has, so a token that carries the permission's name is not enough; ConfigureSettings does not grant it.
--
-- Changed: the approval's rule "the approver is the proposer exactly when the approval is the automatic one of a
-- change set that only tightens" (settings_approval_other_actor, the same name) now also lets the approver be the
-- proposer when the approval carries a risk acknowledgement, and the trigger lets that happen only when the approver is
-- the administrator the tenant's latest declaration names; the acknowledgement is of 1 to 2000 characters, only on
-- such an approval, and only of a change set that loosens (settings_approval_acknowledgement_*). The approval's trigger
-- refuses such an approval unless the approver is the administrator that the tenant's latest declaration at or before the
-- approval names, so a tenant with none, or whose latest names another person, is refused (settings_approval_single_administrator; it looks at the acknowledgement, so that a proposer's approval without one is still
-- refused by settings_approval_other_actor, as before).
--
-- The basis (batch F). Each such approval records, beside its acknowledgement, the onboarding of the declaration it relies on
-- (basis_onboarding_id; present exactly when the acknowledgement is: settings_approval_basis_with_acknowledgement), and the
-- trigger refuses an approval whose basis is not the onboarding of the tenant's latest declaration at or before the approval
-- (settings_approval_basis), so that the record says which declaration, with its administrator and time, the exception stands on.
--
-- Held change sets (batch F). The onboarding's change set is proposed by the consultant, in the consultant's own name, when
-- the consultant proposes the onboarding, and approved by the owner, in the owner's own name, when the owner confirms it. Until
-- then it is held: a row of settings_hold names the onboarding (ORG's record, which FIN does not read). The database refuses
-- for a held change set: an automatic approval, which a change set that only tightens would have (settings_approval_held_auto),
-- an approval by its proposer, with or without an acknowledgement (settings_approval_held_proposer), and a hold written after
-- the change set was approved or in another transaction than its proposal (settings_hold_unapproved, settings_hold_with_change).
-- While a change set held for an onboarding is neither approved nor cancelled, the database refuses every other change set of the
-- tenant, under the tenant's lock: an approval of one that is not held (settings_approval_onboarding_waiting), a proposal of one
-- that is not held (settings_onboarding_waiting, checked at commit, when the change set's own hold, if it has one, is written),
-- and a hold for another onboarding (settings_hold_other_onboarding). A change set that was cancelled or refused no longer
-- waits; the way out of a held change set is to confirm the onboarding or withdraw it, which cancels the held change sets.
--
-- The review of a self-approval (batch F). A loosening that its own proposer approved as the single administrator is reviewed once
-- by someone else, with a conclusion, ACCEPTED or NOT_ACCEPTED, and a note: a row of settings_self_approval_review, only
-- inserted, with row security enabled and forced. The database refuses a second review of a change set (the primary key), a
-- review of a change set that no single administrator approved (settings_review_self_approved, which reads the approval's
-- acknowledgement), a reviewer who is the approver (settings_review_reviewer, a check on the approver the row repeats in a foreign
-- key to the approval), a review before the approval (settings_review_after_approval), a conclusion that is not one of the two
-- (settings_review_conclusion), a NOT_ACCEPTED without a note (settings_review_note_required), a note of other than 1 to 2000
-- characters (settings_review_note_length), and a reviewer or a tenant that is blank, has spaces around it or is too long. A NOT_ACCEPTED changes no setting: undoing
-- the change is a normal change set.
--
-- Everything else about approvals is as 000017 has it. The ALTER TABLE takes an ACCESS EXCLUSIVE lock on settings_approval for its
-- own duration and checks its rows once (reasoned, not measured); no column has a default, so no table is rewritten.
--
-- What the schema does not know, and only the domain keeps: that the approver holds the permission ApproveSettings and the reviewer
-- the permission ReviewSettingsExceptions; that the declaration comes from a confirmed onboarding and names the administrator that
-- the owner named there; that the onboarding a change set is held for exists, whom it was proposed by, and whether it is confirmed
-- or withdrawn (ORG's record: FIN reads only its own change sets, so a held change set waits until it is approved or cancelled); that a
-- held change set is approved only by the platform's onboarding in the owner's name; that the acknowledgement is not blank beyond
-- its length (the database checks the trimmed length).

CREATE TABLE cargo_fin.single_administrator (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 onboarding_id text NOT NULL CHECK (onboarding_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$'),
 proposed_by text NOT NULL CHECK (length(btrim(proposed_by)) BETWEEN 1 AND 128),
 confirmed_by text NOT NULL CHECK (length(btrim(confirmed_by)) BETWEEN 1 AND 128),
 administrator text NOT NULL CHECK (length(btrim(administrator)) BETWEEN 1 AND 128),
 declared_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, onboarding_id),
 CONSTRAINT single_administrator_two_people CHECK (confirmed_by <> proposed_by),
 CONSTRAINT single_administrator_not_the_consultant CHECK (administrator <> proposed_by),
 CONSTRAINT single_administrator_time_once UNIQUE (tenant_id, declared_at)
);
CREATE TRIGGER single_administrator_immutable BEFORE UPDATE OR DELETE ON cargo_fin.single_administrator
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
CREATE TRIGGER single_administrator_read_committed BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.single_administrator
  FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed();
ALTER TABLE cargo_fin.single_administrator ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.single_administrator FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.single_administrator USING (tenant_id = current_setting('app.tenant_id',true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id',true));

ALTER TABLE cargo_fin.settings_approval
  ADD COLUMN risk_acknowledgement text,
  ADD COLUMN basis_onboarding_id text,
  ADD CONSTRAINT settings_approval_acknowledgement_length CHECK (risk_acknowledgement IS NULL OR char_length(btrim(risk_acknowledgement)) BETWEEN 1 AND 2000),
  ADD CONSTRAINT settings_approval_acknowledgement_self CHECK (risk_acknowledgement IS NULL OR (NOT auto AND approved_by = proposed_by)),
  ADD CONSTRAINT settings_approval_acknowledgement_loosening CHECK (risk_acknowledgement IS NULL OR loosening),
  ADD CONSTRAINT settings_approval_basis_with_acknowledgement CHECK ((basis_onboarding_id IS NULL) = (risk_acknowledgement IS NULL)),
  ADD CONSTRAINT settings_approval_for_review UNIQUE (tenant_id, change_id, approved_by),
  DROP CONSTRAINT settings_approval_other_actor,
  ADD CONSTRAINT settings_approval_other_actor CHECK ((approved_by = proposed_by) = (auto OR risk_acknowledgement IS NOT NULL));

-- A change set held for an onboarding: written with the change set, never changed.
CREATE TABLE cargo_fin.settings_hold (
 tenant_id text NOT NULL,
 change_id text NOT NULL,
 onboarding_id text NOT NULL CONSTRAINT settings_hold_onboarding_shape CHECK (onboarding_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,99}$'),
 PRIMARY KEY (tenant_id, change_id),
 FOREIGN KEY (tenant_id, change_id) REFERENCES cargo_fin.settings_change (tenant_id, change_id)
);
CREATE TRIGGER settings_hold_immutable BEFORE UPDATE OR DELETE ON cargo_fin.settings_hold
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
CREATE TRIGGER settings_hold_read_committed BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.settings_hold
  FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed();
ALTER TABLE cargo_fin.settings_hold ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.settings_hold FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.settings_hold USING (tenant_id = current_setting('app.tenant_id',true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id',true));

-- Whether a change set of the tenant is held for an onboarding and neither approved nor cancelled, other than the one named
-- (the onboarding that waits, or NULL).
CREATE FUNCTION cargo_fin.settings_waiting_onboarding(t text, except_id text) RETURNS text LANGUAGE sql STABLE AS $fn$
  SELECT h.onboarding_id FROM cargo_fin.settings_hold h
    WHERE h.tenant_id = t AND h.change_id <> except_id
      AND NOT EXISTS (SELECT 1 FROM cargo_fin.settings_approval a WHERE a.tenant_id = h.tenant_id AND a.change_id = h.change_id)
      AND NOT EXISTS (SELECT 1 FROM cargo_fin.settings_cancellation x WHERE x.tenant_id = h.tenant_id AND x.change_id = h.change_id)
    ORDER BY h.change_id LIMIT 1
$fn$;

CREATE FUNCTION cargo_fin.check_settings_hold() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE waiting text;
BEGIN
  PERFORM cargo_fin.require_read_committed();
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_fin.settings/' || NEW.tenant_id, 0));
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.settings_change c WHERE c.tenant_id = NEW.tenant_id AND c.change_id = NEW.change_id
       AND c.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: a settings hold is written with its change set' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'settings_hold_with_change';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_fin.settings_approval a WHERE a.tenant_id = NEW.tenant_id AND a.change_id = NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% is approved, so it is not held for an onboarding', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_hold_unapproved';
  END IF;
  waiting := cargo_fin.settings_waiting_onboarding(NEW.tenant_id, NEW.change_id);
  IF waiting IS NOT NULL AND waiting <> NEW.onboarding_id THEN
    RAISE EXCEPTION 'cargo_fin: onboarding % of tenant % waits with a change set held for it; confirm or withdraw it before holding a change set for onboarding %',
      waiting, NEW.tenant_id, NEW.onboarding_id USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_hold_other_onboarding';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER settings_hold_check BEFORE INSERT ON cargo_fin.settings_hold
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_hold();

-- At commit, a change set that is not held is not proposed while a change set held for an onboarding waits.
CREATE FUNCTION cargo_fin.check_settings_waiting_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE waiting text;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.settings_hold h WHERE h.tenant_id = NEW.tenant_id AND h.change_id = NEW.change_id) THEN
    waiting := cargo_fin.settings_waiting_onboarding(NEW.tenant_id, NEW.change_id);
    IF waiting IS NOT NULL THEN
      RAISE EXCEPTION 'cargo_fin: settings change set %/% is proposed while onboarding % waits with a change set held for it; confirm or withdraw the onboarding first',
        NEW.tenant_id, NEW.change_id, waiting USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_onboarding_waiting';
    END IF;
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER settings_change_onboarding_waiting AFTER INSERT ON cargo_fin.settings_change
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_waiting_at_commit();

CREATE OR REPLACE FUNCTION cargo_fin.check_settings_approval() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  held boolean;
  waiting text;
BEGIN
  PERFORM cargo_fin.require_read_committed();
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_fin.settings/' || NEW.tenant_id, 0));
  IF EXISTS (SELECT 1 FROM cargo_fin.settings_cancellation x WHERE x.tenant_id = NEW.tenant_id AND x.change_id = NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% was cancelled; it is not approved', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_cancelled';
  END IF;
  held := EXISTS (SELECT 1 FROM cargo_fin.settings_hold h WHERE h.tenant_id = NEW.tenant_id AND h.change_id = NEW.change_id);
  IF held AND NEW.auto THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% is held for an onboarding, so it is not approved automatically; its owner confirms the onboarding', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_held_auto';
  END IF;
  IF held AND NEW.approved_by = NEW.proposed_by THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% is held for an onboarding, so its proposer may not approve it', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_held_proposer';
  END IF;
  IF NOT held THEN
    waiting := cargo_fin.settings_waiting_onboarding(NEW.tenant_id, NEW.change_id);
    IF waiting IS NOT NULL THEN
      RAISE EXCEPTION 'cargo_fin: settings change set %/% is approved while onboarding % waits with a change set held for it; confirm or withdraw the onboarding first',
        NEW.tenant_id, NEW.change_id, waiting USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_onboarding_waiting';
    END IF;
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
  IF NEW.risk_acknowledgement IS NOT NULL AND NOT EXISTS (SELECT 1 FROM (SELECT d.administrator FROM cargo_fin.single_administrator d
       WHERE d.tenant_id = NEW.tenant_id AND d.declared_at <= NEW.approved_at ORDER BY d.declared_at DESC LIMIT 1) latest
       WHERE latest.administrator = NEW.approved_by) THEN
    RAISE EXCEPTION 'cargo_fin: the single administrator of tenant % in force is not %, so the proposer of change set % may not approve it', NEW.tenant_id, NEW.approved_by, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_single_administrator';
  END IF;
  IF NEW.risk_acknowledgement IS NOT NULL AND NOT EXISTS (SELECT 1 FROM (SELECT d.onboarding_id FROM cargo_fin.single_administrator d
       WHERE d.tenant_id = NEW.tenant_id AND d.declared_at <= NEW.approved_at ORDER BY d.declared_at DESC LIMIT 1) latest
       WHERE latest.onboarding_id = NEW.basis_onboarding_id) THEN
    RAISE EXCEPTION 'cargo_fin: the declaration in force for tenant % at the approval of change set % is not the one of onboarding %, which the approval names as its basis', NEW.tenant_id, NEW.change_id, NEW.basis_onboarding_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_basis';
  END IF;
  RETURN NEW;
END $fn$;

-- The review of one self-approval.
CREATE TABLE cargo_fin.settings_self_approval_review (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 change_id text NOT NULL,
 -- the approver, repeated so that a foreign key holds it to the approval's own
 approved_by text NOT NULL,
 reviewed_by text NOT NULL CONSTRAINT settings_review_reviewer_shape CHECK (reviewed_by = btrim(reviewed_by) AND length(reviewed_by) BETWEEN 1 AND 128),
 reviewed_at timestamptz NOT NULL,
 conclusion text NOT NULL CONSTRAINT settings_review_conclusion CHECK (conclusion IN ('ACCEPTED','NOT_ACCEPTED')),
 note text CONSTRAINT settings_review_note_length CHECK (note IS NULL OR char_length(btrim(note)) BETWEEN 1 AND 2000),
 PRIMARY KEY (tenant_id, change_id),
 CONSTRAINT settings_review_of_approval FOREIGN KEY (tenant_id, change_id, approved_by)
   REFERENCES cargo_fin.settings_approval (tenant_id, change_id, approved_by),
 CONSTRAINT settings_review_reviewer CHECK (reviewed_by <> approved_by),
 CONSTRAINT settings_review_note_required CHECK (conclusion <> 'NOT_ACCEPTED' OR note IS NOT NULL)
);
CREATE FUNCTION cargo_fin.check_settings_review() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE a cargo_fin.settings_approval;
BEGIN
  SELECT * INTO a FROM cargo_fin.settings_approval x WHERE x.tenant_id = NEW.tenant_id AND x.change_id = NEW.change_id;
  IF a.risk_acknowledgement IS NULL THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% was not approved by its own proposer as a single administrator, so it is not reviewed here', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_review_self_approved';
  END IF;
  IF NEW.reviewed_at < a.approved_at THEN
    RAISE EXCEPTION 'cargo_fin: settings change set %/% is reviewed before it was approved', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_review_after_approval';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER settings_review_check BEFORE INSERT ON cargo_fin.settings_self_approval_review
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_settings_review();
CREATE TRIGGER settings_self_approval_review_immutable BEFORE UPDATE OR DELETE ON cargo_fin.settings_self_approval_review
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
CREATE TRIGGER settings_self_approval_review_read_committed BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.settings_self_approval_review
  FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed();
ALTER TABLE cargo_fin.settings_self_approval_review ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.settings_self_approval_review FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.settings_self_approval_review USING (tenant_id = current_setting('app.tenant_id',true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id',true));
