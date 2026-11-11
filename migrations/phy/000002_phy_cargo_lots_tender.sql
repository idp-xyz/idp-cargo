-- IDP-Cargo PHY V0.2 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Physical facts of a consolidation's houses (backlog PB3; decisions N1-PHY OD-3, OD-4 C, OD-5; C4): cargo lots
-- with receipt lines, measurement versions, screening results and discrepancies; tender load executions with
-- load lines, reversals, holds, carrier evidence and loading discrepancies; reconciliation of a consolidation.
-- Facts are insert-only; corrections are reversal lines and new versions. Conservation is checked at commit
-- under the cargo lot's and the membership ledger's locks: a lot's live loaded packs never exceed its received
-- packs, a house's live loaded packs never exceed its live entry's planned packs, and a house leaves a
-- consolidation only once nothing of it is loaded there.

-- A consolidation is reconciled once its physical facts are complete.
ALTER TABLE cargo_phy.consolidation_version
  DROP CONSTRAINT consolidation_version_change_check,
  ADD CONSTRAINT consolidation_version_change_check CHECK (change IN ('CREATED','AMENDED','CLOSED','REOPENED','CANCELLED','RECONCILED')),
  DROP CONSTRAINT consolidation_version_state,
  ADD CONSTRAINT consolidation_version_state CHECK (CASE change
    WHEN 'CREATED' THEN state = 'OPEN' WHEN 'REOPENED' THEN state = 'OPEN' WHEN 'CLOSED' THEN state = 'CLOSED'
    WHEN 'CANCELLED' THEN state = 'CANCELLED' WHEN 'RECONCILED' THEN state = 'RECONCILED' ELSE state IN ('OPEN','CLOSED') END);

-- Receipts of cargo lot and load execution commands name the lot or execution and the item they wrote.
ALTER TABLE cargo_phy.command_receipt
  DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check CHECK (result_type IN ('Consolidation','ConsolMembership','CargoLot','LoadExecution')),
  DROP CONSTRAINT command_receipt_entry,
  ADD CONSTRAINT command_receipt_entry CHECK (CASE result_type WHEN 'ConsolMembership' THEN result_entry_no IS NOT NULL
    WHEN 'Consolidation' THEN result_entry_no IS NULL ELSE true END);

-- A cargo lot is a house's cargo at a facility (the forwarder's warehouse, a CFS, the airline terminal): one lot
-- in force per shipment and facility. It keeps the plan it was compared with (LoadIntent v1 at receipt, again at
-- completion); FUL's plan is never changed from here (C4).
CREATE TABLE cargo_phy.cargo_lot (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 lot_id text NOT NULL CHECK (length(btrim(lot_id)) BETWEEN 1 AND 128),
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 facility_location text NOT NULL CHECK (facility_location ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 facility_code text NOT NULL CHECK (facility_code ~ '^[0-9A-Z][0-9A-Z./-]{0,34}$'),
 transport_mode text NOT NULL CHECK (transport_mode IN ('AIR','SEA')),
 state text NOT NULL CHECK (state IN ('RECEIVING','RECEIVED','CLOSED')),
 expected_ful_revision bigint NOT NULL CHECK (expected_ful_revision > 0),
 expected_plan_version integer NOT NULL CHECK (expected_plan_version > 0),
 expected_packs bigint NOT NULL CHECK (expected_packs > 0),
 expected_weight_unit text NOT NULL CHECK (expected_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 expected_gross_weight bigint NOT NULL CHECK (expected_gross_weight > 0),
 expected_volume_unit text CHECK (expected_volume_unit ~ '^[A-Z0-9]{1,3}$'),
 expected_volume bigint CHECK (expected_volume > 0),
 completed_by text CHECK (length(btrim(completed_by)) > 0),
 completed_at timestamptz,
 completed_entered_at timestamptz,
 closed_by text CHECK (length(btrim(closed_by)) > 0),
 closed_at timestamptz,
 closed_entered_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, lot_id),
 CONSTRAINT cargo_lot_volume CHECK ((expected_volume_unit IS NULL) = (expected_volume IS NULL)),
 CONSTRAINT cargo_lot_completed CHECK ((state = 'RECEIVING') = (completed_at IS NULL) AND (completed_by IS NULL) = (completed_at IS NULL)
   AND (completed_entered_at IS NULL) = (completed_at IS NULL)),
 CONSTRAINT cargo_lot_closed CHECK ((state = 'CLOSED') = (closed_at IS NOT NULL) AND (closed_by IS NULL) = (closed_at IS NULL)
   AND (closed_entered_at IS NULL) = (closed_at IS NULL) AND (closed_at IS NULL OR closed_at >= completed_at))
);
CREATE UNIQUE INDEX cargo_lot_one_live ON cargo_phy.cargo_lot (tenant_id, shipment_id, facility_location, facility_code) WHERE state <> 'CLOSED';
CREATE INDEX cargo_lot_by_shipment ON cargo_phy.cargo_lot (tenant_id, shipment_id);

CREATE TABLE cargo_phy.receipt_line (
 tenant_id text NOT NULL,
 lot_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 packs bigint NOT NULL CHECK (packs > 0),
 package_type text NOT NULL CHECK (package_type ~ '^[A-Z0-9]{2}$'),
 condition text NOT NULL CHECK (condition IN ('GOOD','DAMAGED','WET','OPENED')),
 evidence_source text NOT NULL CHECK (length(btrim(evidence_source)) BETWEEN 1 AND 64),
 evidence_reference text NOT NULL CHECK (length(btrim(evidence_reference)) BETWEEN 1 AND 128),
 evidence_sha256 char(64) CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_occurred_at timestamptz NOT NULL,
 received_by text NOT NULL CHECK (length(btrim(received_by)) > 0),
 received_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, lot_id, line_no),
 FOREIGN KEY (tenant_id, lot_id) REFERENCES cargo_phy.cargo_lot (tenant_id, lot_id),
 CONSTRAINT receipt_line_evidence_time CHECK (evidence_occurred_at <= received_at)
);

