-- IDP-Cargo COM V0.7 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Decision P1 with the owner's rules of 2026-11-10: an order whose lines are all allocated may be closed, and a
-- closed order no longer changes, except that shipments may release the obligations they hold. Once an order
-- is closed or cancelled its contacts are kept, restricted, for 36 to 120 months from that business time; then
-- they may be erased, every version at once, with a record of the erasure. Orders cancelled before V0.7 get a
-- retention of 36 months from their cancellation.

ALTER TABLE cargo_com.customer_order
  DROP CONSTRAINT customer_order_status_check,
  ADD CONSTRAINT customer_order_status_check CHECK (status IN ('RECORDED','PARTIALLY_ALLOCATED','ALLOCATED','CANCELLED','CLOSED')),
  ADD COLUMN closed_by text CHECK (length(btrim(closed_by)) > 0),
  ADD COLUMN closed_at timestamptz,
  ADD CONSTRAINT customer_order_closure CHECK ((status = 'CLOSED') = (closed_at IS NOT NULL)
    AND (closed_by IS NULL) = (closed_at IS NULL) AND (closed_at IS NULL OR closed_at >= recorded_at));

-- The commitment of an order never changes. Status, revision and updated_at change with the order; the
-- cancellation and the closure are set once. A cancelled order no longer changes at all. A closed order keeps
-- its status and closure and takes only a new revision, when a shipment releases an obligation it holds.
-- Only an ALLOCATED order is closed.
CREATE OR REPLACE FUNCTION cargo_com.reject_order_commitment_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['status','revision','updated_at','cancelled_by','cancelled_at','closed_by','closed_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_com: order %/% cannot be deleted', OLD.tenant_id, OLD.order_id USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.status = 'CANCELLED' OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable
     OR (OLD.status = 'CLOSED' AND (NEW.status, NEW.closed_by, NEW.closed_at) IS DISTINCT FROM ('CLOSED', OLD.closed_by, OLD.closed_at))
     OR (NEW.status = 'CLOSED' AND OLD.status NOT IN ('ALLOCATED','CLOSED')) THEN
    RAISE EXCEPTION 'cargo_com: the commitment of order %/% is immutable', OLD.tenant_id, OLD.order_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- A closed order is CLOSED whatever its lines and obligations make; otherwise as in V0.5.1.
CREATE OR REPLACE FUNCTION cargo_com.derived_order_status(p_tenant text, p_order text) RETURNS text LANGUAGE sql STABLE AS $fn$
  WITH active AS (
    SELECT f.line_no, sum(f.packs) AS packs FROM cargo_com.fulfillment_obligation f
      WHERE f.tenant_id = p_tenant AND f.order_id = p_order AND f.status = 'ACTIVE'
        AND NOT EXISTS (SELECT 1 FROM cargo_com.fulfillment_obligation v
          WHERE v.tenant_id = f.tenant_id AND v.obligation_id = f.obligation_id AND v.obligation_version > f.obligation_version)
      GROUP BY f.line_no),
  lines AS (
    SELECT l.line_no, (cargo_com.current_order_line(p_tenant, p_order, l.line_no)).packs AS packs FROM cargo_com.order_line l
      WHERE l.tenant_id = p_tenant AND l.order_id = p_order),
  ord AS (SELECT o.cancelled_at, o.closed_at FROM cargo_com.customer_order o WHERE o.tenant_id = p_tenant AND o.order_id = p_order)
  SELECT CASE
    WHEN (SELECT cancelled_at FROM ord) IS NOT NULL THEN 'CANCELLED'
    WHEN (SELECT closed_at FROM ord) IS NOT NULL THEN 'CLOSED'
    WHEN NOT EXISTS (SELECT 1 FROM active) THEN 'RECORDED'
    WHEN EXISTS (SELECT 1 FROM lines l LEFT JOIN active a USING (line_no) WHERE l.packs <> coalesce(a.packs, 0)) THEN 'PARTIALLY_ALLOCATED'
    ELSE 'ALLOCATED' END
$fn$;

-- No line, obligation or reservation is added to a closed order, and no party or transport version to a
-- closed or cancelled one. Cancelling writes revocations after the order turns CANCELLED, so obligations
-- are refused for closed orders only; lines and obligations of cancelled orders are refused since V0.3.
CREATE FUNCTION cargo_com.refuse_closed_order_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  order_status text;
BEGIN
  SELECT status INTO order_status FROM cargo_com.customer_order WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id;
  IF order_status = 'CLOSED' OR (order_status = 'CANCELLED' AND TG_TABLE_NAME IN ('order_party','order_transport')) THEN
    RAISE EXCEPTION 'cargo_com: order %/% is %; no % row is added to it', NEW.tenant_id, NEW.order_id, lower(order_status), TG_TABLE_NAME
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
DO $closed$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['order_line','order_line_amendment','fulfillment_obligation','obligation_reservation','order_party','order_transport'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE INSERT ON cargo_com.%I FOR EACH ROW EXECUTE FUNCTION cargo_com.refuse_closed_order_change()',
      tbl || '_order_open', tbl);
  END LOOP;
END $closed$;

-- When an order's contacts stopped serving it and until when they are kept: months after started_at, the
-- business time the order was closed or cancelled, added in UTC (a day the month lacks becomes its last day).
CREATE TABLE cargo_com.order_contact_retention (
  tenant_id text NOT NULL,
  order_id text NOT NULL,
  cause text NOT NULL CHECK (cause IN ('CLOSED','CANCELLED')),
  started_at timestamptz NOT NULL,
  months integer NOT NULL CHECK (months BETWEEN 36 AND 120),
  retained_until timestamptz NOT NULL,
  entered_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, order_id),
  FOREIGN KEY (tenant_id, order_id) REFERENCES cargo_com.customer_order (tenant_id, order_id),
  CONSTRAINT order_contact_retention_end
    CHECK (retained_until = ((started_at AT TIME ZONE 'UTC') + make_interval(months => months)) AT TIME ZONE 'UTC')
);
CREATE INDEX order_contact_retention_due ON cargo_com.order_contact_retention (tenant_id, retained_until);

