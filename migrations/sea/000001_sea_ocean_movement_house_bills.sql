-- IDP-Cargo SEA V0.1 (sea freight part 1d) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The ocean movement of a sea LCL consolidation (N1, decisions N1-PHY OD-2, OD-6, OD-8, OD-10): the voyage legs and
-- cut-offs from the carrier's or co-loader's booking confirmation (BOOK), the master bill reference the carrier or
-- co-loader states, and one immutable house bill entry per house and plan version, each with the HBL number the
-- operating legal entity's series gave the shipment for good, its parts and its per-container breakdown as FUL
-- relays it from PHY. HBL numbers come from idp-platform-go's numbering (MD-28): cargo-migrate creates
-- cargo_sea.numbering_sequence from the platform's asset just before this migration.
-- No foreign key leaves cargo_sea: the consolidation is PHY's, the booking BOOK's, the shipment FUL's.
CREATE SCHEMA IF NOT EXISTS cargo_sea;

CREATE TABLE cargo_sea.ocean_movement (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 movement_id text NOT NULL CHECK (length(btrim(movement_id)) BETWEEN 1 AND 128),
 consolidation_id text NOT NULL CHECK (length(btrim(consolidation_id)) BETWEEN 1 AND 128),
 kind text NOT NULL CHECK (kind IN ('SEA_LCL_OWN','SEA_LCL_COLOAD')),
 operating_legal_entity_id text NOT NULL CHECK (length(btrim(operating_legal_entity_id)) BETWEEN 1 AND 128),
 carrier_party_id text NOT NULL CHECK (length(btrim(carrier_party_id)) BETWEEN 1 AND 128),
 carrier_party_revision bigint NOT NULL CHECK (carrier_party_revision > 0),
 carrier_role text NOT NULL CHECK (carrier_role IN ('CARRIER','CO_LOADER')),
 origin_code text NOT NULL CHECK (origin_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 destination_code text NOT NULL CHECK (destination_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 location_edition text NOT NULL CHECK (location_edition ~ '^[A-Z0-9][A-Z0-9._-]{0,63}$'),
 state text NOT NULL CHECK (state IN ('OPEN','CANCELLED')),
 booking_id text NOT NULL CHECK (length(btrim(booking_id)) BETWEEN 1 AND 128),
 cancel_reason text CHECK (length(btrim(cancel_reason)) BETWEEN 1 AND 500),
 cancelled_by text CHECK (length(btrim(cancelled_by)) > 0),
 cancelled_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, movement_id),
 CONSTRAINT ocean_movement_route CHECK (origin_code <> destination_code),
 CONSTRAINT ocean_movement_carrier_role CHECK ((kind = 'SEA_LCL_COLOAD') = (carrier_role = 'CO_LOADER')),
 CONSTRAINT ocean_movement_cancellation CHECK ((state = 'CANCELLED') = (cancelled_at IS NOT NULL) AND (cancelled_by IS NULL) = (cancelled_at IS NULL)
   AND (cancel_reason IS NULL) = (cancelled_at IS NULL))
);
-- One open movement per sea consolidation.
CREATE UNIQUE INDEX ocean_movement_one_open ON cargo_sea.ocean_movement (tenant_id, consolidation_id) WHERE state = 'OPEN';

-- Each link to a capacity version of the booking, with its voyage legs and their cut-offs; the latest is current.
CREATE TABLE cargo_sea.booking_link (
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
 FOREIGN KEY (tenant_id, movement_id) REFERENCES cargo_sea.ocean_movement (tenant_id, movement_id)
);
CREATE TABLE cargo_sea.voyage_leg (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 link_no integer NOT NULL,
 leg_no integer NOT NULL CHECK (leg_no BETWEEN 1 AND 9),
 vessel text NOT NULL CHECK (length(vessel) BETWEEN 1 AND 35 AND btrim(vessel) = vessel),
 vessel_imo text,
 voyage text NOT NULL CHECK (voyage ~ '^[0-9A-Z][0-9A-Z/-]{0,16}$'),
 from_code text NOT NULL CHECK (from_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 to_code text NOT NULL CHECK (to_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 etd timestamptz NOT NULL,
 eta timestamptz,
 PRIMARY KEY (tenant_id, movement_id, link_no, leg_no),
 FOREIGN KEY (tenant_id, movement_id, link_no) REFERENCES cargo_sea.booking_link (tenant_id, movement_id, link_no),
 CONSTRAINT voyage_leg_ports CHECK (from_code <> to_code AND (eta IS NULL OR eta > etd)),
 -- An IMO number has seven digits; the seventh is the sum of the first six weighted 7 to 2, modulo 10.
 CONSTRAINT voyage_leg_imo CHECK (vessel_imo IS NULL OR CASE WHEN vessel_imo ~ '^[0-9]{7}$' THEN right(vessel_imo, 1)::int = (substr(vessel_imo, 1, 1)::int * 7
   + substr(vessel_imo, 2, 1)::int * 6 + substr(vessel_imo, 3, 1)::int * 5 + substr(vessel_imo, 4, 1)::int * 4 + substr(vessel_imo, 5, 1)::int * 3
   + substr(vessel_imo, 6, 1)::int * 2) % 10 ELSE false END)
);
-- Cut-offs as the carrier set them and BOOK recorded them: an instant with the place's IANA time zone.
CREATE TABLE cargo_sea.voyage_cut_off (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 link_no integer NOT NULL,
 leg_no integer NOT NULL,
 cut_off_no integer NOT NULL CHECK (cut_off_no > 0),
 kind text NOT NULL CHECK (kind IN ('DOCUMENTS','MANIFEST','CY_FULL_CONTAINER','SHIPPING_INSTRUCTIONS_DOCUMENTS','VGM','CUSTOMS_RELEASE','HAZARDOUS',
   'RECEIVING_COMMENCES')),
 cut_off_at timestamptz NOT NULL,
 time_zone text NOT NULL CHECK (time_zone ~ '^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$' AND length(time_zone) <= 64),
 location_code text CHECK (location_code ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 PRIMARY KEY (tenant_id, movement_id, link_no, leg_no, cut_off_no),
 UNIQUE (tenant_id, movement_id, link_no, leg_no, kind),
 FOREIGN KEY (tenant_id, movement_id, link_no, leg_no) REFERENCES cargo_sea.voyage_leg (tenant_id, movement_id, link_no, leg_no)
);

-- Master bill references as the movement's carrier or co-loader stated them, with the evidence. Only the status
-- changes: the latest is CURRENT until a further one supersedes it or the movement is cancelled (VOIDED).
CREATE TABLE cargo_sea.master_bill (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 bill_no integer NOT NULL CHECK (bill_no > 0),
 bill_number text NOT NULL CHECK (bill_number ~ '^[0-9A-Z]+([/-][0-9A-Z]+)*$' AND length(bill_number) <= 35),
 kind text NOT NULL CHECK (kind IN ('CARRIER_MBL','CARRIER_SEA_WAYBILL','COLOAD_BILL')),
 issuer_party_id text NOT NULL CHECK (length(btrim(issuer_party_id)) BETWEEN 1 AND 128),
 evidence_sha256 text NOT NULL CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_storage_id text NOT NULL CHECK (length(evidence_storage_id) BETWEEN 1 AND 256 AND btrim(evidence_storage_id) = evidence_storage_id),
 evidence_media_type text NOT NULL CHECK (length(evidence_media_type) BETWEEN 1 AND 127 AND btrim(evidence_media_type) = evidence_media_type),
 evidence_captured_at timestamptz NOT NULL,
 status text NOT NULL CHECK (status IN ('CURRENT','SUPERSEDED','VOIDED')),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, movement_id, bill_no),
 FOREIGN KEY (tenant_id, movement_id) REFERENCES cargo_sea.ocean_movement (tenant_id, movement_id),
 CONSTRAINT master_bill_evidence_time CHECK (evidence_captured_at <= recorded_at)
);
-- One current reference per movement; a number is current on one movement of the tenant per issuer.
CREATE UNIQUE INDEX master_bill_one_current ON cargo_sea.master_bill (tenant_id, movement_id) WHERE status = 'CURRENT';
CREATE UNIQUE INDEX master_bill_current_number ON cargo_sea.master_bill (tenant_id, issuer_party_id, bill_number) WHERE status = 'CURRENT';

-- House bill entries: frozen per FUL plan version and relayed measures; only the end is written later, once.
-- Quantities are in the units FUL relays.
CREATE TABLE cargo_sea.house_bill (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no > 0),
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 hbl text NOT NULL CHECK (hbl ~ '^[0-9A-Z]{1,16}$'),
 ful_revision bigint NOT NULL CHECK (ful_revision > 0),
 plan_version integer NOT NULL CHECK (plan_version > 0),
 container_mode text CHECK (container_mode = 'LCL'),
 window_from timestamptz NOT NULL,
 window_until timestamptz,
 packs bigint NOT NULL CHECK (packs > 0),
 gross_weight_unit text NOT NULL CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint NOT NULL CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume > 0),
 basis text NOT NULL CHECK (basis IN ('PLANNED','RECEIVED','TENDERED')),
 evidence_version integer NOT NULL DEFAULT 0 CHECK (evidence_version >= 0),
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
 FOREIGN KEY (tenant_id, movement_id) REFERENCES cargo_sea.ocean_movement (tenant_id, movement_id),
 CONSTRAINT house_bill_volume CHECK ((volume_unit IS NULL) = (volume IS NULL)),
 CONSTRAINT house_bill_measured CHECK (basis = 'PLANNED' OR evidence_version > 0),
 CONSTRAINT house_bill_start CHECK ((start_kind = 'ASSOCIATED') = (predecessor_no IS NULL) AND (predecessor_no IS NULL OR predecessor_no < entry_no)),
 CONSTRAINT house_bill_end CHECK ((end_kind IS NULL) = (ended_at IS NULL) AND (ended_by IS NULL) = (ended_at IS NULL)
   AND (end_entered_at IS NULL) = (ended_at IS NULL) AND (ended_at IS NULL OR ended_at >= associated_at)
   AND (successor_no IS NOT NULL) = coalesce(end_kind = 'REFRESHED', false) AND (successor_no IS NULL OR successor_no > entry_no))
);
-- A shipment has at most one live house bill in the tenant; an HBL at most one live entry per movement.
CREATE UNIQUE INDEX house_bill_one_live ON cargo_sea.house_bill (tenant_id, shipment_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX house_bill_live_hbl ON cargo_sea.house_bill (tenant_id, movement_id, hbl) WHERE ended_at IS NULL;
CREATE TABLE cargo_sea.house_bill_part (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 entry_no integer NOT NULL,
 part_no integer NOT NULL CHECK (part_no > 0),
 obligation_id text NOT NULL CHECK (length(btrim(obligation_id)) BETWEEN 1 AND 128),
 obligation_version bigint NOT NULL CHECK (obligation_version > 0),
 order_id text NOT NULL CHECK (length(btrim(order_id)) BETWEEN 1 AND 128),
 packs bigint NOT NULL CHECK (packs > 0),
 package_type text NOT NULL CHECK (package_type ~ '^[A-Z0-9]{2}$'),
 gross_weight_unit text NOT NULL CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint NOT NULL CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume > 0),
 goods_description text CHECK (length(btrim(goods_description)) BETWEEN 1 AND 500),
 PRIMARY KEY (tenant_id, movement_id, entry_no, part_no),
 FOREIGN KEY (tenant_id, movement_id, entry_no) REFERENCES cargo_sea.house_bill (tenant_id, movement_id, entry_no),
 CONSTRAINT house_bill_part_volume CHECK ((volume_unit IS NULL) = (volume IS NULL))
);
-- The sealed containers of the consolidation with the house's packs in them, as PHY verified them and FUL relays
-- them (OceanOperationPlan v1): PHY's stuffing and planned unit with its ISO 6346 size-type, the house's packs,
-- gross weight and volume in it, PHY's load lines and the seal PHY recorded. The container's number is not here: it
-- comes with the container visit (part 1e).
CREATE TABLE cargo_sea.house_bill_stuffing (
 tenant_id text NOT NULL,
 movement_id text NOT NULL,
 entry_no integer NOT NULL,
 stuffing_no integer NOT NULL CHECK (stuffing_no > 0),
 execution_id text NOT NULL CHECK (length(btrim(execution_id)) BETWEEN 1 AND 128),
 load_unit_no integer NOT NULL CHECK (load_unit_no > 0),
 type_code text NOT NULL CHECK (type_code ~ '^[0-9A-Z]{2}[A-Z][0-9A-Z]$'),
 packs bigint NOT NULL CHECK (packs > 0),
 gross_weight_unit text CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume > 0),
 line_nos integer[] NOT NULL CHECK (cardinality(line_nos) >= 1 AND 0 < ALL (line_nos) AND array_position(line_nos, NULL) IS NULL),
 seal text NOT NULL CHECK (length(btrim(seal)) BETWEEN 1 AND 500),
 seal_sha256 text CHECK (seal_sha256 ~ '^[0-9a-f]{64}$'),
 sealed_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, movement_id, entry_no, stuffing_no),
 UNIQUE (tenant_id, movement_id, entry_no, execution_id),
 FOREIGN KEY (tenant_id, movement_id, entry_no) REFERENCES cargo_sea.house_bill (tenant_id, movement_id, entry_no),
 CONSTRAINT house_bill_stuffing_units CHECK ((gross_weight_unit IS NULL) = (gross_weight IS NULL) AND (volume_unit IS NULL) = (volume IS NULL))
);