-- A later measurement adds a version, never overwrites (C4).
CREATE TABLE cargo_phy.measurement_version (
 tenant_id text NOT NULL,
 lot_id text NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 gross_weight_unit text NOT NULL CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint NOT NULL CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume > 0),
 method text NOT NULL CHECK (method IN ('SCALE','DIMENSIONER','CFS_TALLY','TERMINAL_WEIGHT')),
 evidence_source text NOT NULL CHECK (length(btrim(evidence_source)) BETWEEN 1 AND 64),
 evidence_reference text NOT NULL CHECK (length(btrim(evidence_reference)) BETWEEN 1 AND 128),
 evidence_sha256 char(64) CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_occurred_at timestamptz NOT NULL,
 measured_by text NOT NULL CHECK (length(btrim(measured_by)) > 0),
 measured_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, lot_id, version_no),
 FOREIGN KEY (tenant_id, lot_id) REFERENCES cargo_phy.cargo_lot (tenant_id, lot_id),
 CONSTRAINT measurement_version_volume CHECK ((volume_unit IS NULL) = (volume IS NULL)),
 CONSTRAINT measurement_version_evidence_time CHECK (evidence_occurred_at <= measured_at)
);

CREATE TABLE cargo_phy.measurement_package_group (
 tenant_id text NOT NULL,
 lot_id text NOT NULL,
 version_no integer NOT NULL,
 group_no integer NOT NULL CHECK (group_no > 0),
 package_count bigint NOT NULL CHECK (package_count > 0),
 package_type text NOT NULL CHECK (package_type ~ '^[A-Z0-9]{2}$'),
 length_mm bigint NOT NULL CHECK (length_mm > 0),
 width_mm bigint NOT NULL CHECK (width_mm > 0),
 height_mm bigint NOT NULL CHECK (height_mm > 0),
 PRIMARY KEY (tenant_id, lot_id, version_no, group_no),
 FOREIGN KEY (tenant_id, lot_id, version_no) REFERENCES cargo_phy.measurement_version (tenant_id, lot_id, version_no)
);

-- A screening result is external evidence of the airport security station or another screening party (CN-7).
CREATE TABLE cargo_phy.screening_result (
 tenant_id text NOT NULL,
 lot_id text NOT NULL,
 result_no integer NOT NULL CHECK (result_no > 0),
 outcome text NOT NULL CHECK (outcome IN ('PASSED','FAILED','PENDING','NOT_REQUIRED')),
 screener text NOT NULL CHECK (length(btrim(screener)) BETWEEN 1 AND 500),
 method text NOT NULL CHECK (method ~ '^[0-9A-Z][0-9A-Z_-]{0,34}$'),
 screening_reference text NOT NULL CHECK (length(btrim(screening_reference)) BETWEEN 1 AND 500),
 evidence_source text NOT NULL CHECK (length(btrim(evidence_source)) BETWEEN 1 AND 64),
 evidence_reference text NOT NULL CHECK (length(btrim(evidence_reference)) BETWEEN 1 AND 128),
 evidence_sha256 char(64) CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_occurred_at timestamptz NOT NULL,
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, lot_id, result_no),
 FOREIGN KEY (tenant_id, lot_id) REFERENCES cargo_phy.cargo_lot (tenant_id, lot_id),
 CONSTRAINT screening_result_evidence_time CHECK (evidence_occurred_at <= recorded_at)
);

-- A difference against the plan found at receipt; only its resolution is written later, once.
CREATE TABLE cargo_phy.cargo_discrepancy (
 tenant_id text NOT NULL,
 lot_id text NOT NULL,
 discrepancy_no integer NOT NULL CHECK (discrepancy_no > 0),
 kind text NOT NULL CHECK (kind IN ('SHORT','OVER','DAMAGED','WEIGHT_VARIANCE','VOLUME_VARIANCE')),
 expected_amount bigint NOT NULL CHECK (expected_amount >= 0),
 observed_amount bigint NOT NULL CHECK (observed_amount >= 0),
 unit text NOT NULL CHECK (unit ~ '^[A-Z0-9]{1,3}$'),
 raised_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 resolution text CHECK (resolution IN ('FUL_REPLANNED','ACCEPTED','CLAIM_OPENED')),
 resolution_reference text CHECK (length(btrim(resolution_reference)) BETWEEN 1 AND 500),
 resolution_text text CHECK (length(btrim(resolution_text)) BETWEEN 1 AND 500),
 resolved_by text CHECK (length(btrim(resolved_by)) > 0),
 resolved_at timestamptz,
 resolution_entered_at timestamptz,
 PRIMARY KEY (tenant_id, lot_id, discrepancy_no),
 FOREIGN KEY (tenant_id, lot_id) REFERENCES cargo_phy.cargo_lot (tenant_id, lot_id),
 CONSTRAINT cargo_discrepancy_resolution CHECK ((resolution IS NULL) = (resolved_at IS NULL) AND (resolved_by IS NULL) = (resolved_at IS NULL)
   AND (resolution_entered_at IS NULL) = (resolved_at IS NULL) AND (resolved_at IS NULL OR resolved_at >= raised_at)
   AND (resolution IS DISTINCT FROM 'ACCEPTED' OR resolution_text IS NOT NULL)
   AND (resolution IS DISTINCT FROM 'CLAIM_OPENED' OR resolution_reference IS NOT NULL))
);
CREATE INDEX cargo_discrepancy_open ON cargo_phy.cargo_discrepancy (tenant_id) WHERE resolved_at IS NULL;

