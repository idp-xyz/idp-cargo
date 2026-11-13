-- IDP-Cargo COM / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Orders on AGREEMENT quotes (RATE V0.5 part 3b; C1 option 3, N1-RATE OD-5 A): an order records the kind of the quote
-- it relies on. An AGREEMENT order relies on QuoteSnapshotV3, has no quote total, and is not counted against the order
-- policy's orders per quote; the agreement's validity (and the policy's grace) bounds when it is recorded. ONE_OFF
-- orders are unchanged.

ALTER TABLE cargo_com.customer_order ADD COLUMN quote_kind text NOT NULL DEFAULT 'ONE_OFF' CHECK (quote_kind IN ('ONE_OFF','AGREEMENT'));
ALTER TABLE cargo_com.customer_order ALTER COLUMN quote_total_minor DROP NOT NULL;
ALTER TABLE cargo_com.customer_order DROP CONSTRAINT customer_order_accepted_terms_contract_check;
ALTER TABLE cargo_com.customer_order ADD CONSTRAINT customer_order_quote_kind CHECK (CASE quote_kind
  WHEN 'AGREEMENT' THEN accepted_terms_contract = 'QuoteSnapshot.v3' AND quote_total_minor IS NULL
  ELSE accepted_terms_contract IN ('QuoteSnapshot.v1','QuoteSnapshot.v2') AND quote_total_minor IS NOT NULL END) NOT VALID;
ALTER TABLE cargo_com.customer_order VALIDATE CONSTRAINT customer_order_quote_kind;
ALTER TABLE cargo_com.customer_order DROP CONSTRAINT customer_order_quote_v1;
ALTER TABLE cargo_com.customer_order ADD CONSTRAINT customer_order_quote_v1 CHECK (accepted_terms_contract <> 'QuoteSnapshot.v1'
  OR (quote_recipient_role = 'CUSTOMER' AND incoterms_source IS DISTINCT FROM 'QUOTE' AND service_level_source IS DISTINCT FROM 'QUOTE')) NOT VALID;
ALTER TABLE cargo_com.customer_order VALIDATE CONSTRAINT customer_order_quote_v1;

-- As com/000004, but only orders on ONE_OFF quotes are counted against the policy's orders per quote.
CREATE OR REPLACE FUNCTION cargo_com.check_order_policy() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
  IF EXISTS (SELECT 1 FROM cargo_com.customer_order WHERE tenant_id = NEW.tenant_id AND quote_id = NEW.quote_id AND quote_kind <> NEW.quote_kind) THEN
    RAISE EXCEPTION 'cargo_com: quote %/% backs orders of another kind', NEW.tenant_id, NEW.quote_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'customer_order_quote_one_kind';
  END IF;
  IF NEW.quote_kind = 'ONE_OFF' THEN
    SELECT count(*) INTO live FROM cargo_com.customer_order
      WHERE tenant_id = NEW.tenant_id AND quote_id = NEW.quote_id AND status <> 'CANCELLED';
    IF live >= NEW.max_orders_per_quote THEN
      RAISE EXCEPTION 'cargo_com: quote %/% already backs % orders', NEW.tenant_id, NEW.quote_id, live
        USING ERRCODE = 'check_violation', CONSTRAINT = 'customer_order_orders_per_quote';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;