-- A legal entity's HBL series (a numbering template and its first number, fixed once) and the register of every HBL
-- number it gave or was given: a number belongs to one shipment for ever and a shipment keeps one number. HBL
-- templates do not reset yet (SEA does not know the issuing office's time zone).
CREATE TABLE cargo_sea.hbl_series (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 template text NOT NULL,
 first_number bigint NOT NULL,
 seed_period text NOT NULL DEFAULT '',
 registered_by text NOT NULL CHECK (length(btrim(registered_by)) > 0),
 registered_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 PRIMARY KEY (tenant_id, legal_entity_id),
 CONSTRAINT hbl_series_template CHECK (template ~ '^[0-9A-Z]+\{seq:([4-9]|10)\}[0-9A-Z]*$' AND length(template) <= 40 AND first_number >= 1
   AND seed_period = '')
);
CREATE TABLE cargo_sea.hbl_number (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 hbl text NOT NULL CHECK (hbl ~ '^[0-9A-Z]{1,16}$'),
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 drawn boolean NOT NULL,
 claimed_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, hbl),
 CONSTRAINT hbl_number_one_per_shipment UNIQUE (tenant_id, legal_entity_id, shipment_id)
);

CREATE TABLE cargo_sea.command_receipt (
 tenant_id text NOT NULL,
 command_id text NOT NULL,
 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 result_type text NOT NULL CHECK (result_type IN ('OceanMovement','HblSeries')),
 result_id text NOT NULL,
 result_revision bigint NOT NULL CHECK (result_revision > 0),
 result_entry_no integer CHECK (result_entry_no > 0),
 result_hbl text,
 result_bill_no integer CHECK (result_bill_no > 0),
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, command_id)
);