-- A load execution tenders a consolidation's houses: loose AIR cargo to the airline terminal, or a co-load's
-- houses to the co-loader's CFS. Stuffing and build-up come with load units.
CREATE TABLE cargo_phy.load_execution (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 execution_id text NOT NULL CHECK (length(btrim(execution_id)) BETWEEN 1 AND 128),
 consolidation_id text NOT NULL,
 kind text NOT NULL CHECK (kind = 'TENDER'),
 transport_mode text NOT NULL CHECK (transport_mode IN ('AIR','SEA')),
 facility_location text NOT NULL CHECK (facility_location ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 facility_code text NOT NULL CHECK (facility_code ~ '^[0-9A-Z][0-9A-Z./-]{0,34}$'),
 state text NOT NULL CHECK (state IN ('OPEN','LOADING','ON_HOLD','CLOSED')),
 opened_by text NOT NULL CHECK (length(btrim(opened_by)) > 0),
 opened_at timestamptz NOT NULL,
 opened_entered_at timestamptz NOT NULL,
 started_by text CHECK (length(btrim(started_by)) > 0),
 started_at timestamptz,
 started_entered_at timestamptz,
 closed_by text CHECK (length(btrim(closed_by)) > 0),
 closed_at timestamptz,
 closed_entered_at timestamptz,
 close_evidence_source text CHECK (length(btrim(close_evidence_source)) BETWEEN 1 AND 64),
 close_evidence_reference text CHECK (length(btrim(close_evidence_reference)) BETWEEN 1 AND 128),
 close_evidence_sha256 char(64) CHECK (close_evidence_sha256 ~ '^[0-9a-f]{64}$'),
 close_evidence_occurred_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, execution_id),
 FOREIGN KEY (tenant_id, consolidation_id) REFERENCES cargo_phy.consolidation (tenant_id, consolidation_id),
 CONSTRAINT load_execution_started CHECK ((state = 'OPEN') = (started_at IS NULL) AND (started_by IS NULL) = (started_at IS NULL)
   AND (started_entered_at IS NULL) = (started_at IS NULL) AND (started_at IS NULL OR started_at >= opened_at)),
 CONSTRAINT load_execution_closed CHECK ((state = 'CLOSED') = (closed_at IS NOT NULL) AND (closed_by IS NULL) = (closed_at IS NULL)
   AND (closed_entered_at IS NULL) = (closed_at IS NULL) AND (close_evidence_source IS NULL) = (closed_at IS NULL)
   AND (close_evidence_reference IS NULL) = (closed_at IS NULL) AND (close_evidence_occurred_at IS NULL) = (closed_at IS NULL)
   AND (close_evidence_sha256 IS NULL OR closed_at IS NOT NULL) AND (closed_at IS NULL OR closed_at >= started_at))
);
CREATE INDEX load_execution_by_consolidation ON cargo_phy.load_execution (tenant_id, consolidation_id);

-- A load line is cargo of a lot loaded or tendered under the house's live membership entry; its weight and
-- volume are measured for it or apportioned from the lot's measurement version.
CREATE TABLE cargo_phy.load_line (
 tenant_id text NOT NULL,
 execution_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 consolidation_id text NOT NULL,
 shipment_id text NOT NULL,
 lot_id text NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no > 0),
 packs bigint NOT NULL CHECK (packs > 0),
 gross_weight_unit text NOT NULL CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint NOT NULL CHECK (gross_weight >= 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume >= 0),
 basis text NOT NULL CHECK (basis IN ('MEASURED','APPORTIONED')),
 measurement_no integer CHECK (measurement_no > 0),
 evidence_source text NOT NULL CHECK (length(btrim(evidence_source)) BETWEEN 1 AND 64),
 evidence_reference text NOT NULL CHECK (length(btrim(evidence_reference)) BETWEEN 1 AND 128),
 evidence_sha256 char(64) CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_occurred_at timestamptz NOT NULL,
 loaded_by text NOT NULL CHECK (length(btrim(loaded_by)) > 0),
 loaded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, execution_id, line_no),
 FOREIGN KEY (tenant_id, execution_id) REFERENCES cargo_phy.load_execution (tenant_id, execution_id),
 FOREIGN KEY (tenant_id, lot_id) REFERENCES cargo_phy.cargo_lot (tenant_id, lot_id),
 CONSTRAINT load_line_volume CHECK ((volume_unit IS NULL) = (volume IS NULL)),
 CONSTRAINT load_line_basis CHECK ((basis = 'APPORTIONED') = (measurement_no IS NOT NULL)),
 CONSTRAINT load_line_measured CHECK (basis = 'APPORTIONED' OR gross_weight > 0),
 CONSTRAINT load_line_evidence_time CHECK (evidence_occurred_at <= loaded_at)
);
CREATE INDEX load_line_by_lot ON cargo_phy.load_line (tenant_id, lot_id);
CREATE INDEX load_line_by_house ON cargo_phy.load_line (tenant_id, consolidation_id, shipment_id);

