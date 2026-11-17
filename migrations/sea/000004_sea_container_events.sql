-- IDP-Cargo SEA V0.2 (sea freight part 1e-iii) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A carrier or terminal fact about a container. It never moves a house. A container is gated in only once its
-- verified gross mass is submitted, and loaded on board only once it is gated in. The source reference is recorded
-- once in the tenant. Events carry the fact's number and its kind, never the place or the source's text.

ALTER TABLE cargo_sea.command_receipt ADD COLUMN result_event_no integer CHECK (result_event_no > 0);
ALTER TABLE cargo_sea.command_receipt ADD CONSTRAINT command_receipt_event_of_visit
  CHECK (result_event_no IS NULL OR result_type = 'ContainerVisit');

ALTER TABLE cargo_sea.container_visit DROP CONSTRAINT container_visit_status_check;
ALTER TABLE cargo_sea.container_visit ADD CONSTRAINT container_visit_status_check
  CHECK (status IN ('ASSIGNED','STUFFED','VGM_SUBMITTED','GATED_IN','LOADED','RELEASED'));

-- A gated-in or loaded container is still the stuffed one: it keeps the stuffing, the seal and any difference.
ALTER TABLE cargo_sea.container_visit DROP CONSTRAINT container_visit_stuffing;
ALTER TABLE cargo_sea.container_visit ADD CONSTRAINT container_visit_stuffing CHECK (
  (status IN ('STUFFED','VGM_SUBMITTED','GATED_IN','LOADED')) = (execution_id IS NOT NULL)
  AND (status IN ('STUFFED','VGM_SUBMITTED','GATED_IN','LOADED')) = (stuffed_at IS NOT NULL)
  AND (stuffed_by IS NULL) = (stuffed_at IS NULL)
  AND (execution_id IS NULL OR length(btrim(execution_id)) BETWEEN 1 AND 128)
  AND (stuffed_by IS NULL OR length(btrim(stuffed_by)) > 0)
  AND (stuffed_at IS NULL OR stuffed_at >= assigned_at)
  AND (difference_reason IS NULL OR (status IN ('STUFFED','VGM_SUBMITTED','GATED_IN','LOADED')
       AND length(btrim(difference_reason)) > 0 AND length(difference_reason) <= 500)));

-- A submitted container may be gated in, and a gated-in container loaded, changing only its status.
CREATE OR REPLACE FUNCTION cargo_sea.check_visit_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  step text[] := ARRAY['revision','updated_at','written_xact'];
  stuffing text[] := ARRAY['status','execution_id','difference_reason','stuffed_by','stuffed_at','revision','updated_at','written_xact'];
  release text[] := ARRAY['status','release_reason','released_by','released_at','revision','updated_at','written_xact'];
  move text[] := ARRAY['status','revision','updated_at','written_xact'];
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
    OR (OLD.status = 'STUFFED' AND NEW.status = 'VGM_SUBMITTED' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status = 'VGM_SUBMITTED' AND NEW.status = 'GATED_IN' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status = 'GATED_IN' AND NEW.status = 'LOADED' AND to_jsonb(OLD) - move = to_jsonb(NEW) - move)
    OR (OLD.status <> 'RELEASED' AND NEW.status = OLD.status AND to_jsonb(OLD) - step = to_jsonb(NEW) - step)) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'cargo_sea: container visit %/% leaves ASSIGNED once, for STUFFED or RELEASED; STUFFED only for VGM_SUBMITTED, that only for GATED_IN, and that only for LOADED; otherwise only its revision moves, by one, and never once released',
    OLD.tenant_id, OLD.visit_id USING ERRCODE = 'restrict_violation';
END $fn$;