-- The erasure of an order's contacts: who, the business time, and when COM recorded it, never before the
-- retention's end.
CREATE TABLE cargo_com.order_contact_erasure (
  tenant_id text NOT NULL,
  order_id text NOT NULL,
  erased_by text NOT NULL CHECK (length(btrim(erased_by)) > 0),
  erased_at timestamptz NOT NULL,
  entered_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, order_id),
  FOREIGN KEY (tenant_id, order_id) REFERENCES cargo_com.order_contact_retention (tenant_id, order_id)
);

CREATE TRIGGER order_contact_retention_immutable BEFORE UPDATE OR DELETE ON cargo_com.order_contact_retention
  FOR EACH ROW EXECUTE FUNCTION cargo_com.reject_row_change();
CREATE TRIGGER order_contact_erasure_immutable BEFORE UPDATE OR DELETE ON cargo_com.order_contact_erasure
  FOR EACH ROW EXECUTE FUNCTION cargo_com.reject_row_change();

CREATE FUNCTION cargo_com.check_contact_erasure() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  r cargo_com.order_contact_retention;
BEGIN
  SELECT * INTO r FROM cargo_com.order_contact_retention WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id;
  IF NEW.entered_at < r.retained_until OR NEW.erased_at < r.started_at THEN
    RAISE EXCEPTION 'cargo_com: the contacts of order %/% are kept until %', NEW.tenant_id, NEW.order_id, r.retained_until
      USING ERRCODE = 'check_violation', CONSTRAINT = 'order_contact_erasure_after_retention';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER order_contact_erasure_after_retention BEFORE INSERT ON cargo_com.order_contact_erasure
  FOR EACH ROW EXECUTE FUNCTION cargo_com.check_contact_erasure();

-- A contact's personal fields are cleared only together with the record of the order's erasure.
CREATE OR REPLACE FUNCTION cargo_com.erase_order_contact_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.erased OR NOT NEW.erased
     OR NEW.name IS NOT NULL OR NEW.phone IS NOT NULL OR NEW.mobile IS NOT NULL OR NEW.email IS NOT NULL
     OR (NEW.tenant_id, NEW.order_id, NEW.role, NEW.version, NEW.source, NEW.org_contact_id, NEW.org_contact_version, NEW.overridden_by, NEW.override_reason)
        IS DISTINCT FROM (OLD.tenant_id, OLD.order_id, OLD.role, OLD.version, OLD.source, OLD.org_contact_id, OLD.org_contact_version, OLD.overridden_by, OLD.override_reason)
     OR NOT EXISTS (SELECT 1 FROM cargo_com.order_contact_erasure e WHERE e.tenant_id = OLD.tenant_id AND e.order_id = OLD.order_id) THEN
    RAISE EXCEPTION 'cargo_com: the contact of % version % on order %/% can only be erased, with the record of the order''s erasure',
      OLD.role, OLD.version, OLD.tenant_id, OLD.order_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- At commit, every order a transaction touched has a contact retention exactly if it is closed or cancelled,
