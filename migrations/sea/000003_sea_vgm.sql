-- IDP-Cargo SEA V0.2 (sea freight part 1e-ii) / PostgreSQL 16. Run only as the migrator role, never at service start.

-- A command that asserts a verified gross mass, or records the carrier's answer, names the assertion's version.
ALTER TABLE cargo_sea.command_receipt ADD COLUMN result_assertion_no integer CHECK (result_assertion_no > 0);
ALTER TABLE cargo_sea.command_receipt ADD CONSTRAINT command_receipt_assertion_of_visit
  CHECK (result_assertion_no IS NULL OR result_type = 'ContainerVisit');
-- The verified gross mass of a stuffed container (SOLAS). An assertion states the mass, the method and the
-- responsible party; its versions are immutable. The authorised signatory's name is personal data (privacy
-- assessment V1.2, decisions P4, P6 and P7 of 2026-11-10): it is stored apart from the assertion, the application
-- role cannot select it, and a read goes through cargo_sea.read_vgm_signatory, which logs the read in the same
-- call. Events carry the assertion's version, never the name or the mass.

ALTER TABLE cargo_sea.container_visit DROP CONSTRAINT container_visit_status_check;
ALTER TABLE cargo_sea.container_visit ADD CONSTRAINT container_visit_status_check
  CHECK (status IN ('ASSIGNED','STUFFED','VGM_SUBMITTED','RELEASED'));

-- A submitted container is still stuffed: it keeps the stuffing, the seal and any difference reason.
ALTER TABLE cargo_sea.container_visit DROP CONSTRAINT container_visit_stuffing;
ALTER TABLE cargo_sea.container_visit ADD CONSTRAINT container_visit_stuffing CHECK (
  (status IN ('STUFFED','VGM_SUBMITTED')) = (execution_id IS NOT NULL)
  AND (status IN ('STUFFED','VGM_SUBMITTED')) = (stuffed_at IS NOT NULL)
  AND (stuffed_by IS NULL) = (stuffed_at IS NULL)
  AND (execution_id IS NULL OR length(btrim(execution_id)) BETWEEN 1 AND 128)
  AND (stuffed_by IS NULL OR length(btrim(stuffed_by)) > 0)
  AND (stuffed_at IS NULL OR stuffed_at >= assigned_at)
  AND (difference_reason IS NULL OR (status IN ('STUFFED','VGM_SUBMITTED') AND length(btrim(difference_reason)) > 0 AND length(difference_reason) <= 500)));

