-- IDP-Cargo SEA V0.2 (sea freight: the on-board date, and the lock of the VGM check) / PostgreSQL 16.
-- Run only as the migrator role, never at service start.
-- The published facts state the on-board date: the earliest loading recorded, as a container's LOADED_ON_BOARD event
-- or as the carrier's LOADED_ON_BOARD fact. A loading that changes that date (the facts state none, or a later one)
-- is written to a FINAL movement only once the same transaction has returned the movement to OPEN, as a new seal is.
-- A loading that leaves the date as published, and every other event or fact, is written while it stays FINAL.
-- The check of a VGM assertion now reads the movement under its share lock: without it, an assertion written past
-- SEA while a finalization held the movement was accepted on what then became a FINAL movement.
-- 000001 through 000007 are unchanged.

-- The on-board date of a movement's latest published facts, or NULL when they state none or none are published.
-- Versions published before this migration state no date as the year 1.
CREATE FUNCTION cargo_sea.published_on_board_at(text, text) RETURNS timestamptz LANGUAGE sql STABLE AS $fn$
  SELECT nullif((d.content::jsonb)->>'on_board_at', '0001-01-01T00:00:00Z')::timestamptz
    FROM cargo_sea.ocean_doc_facts_version d
   WHERE d.tenant_id = $1 AND d.movement_id = $2
   ORDER BY d.version_no DESC LIMIT 1
$fn$;

-- As in 000004, and: a loading that changes the published on-board date is not written to a FINAL movement.
-- visit_inconsistency holds the movement's share lock, so its state and its published facts stand until commit.
CREATE OR REPLACE FUNCTION cargo_sea.check_container_event_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_sea.container_visit;
  broken text;
  published timestamptz;
BEGIN
  SELECT * INTO v FROM cargo_sea.container_visit WHERE tenant_id = NEW.tenant_id AND visit_id = NEW.visit_id FOR SHARE;
  IF v.status = 'RELEASED' THEN
    broken := 'a released container takes no event';
  ELSIF v.written_xact IS DISTINCT FROM pg_current_xact_id() THEN
    broken := 'a container event is written with a step of its visit''s revision';
  ELSE
    broken := cargo_sea.visit_inconsistency(v);
  END IF;
  IF broken IS NULL AND NEW.kind = 'LOADED_ON_BOARD' AND (SELECT m.state FROM cargo_sea.ocean_movement m
       WHERE m.tenant_id = v.tenant_id AND m.movement_id = v.movement_id) = 'FINAL' THEN
    published := cargo_sea.published_on_board_at(v.tenant_id, v.movement_id);
    IF published IS NULL OR NEW.occurred_at < published THEN
      broken := 'a loading that changes the published on-board date is recorded while the movement is open';
    END IF;
  END IF;
  IF broken IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_sea: container event %/%/%: %', NEW.tenant_id, NEW.visit_id, NEW.event_no, broken
      USING ERRCODE = 'check_violation', CONSTRAINT = 'container_event_consistent';
  END IF;
  RETURN NULL;
END $fn$;

-- As in 000007, and the same rule for the carrier's LOADED_ON_BOARD fact.
CREATE OR REPLACE FUNCTION cargo_sea.check_ocean_carrier_fact_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  m cargo_sea.ocean_movement;
  n int;
  top int;
  published timestamptz;
BEGIN
  SELECT * INTO m FROM cargo_sea.ocean_movement WHERE tenant_id = NEW.tenant_id AND movement_id = NEW.movement_id FOR SHARE;
  IF m.state IS DISTINCT FROM 'OPEN' AND m.state IS DISTINCT FROM 'FINAL' THEN
    RAISE EXCEPTION 'cargo_sea: carrier fact %/%/% is recorded while the movement is open or final', NEW.tenant_id, NEW.movement_id, NEW.fact_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_carrier_fact_consistent';
  END IF;
  IF NEW.kind = 'LOADED_ON_BOARD' AND m.state = 'FINAL' THEN
    published := cargo_sea.published_on_board_at(NEW.tenant_id, NEW.movement_id);
    IF published IS NULL OR NEW.occurred_at < published THEN
      RAISE EXCEPTION 'cargo_sea: carrier fact %/%/%: a loading that changes the published on-board date is recorded while the movement is open',
        NEW.tenant_id, NEW.movement_id, NEW.fact_no
        USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_carrier_fact_consistent';
    END IF;
  END IF;
  IF NEW.container_number IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_sea.container_visit v
       WHERE v.tenant_id = NEW.tenant_id AND v.movement_id = NEW.movement_id AND v.container_number = NEW.container_number
         AND v.status <> 'RELEASED') THEN
    RAISE EXCEPTION 'cargo_sea: carrier fact %/%/% names a live container of its movement', NEW.tenant_id, NEW.movement_id, NEW.fact_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_carrier_fact_consistent';
  END IF;
  IF NEW.bill_number IS NOT NULL AND NOT EXISTS (
       SELECT 1 FROM cargo_sea.master_bill b WHERE b.tenant_id = NEW.tenant_id AND b.movement_id = NEW.movement_id
         AND b.status = 'CURRENT' AND b.bill_number = NEW.bill_number
       UNION ALL
       SELECT 1 FROM cargo_sea.house_bill h WHERE h.tenant_id = NEW.tenant_id AND h.movement_id = NEW.movement_id
         AND h.ended_at IS NULL AND h.hbl = NEW.bill_number) THEN
    RAISE EXCEPTION 'cargo_sea: carrier fact %/%/% names the current master bill or a live house bill', NEW.tenant_id, NEW.movement_id, NEW.fact_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_carrier_fact_consistent';
  END IF;
  SELECT count(*), coalesce(max(f.fact_no), 0) INTO n, top FROM cargo_sea.ocean_carrier_fact f
    WHERE f.tenant_id = NEW.tenant_id AND f.movement_id = NEW.movement_id;
  IF top <> n THEN
    RAISE EXCEPTION 'cargo_sea: carrier facts of %/% are numbered without gaps', NEW.tenant_id, NEW.movement_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_carrier_fact_consistent';
  END IF;
  RETURN NULL;
END $fn$;

-- As in 000006, with the movement read under its share lock. A finalization that holds the movement is waited for,
-- and its FINAL state is then the one this check sees, whichever of the transaction's checks runs first.
CREATE OR REPLACE FUNCTION cargo_sea.check_vgm_assertion_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  v cargo_sea.container_visit;
  cut timestamptz;
  st text;
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
  SELECT m.state INTO st FROM cargo_sea.ocean_movement m WHERE m.tenant_id = v.tenant_id AND m.movement_id = v.movement_id FOR SHARE;
  IF TG_OP = 'INSERT' AND st IS DISTINCT FROM 'OPEN' THEN
    RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% is recorded while the movement is open', NEW.tenant_id, NEW.visit_id, NEW.version
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
  END IF;
  IF TG_OP = 'UPDATE' AND NEW.response = 'REJECTED' AND st IS DISTINCT FROM 'OPEN' THEN
    RAISE EXCEPTION 'cargo_sea: VGM assertion %/%/% is rejected while the movement is open', NEW.tenant_id, NEW.visit_id, NEW.version
      USING ERRCODE = 'check_violation', CONSTRAINT = 'vgm_assertion_consistent';
  END IF;
  IF TG_OP = 'UPDATE' AND st IS DISTINCT FROM 'OPEN' AND st IS DISTINCT FROM 'FINAL' THEN
    RAISE EXCEPTION 'cargo_sea: the carrier''s answer to VGM assertion %/%/% is recorded while the movement is open or final', NEW.tenant_id, NEW.visit_id, NEW.version
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
