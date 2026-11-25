-- IDP-Cargo FIN V2.102 (batch A) / PostgreSQL 16. Run only as the migrator role, never at service start. A single
-- administrator (design section 9, item 3): a tenant whose onboarding declared that it has one administrator may have a
-- loosening of FIN's settings approved by its own proposer, but only with a written risk acknowledgement in the approval.
-- 000001 through 000017 are unchanged.
--
-- Added: the table single_administrator, the declaration, and on settings_approval the column risk_acknowledgement.
-- A declaration names the tenant, the onboarding that made it (ORG's record, which FIN does not read), the consultant
-- who proposed that onboarding and the customer's owner who confirmed it (two different people), and when FIN recorded
-- it. It is only inserted (immutable, row security enabled and forced, written at READ COMMITTED) and is written by
-- FIN's command DeclareFinSingleAdministrator, which the platform's bootstrap calls once the owner has confirmed the
-- onboarding; a tenant has as many declarations as onboardings that made one, and one is enough. There is no way to
-- withdraw one yet.
--
-- How the database knows the declaration. FIN's schema holds it: the approval's trigger reads single_administrator in
-- the same schema, so no read crosses a schema and the roles stay as they are. What the database cannot know is that
-- ORG's onboarding exists and was confirmed by the owner it names: FIN's command takes it on the word of its caller,
-- which holds a permission granted by name only (DeclareSingleAdministrator) that the bootstrap's onboarding alone is
-- given; the declaration cannot be set by a configurer alone, since ConfigureSettings does not grant it.
--
-- Changed: the approval's rule "the approver is the proposer exactly when the approval is the automatic one of a
-- change set that only tightens" (settings_approval_other_actor, the same name) now also lets the approver be the
-- proposer when the approval carries a risk acknowledgement; the acknowledgement is of 1 to 2000 characters, only on
-- such an approval, and only of a change set that loosens (settings_approval_acknowledgement_*). The approval's trigger
-- refuses such an approval of a tenant with no declaration recorded at or before it
-- (settings_approval_single_administrator; it looks at the acknowledgement, so that a proposer's approval without one is still
-- refused by settings_approval_other_actor, as before). Everything else about approvals is as 000017 has it. The ALTER TABLE
-- takes an ACCESS EXCLUSIVE lock on settings_approval for its own duration and checks its rows once (reasoned, not
-- measured); no column has a default, so no table is rewritten.
--
-- What the schema does not know, and only the domain keeps: that the approver holds the permission ApproveSettings; that
-- the declaration comes from a confirmed onboarding; that the acknowledgement is not blank beyond its length (the
-- database checks the trimmed length).

CREATE TABLE cargo_fin.single_administrator (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 onboarding_id text NOT NULL CHECK (onboarding_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$'),
 proposed_by text NOT NULL CHECK (length(btrim(proposed_by)) BETWEEN 1 AND 128),
 confirmed_by text NOT NULL CHECK (length(btrim(confirmed_by)) BETWEEN 1 AND 128),
 declared_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, onboarding_id),
 CONSTRAINT single_administrator_two_people CHECK (confirmed_by <> proposed_by)
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
  ADD CONSTRAINT settings_approval_acknowledgement_length CHECK (risk_acknowledgement IS NULL OR char_length(btrim(risk_acknowledgement)) BETWEEN 1 AND 2000),
  ADD CONSTRAINT settings_approval_acknowledgement_self CHECK (risk_acknowledgement IS NULL OR (NOT auto AND approved_by = proposed_by)),
  ADD CONSTRAINT settings_approval_acknowledgement_loosening CHECK (risk_acknowledgement IS NULL OR loosening),
  DROP CONSTRAINT settings_approval_other_actor,
  ADD CONSTRAINT settings_approval_other_actor CHECK ((approved_by = proposed_by) = (auto OR risk_acknowledgement IS NOT NULL));

CREATE OR REPLACE FUNCTION cargo_fin.check_settings_approval() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
  IF NEW.risk_acknowledgement IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_fin.single_administrator d
       WHERE d.tenant_id = NEW.tenant_id AND d.declared_at <= NEW.approved_at) THEN
    RAISE EXCEPTION 'cargo_fin: tenant % declared no single administrator, so the proposer of change set % may not approve it', NEW.tenant_id, NEW.change_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'settings_approval_single_administrator';
  END IF;
  RETURN NEW;
END $fn$;
