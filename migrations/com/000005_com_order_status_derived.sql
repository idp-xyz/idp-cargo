-- IDP-Cargo COM V0.5.1 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- An order's status is derived from its lines and obligations: CANCELLED once cancelled; RECORDED while no
-- obligation is active; ALLOCATED when every line's active obligations take all its packs; otherwise
-- PARTIALLY_ALLOCATED. COM writes the status with every change. Until now the database did not check it, so a
-- write past COM (an obligation version, a line or an amendment without the order's status) left a status
-- COM refuses to restore. From now on the database checks it when the transaction commits.

-- The status an order's lines and obligations make, as domain.CustomerOrder derives it.
CREATE FUNCTION cargo_com.derived_order_status(p_tenant text, p_order text) RETURNS text LANGUAGE sql STABLE AS $fn$
  WITH active AS (
    SELECT f.line_no, sum(f.packs) AS packs FROM cargo_com.fulfillment_obligation f
      WHERE f.tenant_id = p_tenant AND f.order_id = p_order AND f.status = 'ACTIVE'
        AND NOT EXISTS (SELECT 1 FROM cargo_com.fulfillment_obligation v
          WHERE v.tenant_id = f.tenant_id AND v.obligation_id = f.obligation_id AND v.obligation_version > f.obligation_version)
      GROUP BY f.line_no),
  lines AS (
    SELECT l.line_no, (cargo_com.current_order_line(p_tenant, p_order, l.line_no)).packs AS packs FROM cargo_com.order_line l
      WHERE l.tenant_id = p_tenant AND l.order_id = p_order)
  SELECT CASE
    WHEN (SELECT o.cancelled_at FROM cargo_com.customer_order o WHERE o.tenant_id = p_tenant AND o.order_id = p_order) IS NOT NULL THEN 'CANCELLED'
    WHEN NOT EXISTS (SELECT 1 FROM active) THEN 'RECORDED'
    WHEN EXISTS (SELECT 1 FROM lines l LEFT JOIN active a USING (line_no) WHERE l.packs <> coalesce(a.packs, 0)) THEN 'PARTIALLY_ALLOCATED'
    ELSE 'ALLOCATED' END
$fn$;

-- Orders whose status a write past COM left stale get the status their lines and obligations make. Status is
-- derived data, so nothing is lost; a cancelled order keeps its status. The tables force row level security,
-- so the owner is exempt for the duration.
ALTER TABLE cargo_com.customer_order NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_com.order_line NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_com.order_line_amendment NO FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_com.fulfillment_obligation NO FORCE ROW LEVEL SECURITY;
DO $repair$
DECLARE
  n bigint;
BEGIN
  UPDATE cargo_com.customer_order o SET status = cargo_com.derived_order_status(o.tenant_id, o.order_id), updated_at = now()
    WHERE o.status <> 'CANCELLED' AND o.status <> cargo_com.derived_order_status(o.tenant_id, o.order_id);
  GET DIAGNOSTICS n = ROW_COUNT;
  RAISE NOTICE 'cargo_com: % order statuses derived again from their lines and obligations', n;
END $repair$;
ALTER TABLE cargo_com.customer_order FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_com.order_line FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_com.order_line_amendment FORCE ROW LEVEL SECURITY;
ALTER TABLE cargo_com.fulfillment_obligation FORCE ROW LEVEL SECURITY;

-- At commit, every order touched by the transaction has the status its lines and obligations make. The
-- check is deferred because COM writes the order row before the rows that change what it derives from.
CREATE FUNCTION cargo_com.check_order_status() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  stored text;
  derived text;
BEGIN
  SELECT status INTO stored FROM cargo_com.customer_order WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id;
  derived := cargo_com.derived_order_status(NEW.tenant_id, NEW.order_id);
  IF stored IS DISTINCT FROM derived THEN
    RAISE EXCEPTION 'cargo_com: order %/% has status %, but its lines and obligations make it %', NEW.tenant_id, NEW.order_id, stored, derived
      USING ERRCODE = 'check_violation', CONSTRAINT = 'customer_order_status_derived';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER customer_order_status_derived AFTER INSERT OR UPDATE ON cargo_com.customer_order
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_status();
CREATE CONSTRAINT TRIGGER order_line_order_status AFTER INSERT ON cargo_com.order_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_status();
CREATE CONSTRAINT TRIGGER order_line_amendment_order_status AFTER INSERT ON cargo_com.order_line_amendment
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_status();
CREATE CONSTRAINT TRIGGER fulfillment_obligation_order_status AFTER INSERT ON cargo_com.fulfillment_obligation
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_status();
