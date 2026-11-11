-- IDP-Cargo FUL V0.4 (second part) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A shipment keeps versions of what PHY verified of its cargo (PhysicalLoadEvidence v1, relationship 8), next to
-- its plan and never in it (UL-13): received packs and the lots' latest measures, what was tendered in the
-- consolidation it is attached to, the pieces the carrier's evidence shows departed, arrived and offloaded, the
-- security state of AIR cargo, open discrepancies, the execution status derived from them, and the departed
-- packs final for planning (those departed, up to what the plan held then). A version is written only when PHY's
-- facts changed (digest); rows are immutable. No foreign key reaches cargo_phy.

CREATE TABLE cargo_ful.shipment_load_evidence (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 evidence_no integer NOT NULL CHECK (evidence_no > 0),
 digest char(64) NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
 lots integer NOT NULL CHECK (lots >= 0),
 receipt_complete boolean NOT NULL,
 received_packs bigint NOT NULL CHECK (received_packs >= 0),
 received_weight_unit text CHECK (received_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 received_weight bigint NOT NULL CHECK (received_weight >= 0),
 received_volume_unit text CHECK (received_volume_unit ~ '^[A-Z0-9]{1,3}$'),
 received_volume bigint NOT NULL CHECK (received_volume >= 0),
 consolidation_id text CHECK (length(btrim(consolidation_id)) BETWEEN 1 AND 128),
 tendered_packs bigint NOT NULL CHECK (tendered_packs >= 0),
 tendered_weight_unit text CHECK (tendered_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 tendered_weight bigint NOT NULL CHECK (tendered_weight >= 0),
 tendered_volume_unit text CHECK (tendered_volume_unit ~ '^[A-Z0-9]{1,3}$'),
 tendered_volume bigint NOT NULL CHECK (tendered_volume >= 0),
 departed bigint NOT NULL CHECK (departed >= 0),
 arrived bigint NOT NULL CHECK (arrived >= 0),
 offloaded bigint NOT NULL CHECK (offloaded >= 0),
 security text CHECK (security IN ('PENDING','CLEARED','FAILED')),
 open_discrepancies integer NOT NULL CHECK (open_discrepancies >= 0),
 execution_status text NOT NULL CHECK (execution_status IN ('NOT_STARTED','PARTIALLY_DEPARTED','DEPARTED','PARTIALLY_ARRIVED','ARRIVED')),
 departed_final bigint NOT NULL CHECK (departed_final >= 0),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, shipment_id, evidence_no),
 FOREIGN KEY (tenant_id, shipment_id) REFERENCES cargo_ful.shipment (tenant_id, shipment_id),
 CONSTRAINT shipment_load_evidence_units CHECK ((received_weight_unit IS NOT NULL OR received_weight = 0)
   AND (received_volume_unit IS NOT NULL OR received_volume = 0) AND (tendered_weight_unit IS NOT NULL OR tendered_weight = 0)
   AND (tendered_volume_unit IS NOT NULL OR tendered_volume = 0)),
 CONSTRAINT shipment_load_evidence_final CHECK (departed_final <= departed),
 CONSTRAINT shipment_load_evidence_consolidation CHECK (consolidation_id IS NOT NULL
   OR (tendered_packs = 0 AND tendered_weight = 0 AND tendered_volume = 0 AND departed = 0 AND arrived = 0 AND offloaded = 0))
);

-- Evidence versions are kept as written. A version joins a shipment that is not cancelled, whose row is locked
-- first, and follows the previous one.
CREATE FUNCTION cargo_ful.check_load_evidence() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_ful.shipment;
  last integer;
BEGIN
  IF TG_OP <> 'INSERT' THEN
    RAISE EXCEPTION 'cargo_ful: load evidence versions are immutable' USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT * INTO s FROM cargo_ful.shipment WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id FOR NO KEY UPDATE;
  SELECT coalesce(max(evidence_no), 0) INTO last FROM cargo_ful.shipment_load_evidence
    WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id;
  IF s.shipment_id IS NULL OR s.status = 'CANCELLED' OR NEW.evidence_no <> last + 1 THEN
    RAISE EXCEPTION 'cargo_ful: shipment %/% takes no load evidence %', NEW.tenant_id, NEW.shipment_id, NEW.evidence_no
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER shipment_load_evidence_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_ful.shipment_load_evidence
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_load_evidence();

-- Departed quantity is final for planning: at commit, a shipment whose latest evidence holds departed packs final
-- is not cancelled and plans at least those packs.
CREATE FUNCTION cargo_ful.check_departed_final() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  departed bigint;
  planned bigint;
  st text;
BEGIN
  SELECT e.departed_final INTO departed FROM cargo_ful.shipment_load_evidence e
    WHERE e.tenant_id = NEW.tenant_id AND e.shipment_id = NEW.shipment_id ORDER BY e.evidence_no DESC LIMIT 1;
  IF coalesce(departed, 0) = 0 THEN
    RETURN NULL;
  END IF;
  SELECT s.status, p.scope_packs INTO st, planned FROM cargo_ful.shipment s JOIN cargo_ful.shipment_plan_version p
      ON p.tenant_id = s.tenant_id AND p.shipment_id = s.shipment_id
    WHERE s.tenant_id = NEW.tenant_id AND s.shipment_id = NEW.shipment_id ORDER BY p.plan_no DESC LIMIT 1;
  IF st = 'CANCELLED' OR planned < departed THEN
    RAISE EXCEPTION 'cargo_ful: shipment %/% has % packs departed; they stay planned', NEW.tenant_id, NEW.shipment_id, departed
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_departed_final';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER shipment_departed_final AFTER UPDATE ON cargo_ful.shipment
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_departed_final();

ALTER TABLE cargo_ful.shipment_load_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_ful.shipment_load_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_ful.shipment_load_evidence
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
