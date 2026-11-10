-- IDP-Cargo COM V0.5 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Owner decisions of 2026-11-09:
--   C10 a customer's correction of what it declared is a new version of the order line, with a reason, and
--       needs no new quote; lines may be added. A line never falls below what its active obligations commit.
--   C1  how many orders one accepted quote backs, and C2 the grace after the quote's validity, follow an
--       order policy kept per legal entity with a tenant default; each order freezes the policy it followed.
--   C8  an order freezes the commercial terms the quote agreed, completed by those given on the order.
--   R7  the ordering party is the quote's recipient, in the role the quote went to.
--   T1  an order and each line change keep the time COM recorded them.
-- Existing orders followed the rules these replace: they become orders of a v1 quote snapshot, placed by a
-- CUSTOMER under the built-in policy (one order, no grace), without terms and without a recording time.

ALTER TABLE cargo_com.customer_order
  ADD COLUMN customer_role_code text NOT NULL DEFAULT 'CUSTOMER'
    CHECK (customer_role_code IN ('CUSTOMER','OVERSEAS_AGENT','SHIPPER','CONSIGNEE')),
  ADD COLUMN quote_recipient_role text NOT NULL DEFAULT 'CUSTOMER'
    CHECK (quote_recipient_role IN ('CUSTOMER','OVERSEAS_AGENT','SHIPPER','CONSIGNEE')),
  ADD COLUMN accepted_terms_contract text NOT NULL DEFAULT 'QuoteSnapshot.v1'
    CHECK (accepted_terms_contract IN ('QuoteSnapshot.v1','QuoteSnapshot.v2')),
  ADD COLUMN incoterms_rule text,
  ADD COLUMN incoterms_version text,
  ADD COLUMN named_place text,
  ADD COLUMN incoterms_source text,
  ADD COLUMN service_level text,
  ADD COLUMN service_level_source text,
  ADD COLUMN policy_scope text NOT NULL DEFAULT 'BUILT_IN',
  ADD COLUMN policy_legal_entity_id text,
  ADD COLUMN policy_version bigint NOT NULL DEFAULT 0,
  ADD COLUMN max_orders_per_quote integer NOT NULL DEFAULT 1,
  ADD COLUMN grace_days integer NOT NULL DEFAULT 0,
  ADD COLUMN entered_at timestamptz,
  ADD CONSTRAINT customer_order_ordering_role CHECK (customer_role_code = quote_recipient_role),
  ADD CONSTRAINT customer_order_quote_v1 CHECK (accepted_terms_contract = 'QuoteSnapshot.v2'
    OR (quote_recipient_role = 'CUSTOMER' AND incoterms_source IS DISTINCT FROM 'QUOTE' AND service_level_source IS DISTINCT FROM 'QUOTE')),
  ADD CONSTRAINT customer_order_incoterms CHECK (CASE
    WHEN incoterms_rule IS NULL THEN incoterms_version IS NULL AND named_place IS NULL AND incoterms_source IS NULL
    ELSE incoterms_rule ~ '^[A-Z]{3}$' AND incoterms_version IN ('2010','2020') AND length(btrim(named_place)) BETWEEN 1 AND 200
      AND incoterms_source IN ('QUOTE','ORDER') END),
  ADD CONSTRAINT customer_order_service_level CHECK (CASE
    WHEN service_level IS NULL THEN service_level_source IS NULL
    WHEN service_level_source = 'QUOTE' THEN length(btrim(service_level)) BETWEEN 1 AND 64
    ELSE service_level_source = 'ORDER' AND service_level ~ '^[A-Z][A-Z0-9_]{1,15}$' END),
  ADD CONSTRAINT customer_order_policy CHECK (CASE policy_scope
    WHEN 'BUILT_IN' THEN policy_version = 0 AND policy_legal_entity_id IS NULL
    WHEN 'TENANT' THEN policy_version > 0 AND policy_legal_entity_id IS NULL
    WHEN 'LEGAL_ENTITY' THEN policy_version > 0 AND policy_legal_entity_id = legal_entity_id
    ELSE false END AND max_orders_per_quote BETWEEN 1 AND 999 AND grace_days BETWEEN 0 AND 30),
  DROP CONSTRAINT customer_order_one_per_quote;
CREATE INDEX customer_order_by_quote ON cargo_com.customer_order (tenant_id, quote_id);

-- C2: an order may be recorded until the end of the quote's validity plus the grace of its policy. The
-- unnamed check of V0.1 that ended at the validity is found by its definition.
DO $c2$
DECLARE c text;
BEGIN
  SELECT conname INTO STRICT c FROM pg_constraint
    WHERE conrelid = 'cargo_com.customer_order'::regclass AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%recorded_at < quote_valid_until%';
  EXECUTE format('ALTER TABLE cargo_com.customer_order DROP CONSTRAINT %I', c);
