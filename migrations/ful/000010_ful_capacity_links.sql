-- IDP-Cargo FUL V0.4, stage 4 (V2.110) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The capacity a shipment's main carriage travels on (LINK_CONFIRMED_CAPACITY, RECORD_CAPACITY_VERSION): FUL links it to
-- BOOK's confirmed capacity (CarrierBooking v1) of the consolidation it records the shipment attached to, and links
-- again when the carrier changes the booking or the cargo moves to another one; every link is kept, with the plan
-- version it was made under, the booking, its revision, capacity version and line, the carrier's reference and the
-- legs with the carrier's estimated departures and arrivals. When a link's first departure lies outside the plan's
-- departure window, the customer's answer is recorded against it (the owner's decision of 2026-11-26): ACCEPTED or
-- REFUSED, with its evidence and the window it answered. Rows are immutable. No foreign key reaches cargo_book.

CREATE TABLE cargo_ful.shipment_capacity_link (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 link_no integer NOT NULL CHECK (link_no > 0),
 stage text NOT NULL CHECK (stage = 'MAIN'),
 plan_no integer NOT NULL CHECK (plan_no > 0),
 booking_id text NOT NULL CHECK (length(btrim(booking_id)) BETWEEN 1 AND 128),
 booking_revision bigint NOT NULL CHECK (booking_revision > 0),
 transport_mode text NOT NULL CHECK (transport_mode IN ('AIR','SEA','ROAD','RAIL')),
 consolidation_id text NOT NULL CHECK (length(btrim(consolidation_id)) BETWEEN 1 AND 128),
 capacity_version integer NOT NULL CHECK (capacity_version > 0),
 line_no integer NOT NULL CHECK (line_no > 0),
 carrier_reference text CHECK (length(btrim(carrier_reference)) BETWEEN 1 AND 500),
 linked_by text NOT NULL CHECK (length(btrim(linked_by)) > 0),
 linked_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, shipment_id, link_no),
 FOREIGN KEY (tenant_id, shipment_id) REFERENCES cargo_ful.shipment (tenant_id, shipment_id),
 FOREIGN KEY (tenant_id, shipment_id, plan_no) REFERENCES cargo_ful.shipment_plan_version (tenant_id, shipment_id, plan_no)
);

CREATE TABLE cargo_ful.shipment_capacity_link_leg (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 link_no integer NOT NULL,
 leg_no integer NOT NULL CHECK (leg_no > 0),
 loading_scheme text CHECK (loading_scheme ~ '^[A-Z]{2,8}$'),
 loading_code text CHECK (length(btrim(loading_code)) BETWEEN 1 AND 128),
 discharge_scheme text CHECK (discharge_scheme ~ '^[A-Z]{2,8}$'),
 discharge_code text CHECK (length(btrim(discharge_code)) BETWEEN 1 AND 128),
 flight text CHECK (length(btrim(flight)) BETWEEN 1 AND 128),
 vessel text CHECK (length(btrim(vessel)) BETWEEN 1 AND 500),
 voyage text CHECK (length(btrim(voyage)) BETWEEN 1 AND 128),
 etd timestamptz NOT NULL,
 eta timestamptz,
 PRIMARY KEY (tenant_id, shipment_id, link_no, leg_no),
 FOREIGN KEY (tenant_id, shipment_id, link_no) REFERENCES cargo_ful.shipment_capacity_link (tenant_id, shipment_id, link_no),
 CONSTRAINT shipment_capacity_link_leg_places CHECK ((loading_scheme IS NULL) = (loading_code IS NULL)
   AND (discharge_scheme IS NULL) = (discharge_code IS NULL)),
 CONSTRAINT shipment_capacity_link_leg_times CHECK (eta IS NULL OR eta >= etd)
);

CREATE TABLE cargo_ful.shipment_date_answer (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 answer_no integer NOT NULL CHECK (answer_no > 0),
 link_no integer NOT NULL,
 answer text NOT NULL CHECK (answer IN ('ACCEPTED','REFUSED')),
 evidence_ref text NOT NULL CHECK (length(btrim(evidence_ref)) BETWEEN 1 AND 500),
 window_from timestamptz,
 window_until timestamptz,
 answered_by text NOT NULL CHECK (length(btrim(answered_by)) > 0),
 answered_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, shipment_id, answer_no),
 FOREIGN KEY (tenant_id, shipment_id, link_no) REFERENCES cargo_ful.shipment_capacity_link (tenant_id, shipment_id, link_no),
 CONSTRAINT shipment_date_answer_window CHECK (window_until IS NULL OR window_from IS NULL OR window_until > window_from)
);

CREATE TRIGGER shipment_capacity_link_immutable BEFORE UPDATE OR DELETE ON cargo_ful.shipment_capacity_link
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.reject_row_change();
CREATE TRIGGER shipment_capacity_link_leg_immutable BEFORE UPDATE OR DELETE ON cargo_ful.shipment_capacity_link_leg
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.reject_row_change();
CREATE TRIGGER shipment_date_answer_immutable BEFORE UPDATE OR DELETE ON cargo_ful.shipment_date_answer
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.reject_row_change();