-- starting when it was; and once its contacts are erased, none of them is left.
CREATE FUNCTION cargo_com.check_contact_retention() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  o cargo_com.customer_order;
  r cargo_com.order_contact_retention;
  bad text;
BEGIN
  SELECT * INTO o FROM cargo_com.customer_order WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id;
  SELECT * INTO r FROM cargo_com.order_contact_retention WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id;
  IF (o.status IN ('CLOSED','CANCELLED')) IS DISTINCT FROM (r.order_id IS NOT NULL) THEN
    bad := 'a closed or cancelled order has a contact retention, and only it';
  ELSIF r.order_id IS NOT NULL AND (r.cause IS DISTINCT FROM o.status OR r.started_at IS DISTINCT FROM coalesce(o.closed_at, o.cancelled_at)) THEN
    bad := 'the contact retention starts when the order was closed or cancelled';
  ELSIF EXISTS (SELECT 1 FROM cargo_com.order_contact_erasure e WHERE e.tenant_id = NEW.tenant_id AND e.order_id = NEW.order_id)
        AND EXISTS (SELECT 1 FROM cargo_com.order_party_contact c WHERE c.tenant_id = NEW.tenant_id AND c.order_id = NEW.order_id AND NOT c.erased) THEN
    bad := 'the erasure left a contact';
  END IF;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_com: order %/%: %', NEW.tenant_id, NEW.order_id, bad
      USING ERRCODE = 'check_violation', CONSTRAINT = 'order_contact_retention_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER customer_order_contact_retention AFTER INSERT OR UPDATE ON cargo_com.customer_order
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_contact_retention();
CREATE CONSTRAINT TRIGGER order_contact_retention_consistent AFTER INSERT ON cargo_com.order_contact_retention
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_contact_retention();
CREATE CONSTRAINT TRIGGER order_contact_erasure_consistent AFTER INSERT ON cargo_com.order_contact_erasure
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_contact_retention();
CREATE CONSTRAINT TRIGGER order_party_contact_retention AFTER INSERT OR UPDATE ON cargo_com.order_party_contact
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_contact_retention();

-- Orders cancelled before V0.7 keep their contacts 36 months from their cancellation. The order table forces
-- row level security, so the owner is exempt for the duration.
ALTER TABLE cargo_com.customer_order NO FORCE ROW LEVEL SECURITY;
DO $retain$
DECLARE
  n bigint;
BEGIN
  INSERT INTO cargo_com.order_contact_retention (tenant_id, order_id, cause, started_at, months, retained_until, entered_at)
    SELECT o.tenant_id, o.order_id, 'CANCELLED', o.cancelled_at, 36,
           ((o.cancelled_at AT TIME ZONE 'UTC') + make_interval(months => 36)) AT TIME ZONE 'UTC', now()
      FROM cargo_com.customer_order o WHERE o.status = 'CANCELLED';
  GET DIAGNOSTICS n = ROW_COUNT;
  -- Checked now, while the owner still sees every row; at commit row level security would hide them.
  SET CONSTRAINTS cargo_com.order_contact_retention_consistent IMMEDIATE;
  RAISE NOTICE 'cargo_com: % cancelled orders keep their contacts 36 months from their cancellation', n;
END $retain$;
ALTER TABLE cargo_com.customer_order FORCE ROW LEVEL SECURITY;

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['order_contact_retention','order_contact_erasure'] LOOP
    EXECUTE format('ALTER TABLE cargo_com.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_com.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format('CREATE POLICY tenant_isolation ON cargo_com.%I USING (tenant_id = current_setting(''app.tenant_id'', true)) '
      'WITH CHECK (tenant_id = current_setting(''app.tenant_id'', true))', tbl);
  END LOOP;
END $rls$;
