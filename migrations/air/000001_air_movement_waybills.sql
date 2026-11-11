-- IDP-Cargo AIR V0.1 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The air movement of an air consolidation (N1, decisions N1-PHY OD-2, OD-7, OD-8, OD-10 and N1-HAWB): the MAWB
-- number and flights from the airline's booking confirmation (BOOK), and one immutable house air waybill entry per
-- house and plan version, each with the HAWB number the operating legal entity's series gave the shipment for good.
-- No foreign key leaves cargo_air: the consolidation is PHY's, the booking BOOK's, the shipment FUL's.
CREATE SCHEMA IF NOT EXISTS cargo_air;

CREATE TABLE cargo_air.air_movement (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 movement_id text NOT NULL CHECK (length(btrim(movement_id)) BETWEEN 1 AND 128),
 consolidation_id text NOT NULL CHECK (length(btrim(consolidation_id)) BETWEEN 1 AND 128),
 operating_legal_entity_id text NOT NULL CHECK (length(btrim(operating_legal_entity_id)) BETWEEN 1 AND 128),
 airline_party_id text NOT NULL CHECK (length(btrim(airline_party_id)) BETWEEN 1 AND 128),
 airline_party_revision bigint NOT NULL CHECK (airline_party_revision > 0),
 airline_designator text NOT NULL CHECK (airline_designator ~ '^([A-Z][0-9A-Z]|[0-9][A-Z])$'),
 awb_prefix text NOT NULL CHECK (awb_prefix ~ '^[0-9]{3}$'),
 origin_code text NOT NULL CHECK (origin_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 origin_iata char(3) NOT NULL CHECK (origin_iata ~ '^[A-Z]{3}$'),
 destination_code text NOT NULL CHECK (destination_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 destination_iata char(3) NOT NULL CHECK (destination_iata ~ '^[A-Z]{3}$'),
 location_edition text NOT NULL CHECK (location_edition ~ '^[A-Z0-9][A-Z0-9._-]{0,63}$'),
 state text NOT NULL CHECK (state IN ('OPEN','CANCELLED')),
 mawb_number text NOT NULL CHECK (mawb_number ~ '^[0-9]{3}-[0-9]{8}$' AND right(mawb_number, 1)::int = substr(mawb_number, 5, 7)::int % 7),
 mawb_status text NOT NULL CHECK (mawb_status IN ('ASSIGNED','VOIDED')),
 mawb_booking_id text NOT NULL,
 mawb_capacity_version integer NOT NULL CHECK (mawb_capacity_version > 0),
 cancel_reason text CHECK (length(btrim(cancel_reason)) BETWEEN 1 AND 500),
 cancelled_by text CHECK (length(btrim(cancelled_by)) > 0),
 cancelled_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, movement_id),
 CONSTRAINT air_movement_route CHECK (origin_code <> destination_code),
 CONSTRAINT air_movement_prefix CHECK (left(mawb_number, 3) = awb_prefix),
 CONSTRAINT air_movement_cancellation CHECK ((state = 'CANCELLED') = (cancelled_at IS NOT NULL) AND (cancelled_by IS NULL) = (cancelled_at IS NULL)
   AND (cancel_reason IS NULL) = (cancelled_at IS NULL) AND (mawb_status = 'VOIDED') = (state = 'CANCELLED'))
);
-- One open movement per air consolidation; a MAWB number is assigned to one movement of the tenant.
CREATE UNIQUE INDEX air_movement_one_open ON cargo_air.air_movement (tenant_id, consolidation_id) WHERE state = 'OPEN';
CREATE UNIQUE INDEX air_movement_mawb_assigned ON cargo_air.air_movement (tenant_id, mawb_number) WHERE mawb_status = 'ASSIGNED';

-- Each link to a capacity version of the booking, with its flights; the latest is current.
CREATE TABLE cargo_air.booking_link (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 link_no integer NOT NULL CHECK (link_no > 0),
 booking_id text NOT NULL CHECK (length(btrim(booking_id)) BETWEEN 1 AND 128),
 booking_revision bigint NOT NULL CHECK (booking_revision > 0),
 capacity_version integer NOT NULL CHECK (capacity_version > 0),
 linked_by text NOT NULL CHECK (length(btrim(linked_by)) > 0),
 linked_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, movement_id, link_no),
 UNIQUE (tenant_id, movement_id, capacity_version),
 FOREIGN KEY (tenant_id, movement_id) REFERENCES cargo_air.air_movement (tenant_id, movement_id)
);
CREATE TABLE cargo_air.flight_segment (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 link_no integer NOT NULL,
 segment_no integer NOT NULL CHECK (segment_no BETWEEN 1 AND 9),
 flight text NOT NULL CHECK (flight ~ '^([A-Z][0-9A-Z]|[0-9][A-Z])[0-9]{1,4}[A-Z]?$'),
 from_code text NOT NULL CHECK (from_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 from_iata char(3) NOT NULL CHECK (from_iata ~ '^[A-Z]{3}$'),
 to_code text NOT NULL CHECK (to_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 to_iata char(3) NOT NULL CHECK (to_iata ~ '^[A-Z]{3}$'),
 etd timestamptz NOT NULL,
 eta timestamptz,
 PRIMARY KEY (tenant_id, movement_id, link_no, segment_no),
 FOREIGN KEY (tenant_id, movement_id, link_no) REFERENCES cargo_air.booking_link (tenant_id, movement_id, link_no),
 CONSTRAINT flight_segment_airports CHECK (from_code <> to_code AND (eta IS NULL OR eta > etd))
);

-- House air waybill entries: frozen per FUL plan version; only the end is written later, once.
CREATE TABLE cargo_air.house_waybill (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no > 0),
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 hawb text NOT NULL CHECK (hawb ~ '^[0-9A-Z]{1,12}$'),
 ful_revision bigint NOT NULL CHECK (ful_revision > 0),
 plan_version integer NOT NULL CHECK (plan_version > 0),
 window_from timestamptz NOT NULL,
 window_until timestamptz,
 pieces bigint NOT NULL CHECK (pieces > 0),
 gross_weight_unit text NOT NULL CHECK (gross_weight_unit IN ('KGM','GRM','TNE')),
 gross_weight bigint NOT NULL CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit IN ('MTQ','DMQ','LTR','CMQ')),
 volume bigint CHECK (volume > 0),
 chargeable_grams bigint NOT NULL CHECK (chargeable_grams > 0 AND chargeable_grams % 500 = 0),
 chargeable_rule text NOT NULL CHECK (length(btrim(chargeable_rule)) BETWEEN 1 AND 64),
 basis text NOT NULL CHECK (basis = 'PLANNED'),
 start_kind text NOT NULL CHECK (start_kind IN ('ASSOCIATED','REFRESHED')),
 predecessor_no integer,
 associated_by text NOT NULL CHECK (length(btrim(associated_by)) > 0),
 associated_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 end_kind text CHECK (end_kind IN ('REFRESHED','DETACHED','TRANSFERRED','SHIPMENT_CANCELLED')),
 ended_by text CHECK (length(btrim(ended_by)) > 0),
 ended_at timestamptz,
 end_entered_at timestamptz,
 successor_no integer,
 PRIMARY KEY (tenant_id, movement_id, entry_no),
 FOREIGN KEY (tenant_id, movement_id) REFERENCES cargo_air.air_movement (tenant_id, movement_id),
 CONSTRAINT house_waybill_volume CHECK ((volume_unit IS NULL) = (volume IS NULL)),
 CONSTRAINT house_waybill_start CHECK ((start_kind = 'ASSOCIATED') = (predecessor_no IS NULL) AND (predecessor_no IS NULL OR predecessor_no < entry_no)),
 CONSTRAINT house_waybill_end CHECK ((end_kind IS NULL) = (ended_at IS NULL) AND (ended_by IS NULL) = (ended_at IS NULL)
   AND (end_entered_at IS NULL) = (ended_at IS NULL) AND (ended_at IS NULL OR ended_at >= associated_at)
   AND (successor_no IS NOT NULL) = coalesce(end_kind = 'REFRESHED', false) AND (successor_no IS NULL OR successor_no > entry_no))
);
-- A shipment has at most one live house waybill in the tenant; a HAWB at most one live entry per movement.
CREATE UNIQUE INDEX house_waybill_one_live ON cargo_air.house_waybill (tenant_id, shipment_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX house_waybill_live_hawb ON cargo_air.house_waybill (tenant_id, movement_id, hawb) WHERE ended_at IS NULL;
CREATE TABLE cargo_air.house_waybill_part (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 entry_no integer NOT NULL,
 part_no integer NOT NULL CHECK (part_no > 0),
 obligation_id text NOT NULL CHECK (length(btrim(obligation_id)) BETWEEN 1 AND 128),
 obligation_version bigint NOT NULL CHECK (obligation_version > 0),
 order_id text NOT NULL CHECK (length(btrim(order_id)) BETWEEN 1 AND 128),
 packs bigint NOT NULL CHECK (packs > 0),
 package_type text NOT NULL CHECK (package_type ~ '^[A-Z0-9]{2}$'),
 gross_weight_unit text NOT NULL,
 gross_weight bigint NOT NULL CHECK (gross_weight > 0),
 volume_unit text,
 volume bigint CHECK (volume > 0),
 PRIMARY KEY (tenant_id, movement_id, entry_no, part_no),
 FOREIGN KEY (tenant_id, movement_id, entry_no) REFERENCES cargo_air.house_waybill (tenant_id, movement_id, entry_no)
);

-- A legal entity's HAWB series and the register of every HAWB number it gave or was given: a number belongs to
-- one shipment for ever and a shipment keeps one number (N1-HAWB).
CREATE TABLE cargo_air.hawb_series (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 prefix text NOT NULL CHECK (prefix ~ '^[0-9A-Z]{1,4}$'),
 width integer NOT NULL CHECK (width BETWEEN 4 AND 8),
 next_seq bigint NOT NULL CHECK (next_seq >= 1),
 registered_by text NOT NULL CHECK (length(btrim(registered_by)) > 0),
 registered_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 PRIMARY KEY (tenant_id, legal_entity_id),
 CONSTRAINT hawb_series_length CHECK (length(prefix) + width <= 12 AND length(next_seq::text) <= width + 1)
);
CREATE TABLE cargo_air.hawb_number (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 hawb text NOT NULL CHECK (hawb ~ '^[0-9A-Z]{1,12}$'),
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 drawn boolean NOT NULL,
 claimed_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, hawb),
 CONSTRAINT hawb_number_one_per_shipment UNIQUE (tenant_id, legal_entity_id, shipment_id)
);