-- A reversal takes packs of a line out again; the line stays (DDD-014).
CREATE TABLE cargo_phy.reversal_line (
 tenant_id text NOT NULL,
 execution_id text NOT NULL,
 reversal_no integer NOT NULL CHECK (reversal_no > 0),
 line_no integer NOT NULL,
 packs bigint NOT NULL CHECK (packs > 0),
 gross_weight bigint NOT NULL CHECK (gross_weight >= 0),
 volume bigint CHECK (volume >= 0),
 reason text NOT NULL CHECK (reason IN ('UNSTUFFED','OFFLOADED_BY_FORWARDER','MISLOADED')),
 evidence_source text NOT NULL CHECK (length(btrim(evidence_source)) BETWEEN 1 AND 64),
 evidence_reference text NOT NULL CHECK (length(btrim(evidence_reference)) BETWEEN 1 AND 128),
 evidence_sha256 char(64) CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_occurred_at timestamptz NOT NULL,
 reversed_by text NOT NULL CHECK (length(btrim(reversed_by)) > 0),
 reversed_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, execution_id, reversal_no),
 FOREIGN KEY (tenant_id, execution_id, line_no) REFERENCES cargo_phy.load_line (tenant_id, execution_id, line_no),
 CONSTRAINT reversal_line_evidence_time CHECK (evidence_occurred_at <= reversed_at)
);
CREATE INDEX reversal_line_by_line ON cargo_phy.reversal_line (tenant_id, execution_id, line_no);

-- A hold stops loading; only its release is written later, once.
CREATE TABLE cargo_phy.hold_entry (
 tenant_id text NOT NULL,
 execution_id text NOT NULL,
 hold_no integer NOT NULL CHECK (hold_no > 0),
 reason text NOT NULL CHECK (reason IN ('SECURITY_SCREENING_FAILED','DG_QUERY','CUSTOMS_HOLD','OVERWEIGHT','DAMAGE')),
 lot_id text,
 hold_text text CHECK (length(btrim(hold_text)) BETWEEN 1 AND 500),
 evidence_source text NOT NULL CHECK (length(btrim(evidence_source)) BETWEEN 1 AND 64),
 evidence_reference text NOT NULL CHECK (length(btrim(evidence_reference)) BETWEEN 1 AND 128),
 evidence_sha256 char(64) CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_occurred_at timestamptz NOT NULL,
 placed_by text NOT NULL CHECK (length(btrim(placed_by)) > 0),
 placed_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 release_evidence_source text CHECK (length(btrim(release_evidence_source)) BETWEEN 1 AND 64),
 release_evidence_reference text CHECK (length(btrim(release_evidence_reference)) BETWEEN 1 AND 128),
 release_evidence_sha256 char(64) CHECK (release_evidence_sha256 ~ '^[0-9a-f]{64}$'),
 release_evidence_occurred_at timestamptz,
 released_by text CHECK (length(btrim(released_by)) > 0),
 released_at timestamptz,
 release_entered_at timestamptz,
 PRIMARY KEY (tenant_id, execution_id, hold_no),
 FOREIGN KEY (tenant_id, execution_id) REFERENCES cargo_phy.load_execution (tenant_id, execution_id),
 FOREIGN KEY (tenant_id, lot_id) REFERENCES cargo_phy.cargo_lot (tenant_id, lot_id),
 CONSTRAINT hold_entry_lot CHECK ((reason = 'SECURITY_SCREENING_FAILED') = (lot_id IS NOT NULL)),
 CONSTRAINT hold_entry_release CHECK ((released_at IS NULL) = (released_by IS NULL) AND (release_entered_at IS NULL) = (released_at IS NULL)
   AND (release_evidence_source IS NULL) = (released_at IS NULL) AND (release_evidence_reference IS NULL) = (released_at IS NULL)
   AND (release_evidence_occurred_at IS NULL) = (released_at IS NULL) AND (release_evidence_sha256 IS NULL OR released_at IS NOT NULL)
   AND (released_at IS NULL OR released_at >= placed_at))
);

-- Carrier evidence of the tendered pieces, entered by hand in N1 (OD-4 C): of the master or of one house, on a
-- flight or voyage. A source reference is recorded once per tenant.
CREATE TABLE cargo_phy.carrier_load_evidence (
 tenant_id text NOT NULL,
 execution_id text NOT NULL,
 evidence_no integer NOT NULL CHECK (evidence_no > 0),
 fact text NOT NULL CHECK (fact IN ('ACCEPTED','MANIFESTED','DEPARTED','OFFLOADED','ARRIVED','LOADED_ON_BOARD')),
 shipment_id text,
 pieces bigint NOT NULL CHECK (pieces > 0),
 gross_weight_unit text CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint CHECK (gross_weight > 0),
 transport_ref text NOT NULL CHECK (transport_ref ~ '^[0-9A-Z][0-9A-Z /._-]{0,34}$'),
 evidence_source text NOT NULL CHECK (length(btrim(evidence_source)) BETWEEN 1 AND 64),
 evidence_reference text NOT NULL CHECK (length(btrim(evidence_reference)) BETWEEN 1 AND 128),
 evidence_sha256 char(64) CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
 evidence_occurred_at timestamptz NOT NULL,
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, execution_id, evidence_no),
 FOREIGN KEY (tenant_id, execution_id) REFERENCES cargo_phy.load_execution (tenant_id, execution_id),
 CONSTRAINT carrier_load_evidence_weight CHECK ((gross_weight_unit IS NULL) = (gross_weight IS NULL)),
 CONSTRAINT carrier_load_evidence_time CHECK (evidence_occurred_at <= recorded_at)
);
CREATE UNIQUE INDEX carrier_load_evidence_source_reference ON cargo_phy.carrier_load_evidence (tenant_id, evidence_source, evidence_reference);

