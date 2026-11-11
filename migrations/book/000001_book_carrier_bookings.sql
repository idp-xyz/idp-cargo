-- IDP-Cargo BOOK V0.1 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Carrier bookings for consolidations (N1, decisions N1-BOOK OD-1 to OD-6): the request versions, what the
-- operator sent through the carrier's channel and what the carrier answered, each with its evidence, the
-- capacity versions the carrier confirmed and their allocation to the booking's consolidation. Everything but
-- the booking's status and pointers is insert-only. No foreign key leaves cargo_book: the carrier is an ORG
-- party at a frozen revision, the consumer a PHY consolidation id.
CREATE SCHEMA IF NOT EXISTS cargo_book;

CREATE TABLE cargo_book.carrier_booking (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 booking_id text NOT NULL CHECK (length(btrim(booking_id)) BETWEEN 1 AND 128),
 kind text NOT NULL CHECK (kind IN ('AIR_CONSOL','SEA_LCL_CONSOL_EQUIPMENT','SEA_LCL_CO_LOAD')),
 status text NOT NULL CHECK (status IN ('DRAFT','REQUESTED','ACKNOWLEDGED','CONFIRMED','CHANGE_REQUESTED','CANCELLATION_REQUESTED',
   'REJECTED','DECLINED_BY_CARRIER','CANCELLED')),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 carrier_party_id text NOT NULL CHECK (length(btrim(carrier_party_id)) BETWEEN 1 AND 128),
 carrier_party_revision bigint NOT NULL CHECK (carrier_party_revision > 0),
 carrier_role text NOT NULL CHECK (carrier_role IN ('CARRIER','CO_LOADER')),
 carrier_role_valid_from timestamptz NOT NULL,
 carrier_role_valid_until timestamptz,
 carrier_role_evidence text NOT NULL CHECK (length(btrim(carrier_role_evidence)) BETWEEN 1 AND 500),
 carrier_iata_designator text CHECK (carrier_iata_designator ~ '^([A-Z][0-9A-Z]|[0-9][A-Z])$'),
 carrier_awb_prefix text CHECK (carrier_awb_prefix ~ '^[0-9]{3}$'),
 consumer_kind text NOT NULL CHECK (consumer_kind = 'CONSOL'),
 consumer_id text NOT NULL CHECK (length(btrim(consumer_id)) BETWEEN 1 AND 128),
 rebooks_booking_id text CHECK (length(btrim(rebooks_booking_id)) BETWEEN 1 AND 128),
 current_request_version integer NOT NULL CHECK (current_request_version > 0),
 current_capacity_version integer CHECK (current_capacity_version > 0),
 withdraw_reason text CHECK (length(btrim(withdraw_reason)) BETWEEN 1 AND 500),
 withdrawn_by text CHECK (length(btrim(withdrawn_by)) > 0),
 withdrawn_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, booking_id),
 CONSTRAINT carrier_booking_role CHECK ((kind = 'SEA_LCL_CO_LOAD') = (carrier_role = 'CO_LOADER')),
 CONSTRAINT carrier_booking_airline CHECK ((kind = 'AIR_CONSOL') = (carrier_iata_designator IS NOT NULL)
   AND (carrier_iata_designator IS NULL) = (carrier_awb_prefix IS NULL)),
 CONSTRAINT carrier_booking_withdrawal CHECK ((withdrawn_at IS NULL) = (withdrawn_by IS NULL) AND (withdrawn_at IS NULL) = (withdraw_reason IS NULL)
   AND (withdrawn_at IS NULL OR status = 'CANCELLED')),
 CONSTRAINT carrier_booking_rebooks CHECK (rebooks_booking_id IS DISTINCT FROM booking_id)
);
CREATE INDEX carrier_booking_consumer ON cargo_book.carrier_booking (tenant_id, consumer_kind, consumer_id);