CREATE TABLE cargo_air.command_receipt (
 tenant_id text NOT NULL,
 command_id text NOT NULL,
 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 result_type text NOT NULL CHECK (result_type IN ('AirMovement','HawbSeries')),
 result_id text NOT NULL,
 result_revision bigint NOT NULL CHECK (result_revision > 0),
 result_entry_no integer CHECK (result_entry_no > 0),
 result_hawb text,
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, command_id)
);

-- Only the state, cancellation, MAWB status, revision and updated_at of a movement change, and a cancelled one
-- never changes again; a series changes only its next number and revision; nothing is deleted.
CREATE FUNCTION cargo_air.reject_movement_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','mawb_status','cancel_reason','cancelled_by','cancelled_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.state = 'CANCELLED' OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_air: movement %/% changes only its state, and never once cancelled', OLD.tenant_id, OLD.movement_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER air_movement_rules BEFORE UPDATE OR DELETE ON cargo_air.air_movement FOR EACH ROW EXECUTE FUNCTION cargo_air.reject_movement_change();

CREATE FUNCTION cargo_air.reject_series_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['next_seq','revision'];
BEGIN
  IF TG_OP = 'DELETE' OR NEW.next_seq <= OLD.next_seq OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_air: HAWB series %/% only moves forward', OLD.tenant_id, OLD.legal_entity_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER hawb_series_rules BEFORE UPDATE OR DELETE ON cargo_air.hawb_series FOR EACH ROW EXECUTE FUNCTION cargo_air.reject_series_change();