-- A difference the carrier's evidence shows against the tender; only its resolution is written later, once.
CREATE TABLE cargo_phy.loading_discrepancy (
 tenant_id text NOT NULL,
 execution_id text NOT NULL,
 discrepancy_no integer NOT NULL CHECK (discrepancy_no > 0),
 kind text NOT NULL CHECK (kind IN ('LOAD_CONFLICT','OFFLOADED')),
 evidence_no integer NOT NULL,
 shipment_id text,
 expected_pieces bigint NOT NULL CHECK (expected_pieces >= 0),
 observed_pieces bigint NOT NULL CHECK (observed_pieces >= 0),
 raised_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 resolution text CHECK (resolution IN ('ACCEPTED','CLAIM_OPENED')),
 resolution_reference text CHECK (length(btrim(resolution_reference)) BETWEEN 1 AND 500),
 resolution_text text CHECK (length(btrim(resolution_text)) BETWEEN 1 AND 500),
 resolved_by text CHECK (length(btrim(resolved_by)) > 0),
 resolved_at timestamptz,
 resolution_entered_at timestamptz,
 PRIMARY KEY (tenant_id, execution_id, discrepancy_no),
 FOREIGN KEY (tenant_id, execution_id, evidence_no) REFERENCES cargo_phy.carrier_load_evidence (tenant_id, execution_id, evidence_no),
 CONSTRAINT loading_discrepancy_resolution CHECK ((resolution IS NULL) = (resolved_at IS NULL) AND (resolved_by IS NULL) = (resolved_at IS NULL)
   AND (resolution_entered_at IS NULL) = (resolved_at IS NULL) AND (resolved_at IS NULL OR resolved_at >= raised_at)
   AND (resolution IS DISTINCT FROM 'ACCEPTED' OR resolution_text IS NOT NULL)
   AND (resolution IS DISTINCT FROM 'CLAIM_OPENED' OR resolution_reference IS NOT NULL))
);
CREATE INDEX loading_discrepancy_open ON cargo_phy.loading_discrepancy (tenant_id) WHERE resolved_at IS NULL;

-- Live loaded packs: load lines less their reversals, of a lot or of a house in a consolidation.
CREATE FUNCTION cargo_phy.live_packs_of_lot(p_tenant text, p_lot text) RETURNS bigint LANGUAGE sql STABLE AS $fn$
  SELECT coalesce(sum(l.packs), 0) - coalesce((SELECT sum(r.packs) FROM cargo_phy.reversal_line r JOIN cargo_phy.load_line x
      ON x.tenant_id = r.tenant_id AND x.execution_id = r.execution_id AND x.line_no = r.line_no
    WHERE x.tenant_id = p_tenant AND x.lot_id = p_lot), 0)
  FROM cargo_phy.load_line l WHERE l.tenant_id = p_tenant AND l.lot_id = p_lot
$fn$;
CREATE FUNCTION cargo_phy.live_packs_of_house(p_tenant text, p_consolidation text, p_shipment text) RETURNS bigint LANGUAGE sql STABLE AS $fn$
  SELECT coalesce(sum(l.packs), 0) - coalesce((SELECT sum(r.packs) FROM cargo_phy.reversal_line r JOIN cargo_phy.load_line x
      ON x.tenant_id = r.tenant_id AND x.execution_id = r.execution_id AND x.line_no = r.line_no
    WHERE x.tenant_id = p_tenant AND x.consolidation_id = p_consolidation AND x.shipment_id = p_shipment), 0)
  FROM cargo_phy.load_line l WHERE l.tenant_id = p_tenant AND l.consolidation_id = p_consolidation AND l.shipment_id = p_shipment
$fn$;

-- Facts are kept: receipt lines, measurements, screening results, load lines, reversals and carrier evidence
-- never change.
CREATE TRIGGER receipt_line_immutable BEFORE UPDATE OR DELETE ON cargo_phy.receipt_line
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();
CREATE TRIGGER measurement_version_immutable BEFORE UPDATE OR DELETE ON cargo_phy.measurement_version
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();
CREATE TRIGGER measurement_package_group_immutable BEFORE UPDATE OR DELETE ON cargo_phy.measurement_package_group
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();
CREATE TRIGGER screening_result_immutable BEFORE UPDATE OR DELETE ON cargo_phy.screening_result
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();
CREATE TRIGGER load_line_immutable BEFORE UPDATE OR DELETE ON cargo_phy.load_line
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();
CREATE TRIGGER reversal_line_immutable BEFORE UPDATE OR DELETE ON cargo_phy.reversal_line
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();
CREATE TRIGGER carrier_load_evidence_immutable BEFORE UPDATE OR DELETE ON cargo_phy.carrier_load_evidence
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.reject_row_change();

