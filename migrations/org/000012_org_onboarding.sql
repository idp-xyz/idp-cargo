-- IDP-Cargo ORG V2.102 (batches A, D and E) / PostgreSQL 16. Run only as the migrator role, never at service start. Customer
-- onboarding (design section 9, item 2): an implementation consultant proposes a profile of the rules, overrides, the
-- entries that these resolve to in each module with the version of each module's catalogue (the plan: what will be
-- applied), a risk statement, the customer's declared number of finance staff and whether the customer has a single
-- administrator; the customer's appointed owner, a different person, confirms it, acknowledges the risk statement, whose
-- text is kept as acknowledged, confirms the SHA-256 of the plan they read, and names the single administrator when the proposal says there is one. The platform then
-- proposes and approves FIN's and SEA's change sets from the plan, one module at a time, and records here each module's
-- change set and state. 000001 through 000011 are unchanged.
--
-- Added: onboarding (the proposal, with its plan), onboarding_confirmation (at most one for an onboarding) and onboarding_module (the
-- change set a module made for it, once as PROPOSED and once as APPROVED), each immutable (rows are only inserted), with
-- row security enabled and forced.
--
-- What the database refuses: a profile other than STANDARD, STRICT or LEAN; modules other than FIN and SEA in the
-- overrides, which are a JSON object; a plan that is not a JSON object of exactly FIN and SEA, each with a catalogue
-- version and 1 to 64 entries; a risk statement that is blank or longer than 2000 characters; a number of finance
-- staff outside 1 to 10000; an onboarding named by anything but a plain identifier of 1 to 100 characters; a command id
-- used twice by a tenant; a confirmation by the proposer (the confirmer is not the proposer: the confirmation repeats
-- the proposer and the risk statement of its proposal in a foreign key, and its own check compares the two people); a
-- confirmation that names a single administrator unless the proposal says the customer declares one, or that names none
-- when it does (the confirmation repeats the proposal's flag in the same foreign key), and an administrator who is the
-- proposer or whose id has spaces around it; a confirmation whose acknowledged text is not the proposal's risk statement, character for character (the same foreign
-- key); a confirmation whose SHA-256 of the plan is not the proposal's (the same foreign key); a proposer or a confirmer whose actor id is
-- blank, has spaces around it or is longer than 128 characters; a confirmation before the proposal; a module row of an onboarding not confirmed, recorded by anyone but the
-- confirmer (a foreign key to the confirmation's confirmer), before the confirmation, or whose change set is not the one
-- derived from the onboarding and the module ('onb-' || onboarding || '-fin' or '-sea', so that a retry never makes a
-- second change set); an APPROVED row without the PROPOSED row of the same change set; a change of any row; another
-- tenant's rows.
--
-- What the schema does not know, and only the domain keeps: that the proposer holds ProposeOnboarding and the confirmer
-- ConfirmOnboarding (granted by name only), that a module row is written by the platform (RecordOnboardingModule, granted
-- by name only to the principal the bootstrap builds), that the plan is what the profile and the overrides resolve to and
-- is valid for FIN and SEA (the platform checks it with the modules' own rules when it is proposed, and the modules check
-- it again when it proposes their change sets; a module's catalogue version that differs at the confirmation is refused
-- by the platform), that the change set a module row names exists in that module (the schemas are separate, and the roles
-- forbid a read across them), and that the confirmer is the customer's appointed owner (ORG has no user directory yet:
-- it knows two different people, not who they are), and that the proposal's SHA-256 is the digest of its plan (the domain computes it; the
-- schema checks its shape and that the confirmation repeats it). ORG's own commands for the proposal and the confirmation run only
-- under the platform's mark, which the schema does not know either.

CREATE TABLE cargo_org.onboarding (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 onboarding_id text NOT NULL CONSTRAINT onboarding_id_shape CHECK (onboarding_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,99}$'),
 command_id text NOT NULL CHECK (length(btrim(command_id)) BETWEEN 1 AND 128),
 request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
 profile text NOT NULL CONSTRAINT onboarding_profile CHECK (profile IN ('STANDARD','STRICT','LEAN')),
 overrides jsonb NOT NULL CONSTRAINT onboarding_overrides CHECK (jsonb_typeof(overrides) = 'object' AND (overrides - 'FIN' - 'SEA') = '{}'::jsonb),
 -- what will be applied: for each module the catalogue version and the entries the profile and the overrides resolve to
 plan jsonb NOT NULL CONSTRAINT onboarding_plan CHECK (coalesce(jsonb_typeof(plan) = 'object' AND (plan - 'FIN' - 'SEA') = '{}'::jsonb
   AND jsonb_typeof(plan->'FIN') = 'object' AND jsonb_typeof(plan->'SEA') = 'object'
   AND jsonb_typeof(plan->'FIN'->'CatalogueVersion') = 'string' AND length(btrim(plan->'FIN'->>'CatalogueVersion')) BETWEEN 1 AND 64
   AND jsonb_typeof(plan->'SEA'->'CatalogueVersion') = 'string' AND length(btrim(plan->'SEA'->>'CatalogueVersion')) BETWEEN 1 AND 64
   AND jsonb_typeof(plan->'FIN'->'Entries') = 'array' AND jsonb_array_length(plan->'FIN'->'Entries') BETWEEN 1 AND 64
   AND jsonb_typeof(plan->'SEA'->'Entries') = 'array' AND jsonb_array_length(plan->'SEA'->'Entries') BETWEEN 1 AND 64, false)),
 -- the SHA-256 of the plan as JSON, which the owner reads and confirms (the domain computes it; the confirmation repeats it in a foreign key)
 plan_sha256 text NOT NULL CONSTRAINT onboarding_plan_sha256 CHECK (plan_sha256 ~ '^[0-9a-f]{64}$'),
 risk_statement text NOT NULL CONSTRAINT onboarding_risk_statement CHECK (char_length(btrim(risk_statement)) BETWEEN 1 AND 2000),
 finance_staff integer NOT NULL CONSTRAINT onboarding_finance_staff CHECK (finance_staff BETWEEN 1 AND 10000),
 single_administrator boolean NOT NULL,
 proposed_by text NOT NULL CHECK (proposed_by = btrim(proposed_by) AND length(proposed_by) BETWEEN 1 AND 128),
 proposed_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, onboarding_id),
 CONSTRAINT onboarding_command_once UNIQUE (tenant_id, command_id),
 CONSTRAINT onboarding_for_confirmation UNIQUE (tenant_id, onboarding_id, proposed_by, risk_statement, proposed_at, single_administrator, plan_sha256)
);