END $c2$;
ALTER TABLE cargo_com.customer_order ADD CONSTRAINT customer_order_within_validity CHECK (recorded_at >= quote_accepted_at
  AND recorded_at >= quote_valid_from AND recorded_at < quote_valid_until + make_interval(days => grace_days));

-- Order policies (C1, C2): every version of the policy of a legal entity, or of the tenant default when
-- legal_entity_id is empty. Versions are never changed; the latest one is in force from set_at.
CREATE TABLE cargo_com.order_policy (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL CHECK (legal_entity_id = '' OR length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 version bigint NOT NULL CHECK (version > 0),
 max_orders_per_quote integer NOT NULL CHECK (max_orders_per_quote BETWEEN 1 AND 999),
 grace_days integer NOT NULL CHECK (grace_days BETWEEN 0 AND 30),
 set_by text NOT NULL CHECK (length(btrim(set_by)) > 0),
 set_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, version)
);
COMMENT ON COLUMN cargo_com.order_policy.legal_entity_id IS 'Empty for the tenant default.';

CREATE FUNCTION cargo_com.check_order_policy_version() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  prev cargo_com.order_policy;
BEGIN
  IF TG_OP <> 'INSERT' THEN
    RAISE EXCEPTION 'cargo_com: order policy versions are immutable' USING ERRCODE = 'restrict_violation';
  END IF;
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_com.order_policy:' || NEW.tenant_id || '/' || NEW.legal_entity_id, 0));
  SELECT * INTO prev FROM cargo_com.order_policy
    WHERE tenant_id = NEW.tenant_id AND legal_entity_id = NEW.legal_entity_id ORDER BY version DESC LIMIT 1;
  IF NEW.version <> coalesce(prev.version, 0) + 1 OR NEW.set_at < prev.set_at THEN
    RAISE EXCEPTION 'cargo_com: version % does not follow the order policy of %/%', NEW.version, NEW.tenant_id, NEW.legal_entity_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'order_policy_sequence';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER order_policy_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_com.order_policy
  FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_policy_version();

-- A new order froze the policy in force for its legal entity: the entity's latest version, else the tenant
-- default's, else the built-in policy. The quote is locked, and the orders it backs that are not cancelled
-- must stay below the policy's bound (C1). COM takes the same lock before it counts.
CREATE FUNCTION cargo_com.check_order_policy() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  p cargo_com.order_policy;
  live bigint;
BEGIN
  SELECT * INTO p FROM cargo_com.order_policy
    WHERE tenant_id = NEW.tenant_id AND legal_entity_id IN (NEW.legal_entity_id, '')
    ORDER BY (legal_entity_id = '') ASC, version DESC LIMIT 1;
  IF FOUND THEN
    IF (NEW.policy_scope, NEW.policy_legal_entity_id, NEW.policy_version, NEW.max_orders_per_quote, NEW.grace_days)
       IS DISTINCT FROM (CASE WHEN p.legal_entity_id = '' THEN 'TENANT' ELSE 'LEGAL_ENTITY' END, nullif(p.legal_entity_id, ''),
         p.version, p.max_orders_per_quote, p.grace_days) THEN
      RAISE EXCEPTION 'cargo_com: order %/% does not follow the order policy in force', NEW.tenant_id, NEW.order_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'customer_order_policy_in_force';
    END IF;
  ELSIF NEW.policy_scope <> 'BUILT_IN' THEN
    RAISE EXCEPTION 'cargo_com: order %/% names a policy that is not recorded', NEW.tenant_id, NEW.order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'customer_order_policy_in_force';
  END IF;
  PERFORM pg_advisory_xact_lock(hashtextextended('cargo_com.quote:' || NEW.tenant_id || '/' || NEW.quote_id, 0));
  SELECT count(*) INTO live FROM cargo_com.customer_order
    WHERE tenant_id = NEW.tenant_id AND quote_id = NEW.quote_id AND status <> 'CANCELLED';
  IF live >= NEW.max_orders_per_quote THEN
    RAISE EXCEPTION 'cargo_com: quote %/% already backs % orders', NEW.tenant_id, NEW.quote_id, live
      USING ERRCODE = 'check_violation', CONSTRAINT = 'customer_order_orders_per_quote';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER customer_order_policy BEFORE INSERT ON cargo_com.customer_order
  FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_policy();

