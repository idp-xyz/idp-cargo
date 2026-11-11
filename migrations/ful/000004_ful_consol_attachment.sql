-- IDP-Cargo FUL V0.4 (first part) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A shipment records the consolidation PHY attached it to on a stage, with the PHY membership entry and
-- revision FUL read (relationship 8, ConsolMembership v1). PHY decides the membership and commits first
-- (decision N1-PHY, OD-1); FUL only records it. Rows are immutable but for their end, written once; at most
-- one attachment per shipment and stage is current. No foreign key reaches cargo_phy.

CREATE TABLE cargo_ful.shipment_consol_attachment (
 tenant_id text NOT NULL,
 shipment_id text NOT NULL,
 attachment_no integer NOT NULL CHECK (attachment_no > 0),
 stage text NOT NULL CHECK (stage = 'MAIN'),
 consolidation_id text NOT NULL CHECK (length(btrim(consolidation_id)) BETWEEN 1 AND 128),
 membership_entry_no integer NOT NULL CHECK (membership_entry_no > 0),
 membership_revision bigint NOT NULL CHECK (membership_revision > 0),
 attached_from timestamptz NOT NULL,
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 ended_by text CHECK (length(btrim(ended_by)) > 0),
 ended_at timestamptz,
 end_reason text CHECK (end_reason IN ('DETACHED','TRANSFERRED','REFRESHED','SHIPMENT_CANCELLED')),
 end_entered_at timestamptz,
 PRIMARY KEY (tenant_id, shipment_id, attachment_no),
 FOREIGN KEY (tenant_id, shipment_id) REFERENCES cargo_ful.shipment (tenant_id, shipment_id),
 CONSTRAINT shipment_consol_attachment_times CHECK (recorded_at >= attached_from),
 CONSTRAINT shipment_consol_attachment_end CHECK ((ended_by IS NULL) = (ended_at IS NULL)
   AND (end_reason IS NULL) = (ended_at IS NULL) AND (end_entered_at IS NULL) = (ended_at IS NULL)
   AND (ended_at IS NULL OR ended_at >= recorded_at))
);
CREATE UNIQUE INDEX shipment_consol_attachment_one_current ON cargo_ful.shipment_consol_attachment (tenant_id, shipment_id, stage)
  WHERE ended_at IS NULL;

-- An attachment joins a shipment that is not cancelled, whose row is locked first; afterwards only its end is
-- written, once. Attachments are never deleted.
CREATE FUNCTION cargo_ful.check_consol_attachment() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  s cargo_ful.shipment;
  ends text[] := ARRAY['ended_by','ended_at','end_reason','end_entered_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_ful: consol attachments are kept' USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF OLD.ended_at IS NOT NULL OR NEW.ended_at IS NULL OR to_jsonb(OLD) - ends IS DISTINCT FROM to_jsonb(NEW) - ends THEN
      RAISE EXCEPTION 'cargo_ful: consol attachment %/%/% can only be ended, once', OLD.tenant_id, OLD.shipment_id, OLD.attachment_no
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  SELECT * INTO s FROM cargo_ful.shipment WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id FOR NO KEY UPDATE;
  IF NOT FOUND OR s.status = 'CANCELLED' THEN
    RAISE EXCEPTION 'cargo_ful: shipment %/% takes no consol attachment', NEW.tenant_id, NEW.shipment_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER shipment_consol_attachment_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_ful.shipment_consol_attachment
  FOR EACH ROW EXECUTE FUNCTION cargo_ful.check_consol_attachment();

-- A cancelled shipment keeps no current attachment: CancelShipment ends it in the same transaction.
CREATE FUNCTION cargo_ful.check_cancelled_without_attachment() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_ful.shipment s JOIN cargo_ful.shipment_consol_attachment a
               ON a.tenant_id = s.tenant_id AND a.shipment_id = s.shipment_id AND a.ended_at IS NULL
             WHERE s.tenant_id = NEW.tenant_id AND s.shipment_id = NEW.shipment_id AND s.status = 'CANCELLED') THEN
    RAISE EXCEPTION 'cargo_ful: cancelled shipment %/% keeps a current consol attachment', NEW.tenant_id, NEW.shipment_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'shipment_cancelled_without_attachment';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER shipment_cancelled_without_attachment AFTER UPDATE OF status ON cargo_ful.shipment
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.status = 'CANCELLED')
  EXECUTE FUNCTION cargo_ful.check_cancelled_without_attachment();

ALTER TABLE cargo_ful.shipment_consol_attachment ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_ful.shipment_consol_attachment FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_ful.shipment_consol_attachment
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