-- A gated-in or loaded container still has its VGM, and a gate-in or a loading is exactly the status it names.
CREATE OR REPLACE FUNCTION cargo_sea.visit_inconsistency(v cargo_sea.container_visit) RETURNS text LANGUAGE plpgsql AS $fn$
DECLARE
  m cargo_sea.ocean_movement;
  n int;
  top int;
  early boolean;
  versions int;
  version_top int;
  signed int;
  events int;
  event_top int;
  gated boolean;
  loaded boolean;
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
  IF v.status IN ('STUFFED','VGM_SUBMITTED','GATED_IN','LOADED') AND n = 0 THEN
    RETURN 'a stuffed container has a seal';
  END IF;
  SELECT count(*), coalesce(max(e.event_no), 0),
         coalesce(bool_or(e.kind = 'GATE_IN_TERMINAL'), false), coalesce(bool_or(e.kind = 'LOADED_ON_BOARD'), false)
    INTO events, event_top, gated, loaded
    FROM cargo_sea.container_event e WHERE e.tenant_id = v.tenant_id AND e.visit_id = v.visit_id;
  IF event_top <> events THEN
    RETURN 'container events are numbered without gaps';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_sea.container_event e WHERE e.tenant_id = v.tenant_id AND e.visit_id = v.visit_id AND e.recorded_at < v.assigned_at) THEN
    RETURN 'a container event is recorded not before the container was assigned';
  END IF;
  IF (v.status IN ('GATED_IN','LOADED')) IS DISTINCT FROM gated THEN
    RETURN 'a container is gated in exactly when a gate-in is recorded, and stays so once loaded';
  END IF;
  IF (v.status = 'LOADED') IS DISTINCT FROM loaded THEN
    RETURN 'a container is loaded exactly when it is recorded loaded on board';
  END IF;
  IF v.status IN ('VGM_SUBMITTED','GATED_IN','LOADED') THEN
    SELECT count(*), coalesce(max(a.version), 0) INTO versions, version_top FROM cargo_sea.vgm_assertion a
      WHERE a.tenant_id = v.tenant_id AND a.visit_id = v.visit_id;
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

-- One fact. Stuffing is not a row here: RecordStuffingDone records it on the visit.
CREATE TABLE cargo_sea.container_event (
 tenant_id text NOT NULL,
 visit_id text NOT NULL,
 event_no integer NOT NULL CHECK (event_no > 0),
 kind text NOT NULL CHECK (kind IN ('EMPTY_RELEASED','EMPTY_PICKED_UP','ARRIVED_AT_CFS','GATE_IN_TERMINAL','LOADED_ON_BOARD','ROLLED')),
 location_code text NOT NULL CHECK (location_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 occurred_at timestamptz NOT NULL,
 source text NOT NULL CHECK (length(btrim(source)) > 0 AND length(source) <= 64 AND source = btrim(source)),
 reference text NOT NULL CHECK (length(btrim(reference)) > 0 AND length(reference) <= 128 AND reference = btrim(reference)),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, visit_id, event_no),
 CONSTRAINT container_event_source UNIQUE (tenant_id, source, reference),
 FOREIGN KEY (tenant_id, visit_id) REFERENCES cargo_sea.container_visit (tenant_id, visit_id),
 CONSTRAINT container_event_times CHECK (occurred_at <= recorded_at)
);
CREATE TRIGGER container_event_immutable BEFORE UPDATE OR DELETE ON cargo_sea.container_event
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.reject_row_change();

-- At commit the fact belongs to a visit that is not released and that this transaction wrote, and the visit agrees
-- with it: a gate-in is a gated-in or loaded container, a loading is a loaded one.
CREATE FUNCTION cargo_sea.check_container_event_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_sea.container_visit;
  broken text;
BEGIN
  SELECT * INTO v FROM cargo_sea.container_visit WHERE tenant_id = NEW.tenant_id AND visit_id = NEW.visit_id FOR SHARE;
  IF v.status = 'RELEASED' THEN
    broken := 'a released container takes no event';
  ELSIF v.written_xact IS DISTINCT FROM pg_current_xact_id() THEN
    broken := 'a container event is written with a step of its visit''s revision';
  ELSE
    broken := cargo_sea.visit_inconsistency(v);
  END IF;
  IF broken IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_sea: container event %/%/%: %', NEW.tenant_id, NEW.visit_id, NEW.event_no, broken
      USING ERRCODE = 'check_violation', CONSTRAINT = 'container_event_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER container_event_consistent AFTER INSERT ON cargo_sea.container_event
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_container_event_at_commit();

ALTER TABLE cargo_sea.container_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_sea.container_event FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_sea.container_event
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