-- A stuffed visit may become VGM_SUBMITTED, changing only its status. A submitted visit then changes only its
-- revision, as a stuffed one does when a seal is added.
CREATE OR REPLACE FUNCTION cargo_sea.check_visit_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  step text[] := ARRAY['revision','updated_at','written_xact'];
  stuffing text[] := ARRAY['status','execution_id','difference_reason','stuffed_by','stuffed_at','revision','updated_at','written_xact'];
  release text[] := ARRAY['status','release_reason','released_by','released_at','revision','updated_at','written_xact'];
  vgm text[] := ARRAY['status','revision','updated_at','written_xact'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_sea: container visit %/% is never deleted', OLD.tenant_id, OLD.visit_id USING ERRCODE = 'restrict_violation';
  END IF;
  NEW.written_xact := pg_current_xact_id();
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'ASSIGNED' OR NEW.revision <> 1 THEN
      RAISE EXCEPTION 'cargo_sea: container visit %/% starts ASSIGNED at revision 1', NEW.tenant_id, NEW.visit_id
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.revision = OLD.revision + 1 AND (
       (OLD.status = 'ASSIGNED' AND NEW.status = 'STUFFED' AND to_jsonb(OLD) - stuffing = to_jsonb(NEW) - stuffing)
    OR (OLD.status = 'ASSIGNED' AND NEW.status = 'RELEASED' AND to_jsonb(OLD) - release = to_jsonb(NEW) - release)
    OR (OLD.status = 'STUFFED' AND NEW.status = 'VGM_SUBMITTED' AND to_jsonb(OLD) - vgm = to_jsonb(NEW) - vgm)
    OR (OLD.status <> 'RELEASED' AND NEW.status = OLD.status AND to_jsonb(OLD) - step = to_jsonb(NEW) - step)) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'cargo_sea: container visit %/% leaves ASSIGNED once, for STUFFED or RELEASED, and STUFFED only for VGM_SUBMITTED; otherwise only its revision moves, by one, and never once released',
    OLD.tenant_id, OLD.visit_id USING ERRCODE = 'restrict_violation';
END $fn$;

-- A submitted container still has its seal, and its assertions are numbered without gaps. The mass is not below the
-- tare when the plate states one.
CREATE OR REPLACE FUNCTION cargo_sea.visit_inconsistency(v cargo_sea.container_visit) RETURNS text LANGUAGE plpgsql AS $fn$
DECLARE
  m cargo_sea.ocean_movement;
  n int;
  top int;
  early boolean;
  versions int;
  version_top int;
  signed int;
BEGIN
  SELECT * INTO m FROM cargo_sea.ocean_movement WHERE tenant_id = v.tenant_id AND movement_id = v.movement_id FOR SHARE;
  IF m.kind IS DISTINCT FROM 'SEA_LCL_OWN' OR m.consolidation_id IS DISTINCT FROM v.consolidation_id THEN
    RETURN 'a container is assigned on an own-container movement, for its consolidation';
  END IF;
  IF v.status <> 'RELEASED' AND m.state <> 'OPEN' THEN
    RETURN 'a movement that is not open holds no container';
  END IF;
  SELECT count(*), coalesce(max(s.seal_no), 0), coalesce(bool_or(s.recorded_at < v.assigned_at), false) INTO n, top, early
    FROM cargo_sea.container_seal s WHERE s.tenant_id = v.tenant_id AND s.visit_id = v.visit_id;
  IF top <> n THEN
    RETURN 'seals are numbered without gaps';
  END IF;
  IF early THEN
    RETURN 'a seal is recorded not before the container was assigned';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_sea.container_seal a JOIN cargo_sea.container_seal b
       ON b.tenant_id = a.tenant_id AND b.visit_id = a.visit_id AND b.seal_no = a.seal_no + 1
       WHERE a.tenant_id = v.tenant_id AND a.visit_id = v.visit_id AND (b.number = a.number OR b.recorded_at < a.recorded_at)) THEN
    RETURN 'a further seal has another number and is recorded not before the one it replaces';
  END IF;
  IF v.status IN ('STUFFED','VGM_SUBMITTED') AND n = 0 THEN
    RETURN 'a stuffed container has a seal';
  END IF;
  IF v.status = 'VGM_SUBMITTED' THEN
    SELECT count(*), coalesce(max(a.version), 0) INTO versions, version_top FROM cargo_sea.vgm_assertion a
      WHERE a.tenant_id = v.tenant_id AND a.visit_id = v.visit_id;
    -- The application role cannot select the names. The count is the only thing this check needs, and it sees the
    -- caller's tenant alone.
    SELECT cargo_sea.vgm_signatory_count(v.visit_id) INTO signed;
    IF versions = 0 OR version_top <> versions OR signed <> versions THEN
      RETURN 'a submitted container has its VGM assertions, numbered without gaps, each with its signatory';
    END IF;
    IF v.tare_kg IS NOT NULL AND EXISTS (SELECT 1 FROM cargo_sea.vgm_assertion a
         WHERE a.tenant_id = v.tenant_id AND a.visit_id = v.visit_id AND a.mass_grams < v.tare_kg * 1000) THEN
      RETURN 'the verified gross mass is not below the tare';
    END IF;
  END IF;
  RETURN NULL;
END $fn$;

-- One assertion of the verified gross mass. The signatory's name is not a column of this table.
CREATE TABLE cargo_sea.vgm_assertion (
 tenant_id text NOT NULL,
 visit_id text NOT NULL,
 version integer NOT NULL CHECK (version > 0),
 mass_grams bigint NOT NULL CHECK (mass_grams > 0),
 method text NOT NULL CHECK (method IN ('METHOD_1','METHOD_2')),
 responsible_party_id text NOT NULL CHECK (length(btrim(responsible_party_id)) BETWEEN 1 AND 128),
 responsible_party_revision bigint NOT NULL CHECK (responsible_party_revision > 0),
 determined_at timestamptz NOT NULL,
 submitted_at timestamptz NOT NULL,
 late_reason text,
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 response text,
 response_source text,
 responded_at timestamptz,
 response_by text,
 response_entered_at timestamptz,
 PRIMARY KEY (tenant_id, visit_id, version),
 FOREIGN KEY (tenant_id, visit_id) REFERENCES cargo_sea.container_visit (tenant_id, visit_id),
 CONSTRAINT vgm_assertion_times CHECK (determined_at <= submitted_at),
 CONSTRAINT vgm_assertion_late CHECK (late_reason IS NULL OR (length(btrim(late_reason)) > 0 AND length(late_reason) <= 500)),
 CONSTRAINT vgm_assertion_response CHECK ((response IS NULL AND response_source IS NULL AND responded_at IS NULL AND response_by IS NULL AND response_entered_at IS NULL)
   OR (response IN ('ACCEPTED','REJECTED') AND length(btrim(response_source)) > 0 AND length(response_source) <= 500
       AND responded_at >= submitted_at AND length(btrim(response_by)) > 0 AND response_entered_at IS NOT NULL))
);