-- A discrepancy or hold is only resolved or released, once.
CREATE FUNCTION cargo_phy.check_resolution() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  done text[] := CASE TG_TABLE_NAME WHEN 'hold_entry' THEN ARRAY['release_evidence_source','release_evidence_reference',
      'release_evidence_sha256','release_evidence_occurred_at','released_by','released_at','release_entered_at']
    ELSE ARRAY['resolution','resolution_reference','resolution_text','resolved_by','resolved_at','resolution_entered_at'] END;
  before timestamptz := CASE TG_TABLE_NAME WHEN 'hold_entry' THEN (to_jsonb(OLD)->>'released_at')::timestamptz
    ELSE (to_jsonb(OLD)->>'resolved_at')::timestamptz END;
BEGIN
  IF TG_OP = 'DELETE' OR before IS NOT NULL OR to_jsonb(OLD) - done IS DISTINCT FROM to_jsonb(NEW) - done THEN
    RAISE EXCEPTION 'cargo_phy: % rows are only resolved or released, once', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER cargo_discrepancy_rules BEFORE UPDATE OR DELETE ON cargo_phy.cargo_discrepancy
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_resolution();
CREATE TRIGGER loading_discrepancy_rules BEFORE UPDATE OR DELETE ON cargo_phy.loading_discrepancy
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_resolution();
CREATE TRIGGER hold_entry_rules BEFORE UPDATE OR DELETE ON cargo_phy.hold_entry
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_resolution();

-- A lot keeps its shipment, facility and mode; its state moves RECEIVING, RECEIVED, CLOSED, and a closed lot
-- never changes. Lots are never deleted.
CREATE FUNCTION cargo_phy.check_cargo_lot_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  fixed text[] := ARRAY['tenant_id','lot_id','shipment_id','facility_location','facility_code','transport_mode','created_at'];
  k text;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_phy: cargo lot %/% cannot be deleted', OLD.tenant_id, OLD.lot_id USING ERRCODE = 'restrict_violation';
  END IF;
  FOREACH k IN ARRAY fixed LOOP
    IF to_jsonb(OLD)->k IS DISTINCT FROM to_jsonb(NEW)->k THEN
      RAISE EXCEPTION 'cargo_phy: cargo lot %/% keeps its %', OLD.tenant_id, OLD.lot_id, k USING ERRCODE = 'restrict_violation';
    END IF;
  END LOOP;
  IF OLD.state = 'CLOSED' OR (OLD.state = 'RECEIVED' AND NEW.state = 'RECEIVING') THEN
    RAISE EXCEPTION 'cargo_phy: cargo lot %/% cannot go from % to %', OLD.tenant_id, OLD.lot_id, OLD.state, NEW.state
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER cargo_lot_rules BEFORE UPDATE OR DELETE ON cargo_phy.cargo_lot
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_cargo_lot_change();

-- What a lot gains it gains while it is not closed: receipt lines only while receiving, screening results for
-- AIR cargo only. The lot row is locked first.
CREATE FUNCTION cargo_phy.check_lot_addition() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  l cargo_phy.cargo_lot;
BEGIN
  SELECT * INTO l FROM cargo_phy.cargo_lot WHERE tenant_id = NEW.tenant_id AND lot_id = NEW.lot_id FOR NO KEY UPDATE;
  IF l.lot_id IS NULL OR l.state = 'CLOSED' OR (TG_TABLE_NAME = 'receipt_line' AND l.state <> 'RECEIVING')
     OR (TG_TABLE_NAME = 'screening_result' AND l.transport_mode <> 'AIR') THEN
    RAISE EXCEPTION 'cargo_phy: cargo lot %/% takes no % now', NEW.tenant_id, NEW.lot_id, TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER receipt_line_rules BEFORE INSERT ON cargo_phy.receipt_line FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_lot_addition();
CREATE TRIGGER measurement_version_rules BEFORE INSERT ON cargo_phy.measurement_version FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_lot_addition();
CREATE TRIGGER screening_result_rules BEFORE INSERT ON cargo_phy.screening_result FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_lot_addition();

-- At commit, a closed lot has no open discrepancy, and a completed one has a measurement.
CREATE FUNCTION cargo_phy.check_cargo_lot_complete() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  l cargo_phy.cargo_lot;
BEGIN
  SELECT * INTO l FROM cargo_phy.cargo_lot WHERE tenant_id = NEW.tenant_id AND lot_id = NEW.lot_id;
  IF (l.state <> 'RECEIVING' AND NOT EXISTS (SELECT 1 FROM cargo_phy.measurement_version m WHERE m.tenant_id = l.tenant_id AND m.lot_id = l.lot_id))
     OR (l.state = 'CLOSED' AND EXISTS (SELECT 1 FROM cargo_phy.cargo_discrepancy d
          WHERE d.tenant_id = l.tenant_id AND d.lot_id = l.lot_id AND d.resolved_at IS NULL)) THEN
    RAISE EXCEPTION 'cargo_phy: cargo lot %/% is % without a measurement or with an open discrepancy', l.tenant_id, l.lot_id, l.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cargo_lot_complete';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER cargo_lot_complete AFTER INSERT OR UPDATE ON cargo_phy.cargo_lot
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_cargo_lot_complete();