-- C10: lines added after the order was recorded carry who added them, when, why and when COM recorded it, and
-- the number of the change; the lines recorded with the order carry none of it.
ALTER TABLE cargo_com.order_line
  ADD COLUMN added_by text CHECK (length(btrim(added_by)) > 0),
  ADD COLUMN added_at timestamptz,
  ADD COLUMN added_entered_at timestamptz,
  ADD COLUMN added_reason text CHECK (length(btrim(added_reason)) BETWEEN 1 AND 500),
  ADD COLUMN change_no integer CHECK (change_no > 0),
  ADD CONSTRAINT order_line_added CHECK ((added_at IS NULL) = (added_by IS NULL) AND (added_at IS NULL) = (added_entered_at IS NULL)
    AND (added_at IS NULL) = (added_reason IS NULL) AND (added_at IS NULL) = (change_no IS NULL));

-- Every later version of a line, as the customer corrected it.
CREATE TABLE cargo_com.order_line_amendment (
 tenant_id text NOT NULL,
 order_id text NOT NULL,
 line_no integer NOT NULL,
 line_version bigint NOT NULL CHECK (line_version > 1),
 description text NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 500),
 package_type text NOT NULL CHECK (package_type ~ '^[A-Z0-9]{2}$'),
 packs bigint NOT NULL CHECK (packs > 0),
 gross_weight_unit text NOT NULL CHECK (gross_weight_unit ~ '^[A-Z0-9]{1,3}$'),
 gross_weight bigint NOT NULL CHECK (gross_weight > 0),
 volume_unit text CHECK (volume_unit ~ '^[A-Z0-9]{1,3}$'),
 volume bigint CHECK (volume > 0),
 amended_by text NOT NULL CHECK (length(btrim(amended_by)) > 0),
 amended_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 change_no integer NOT NULL CHECK (change_no > 0),
 PRIMARY KEY (tenant_id, order_id, line_no, line_version),
 FOREIGN KEY (tenant_id, order_id, line_no) REFERENCES cargo_com.order_line (tenant_id, order_id, line_no),
 CONSTRAINT order_line_amendment_volume_declared CHECK ((volume_unit IS NULL) = (volume IS NULL))
);
CREATE TRIGGER order_line_amendment_immutable BEFORE UPDATE OR DELETE ON cargo_com.order_line_amendment
  FOR EACH ROW EXECUTE FUNCTION cargo_com.reject_row_change();

-- The current version of a line: its latest amendment, else the line as recorded or added.
CREATE FUNCTION cargo_com.current_order_line(p_tenant text, p_order text, p_line integer,
  OUT line_version bigint, OUT package_type text, OUT packs bigint, OUT gross_weight_unit text, OUT gross_weight bigint,
  OUT volume_unit text, OUT volume bigint)
LANGUAGE sql STABLE AS $fn$
  SELECT v.line_version, v.package_type, v.packs, v.gross_weight_unit, v.gross_weight, v.volume_unit, v.volume FROM (
    SELECT a.line_version, a.package_type, a.packs, a.gross_weight_unit, a.gross_weight, a.volume_unit, a.volume
      FROM cargo_com.order_line_amendment a WHERE a.tenant_id = p_tenant AND a.order_id = p_order AND a.line_no = p_line
    UNION ALL
    SELECT 1, l.package_type, l.packs, l.gross_weight_unit, l.gross_weight, l.volume_unit, l.volume
      FROM cargo_com.order_line l WHERE l.tenant_id = p_tenant AND l.order_id = p_order AND l.line_no = p_line) v
  ORDER BY v.line_version DESC LIMIT 1
$fn$;

-- The number of the next line change of an order, across added lines and amendments.
CREATE FUNCTION cargo_com.next_line_change(p_tenant text, p_order text) RETURNS integer LANGUAGE sql STABLE AS $fn$
  SELECT coalesce(max(c), 0) + 1 FROM (
    SELECT change_no AS c FROM cargo_com.order_line WHERE tenant_id = p_tenant AND order_id = p_order
    UNION ALL
    SELECT change_no FROM cargo_com.order_line_amendment WHERE tenant_id = p_tenant AND order_id = p_order) changes
$fn$;