-- The name only. The application role is not granted SELECT; cargo_sea.read_vgm_signatory is the read.
CREATE TABLE cargo_sea.vgm_signatory (
 tenant_id text NOT NULL,
 visit_id text NOT NULL,
 version integer NOT NULL,
 name text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 100 AND name = btrim(name)),
 PRIMARY KEY (tenant_id, visit_id, version),
 FOREIGN KEY (tenant_id, visit_id, version) REFERENCES cargo_sea.vgm_assertion (tenant_id, visit_id, version)
);
CREATE TRIGGER vgm_signatory_immutable BEFORE UPDATE OR DELETE ON cargo_sea.vgm_signatory
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.reject_row_change();

-- How many names are stored for a visit of the caller's tenant. visit_inconsistency uses it; the application role
-- cannot select the table.
CREATE FUNCTION cargo_sea.vgm_signatory_count(visit text) RETURNS integer
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, cargo_sea AS $fn$
  SELECT count(*)::integer FROM cargo_sea.vgm_signatory
    WHERE tenant_id = current_setting('app.tenant_id', true) AND visit_id = visit
$fn$;
REVOKE ALL ON FUNCTION cargo_sea.vgm_signatory_count(text) FROM PUBLIC;

-- An assertion is inserted without an answer. The answer is filled once, and nothing else changes.
CREATE FUNCTION cargo_sea.check_vgm_assertion_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  answer text[] := ARRAY['response','response_source','responded_at','response_by','response_entered_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% is never deleted', OLD.tenant_id, OLD.visit_id, OLD.version USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'INSERT' THEN
    IF NEW.response IS NOT NULL THEN
      RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% is recorded before the carrier answers', NEW.tenant_id, NEW.visit_id, NEW.version
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.response IS NULL AND NEW.response IS NOT NULL AND to_jsonb(OLD) - answer = to_jsonb(NEW) - answer THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% takes the carrier''s answer once, and nothing else changes', OLD.tenant_id, OLD.visit_id, OLD.version
    USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER vgm_assertion_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_sea.vgm_assertion
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_vgm_assertion_change();

-- At commit the assertion belongs to a submitted visit written in this transaction. On insert it is late only with a
-- reason, against the current voyage's VGM cut-off; a later roll of the vessel does not reopen an assertion already
-- stored, so the answer is not checked against the cut-off again.
CREATE FUNCTION cargo_sea.check_vgm_assertion_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_sea.container_visit;
  cut timestamptz;
BEGIN
  SELECT * INTO v FROM cargo_sea.container_visit WHERE tenant_id = NEW.tenant_id AND visit_id = NEW.visit_id FOR SHARE;
  IF v.status IS DISTINCT FROM 'VGM_SUBMITTED' OR (TG_OP = 'INSERT' AND v.written_xact IS DISTINCT FROM pg_current_xact_id()) THEN
    RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% is written with a step of a submitted visit', NEW.tenant_id, NEW.visit_id, NEW.version
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
  END IF;
  IF TG_OP = 'UPDATE' AND v.written_xact IS DISTINCT FROM pg_current_xact_id() THEN
    RAISE EXCEPTION 'cargo_sea: the carrier''s answer to VGM assertion %/%/% is written with a step of the visit', NEW.tenant_id, NEW.visit_id, NEW.version
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
  END IF;
  IF TG_OP = 'INSERT' THEN
    SELECT c.cut_off_at INTO cut FROM cargo_sea.voyage_cut_off c
      JOIN cargo_sea.booking_link l ON l.tenant_id = c.tenant_id AND l.movement_id = c.movement_id AND l.link_no = c.link_no
      WHERE c.tenant_id = v.tenant_id AND c.movement_id = v.movement_id AND c.leg_no = 1 AND c.kind = 'VGM'
        AND l.link_no = (SELECT max(link_no) FROM cargo_sea.booking_link WHERE tenant_id = v.tenant_id AND movement_id = v.movement_id);
    -- A missing cut-off makes "submitted after it" false, so a reason is refused; after it, a reason is required.
    IF (cut IS NOT NULL AND NEW.submitted_at > cut) IS DISTINCT FROM (NEW.late_reason IS NOT NULL) THEN
      RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% has a late reason exactly when it is submitted after the VGM cut-off', NEW.tenant_id, NEW.visit_id, NEW.version
        USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
    END IF;
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER vgm_assertion_consistent AFTER INSERT OR UPDATE ON cargo_sea.vgm_assertion
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_vgm_assertion_at_commit();

-- Who read a signatory's name, for which purpose. The database stamps the time. A row is never rewritten, and it is
-- deleted only once it is three years old.
CREATE TABLE cargo_sea.vgm_signatory_access (
 access_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 tenant_id text NOT NULL,
 visit_id text NOT NULL,
 version integer NOT NULL CHECK (version > 0),
 actor_id text NOT NULL CHECK (length(btrim(actor_id)) > 0),
 purpose text NOT NULL CHECK (purpose IN ('CUSTOMS_AUDIT','LEGAL_CLAIM','DATA_SUBJECT_REQUEST')),
 recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX vgm_signatory_access_by_visit ON cargo_sea.vgm_signatory_access (tenant_id, visit_id, version, recorded_at DESC);
CREATE FUNCTION cargo_sea.stamp_vgm_signatory_access() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  NEW.recorded_at := now();
  RETURN NEW;
END $fn$;
CREATE TRIGGER vgm_signatory_access_stamped BEFORE INSERT ON cargo_sea.vgm_signatory_access
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.stamp_vgm_signatory_access();
CREATE FUNCTION cargo_sea.keep_vgm_signatory_access() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'UPDATE' OR OLD.recorded_at > now() - interval '3 years' THEN
    RAISE EXCEPTION 'cargo_sea: the read of VGM signatory % is kept three years and never rewritten', OLD.access_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN OLD;
END $fn$;
CREATE TRIGGER vgm_signatory_access_kept BEFORE UPDATE OR DELETE ON cargo_sea.vgm_signatory_access
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.keep_vgm_signatory_access();

-- The only read of a signatory's name. It logs the read and returns the name, or NULL when there is none, without a
-- log. The caller cannot choose the tenant: it is the transaction's app.tenant_id.
CREATE FUNCTION cargo_sea.read_vgm_signatory(visit text, version integer, actor text, purpose text) RETURNS text
  LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, cargo_sea AS $fn$
DECLARE
  tenant text := current_setting('app.tenant_id', true);
  found text;
BEGIN
  IF tenant IS NULL OR btrim(tenant) = '' OR purpose IS NULL OR purpose NOT IN ('CUSTOMS_AUDIT','LEGAL_CLAIM','DATA_SUBJECT_REQUEST')
     OR actor IS NULL OR length(btrim(actor)) = 0 THEN
    RAISE EXCEPTION 'cargo_sea: a signatory is read for a customs audit, a legal claim or the person''s own request'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_signatory_access_purpose';
  END IF;
  SELECT s.name INTO found FROM cargo_sea.vgm_signatory s
    WHERE s.tenant_id = tenant AND s.visit_id = visit AND s.version = read_vgm_signatory.version;
  IF found IS NULL THEN
    RETURN NULL;
  END IF;
  INSERT INTO cargo_sea.vgm_signatory_access (tenant_id, visit_id, version, actor_id, purpose)
    VALUES (tenant, visit, read_vgm_signatory.version, actor, purpose);
  RETURN found;
END $fn$;
REVOKE ALL ON FUNCTION cargo_sea.read_vgm_signatory(text, integer, text, text) FROM PUBLIC;

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['vgm_assertion','vgm_signatory','vgm_signatory_access'] LOOP
    EXECUTE format('ALTER TABLE cargo_sea.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_sea.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_sea.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl);
  END LOOP;
END $rls$;