-- Only the state, cancellation, revision and updated_at of a movement change, and a cancelled one never changes
-- again; nothing is deleted.
CREATE FUNCTION cargo_sea.reject_movement_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','cancel_reason','cancelled_by','cancelled_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.state = 'CANCELLED' OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_sea: movement %/% changes only its state, and never once cancelled', OLD.tenant_id, OLD.movement_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER ocean_movement_rules BEFORE UPDATE OR DELETE ON cargo_sea.ocean_movement FOR EACH ROW EXECUTE FUNCTION cargo_sea.reject_movement_change();

CREATE FUNCTION cargo_sea.reject_row_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_sea: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['booking_link','voyage_leg','voyage_cut_off','house_bill_part','house_bill_stuffing','hbl_series','hbl_number'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_sea.%I FOR EACH ROW EXECUTE FUNCTION cargo_sea.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- A master bill reference only leaves CURRENT, once.
CREATE FUNCTION cargo_sea.check_master_bill_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.status <> 'CURRENT' OR NEW.status = 'CURRENT' OR to_jsonb(OLD) - 'status' IS DISTINCT FROM to_jsonb(NEW) - 'status' THEN
    RAISE EXCEPTION 'cargo_sea: master bill %/%/% only leaves CURRENT, once', OLD.tenant_id, OLD.movement_id, OLD.bill_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER master_bill_rules BEFORE UPDATE OR DELETE ON cargo_sea.master_bill FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_master_bill_change();

-- An entry is only ended, once.
CREATE FUNCTION cargo_sea.check_house_bill_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  ends text[] := ARRAY['end_kind','ended_by','ended_at','end_entered_at','successor_no'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.ended_at IS NOT NULL OR NEW.ended_at IS NULL OR to_jsonb(OLD) - ends IS DISTINCT FROM to_jsonb(NEW) - ends THEN
    RAISE EXCEPTION 'cargo_sea: house bill %/%/% can only be ended, once', OLD.tenant_id, OLD.movement_id, OLD.entry_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER house_bill_rules BEFORE UPDATE OR DELETE ON cargo_sea.house_bill FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_house_bill_change();

-- At commit, for every house bill a transaction writes: a live entry belongs to an open movement and its HBL is the
-- number registered to its shipment under the movement's legal entity.
CREATE FUNCTION cargo_sea.check_house_bill_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  e cargo_sea.house_bill;
  m cargo_sea.ocean_movement;
BEGIN
  SELECT * INTO e FROM cargo_sea.house_bill WHERE tenant_id = NEW.tenant_id AND movement_id = NEW.movement_id AND entry_no = NEW.entry_no;
  IF e.ended_at IS NOT NULL THEN
    RETURN NULL;
  END IF;
  SELECT * INTO m FROM cargo_sea.ocean_movement WHERE tenant_id = e.tenant_id AND movement_id = e.movement_id;
  IF m.state <> 'OPEN' OR NOT EXISTS (SELECT 1 FROM cargo_sea.hbl_number n WHERE n.tenant_id = e.tenant_id
       AND n.legal_entity_id = m.operating_legal_entity_id AND n.hbl = e.hbl AND n.shipment_id = e.shipment_id) THEN
    RAISE EXCEPTION 'cargo_sea: house bill %/%/% needs an open movement and its shipment''s registered HBL', e.tenant_id, e.movement_id, e.entry_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'house_bill_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER house_bill_consistent AFTER INSERT ON cargo_sea.house_bill
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_house_bill_at_commit();

-- A movement as it stands at commit: a cancelled movement keeps no live house bill; its booking is its first link's;
-- its master bill references are its carrier's or co-loader's, of a kind of the movement's kind, numbered from one
-- without gaps, the latest CURRENT (VOIDED once the movement is cancelled) and every earlier one SUPERSEDED.
CREATE FUNCTION cargo_sea.movement_consistent(t text, mv text) RETURNS boolean LANGUAGE sql STABLE AS $fn$
  SELECT NOT EXISTS (SELECT 1 FROM cargo_sea.ocean_movement m WHERE m.tenant_id = t AND m.movement_id = mv AND (
      m.state = 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_sea.house_bill e
        WHERE e.tenant_id = m.tenant_id AND e.movement_id = m.movement_id AND e.ended_at IS NULL)
      OR NOT EXISTS (SELECT 1 FROM cargo_sea.booking_link l WHERE l.tenant_id = m.tenant_id AND l.movement_id = m.movement_id
        AND l.link_no = 1 AND l.booking_id = m.booking_id)
      OR EXISTS (SELECT 1 FROM cargo_sea.master_bill b,
          LATERAL (SELECT count(*) AS bills FROM cargo_sea.master_bill x WHERE x.tenant_id = m.tenant_id AND x.movement_id = m.movement_id) n
        WHERE b.tenant_id = m.tenant_id AND b.movement_id = m.movement_id AND (
          b.issuer_party_id <> m.carrier_party_id
          OR (b.kind = 'COLOAD_BILL') <> (m.kind = 'SEA_LCL_COLOAD')
          OR b.bill_no > n.bills
          OR b.status <> CASE WHEN b.bill_no < n.bills THEN 'SUPERSEDED' WHEN m.state = 'CANCELLED' THEN 'VOIDED' ELSE 'CURRENT' END))))
$fn$;
CREATE FUNCTION cargo_sea.check_movement_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT cargo_sea.movement_consistent(NEW.tenant_id, NEW.movement_id) THEN
    RAISE EXCEPTION 'cargo_sea: movement %/% is inconsistent at commit', NEW.tenant_id, NEW.movement_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ocean_movement_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER ocean_movement_consistent AFTER INSERT OR UPDATE ON cargo_sea.ocean_movement
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_movement_at_commit();
CREATE CONSTRAINT TRIGGER master_bill_consistent AFTER INSERT OR UPDATE ON cargo_sea.master_bill
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_movement_at_commit();

-- At commit, a house's parts and containers are written by the transaction that writes the entry, and a link's legs
-- and cut-offs by the one that writes the link: an entry's facts and a link's voyage never grow later.
CREATE FUNCTION cargo_sea.check_entry_facts_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_sea.house_bill e WHERE e.tenant_id = NEW.tenant_id AND e.movement_id = NEW.movement_id
       AND e.entry_no = NEW.entry_no AND e.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_sea: % of house bill %/%/% are written with the entry', TG_TABLE_NAME, NEW.tenant_id, NEW.movement_id, NEW.entry_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'house_bill_facts_with_entry';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER house_bill_part_with_entry AFTER INSERT ON cargo_sea.house_bill_part
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_entry_facts_at_commit();
CREATE CONSTRAINT TRIGGER house_bill_stuffing_with_entry AFTER INSERT ON cargo_sea.house_bill_stuffing
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_entry_facts_at_commit();
CREATE FUNCTION cargo_sea.check_link_voyage_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_sea.booking_link l WHERE l.tenant_id = NEW.tenant_id AND l.movement_id = NEW.movement_id
       AND l.link_no = NEW.link_no AND l.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_sea: % of booking link %/%/% are written with the link', TG_TABLE_NAME, NEW.tenant_id, NEW.movement_id, NEW.link_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'voyage_with_link';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER voyage_leg_with_link AFTER INSERT ON cargo_sea.voyage_leg
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_link_voyage_at_commit();
CREATE CONSTRAINT TRIGGER voyage_cut_off_with_link AFTER INSERT ON cargo_sea.voyage_cut_off
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_sea.check_link_voyage_at_commit();

-- The platform's sequence table in SEA's schema: Cargo owns its data and permissions. Rows are scoped by tenant; a
-- sequence starts at 1 or at its series' seed, only for a registered series, moves one number at a time and is never
-- deleted.
ALTER TABLE cargo_sea.numbering_sequence ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_sea.numbering_sequence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_sea.numbering_sequence USING (scope = current_setting('app.tenant_id', true))
  WITH CHECK (scope = current_setting('app.tenant_id', true));
CREATE FUNCTION cargo_sea.numbering_forward_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_sea.hbl_series;
BEGIN
  IF TG_OP = 'INSERT' THEN
    SELECT * INTO s FROM cargo_sea.hbl_series x WHERE x.tenant_id = NEW.scope AND starts_with(NEW.doc_type, 'HBL/')
      AND x.legal_entity_id = substr(NEW.doc_type, 5);
    IF s.tenant_id IS NULL OR NOT (NEW.next_val = 1 OR (NEW.period = s.seed_period AND NEW.next_val = s.first_number - 1)) THEN
      RAISE EXCEPTION 'cargo_sea: a numbering sequence starts at 1 or at its series'' seed' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' OR NEW.scope <> OLD.scope OR NEW.doc_type <> OLD.doc_type OR NEW.period <> OLD.period OR NEW.next_val <> OLD.next_val + 1 THEN
    RAISE EXCEPTION 'cargo_sea: a numbering sequence moves one number at a time' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER numbering_sequence_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_sea.numbering_sequence
  FOR EACH ROW EXECUTE FUNCTION cargo_sea.numbering_forward_only();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['ocean_movement','booking_link','voyage_leg','voyage_cut_off','master_bill','house_bill','house_bill_part',
      'house_bill_stuffing','hbl_series','hbl_number','command_receipt'] LOOP
    EXECUTE format('ALTER TABLE cargo_sea.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_sea.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_sea.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