-- A line recorded with the order belongs to an order still at revision 1. A line added later belongs to an
-- order that is not cancelled, is numbered after the order's lines and is the order's next change. The order
-- row is locked first, as for obligations.
CREATE FUNCTION cargo_com.check_order_line_insert() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  o cargo_com.customer_order;
BEGIN
  SELECT * INTO o FROM cargo_com.customer_order WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id FOR NO KEY UPDATE;
  IF NEW.added_at IS NULL THEN
    IF o.revision <> 1 OR cargo_com.next_line_change(NEW.tenant_id, NEW.order_id) <> 1 THEN
      RAISE EXCEPTION 'cargo_com: line %/%/% is not recorded with its order', NEW.tenant_id, NEW.order_id, NEW.line_no
        USING ERRCODE = 'check_violation', CONSTRAINT = 'order_line_change_sequence';
    END IF;
  ELSIF o.status = 'CANCELLED' OR NEW.added_at < o.recorded_at
     OR NEW.line_no <> (SELECT coalesce(max(line_no), 0) + 1 FROM cargo_com.order_line WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id)
     OR NEW.change_no <> cargo_com.next_line_change(NEW.tenant_id, NEW.order_id) THEN
    RAISE EXCEPTION 'cargo_com: line %/%/% cannot be added', NEW.tenant_id, NEW.order_id, NEW.line_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'order_line_change_sequence';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER order_line_insert_rules BEFORE INSERT ON cargo_com.order_line
  FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_line_insert();

-- An amendment is the next version of a line of an order that is not cancelled, and the order's next change.
-- While the line has active obligations it keeps its package type and units and does not fall below what the
-- obligations commit.
CREATE FUNCTION cargo_com.check_order_line_amendment() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  o cargo_com.customer_order;
  cur record;
  active_count bigint;
  active_packs bigint;
  active_weight bigint;
  active_volume bigint;
BEGIN
  SELECT * INTO o FROM cargo_com.customer_order WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id FOR NO KEY UPDATE;
  SELECT * INTO cur FROM cargo_com.current_order_line(NEW.tenant_id, NEW.order_id, NEW.line_no);
  SELECT count(*), coalesce(sum(f.packs), 0), coalesce(sum(f.gross_weight), 0), coalesce(sum(f.volume), 0)
    INTO active_count, active_packs, active_weight, active_volume
    FROM cargo_com.fulfillment_obligation f
    WHERE f.tenant_id = NEW.tenant_id AND f.order_id = NEW.order_id AND f.line_no = NEW.line_no AND f.status = 'ACTIVE'
      AND NOT EXISTS (SELECT 1 FROM cargo_com.fulfillment_obligation v
        WHERE v.tenant_id = f.tenant_id AND v.obligation_id = f.obligation_id AND v.obligation_version > f.obligation_version);
  IF o.status = 'CANCELLED' OR NEW.amended_at < o.recorded_at OR cur.line_version IS NULL OR NEW.line_version <> cur.line_version + 1
     OR NEW.change_no <> cargo_com.next_line_change(NEW.tenant_id, NEW.order_id)
     OR (active_count > 0 AND (NEW.package_type <> cur.package_type OR NEW.gross_weight_unit <> cur.gross_weight_unit
         OR NEW.volume_unit IS DISTINCT FROM cur.volume_unit OR NEW.packs < active_packs OR NEW.gross_weight < active_weight
         OR coalesce(NEW.volume, 0) < active_volume)) THEN
    RAISE EXCEPTION 'cargo_com: version % does not correct line %/%/%', NEW.line_version, NEW.tenant_id, NEW.order_id, NEW.line_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'order_line_amendment_rules';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER order_line_amendment_rules BEFORE INSERT ON cargo_com.order_line_amendment
  FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_line_amendment();

-- Obligations fit the current version of their line (as of V0.3 otherwise unchanged).
CREATE OR REPLACE FUNCTION cargo_com.check_obligation_within_line() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  order_status text;
  ln record;
  prev cargo_com.fulfillment_obligation;
  active_packs bigint;
  active_weight bigint;
  active_volume bigint;
BEGIN
  SELECT status INTO order_status FROM cargo_com.customer_order
    WHERE tenant_id = NEW.tenant_id AND order_id = NEW.order_id FOR NO KEY UPDATE;
  IF NEW.obligation_version = 1 THEN
    SELECT * INTO ln FROM cargo_com.current_order_line(NEW.tenant_id, NEW.order_id, NEW.line_no);
    SELECT coalesce(sum(f.packs), 0), coalesce(sum(f.gross_weight), 0), coalesce(sum(f.volume), 0)
      INTO active_packs, active_weight, active_volume
      FROM cargo_com.fulfillment_obligation f
      WHERE f.tenant_id = NEW.tenant_id AND f.order_id = NEW.order_id AND f.line_no = NEW.line_no AND f.status = 'ACTIVE'
        AND NOT EXISTS (SELECT 1 FROM cargo_com.fulfillment_obligation v
          WHERE v.tenant_id = f.tenant_id AND v.obligation_id = f.obligation_id AND v.obligation_version > f.obligation_version);
    IF NEW.status <> 'ACTIVE' OR order_status = 'CANCELLED' OR ln.line_version IS NULL
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

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['order_policy','order_line_amendment'] LOOP
    EXECUTE format('ALTER TABLE cargo_com.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_com.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_com.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