-- A load execution keeps its consolidation, kind, mode and facility; its state moves OPEN, LOADING, ON_HOLD and
-- back, CLOSED; a closed execution changes only its revision. Executions are never deleted. A new one tenders an
-- AIR_CONSOL or SEA_LCL_COLOAD consolidation that is OPEN or CLOSED, read under its FOR SHARE lock.
CREATE FUNCTION cargo_phy.check_load_execution_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  fixed text[] := ARRAY['tenant_id','execution_id','consolidation_id','kind','transport_mode','facility_location','facility_code',
    'opened_by','opened_at','opened_entered_at','created_at'];
  k text;
  c cargo_phy.consolidation;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% cannot be deleted', OLD.tenant_id, OLD.execution_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'INSERT' THEN
    SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
    IF c.consolidation_id IS NULL OR c.kind NOT IN ('AIR_CONSOL','SEA_LCL_COLOAD') OR c.state NOT IN ('OPEN','CLOSED')
       OR c.transport_mode <> NEW.transport_mode OR NEW.state <> 'OPEN' THEN
      RAISE EXCEPTION 'cargo_phy: consolidation %/% takes no tender now', NEW.tenant_id, NEW.consolidation_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'load_execution_consolidation';
    END IF;
    RETURN NEW;
  END IF;
  FOREACH k IN ARRAY fixed LOOP
    IF to_jsonb(OLD)->k IS DISTINCT FROM to_jsonb(NEW)->k THEN
      RAISE EXCEPTION 'cargo_phy: load execution %/% keeps its %', OLD.tenant_id, OLD.execution_id, k USING ERRCODE = 'restrict_violation';
    END IF;
  END LOOP;
  IF (OLD.state = 'CLOSED' AND to_jsonb(OLD) - ARRAY['revision','updated_at'] IS DISTINCT FROM to_jsonb(NEW) - ARRAY['revision','updated_at'])
     OR (OLD.state <> 'OPEN' AND NEW.state = 'OPEN') OR (OLD.state = 'OPEN' AND NEW.state NOT IN ('OPEN','LOADING'))
     OR (OLD.state = 'ON_HOLD' AND NEW.state = 'CLOSED') THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% cannot go from % to %', OLD.tenant_id, OLD.execution_id, OLD.state, NEW.state
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER load_execution_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_phy.load_execution
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_load_execution_change();

-- What an execution gains it gains in its state: lines while loading (or held by the line itself), reversals and
-- holds while loading or on hold, carrier evidence and its discrepancies once closed. The execution row is locked
-- first. A line names its execution's consolidation and a lot of its shipment and mode.
CREATE FUNCTION cargo_phy.check_execution_addition() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  x cargo_phy.load_execution;
  l cargo_phy.cargo_lot;
  allowed text[] := CASE TG_TABLE_NAME WHEN 'carrier_load_evidence' THEN ARRAY['CLOSED'] WHEN 'loading_discrepancy' THEN ARRAY['CLOSED']
    ELSE ARRAY['LOADING','ON_HOLD'] END;
BEGIN
  SELECT * INTO x FROM cargo_phy.load_execution WHERE tenant_id = NEW.tenant_id AND execution_id = NEW.execution_id FOR NO KEY UPDATE;
  IF x.execution_id IS NULL OR NOT x.state = ANY (allowed) THEN
    RAISE EXCEPTION 'cargo_phy: load execution %/% takes no % while %', NEW.tenant_id, NEW.execution_id, TG_TABLE_NAME, x.state
      USING ERRCODE = 'check_violation';
  END IF;
  IF TG_TABLE_NAME = 'load_line' THEN
    SELECT * INTO l FROM cargo_phy.cargo_lot WHERE tenant_id = NEW.tenant_id AND lot_id = (to_jsonb(NEW)->>'lot_id');
    IF (to_jsonb(NEW)->>'consolidation_id') IS DISTINCT FROM x.consolidation_id OR l.shipment_id IS DISTINCT FROM (to_jsonb(NEW)->>'shipment_id')
       OR l.transport_mode <> x.transport_mode OR l.state = 'CLOSED' THEN
      RAISE EXCEPTION 'cargo_phy: load line %/%/% does not fit its execution and lot', NEW.tenant_id, NEW.execution_id, to_jsonb(NEW)->>'line_no'
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER load_line_rules BEFORE INSERT ON cargo_phy.load_line FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_execution_addition();
CREATE TRIGGER reversal_line_rules BEFORE INSERT ON cargo_phy.reversal_line FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_execution_addition();
CREATE TRIGGER hold_entry_insert BEFORE INSERT ON cargo_phy.hold_entry FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_execution_addition();
CREATE TRIGGER carrier_load_evidence_rules BEFORE INSERT ON cargo_phy.carrier_load_evidence FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_execution_addition();
CREATE TRIGGER loading_discrepancy_insert BEFORE INSERT ON cargo_phy.loading_discrepancy FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_execution_addition();

-- At commit, under the lot's lock and the house's membership ledger's FOR SHARE lock: the lot's live loaded packs
-- stay within its received packs; the house has a live entry in the line's consolidation, which is OPEN or
-- CLOSED, and its live loaded packs there stay within the entry's planned packs (CONSERVATION_VIOLATION).
CREATE FUNCTION cargo_phy.check_load_line_conservation() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  received bigint;
  planned bigint;
  s text;