-- A link joins a shipment that is not cancelled, whose row is locked first; it follows the previous link, and the
-- shipment is attached on the main carriage to the link's consolidation now.
CREATE FUNCTION cargo_ful.check_capacity_link() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_ful.shipment;
  last integer;
BEGIN
  SELECT * INTO s FROM cargo_ful.shipment WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id FOR NO KEY UPDATE;
  SELECT coalesce(max(link_no), 0) INTO last FROM cargo_ful.shipment_capacity_link
    WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id;
  IF s.shipment_id IS NULL OR s.status = 'CANCELLED' OR NEW.link_no <> last + 1 THEN
    RAISE EXCEPTION 'cargo_ful: shipment %/% takes no capacity link %', NEW.tenant_id, NEW.shipment_id, NEW.link_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_capacity_link_follows';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM cargo_ful.shipment_consol_attachment a WHERE a.tenant_id = NEW.tenant_id AND a.shipment_id = NEW.shipment_id
       AND a.stage = NEW.stage AND a.ended_at IS NULL AND a.consolidation_id = NEW.consolidation_id) THEN
    RAISE EXCEPTION 'cargo_ful: shipment %/% is not attached to %', NEW.tenant_id, NEW.shipment_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_capacity_link_attached';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER shipment_capacity_link_rules BEFORE INSERT ON cargo_ful.shipment_capacity_link
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_capacity_link();

-- At commit, a link has its legs, written by the transaction that writes the link.
CREATE FUNCTION cargo_ful.check_capacity_link_legs_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_TABLE_NAME = 'shipment_capacity_link_leg' THEN
    IF NOT EXISTS (SELECT 1 FROM cargo_ful.shipment_capacity_link l WHERE l.tenant_id = NEW.tenant_id AND l.shipment_id = NEW.shipment_id
         AND l.link_no = NEW.link_no AND l.xmin = pg_current_xact_id()::xid) THEN
      RAISE EXCEPTION 'cargo_ful: legs of capacity link %/%/% are written with it', NEW.tenant_id, NEW.shipment_id, NEW.link_no
        USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_capacity_link_legs_with_link';
    END IF;
  ELSIF NOT EXISTS (SELECT 1 FROM cargo_ful.shipment_capacity_link_leg g WHERE g.tenant_id = NEW.tenant_id AND g.shipment_id = NEW.shipment_id
       AND g.link_no = NEW.link_no) THEN
    RAISE EXCEPTION 'cargo_ful: capacity link %/%/% has no leg', NEW.tenant_id, NEW.shipment_id, NEW.link_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_capacity_link_legs_with_link';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER shipment_capacity_link_has_legs AFTER INSERT ON cargo_ful.shipment_capacity_link
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_capacity_link_legs_at_commit();
CREATE CONSTRAINT TRIGGER shipment_capacity_link_legs_with_link AFTER INSERT ON cargo_ful.shipment_capacity_link_leg
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_capacity_link_legs_at_commit();

-- An answer joins a shipment that is not cancelled, whose row is locked first; it follows the previous answer, answers
-- the shipment's latest link, and is not dated before it.
CREATE FUNCTION cargo_ful.check_date_answer() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_ful.shipment;
  last integer;
  latest cargo_ful.shipment_capacity_link;
BEGIN
  SELECT * INTO s FROM cargo_ful.shipment WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id FOR NO KEY UPDATE;
  SELECT coalesce(max(answer_no), 0) INTO last FROM cargo_ful.shipment_date_answer
    WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id;
  SELECT * INTO latest FROM cargo_ful.shipment_capacity_link
    WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id ORDER BY link_no DESC LIMIT 1;
  IF s.shipment_id IS NULL OR s.status = 'CANCELLED' OR NEW.answer_no <> last + 1 THEN
    RAISE EXCEPTION 'cargo_ful: shipment %/% takes no date answer %', NEW.tenant_id, NEW.shipment_id, NEW.answer_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_date_answer_follows';
  END IF;
  IF latest.link_no IS DISTINCT FROM NEW.link_no OR NEW.answered_at < latest.linked_at THEN
    RAISE EXCEPTION 'cargo_ful: date answer %/%/% answers link %, not the latest', NEW.tenant_id, NEW.shipment_id, NEW.answer_no, NEW.link_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_date_answer_latest_link';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER shipment_date_answer_rules BEFORE INSERT ON cargo_ful.shipment_date_answer
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_date_answer();

ALTER TABLE cargo_ful.shipment_capacity_link ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_ful.shipment_capacity_link FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_ful.shipment_capacity_link
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
ALTER TABLE cargo_ful.shipment_capacity_link_leg ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_ful.shipment_capacity_link_leg FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_ful.shipment_capacity_link_leg
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
ALTER TABLE cargo_ful.shipment_date_answer ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_ful.shipment_date_answer FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_ful.shipment_date_answer
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
