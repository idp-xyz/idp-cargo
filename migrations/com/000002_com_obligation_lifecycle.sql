-- IDP-Cargo COM V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Business review decisions C3 and C6: lines and obligations are measured in packs of a package type, gross
-- weight and an optional volume; an obligation is revoked by a new version with status REVOKED; an order can
-- be cancelled; a FUL shipment reserves an obligation, and a reserved obligation cannot be revoked.
-- A V0.1 line holds one quantity, which cannot be turned into packs and a gross weight, so this migration
-- needs a cargo_com without orders. The guard below says so; the NOT NULL columns added without defaults
-- fail on any remaining line as well, whatever the migrator's row level security rights.
DO $empty$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_com.customer_order) THEN
    RAISE EXCEPTION 'cargo_com: V0.1 orders cannot be converted to packs, package type and gross weight'
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
END $empty$;

ALTER TABLE cargo_com.customer_order
  DROP CONSTRAINT customer_order_status_check,
  ADD CONSTRAINT customer_order_status_check CHECK (status IN ('RECORDED','PARTIALLY_ALLOCATED','ALLOCATED','CANCELLED')),
  ADD COLUMN cancelled_by text CHECK (length(btrim(cancelled_by)) > 0),
  ADD COLUMN cancelled_at timestamptz,
  ADD CONSTRAINT customer_order_cancellation CHECK ((status = 'CANCELLED') = (cancelled_at IS NOT NULL)
    AND (cancelled_by IS NULL) = (cancelled_at IS NULL) AND (cancelled_at IS NULL OR cancelled_at >= recorded_at));

ALTER TABLE cargo_com.order_line
  DROP COLUMN unit_code,
  DROP COLUMN quantity,
  ADD COLUMN package_type text NOT NULL CHECK (package_type ~ '^[A-Z0-9]{2}$'),
  ADD COLUMN packs bigint NOT NULL CHECK (packs > 0),
  ADD COLUMN gross_weight_unit text NOT NULL CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
  ADD COLUMN gross_weight bigint NOT NULL CHECK (gross_weight > 0),
  ADD COLUMN volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
  ADD COLUMN volume bigint CHECK (volume > 0),
  ADD CONSTRAINT order_line_volume_declared CHECK ((volume_unit IS NULL) = (volume IS NULL));

ALTER TABLE cargo_com.fulfillment_obligation
  DROP COLUMN unit_code,
  DROP COLUMN quantity,
  ADD COLUMN packs bigint NOT NULL CHECK (packs > 0),
  ADD COLUMN gross_weight_unit text NOT NULL CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
  ADD COLUMN gross_weight bigint NOT NULL CHECK (gross_weight > 0),
  ADD COLUMN volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
  ADD COLUMN volume bigint CHECK (volume > 0),
  ADD COLUMN status text NOT NULL CHECK (status IN ('ACTIVE','REVOKED')),
  ADD COLUMN revoked_by text CHECK (length(btrim(revoked_by)) > 0),
  ADD COLUMN revoked_at timestamptz,
  ADD CONSTRAINT fulfillment_obligation_volume_declared CHECK ((volume_unit IS NULL) = (volume IS NULL)),
  ADD CONSTRAINT fulfillment_obligation_revocation CHECK ((status = 'REVOKED') = (revoked_at IS NOT NULL)
    AND (revoked_by IS NULL) = (revoked_at IS NULL) AND (revoked_at IS NULL OR revoked_at >= allocated_at));

-- A FUL shipment's hold on an obligation version (ObligationReservation v1). Rows are kept: an update may only
-- release a reservation, once. At most one reservation of an obligation is in force.
CREATE TABLE cargo_com.obligation_reservation (
 tenant_id text NOT NULL,
 order_id text NOT NULL,
 reservation_no integer NOT NULL CHECK (reservation_no > 0),
 obligation_id text NOT NULL,
 obligation_version bigint NOT NULL,
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 reserved_by text NOT NULL CHECK (length(btrim(reserved_by)) > 0),
 reserved_at timestamptz NOT NULL,
 released_by text CHECK (length(btrim(released_by)) > 0),
 released_at timestamptz,
 PRIMARY KEY (tenant_id, order_id, reservation_no),
 FOREIGN KEY (tenant_id, order_id) REFERENCES cargo_com.customer_order (tenant_id, order_id),
 FOREIGN KEY (tenant_id, obligation_id, obligation_version)
   REFERENCES cargo_com.fulfillment_obligation (tenant_id, obligation_id, obligation_version),
 CONSTRAINT obligation_reservation_release CHECK ((released_by IS NULL) = (released_at IS NULL)
   AND (released_at IS NULL OR released_at >= reserved_at))
);
CREATE UNIQUE INDEX obligation_reservation_one_in_force ON cargo_com.obligation_reservation (tenant_id, obligation_id)
  WHERE released_at IS NULL;