BEGIN
  PERFORM 1 FROM cargo_phy.cargo_lot WHERE tenant_id = NEW.tenant_id AND lot_id = NEW.lot_id FOR NO KEY UPDATE;
  PERFORM 1 FROM cargo_phy.consol_membership WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id AND stage = 'MAIN' FOR SHARE;
  SELECT state INTO s FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
  SELECT coalesce(sum(packs), 0) INTO received FROM cargo_phy.receipt_line WHERE tenant_id = NEW.tenant_id AND lot_id = NEW.lot_id;
  SELECT e.packs INTO planned FROM cargo_phy.membership_entry e WHERE e.tenant_id = NEW.tenant_id AND e.shipment_id = NEW.shipment_id
    AND e.stage = 'MAIN' AND e.consolidation_id = NEW.consolidation_id AND e.ended_at IS NULL;
  IF s NOT IN ('OPEN','CLOSED') OR planned IS NULL OR cargo_phy.live_packs_of_lot(NEW.tenant_id, NEW.lot_id) > received
     OR cargo_phy.live_packs_of_house(NEW.tenant_id, NEW.consolidation_id, NEW.shipment_id) > planned THEN
    RAISE EXCEPTION 'cargo_phy: CONSERVATION_VIOLATION: load line %/%/% exceeds what lot % received or house % plans in %',
      NEW.tenant_id, NEW.execution_id, NEW.line_no, NEW.lot_id, NEW.shipment_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'load_line_conservation';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER load_line_conservation AFTER INSERT ON cargo_phy.load_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_load_line_conservation();

-- At commit: a house leaves a consolidation (detached, transferred, cancelled) only once nothing of it is loaded
-- there, and a live entry (a refresh) plans at least the packs loaded.
CREATE FUNCTION cargo_phy.check_membership_entry_loaded() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  e cargo_phy.membership_entry;
  loaded bigint;
BEGIN
  SELECT * INTO e FROM cargo_phy.membership_entry
    WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id AND stage = NEW.stage AND entry_no = NEW.entry_no;
  loaded := cargo_phy.live_packs_of_house(e.tenant_id, e.consolidation_id, e.shipment_id);
  IF (e.ended_at IS NULL AND loaded > e.packs) OR (e.end_kind IN ('DETACHED','TRANSFERRED','SHIPMENT_CANCELLED') AND loaded > 0) THEN
    RAISE EXCEPTION 'cargo_phy: house %/% has % packs loaded in consolidation %', e.tenant_id, e.shipment_id, loaded, e.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_loaded';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER membership_entry_loaded AFTER INSERT OR UPDATE ON cargo_phy.membership_entry
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_membership_entry_loaded();

-- A consolidation version follows the previous one (as in V0.1), and: a reopen comes before any tendered pack, a
-- cancellation and a reconciliation leave no load execution open.
CREATE OR REPLACE FUNCTION cargo_phy.check_consolidation_version() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  c cargo_phy.consolidation;
  prev cargo_phy.consolidation_version;
BEGIN
  SELECT * INTO c FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR NO KEY UPDATE;
  SELECT * INTO prev FROM cargo_phy.consolidation_version
    WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id ORDER BY version_no DESC LIMIT 1;
  IF NOT FOUND THEN
    prev.version_no := 0;
  END IF;
  IF c.consolidation_id IS NULL OR NEW.version_no <> prev.version_no + 1 OR (NEW.version_no = 1) <> (NEW.change = 'CREATED')
     OR (prev.version_no > 0 AND (NEW.written_at < prev.written_at OR NOT CASE NEW.change
          WHEN 'AMENDED' THEN prev.state IN ('OPEN','CLOSED') WHEN 'CLOSED' THEN prev.state = 'OPEN'
          WHEN 'CANCELLED' THEN prev.state = 'OPEN' WHEN 'REOPENED' THEN prev.state = 'CLOSED'
          WHEN 'RECONCILED' THEN prev.state = 'CLOSED' ELSE false END))
     OR (c.transport_mode = 'AIR') <> (NEW.origin_iata IS NOT NULL) THEN
    RAISE EXCEPTION 'cargo_phy: version % does not follow consolidation %/%', NEW.version_no, NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.change IN ('CANCELLED','RECONCILED') AND EXISTS (SELECT 1 FROM cargo_phy.load_execution x
       WHERE x.tenant_id = NEW.tenant_id AND x.consolidation_id = NEW.consolidation_id AND x.state <> 'CLOSED') THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% has a load execution open', NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_executions_closed';
  END IF;
  IF NEW.change = 'REOPENED' AND EXISTS (SELECT 1 FROM cargo_phy.load_execution x JOIN cargo_phy.load_line l
       ON l.tenant_id = x.tenant_id AND l.execution_id = x.execution_id
       WHERE x.tenant_id = NEW.tenant_id AND x.consolidation_id = NEW.consolidation_id AND x.state = 'CLOSED'
         AND l.packs > coalesce((SELECT sum(r.packs) FROM cargo_phy.reversal_line r
           WHERE r.tenant_id = l.tenant_id AND r.execution_id = l.execution_id AND r.line_no = l.line_no), 0)) THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% has tendered cargo; it cannot reopen', NEW.tenant_id, NEW.consolidation_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consolidation_reopen_before_tender';
  END IF;
  RETURN NEW;
END $fn$;

-- Tenant isolation as in the other contexts.
DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['cargo_lot','receipt_line','measurement_version','measurement_package_group','screening_result','cargo_discrepancy',
      'load_execution','load_line','reversal_line','hold_entry','carrier_load_evidence','loading_discrepancy'] LOOP
    EXECUTE format('ALTER TABLE cargo_phy.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_phy.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_phy.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