CREATE FUNCTION cargo_air.reject_row_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_air: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['booking_link','flight_segment','house_waybill_part','hawb_number'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_air.%I FOR EACH ROW EXECUTE FUNCTION cargo_air.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- An entry is only ended, once, while its movement is open.
CREATE FUNCTION cargo_air.check_house_waybill_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  ends text[] := ARRAY['end_kind','ended_by','ended_at','end_entered_at','successor_no'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.ended_at IS NOT NULL OR NEW.ended_at IS NULL OR to_jsonb(OLD) - ends IS DISTINCT FROM to_jsonb(NEW) - ends THEN
    RAISE EXCEPTION 'cargo_air: house waybill %/%/% can only be ended, once', OLD.tenant_id, OLD.movement_id, OLD.entry_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER house_waybill_rules BEFORE UPDATE OR DELETE ON cargo_air.house_waybill FOR EACH ROW EXECUTE FUNCTION cargo_air.check_house_waybill_change();

-- At commit, for every house waybill a transaction writes: a live entry belongs to an open movement and its
-- HAWB is the number registered to its shipment under the movement's legal entity.
CREATE FUNCTION cargo_air.check_house_waybill_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  e cargo_air.house_waybill;
  m cargo_air.air_movement;
BEGIN
  SELECT * INTO e FROM cargo_air.house_waybill WHERE tenant_id = NEW.tenant_id AND movement_id = NEW.movement_id AND entry_no = NEW.entry_no;
  IF e.ended_at IS NOT NULL THEN
    RETURN NULL;
  END IF;
  SELECT * INTO m FROM cargo_air.air_movement WHERE tenant_id = e.tenant_id AND movement_id = e.movement_id;
  IF m.state <> 'OPEN' OR NOT EXISTS (SELECT 1 FROM cargo_air.hawb_number n WHERE n.tenant_id = e.tenant_id
       AND n.legal_entity_id = m.operating_legal_entity_id AND n.hawb = e.hawb AND n.shipment_id = e.shipment_id) THEN
    RAISE EXCEPTION 'cargo_air: house waybill %/%/% needs an open movement and its shipment''s registered HAWB', e.tenant_id, e.movement_id, e.entry_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'house_waybill_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER house_waybill_consistent AFTER INSERT ON cargo_air.house_waybill
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_air.check_house_waybill_at_commit();

-- At commit, a cancelled movement keeps no live house waybill, and a movement's MAWB comes from its first link.
CREATE FUNCTION cargo_air.check_movement_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.state = 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_air.house_waybill e
       WHERE e.tenant_id = NEW.tenant_id AND e.movement_id = NEW.movement_id AND e.ended_at IS NULL)
     OR NOT EXISTS (SELECT 1 FROM cargo_air.booking_link l WHERE l.tenant_id = NEW.tenant_id AND l.movement_id = NEW.movement_id
       AND l.link_no = 1 AND l.booking_id = NEW.mawb_booking_id AND l.capacity_version = NEW.mawb_capacity_version) THEN
    RAISE EXCEPTION 'cargo_air: movement %/% is inconsistent at commit', NEW.tenant_id, NEW.movement_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'air_movement_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER air_movement_consistent AFTER INSERT OR UPDATE ON cargo_air.air_movement
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_air.check_movement_at_commit();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['air_movement','booking_link','flight_segment','house_waybill','house_waybill_part','hawb_series','hawb_number',
      'command_receipt'] LOOP
    EXECUTE format('ALTER TABLE cargo_air.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_air.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_air.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
