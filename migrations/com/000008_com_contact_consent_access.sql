-- IDP-Cargo COM V0.8 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Privacy assessment V1.2 with decisions P4, P6 and P7 of 2026-11-10: a contact frozen on an order keeps the
-- notice and consent recorded for the person (carried from ORG with its revision, or given with the contact
-- on the order), and every read of an order's contacts is logged with its purpose. A read of restricted
-- contacts, those of a closed or cancelled order, is logged as such and only for a customs audit, a legal
-- claim or the person's own request. The log is kept three years and never rewritten.

ALTER TABLE cargo_com.order_party_contact
  ADD COLUMN consent_notice_version text CHECK (consent_notice_version ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
  ADD COLUMN consent_provide_to_others boolean,
  ADD COLUMN consent_cross_border boolean,
  ADD COLUMN consent_basis text CHECK (consent_basis IN ('CUSTOMER_UNDERTAKING','SUBJECT_CONFIRMED')),
  ADD COLUMN consent_evidence_ref text CHECK (char_length(consent_evidence_ref) BETWEEN 1 AND 200 AND consent_evidence_ref = btrim(consent_evidence_ref)),
  ADD COLUMN consent_org_revision bigint CHECK (consent_org_revision > 0),
  ADD CONSTRAINT order_party_contact_consent CHECK (CASE WHEN consent_notice_version IS NULL
    THEN consent_provide_to_others IS NULL AND consent_cross_border IS NULL AND consent_basis IS NULL AND consent_evidence_ref IS NULL
      AND consent_org_revision IS NULL
    ELSE consent_provide_to_others IS NOT NULL AND consent_cross_border IS NOT NULL AND consent_basis IS NOT NULL
      AND consent_evidence_ref IS NOT NULL AND (source = 'ORG') = (consent_org_revision IS NOT NULL) END);

-- The one change to a frozen contact is still its erasure, which leaves the consent as it was.
CREATE OR REPLACE FUNCTION cargo_com.erase_order_contact_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.erased OR NOT NEW.erased
     OR NEW.name IS NOT NULL OR NEW.phone IS NOT NULL OR NEW.mobile IS NOT NULL OR NEW.email IS NOT NULL
     OR (NEW.tenant_id, NEW.order_id, NEW.role, NEW.version, NEW.source, NEW.org_contact_id, NEW.org_contact_version, NEW.overridden_by, NEW.override_reason,
         NEW.consent_notice_version, NEW.consent_provide_to_others, NEW.consent_cross_border, NEW.consent_basis, NEW.consent_evidence_ref, NEW.consent_org_revision)
        IS DISTINCT FROM (OLD.tenant_id, OLD.order_id, OLD.role, OLD.version, OLD.source, OLD.org_contact_id, OLD.org_contact_version, OLD.overridden_by, OLD.override_reason,
         OLD.consent_notice_version, OLD.consent_provide_to_others, OLD.consent_cross_border, OLD.consent_basis, OLD.consent_evidence_ref, OLD.consent_org_revision)
     OR NOT EXISTS (SELECT 1 FROM cargo_com.order_contact_erasure e WHERE e.tenant_id = OLD.tenant_id AND e.order_id = OLD.order_id) THEN
    RAISE EXCEPTION 'cargo_com: the contact of % version % on order %/% can only be erased, with the record of the order''s erasure',
      OLD.role, OLD.version, OLD.tenant_id, OLD.order_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- Every read of an order's contacts: who, through which action, whether they were restricted, for what
-- purpose, how many contact versions with personal data it returned, and when, by the database's clock.
CREATE TABLE cargo_com.order_contact_access (
  access_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id text NOT NULL,
  order_id text NOT NULL,
  actor_id text NOT NULL CHECK (length(btrim(actor_id)) > 0),
  action text NOT NULL CHECK (action IN ('ReadOrderContacts','ReadRestrictedOrderContacts')),
  restricted boolean NOT NULL,
  purpose text NOT NULL CHECK (purpose IN ('DOCUMENTS','BOOKING','OPERATIONS','ACCOUNTS','ORDER_PARTY','CUSTOMS_AUDIT','LEGAL_CLAIM','DATA_SUBJECT_REQUEST')),
  contacts integer NOT NULL CHECK (contacts >= 0),
  recorded_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT order_contact_access_restricted CHECK (restricted = (action = 'ReadRestrictedOrderContacts')
    AND (NOT restricted OR purpose IN ('CUSTOMS_AUDIT','LEGAL_CLAIM','DATA_SUBJECT_REQUEST')))
);
CREATE INDEX order_contact_access_by_time ON cargo_com.order_contact_access (tenant_id, recorded_at DESC);
CREATE INDEX order_contact_access_by_order ON cargo_com.order_contact_access (tenant_id, order_id, recorded_at DESC);

-- A read is timed by the database and logged as restricted exactly when the order's contacts are retained.
CREATE FUNCTION cargo_com.stamp_contact_access() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  NEW.recorded_at := now();
  IF NEW.restricted IS DISTINCT FROM EXISTS (SELECT 1 FROM cargo_com.order_contact_retention r
                                             WHERE r.tenant_id = NEW.tenant_id AND r.order_id = NEW.order_id) THEN
    RAISE EXCEPTION 'cargo_com: a read of the contacts of order %/% is restricted exactly when the order is closed or cancelled', NEW.tenant_id, NEW.order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'order_contact_access_restricted';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER order_contact_access_stamped BEFORE INSERT ON cargo_com.order_contact_access
  FOR EACH ROW EXECUTE FUNCTION cargo_com.stamp_contact_access();
CREATE FUNCTION cargo_com.keep_contact_access() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'UPDATE' OR OLD.recorded_at > now() - interval '3 years' THEN
    RAISE EXCEPTION 'cargo_com: the read of contacts % is kept three years and never rewritten', OLD.access_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN OLD;
END $fn$;
CREATE TRIGGER order_contact_access_kept BEFORE UPDATE OR DELETE ON cargo_com.order_contact_access
  FOR EACH ROW EXECUTE FUNCTION cargo_com.keep_contact_access();

ALTER TABLE cargo_com.order_contact_access ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_com.order_contact_access FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_com.order_contact_access USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