-- The commitment of an order never changes. Only the status, the revision, updated_at and the cancellation may;
-- the status follows the active obligations, which a revocation lowers, and a cancelled order never changes again.
CREATE OR REPLACE FUNCTION cargo_com.reject_order_commitment_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['status','revision','updated_at','cancelled_by','cancelled_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_com: order %/% cannot be deleted', OLD.tenant_id, OLD.order_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.status = 'CANCELLED' OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_com: the commitment of order %/% is immutable', OLD.tenant_id, OLD.order_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- Version 1 of an obligation is ACTIVE, belongs to an order that is not cancelled and fits what its line has
-- left: the line's active obligations never exceed its packs, gross weight or volume, in the line's units. A
-- later version revokes the latest one, which must be ACTIVE and reserved by no shipment, and repeats its
-- line, measure and allocation. The order row is locked first, so inserts and reservations for one order are
-- checked one after another.
CREATE OR REPLACE FUNCTION cargo_com.check_obligation_within_line() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  order_status text;
  ln cargo_com.order_line;
  prev cargo_com.fulfillment_obligation;
  active_packs bigint;
  active_weight bigint;
  active_volume bigint;
BEGIN
  SELECT status INTO order_status FROM cargo_com.customer_order
    WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id FOR NO KEY UPDATE;
  IF NEW.obligation_version = 1 THEN
    SELECT * INTO ln FROM cargo_com.order_line
      WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id AND line_no = NEW.line_no;
    SELECT coalesce(sum(f.packs), 0), coalesce(sum(f.gross_weight), 0), coalesce(sum(f.volume), 0)
      INTO active_packs, active_weight, active_volume
      FROM cargo_com.fulfillment_obligation f
      WHERE f.tenant_id = NEW.tenant_id AND f.order_id = NEW.order_id AND f.line_no = NEW.line_no AND f.status = 'ACTIVE'
        AND NOT EXISTS (SELECT 1 FROM cargo_com.fulfillment_obligation v
          WHERE v.tenant_id = f.tenant_id AND v.obligation_id = f.obligation_id AND v.obligation_version > f.obligation_version);
    IF NEW.status <> 'ACTIVE' OR order_status = 'CANCELLED'
       OR ln.gross_weight_unit IS DISTINCT FROM NEW.gross_weight_unit OR ln.volume_unit IS DISTINCT FROM NEW.volume_unit
       OR active_packs + NEW.packs > ln.packs OR active_weight + NEW.gross_weight > ln.gross_weight
       OR active_volume + coalesce(NEW.volume, 0) > coalesce(ln.volume, 0) THEN
      RAISE EXCEPTION 'cargo_com: obligation %/% does not fit order line %', NEW.tenant_id, NEW.obligation_id, NEW.line_no
        USING ERRCODE = 'check_violation';
    END IF;
  ELSE
    SELECT * INTO prev FROM cargo_com.fulfillment_obligation
      WHERE tenant_id = NEW.tenant_id AND obligation_id = NEW.obligation_id ORDER BY obligation_version DESC LIMIT 1;
    IF NOT FOUND OR prev.obligation_version <> NEW.obligation_version - 1 OR prev.status <> 'ACTIVE' OR NEW.status <> 'REVOKED'
       OR (prev.order_id, prev.line_no, prev.packs, prev.gross_weight_unit, prev.gross_weight, prev.volume_unit, prev.volume,
           prev.allocated_by, prev.allocated_at)
          IS DISTINCT FROM (NEW.order_id, NEW.line_no, NEW.packs, NEW.gross_weight_unit, NEW.gross_weight, NEW.volume_unit, NEW.volume,
           NEW.allocated_by, NEW.allocated_at)
       OR EXISTS (SELECT 1 FROM cargo_com.obligation_reservation r
          WHERE r.tenant_id = NEW.tenant_id AND r.obligation_id = NEW.obligation_id AND r.released_at IS NULL) THEN
      RAISE EXCEPTION 'cargo_com: version % does not revoke obligation %/%', NEW.obligation_version, NEW.tenant_id, NEW.obligation_id
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;

-- A reservation holds the latest version of an obligation of its order, which must be ACTIVE; the order row is
-- locked first, as for obligations. Reservations are never deleted, and an update only releases one.
CREATE FUNCTION cargo_com.check_reservation() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  latest cargo_com.fulfillment_obligation;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_com: reservation %/%/% cannot be deleted', OLD.tenant_id, OLD.order_id, OLD.reservation_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF OLD.released_at IS NOT NULL OR NEW.released_at IS NULL
       OR to_jsonb(OLD) - ARRAY['released_by','released_at'] IS DISTINCT FROM to_jsonb(NEW) - ARRAY['released_by','released_at'] THEN
      RAISE EXCEPTION 'cargo_com: reservation %/%/% can only be released, once', OLD.tenant_id, OLD.order_id, OLD.reservation_no
        USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  PERFORM 1 FROM cargo_com.customer_order WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id FOR NO KEY UPDATE;
  SELECT * INTO latest FROM cargo_com.fulfillment_obligation
    WHERE tenant_id = NEW.tenant_id AND obligation_id = NEW.obligation_id ORDER BY obligation_version DESC LIMIT 1;
  IF NOT FOUND OR latest.obligation_version <> NEW.obligation_version OR latest.status <> 'ACTIVE'
     OR latest.order_id <> NEW.order_id OR NEW.reserved_at < latest.allocated_at THEN
    RAISE EXCEPTION 'cargo_com: obligation %/% version % cannot be reserved', NEW.tenant_id, NEW.obligation_id, NEW.obligation_version
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER obligation_reservation_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_com.obligation_reservation
  FOR EACH ROW EXECUTE FUNCTION cargo_com.check_reservation();

ALTER TABLE cargo_com.obligation_reservation ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_com.obligation_reservation FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_com.obligation_reservation
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