CREATE TABLE cargo_book.booking_request_version (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 forwarder_reference text CHECK (length(forwarder_reference) BETWEEN 1 AND 35),
 pieces bigint CHECK (pieces > 0),
 gross_weight_unit text CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume > 0),
 handling_codes text[] NOT NULL DEFAULT '{}',
 location_edition text NOT NULL CHECK (location_edition ~ '^[A-Z0-9][A-Z0-9._-]{0,63}$'),
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 written_by text NOT NULL CHECK (length(btrim(written_by)) > 0),
 written_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, booking_id, version_no),
 FOREIGN KEY (tenant_id, booking_id) REFERENCES cargo_book.carrier_booking (tenant_id, booking_id),
 CONSTRAINT booking_request_reason CHECK ((version_no = 1) = (reason IS NULL)),
 CONSTRAINT booking_request_quantities CHECK ((gross_weight_unit IS NULL) = (gross_weight IS NULL) AND (volume_unit IS NULL) = (volume IS NULL))
);
CREATE TABLE cargo_book.booking_request_leg (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 version_no integer NOT NULL,
 leg_no integer NOT NULL CHECK (leg_no BETWEEN 1 AND 9),
 loading_code text NOT NULL CHECK (loading_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 loading_iata char(3) CHECK (loading_iata ~ '^[A-Z]{3}$'),
 discharge_code text NOT NULL CHECK (discharge_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 discharge_iata char(3) CHECK (discharge_iata ~ '^[A-Z]{3}$'),
 flight text CHECK (flight ~ '^([A-Z][0-9A-Z]|[0-9][A-Z])[0-9]{1,4}[A-Z]?$'),
 vessel text CHECK (length(btrim(vessel)) BETWEEN 1 AND 35),
 voyage text CHECK (voyage ~ '^[0-9A-Z][0-9A-Z/-]{0,16}$'),
 departure_date date,
 PRIMARY KEY (tenant_id, booking_id, version_no, leg_no),
 FOREIGN KEY (tenant_id, booking_id, version_no) REFERENCES cargo_book.booking_request_version (tenant_id, booking_id, version_no),
 CONSTRAINT booking_request_leg_ports CHECK (loading_code <> discharge_code AND (loading_iata IS NULL) = (discharge_iata IS NULL)),
 CONSTRAINT booking_request_leg_carriage CHECK ((vessel IS NULL) = (voyage IS NULL) AND (flight IS NULL OR vessel IS NULL))
);
CREATE TABLE cargo_book.booking_request_equipment (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 version_no integer NOT NULL,
 size_type text NOT NULL CHECK (size_type ~ '^[0-9A-Z]{4}$'),
 count bigint NOT NULL CHECK (count > 0),
 PRIMARY KEY (tenant_id, booking_id, version_no, size_type),
 FOREIGN KEY (tenant_id, booking_id, version_no) REFERENCES cargo_book.booking_request_version (tenant_id, booking_id, version_no)
);

-- Steps: what the operator sent and what the carrier answered, numbered together in the order recorded.
CREATE TABLE cargo_book.booking_submission (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 step_no integer NOT NULL CHECK (step_no > 0),
 kind text NOT NULL CHECK (kind IN ('REQUEST','CHANGE_REQUEST','CANCELLATION_REQUEST')),
 request_version integer NOT NULL,
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 channel text NOT NULL CHECK (channel IN ('CARRIER_PORTAL','EMAIL','PHONE','CO_LOADER_PORTAL')),
 evidence_sha256 char(64) NOT NULL CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_storage_id text NOT NULL CHECK (length(btrim(evidence_storage_id)) BETWEEN 1 AND 256),
 evidence_media_type text NOT NULL CHECK (length(btrim(evidence_media_type)) BETWEEN 1 AND 127),
 evidence_captured_at timestamptz NOT NULL,
 sent_by text NOT NULL CHECK (length(btrim(sent_by)) > 0),
 sent_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, booking_id, step_no),
 FOREIGN KEY (tenant_id, booking_id, request_version) REFERENCES cargo_book.booking_request_version (tenant_id, booking_id, version_no),
 CONSTRAINT booking_submission_reason CHECK ((kind = 'REQUEST') = (reason IS NULL)),
 CONSTRAINT booking_submission_evidence CHECK (evidence_captured_at <= sent_at)
);
CREATE TABLE cargo_book.carrier_response (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 step_no integer NOT NULL CHECK (step_no > 0),
 kind text NOT NULL CHECK (kind IN ('ACKNOWLEDGED','CONFIRMED','REJECTED','CARRIER_CHANGED','CARRIER_DECLINED','CHANGE_CONFIRMED',
   'CHANGE_REFUSED','CANCELLATION_CONFIRMED')),
 answers_request_version integer NOT NULL,
 reason_code text CHECK (length(btrim(reason_code)) BETWEEN 1 AND 35),
 reason_text text CHECK (length(btrim(reason_text)) BETWEEN 1 AND 500),
 channel text NOT NULL CHECK (channel IN ('CARRIER_PORTAL','EMAIL','PHONE','CO_LOADER_PORTAL')),
 evidence_sha256 char(64) NOT NULL CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_storage_id text NOT NULL CHECK (length(btrim(evidence_storage_id)) BETWEEN 1 AND 256),
 evidence_media_type text NOT NULL CHECK (length(btrim(evidence_media_type)) BETWEEN 1 AND 127),
 evidence_captured_at timestamptz NOT NULL,
 verification_level text NOT NULL CHECK (verification_level = 'OPERATOR_ENTERED_WITH_EVIDENCE'),
 carrier_time timestamptz,
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 capacity_version integer CHECK (capacity_version > 0),
 PRIMARY KEY (tenant_id, booking_id, step_no),
 FOREIGN KEY (tenant_id, booking_id, answers_request_version) REFERENCES cargo_book.booking_request_version (tenant_id, booking_id, version_no),
 CONSTRAINT carrier_response_capacity CHECK ((kind IN ('CONFIRMED','CARRIER_CHANGED','CHANGE_CONFIRMED')) = (capacity_version IS NOT NULL)),
 CONSTRAINT carrier_response_reason CHECK (kind NOT IN ('REJECTED','CARRIER_DECLINED') OR reason_code IS NOT NULL OR reason_text IS NOT NULL),
 CONSTRAINT carrier_response_evidence CHECK (evidence_captured_at <= recorded_at)
);
-- A carrier reference is unique among the live bookings of the tenant with the same carrier and type (G02-05);
-- a booking's references stop being live when it ends.
CREATE TABLE cargo_book.carrier_reference (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 ref_type text NOT NULL CHECK (ref_type IN ('BOOKING_NUMBER','MAWB_NUMBER','CO_LOAD_REFERENCE','CARRIER_REQUEST_ACK')),
 value text NOT NULL CHECK (length(btrim(value)) BETWEEN 1 AND 35 AND value = upper(value)),
 carrier_party_id text NOT NULL,
 first_step integer NOT NULL,
 live boolean NOT NULL,
 PRIMARY KEY (tenant_id, booking_id, ref_type, value),
 FOREIGN KEY (tenant_id, booking_id, first_step) REFERENCES cargo_book.carrier_response (tenant_id, booking_id, step_no),
 CONSTRAINT carrier_reference_mawb CHECK (ref_type <> 'MAWB_NUMBER' OR value ~ '^[0-9]{3}-[0-9]{8}$'
   AND right(value, 1)::int = (substr(value, 5, 7)::int % 7))
);
CREATE UNIQUE INDEX carrier_reference_live ON cargo_book.carrier_reference (tenant_id, carrier_party_id, ref_type, value) WHERE live;
CREATE UNIQUE INDEX carrier_reference_one_mawb ON cargo_book.carrier_reference (tenant_id, booking_id) WHERE ref_type = 'MAWB_NUMBER';

-- Capacity versions: what a granting answer confirmed, immutable; the latest is current.
CREATE TABLE cargo_book.capacity_version (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 reason text NOT NULL CHECK (reason IN ('INITIAL','PARTIAL_CONFIRMATION','SCHEDULE_CHANGE','ROLLOVER','CARRIER_REDUCED','SPLIT','REQUESTED_CHANGE')),
 response_step integer NOT NULL,
 location_edition text NOT NULL CHECK (location_edition ~ '^[A-Z0-9][A-Z0-9._-]{0,63}$'),
 PRIMARY KEY (tenant_id, booking_id, version_no),
 UNIQUE (tenant_id, booking_id, response_step),
 FOREIGN KEY (tenant_id, booking_id, response_step) REFERENCES cargo_book.carrier_response (tenant_id, booking_id, step_no)
);
CREATE TABLE cargo_book.capacity_line (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 version_no integer NOT NULL,
 line_no integer NOT NULL CHECK (line_no BETWEEN 1 AND 20),
 pieces bigint CHECK (pieces > 0),
 gross_weight_unit text CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume > 0),
 carrier_reference text CHECK (length(btrim(carrier_reference)) BETWEEN 1 AND 35),
 PRIMARY KEY (tenant_id, booking_id, version_no, line_no),
 FOREIGN KEY (tenant_id, booking_id, version_no) REFERENCES cargo_book.capacity_version (tenant_id, booking_id, version_no),
 CONSTRAINT capacity_line_quantities CHECK ((gross_weight_unit IS NULL) = (gross_weight IS NULL) AND (volume_unit IS NULL) = (volume IS NULL))
);
CREATE TABLE cargo_book.capacity_equipment (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 version_no integer NOT NULL,
 line_no integer NOT NULL,
 size_type text NOT NULL CHECK (size_type ~ '^[0-9A-Z]{4}$'),
 count bigint NOT NULL CHECK (count > 0),
 PRIMARY KEY (tenant_id, booking_id, version_no, line_no, size_type),
 FOREIGN KEY (tenant_id, booking_id, version_no, line_no) REFERENCES cargo_book.capacity_line (tenant_id, booking_id, version_no, line_no)
);
CREATE TABLE cargo_book.capacity_leg (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 version_no integer NOT NULL,
 line_no integer NOT NULL,
 leg_no integer NOT NULL CHECK (leg_no BETWEEN 1 AND 9),
 loading_code text NOT NULL CHECK (loading_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 loading_iata char(3) CHECK (loading_iata ~ '^[A-Z]{3}$'),
 discharge_code text NOT NULL CHECK (discharge_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 discharge_iata char(3) CHECK (discharge_iata ~ '^[A-Z]{3}$'),
 flight text CHECK (flight ~ '^([A-Z][0-9A-Z]|[0-9][A-Z])[0-9]{1,4}[A-Z]?$'),
 vessel text CHECK (length(btrim(vessel)) BETWEEN 1 AND 35),
 vessel_imo char(7) CHECK (vessel_imo ~ '^[0-9]{7}$'),
 voyage text CHECK (voyage ~ '^[0-9A-Z][0-9A-Z/-]{0,16}$'),
 etd timestamptz NOT NULL,
 eta timestamptz,
 PRIMARY KEY (tenant_id, booking_id, version_no, line_no, leg_no),
 FOREIGN KEY (tenant_id, booking_id, version_no, line_no) REFERENCES cargo_book.capacity_line (tenant_id, booking_id, version_no, line_no),
 CONSTRAINT capacity_leg_ports CHECK (loading_code <> discharge_code AND (loading_iata IS NULL) = (discharge_iata IS NULL)),
 CONSTRAINT capacity_leg_carriage CHECK ((flight IS NOT NULL) <> (vessel IS NOT NULL) AND (vessel IS NULL) = (voyage IS NULL)
   AND (vessel_imo IS NULL OR vessel IS NOT NULL)),
 CONSTRAINT capacity_leg_times CHECK (eta IS NULL OR eta > etd)
);
CREATE TABLE cargo_book.capacity_cut_off (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 version_no integer NOT NULL,
 line_no integer NOT NULL,
 leg_no integer NOT NULL,
 kind text NOT NULL CHECK (kind IN ('CARGO_ACCEPTANCE','DOCUMENTS','SECURITY_SCREENING','MANIFEST','CY_FULL_CONTAINER',
   'SHIPPING_INSTRUCTIONS_DOCUMENTS','VGM','CUSTOMS_RELEASE','HAZARDOUS','RECEIVING_COMMENCES')),
 cut_off_at timestamptz NOT NULL,
 time_zone text NOT NULL CHECK (length(time_zone) BETWEEN 1 AND 64),
 location_code text NOT NULL CHECK (location_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 PRIMARY KEY (tenant_id, booking_id, version_no, line_no, leg_no, kind),
 FOREIGN KEY (tenant_id, booking_id, version_no, line_no, leg_no) REFERENCES cargo_book.capacity_leg (tenant_id, booking_id, version_no, line_no, leg_no)
);
CREATE INDEX capacity_cut_off_due ON cargo_book.capacity_cut_off (tenant_id, cut_off_at);

-- Allocation versions: the current capacity allocated to the consolidation; moving or releasing it writes the
-- allocation's next version.
CREATE TABLE cargo_book.capacity_allocation (
 tenant_id text NOT NULL,
 booking_id text NOT NULL,
 allocation_no integer NOT NULL CHECK (allocation_no > 0),
 version_no integer NOT NULL CHECK (version_no > 0),
 capacity_version integer NOT NULL,
 consumer_kind text NOT NULL CHECK (consumer_kind = 'CONSOL'),
 consumer_id text NOT NULL CHECK (length(btrim(consumer_id)) BETWEEN 1 AND 128),
 status text NOT NULL CHECK (status IN ('ACTIVE','RELEASED')),
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 written_by text NOT NULL CHECK (length(btrim(written_by)) > 0),
 written_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, booking_id, allocation_no, version_no),
 FOREIGN KEY (tenant_id, booking_id, capacity_version) REFERENCES cargo_book.capacity_version (tenant_id, booking_id, version_no)
);

CREATE TABLE cargo_book.command_receipt (
 tenant_id text NOT NULL,
 command_id text NOT NULL,
 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 result_type text NOT NULL CHECK (result_type = 'CarrierBooking'),
 result_id text NOT NULL,
 result_revision bigint NOT NULL CHECK (result_revision > 0),
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, command_id)
);

-- Only the status, pointers, consumer, withdrawal, revision and updated_at of a booking change, and an ended
-- booking never changes again; bookings are never deleted.
CREATE FUNCTION cargo_book.reject_booking_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['status','current_request_version','current_capacity_version','consumer_id','withdraw_reason','withdrawn_by',
    'withdrawn_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_book: booking %/% cannot be deleted', OLD.tenant_id, OLD.booking_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.status IN ('REJECTED','DECLINED_BY_CARRIER','CANCELLED') OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_book: booking %/% changes only its status, and never once ended', OLD.tenant_id, OLD.booking_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER carrier_booking_rules BEFORE UPDATE OR DELETE ON cargo_book.carrier_booking
  FOR EACH ROW EXECUTE FUNCTION cargo_book.reject_booking_change();

CREATE FUNCTION cargo_book.reject_row_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_book: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['booking_request_version','booking_request_leg','booking_request_equipment','booking_submission','carrier_response',
      'capacity_version','capacity_line','capacity_equipment','capacity_leg','capacity_cut_off','capacity_allocation'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_book.%I FOR EACH ROW EXECUTE FUNCTION cargo_book.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- A reference only stops being live, once; references are never deleted.
CREATE FUNCTION cargo_book.check_reference_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR NOT OLD.live OR NEW.live OR to_jsonb(OLD) - 'live' IS DISTINCT FROM to_jsonb(NEW) - 'live' THEN
    RAISE EXCEPTION 'cargo_book: reference %/% can only stop being live', OLD.booking_id, OLD.value USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER carrier_reference_rules BEFORE UPDATE OR DELETE ON cargo_book.carrier_reference
  FOR EACH ROW EXECUTE FUNCTION cargo_book.check_reference_change();

-- At commit, for every booking a transaction writes: the pointers name the latest request and capacity
-- versions; at most one allocation is ACTIVE, and exactly one, on the current capacity and to the booking's
-- consumer, while capacity is confirmed and the booking in force; an ended booking has no live reference and an
-- air consolidation's confirmed booking has its MAWB number; steps are numbered without gaps.
CREATE FUNCTION cargo_book.check_booking_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  b cargo_book.carrier_booking;
  requests integer;
  capacities integer;
  steps integer;
  max_step integer;
  active integer;
  active_ok boolean;
BEGIN
  SELECT * INTO b FROM cargo_book.carrier_booking WHERE tenant_id = NEW.tenant_id AND booking_id = NEW.booking_id;
  SELECT max(version_no) INTO requests FROM cargo_book.booking_request_version WHERE tenant_id = b.tenant_id AND booking_id = b.booking_id;
  SELECT max(version_no) INTO capacities FROM cargo_book.capacity_version WHERE tenant_id = b.tenant_id AND booking_id = b.booking_id;
  SELECT count(*), max(step_no) INTO steps, max_step FROM (
      SELECT step_no FROM cargo_book.booking_submission WHERE tenant_id = b.tenant_id AND booking_id = b.booking_id
      UNION ALL SELECT step_no FROM cargo_book.carrier_response WHERE tenant_id = b.tenant_id AND booking_id = b.booking_id) s;
  SELECT count(*), bool_and(a.capacity_version = capacities AND a.consumer_id = b.consumer_id) INTO active, active_ok
    FROM cargo_book.capacity_allocation a
    WHERE a.tenant_id = b.tenant_id AND a.booking_id = b.booking_id AND a.status = 'ACTIVE'
      AND a.version_no = (SELECT max(version_no) FROM cargo_book.capacity_allocation x
                          WHERE x.tenant_id = a.tenant_id AND x.booking_id = a.booking_id AND x.allocation_no = a.allocation_no);
  IF requests IS DISTINCT FROM b.current_request_version OR capacities IS DISTINCT FROM b.current_capacity_version
     OR steps <> coalesce(max_step, 0)
     OR active > 1 OR (active = 1) <> (capacities IS NOT NULL AND b.status NOT IN ('REJECTED','DECLINED_BY_CARRIER','CANCELLED'))
     OR (active = 1 AND NOT active_ok)
     OR (b.status IN ('DRAFT') AND steps > 0)
     OR (b.status IN ('CONFIRMED','CHANGE_REQUESTED') AND capacities IS NULL)
     OR (b.status IN ('REJECTED','DECLINED_BY_CARRIER','CANCELLED') AND EXISTS (SELECT 1 FROM cargo_book.carrier_reference r
           WHERE r.tenant_id = b.tenant_id AND r.booking_id = b.booking_id AND r.live))
     OR (b.kind = 'AIR_CONSOL' AND capacities IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_book.carrier_reference r
           WHERE r.tenant_id = b.tenant_id AND r.booking_id = b.booking_id AND r.ref_type = 'MAWB_NUMBER')) THEN
    RAISE EXCEPTION 'cargo_book: booking %/% is inconsistent at commit', b.tenant_id, b.booking_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'carrier_booking_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER carrier_booking_consistent AFTER INSERT OR UPDATE ON cargo_book.carrier_booking
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_book.check_booking_at_commit();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['carrier_booking','booking_request_version','booking_request_leg','booking_request_equipment','booking_submission',
      'carrier_response','carrier_reference','capacity_version','capacity_line','capacity_equipment','capacity_leg','capacity_cut_off',
      'capacity_allocation','command_receipt'] LOOP
    EXECUTE format('ALTER TABLE cargo_book.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_book.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_book.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