CREATE TABLE cargo_org.onboarding_confirmation (
 tenant_id text NOT NULL,
 onboarding_id text NOT NULL,
 command_id text NOT NULL CHECK (length(btrim(command_id)) BETWEEN 1 AND 128),
 proposed_by text NOT NULL,
 proposed_at timestamptz NOT NULL,
 confirmed_by text NOT NULL CHECK (confirmed_by = btrim(confirmed_by) AND length(confirmed_by) BETWEEN 1 AND 128),
 confirmed_at timestamptz NOT NULL,
 -- the risk statement as the owner acknowledged it, which is the proposal's, character for character
 acknowledged_text text NOT NULL,
 -- the owner's declaration: the proposal's flag, repeated so that a foreign key holds the two together, and the administrator
 single_administrator boolean NOT NULL,
 -- the SHA-256 of the plan the owner read, which is the proposal's (the foreign key below)
 plan_sha256 text NOT NULL,
 administrator text CONSTRAINT onboarding_administrator_length CHECK (administrator IS NULL OR (length(btrim(administrator)) BETWEEN 1 AND 128 AND administrator = btrim(administrator))),
 PRIMARY KEY (tenant_id, onboarding_id),
 CONSTRAINT onboarding_confirmation_command_once UNIQUE (tenant_id, command_id),
 CONSTRAINT onboarding_confirmation_for_module UNIQUE (tenant_id, onboarding_id, confirmed_by, confirmed_at),
 CONSTRAINT onboarding_confirmer_not_proposer CHECK (confirmed_by <> proposed_by),
 CONSTRAINT onboarding_confirmation_after_proposal CHECK (confirmed_at >= proposed_at),
 CONSTRAINT onboarding_administrator_declared CHECK ((administrator IS NOT NULL) = single_administrator),
 CONSTRAINT onboarding_administrator_not_proposer CHECK (administrator IS NULL OR administrator <> proposed_by),
 CONSTRAINT onboarding_confirmation_matches_proposal FOREIGN KEY (tenant_id, onboarding_id, proposed_by, acknowledged_text, proposed_at, single_administrator, plan_sha256)
   REFERENCES cargo_org.onboarding (tenant_id, onboarding_id, proposed_by, risk_statement, proposed_at, single_administrator, plan_sha256)
);

CREATE TABLE cargo_org.onboarding_module (
 tenant_id text NOT NULL,
 onboarding_id text NOT NULL,
 module text NOT NULL CONSTRAINT onboarding_module_known CHECK (module IN ('FIN','SEA')),
 state text NOT NULL CONSTRAINT onboarding_module_state CHECK (state IN ('PROPOSED','APPROVED')),
 change_id text NOT NULL,
 recorded_by text NOT NULL,
 confirmed_at timestamptz NOT NULL,
 recorded_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, onboarding_id, module, state),
 CONSTRAINT onboarding_module_change_derived CHECK (change_id = 'onb-' || onboarding_id || '-' || lower(module)),
 CONSTRAINT onboarding_module_after_confirmation CHECK (recorded_at >= confirmed_at),
 CONSTRAINT onboarding_module_by_confirmer FOREIGN KEY (tenant_id, onboarding_id, recorded_by, confirmed_at)
   REFERENCES cargo_org.onboarding_confirmation (tenant_id, onboarding_id, confirmed_by, confirmed_at)
);

CREATE FUNCTION cargo_org.check_onboarding_module() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.state = 'APPROVED' AND NOT EXISTS (SELECT 1 FROM cargo_org.onboarding_module p WHERE p.tenant_id = NEW.tenant_id
       AND p.onboarding_id = NEW.onboarding_id AND p.module = NEW.module AND p.state = 'PROPOSED' AND p.change_id = NEW.change_id) THEN
    RAISE EXCEPTION 'cargo_org: onboarding %/% module % is approved before it is proposed', NEW.tenant_id, NEW.onboarding_id, NEW.module
      USING ERRCODE = 'check_violation', CONSTRAINT = 'onboarding_module_approved_after_proposed';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER onboarding_module_order BEFORE INSERT ON cargo_org.onboarding_module
  FOR EACH ROW EXECUTE FUNCTION cargo_org.check_onboarding_module();

CREATE FUNCTION cargo_org.onboarding_immutable() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_org: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;

DO $tables$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['onboarding','onboarding_confirmation','onboarding_module'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_org.%I FOR EACH ROW EXECUTE FUNCTION cargo_org.onboarding_immutable()',
      tbl || '_immutable', tbl);
    EXECUTE format('ALTER TABLE cargo_org.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_org.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format('CREATE POLICY tenant_isolation ON cargo_org.%I USING (tenant_id = current_setting(''app.tenant_id'', true)) '
      'WITH CHECK (tenant_id = current_setting(''app.tenant_id'', true))', tbl);
  END LOOP;
END $tables$;
