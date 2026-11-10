-- IDP-Cargo ORG V0.5 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Privacy assessment V1.2 with decisions P4, P6 and P7 of 2026-11-10: a contact's notice and consent are
-- recorded as revisions of the party's address book, and every read of contacts is logged with its purpose.
-- The log is kept three years and never rewritten; it names contacts, never their details.

ALTER TABLE cargo_org.party_address_book_change
  DROP CONSTRAINT party_address_book_change_action_check,
  ADD CONSTRAINT party_address_book_change_action_check CHECK (action IN ('ADD_ADDRESS','CHANGE_ADDRESS','DEACTIVATE_ADDRESS',
    'SET_DEFAULT_ADDRESS','ADD_CONTACT','CHANGE_CONTACT','DEACTIVATE_CONTACT','ERASE_CONTACT','SET_DEFAULT_CONTACT','RECORD_CONTACT_CONSENT'));

-- The notice a person was told of and the separate consents the person gave: to the contact being provided
-- to others (PIPL article 23) and abroad (article 39). Each row is one address book revision; the latest is
-- current. It holds no personal data and stays when the contact is erased.
CREATE TABLE cargo_org.party_contact_consent (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  contact_id text NOT NULL,
  revision bigint NOT NULL CHECK (revision > 0),
  notice_version text NOT NULL CHECK (notice_version ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
  provide_to_others boolean NOT NULL,
  cross_border boolean NOT NULL,
  basis text NOT NULL CHECK (basis IN ('CUSTOMER_UNDERTAKING','SUBJECT_CONFIRMED')),
  evidence_ref text NOT NULL CHECK (char_length(evidence_ref) BETWEEN 1 AND 200 AND evidence_ref = btrim(evidence_ref)),
  PRIMARY KEY (tenant_id, party_id, contact_id, revision),
  FOREIGN KEY (tenant_id, party_id, contact_id) REFERENCES cargo_org.party_contact (tenant_id, party_id, contact_id)
);
CREATE TRIGGER party_contact_consent_immutable BEFORE UPDATE OR DELETE ON cargo_org.party_contact_consent
  FOR EACH ROW EXECUTE FUNCTION cargo_org.refuse_address_book_rewrite();

-- At commit, every consent row has its change record (the same revision, RECORD_CONTACT_CONSENT, the
-- contact), every such change record has its row, and no consent follows the contact's erasure.
CREATE FUNCTION cargo_org.check_contact_consent() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  t text := NEW.tenant_id;
  p text := NEW.party_id;
  bad text;
BEGIN
  SELECT 'consent of contact ' || k.contact_id || ' at revision ' || k.revision || ' has no change record' INTO bad
    FROM cargo_org.party_contact_consent k
    WHERE k.tenant_id = t AND k.party_id = p AND NOT EXISTS (SELECT 1 FROM cargo_org.party_address_book_change c
      WHERE c.tenant_id = t AND c.party_id = p AND c.revision = k.revision AND c.action = 'RECORD_CONTACT_CONSENT' AND c.entry_id = k.contact_id)
    LIMIT 1;
  IF bad IS NULL THEN
    SELECT 'revision ' || c.revision || ' records a consent without its row' INTO bad
      FROM cargo_org.party_address_book_change c
      WHERE c.tenant_id = t AND c.party_id = p AND c.action = 'RECORD_CONTACT_CONSENT' AND NOT EXISTS (SELECT 1 FROM cargo_org.party_contact_consent k
        WHERE k.tenant_id = t AND k.party_id = p AND k.revision = c.revision AND k.contact_id = c.entry_id)
      LIMIT 1;
  END IF;
  IF bad IS NULL THEN
    SELECT 'contact ' || k.contact_id || ' was erased before its consent at revision ' || k.revision INTO bad
      FROM cargo_org.party_contact_consent k
      JOIN cargo_org.party_address_book_change e ON e.tenant_id = t AND e.party_id = p AND e.action = 'ERASE_CONTACT'
        AND e.entry_id = k.contact_id AND e.revision < k.revision
      WHERE k.tenant_id = t AND k.party_id = p
      LIMIT 1;
  END IF;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_org: address book of party %/%: %', t, p, bad
      USING ERRCODE = 'check_violation', CONSTRAINT = 'party_contact_consent_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER party_contact_consent_consistent AFTER INSERT ON cargo_org.party_contact_consent
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_contact_consent();
CREATE CONSTRAINT TRIGGER party_address_book_change_consent AFTER INSERT ON cargo_org.party_address_book_change
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_org.check_contact_consent();

-- Every read of a party's contacts: who, through which action, for what purpose, which contacts with
-- personal data it returned, and when, by the database's clock.
CREATE TABLE cargo_org.party_contact_access (
  access_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  actor_id text NOT NULL CHECK (length(btrim(actor_id)) > 0),
  action text NOT NULL CHECK (action IN ('ReadPartyContacts')),
  purpose text NOT NULL CHECK (purpose IN ('DOCUMENTS','BOOKING','OPERATIONS','ACCOUNTS','ORDER_PARTY','CUSTOMS_AUDIT','LEGAL_CLAIM','DATA_SUBJECT_REQUEST')),
  contact_ids text[] NOT NULL,
  recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX party_contact_access_by_time ON cargo_org.party_contact_access (tenant_id, recorded_at DESC);
CREATE INDEX party_contact_access_by_party ON cargo_org.party_contact_access (tenant_id, party_id, recorded_at DESC);

-- The log is timed by the database; it is never updated, and a row is deleted only once it is three years old.
CREATE FUNCTION cargo_org.stamp_contact_access() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  NEW.recorded_at := now();
  RETURN NEW;
END $fn$;
CREATE TRIGGER party_contact_access_stamped BEFORE INSERT ON cargo_org.party_contact_access
  FOR EACH ROW EXECUTE FUNCTION cargo_org.stamp_contact_access();
CREATE FUNCTION cargo_org.keep_contact_access() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'UPDATE' OR OLD.recorded_at > now() - interval '3 years' THEN
    RAISE EXCEPTION 'cargo_org: the read of contacts % is kept three years and never rewritten', OLD.access_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN OLD;
END $fn$;
CREATE TRIGGER party_contact_access_kept BEFORE UPDATE OR DELETE ON cargo_org.party_contact_access
  FOR EACH ROW EXECUTE FUNCTION cargo_org.keep_contact_access();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['party_contact_consent','party_contact_access'] LOOP
    EXECUTE format('ALTER TABLE cargo_org.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_org.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format('CREATE POLICY tenant_isolation ON cargo_org.%I USING (tenant_id = current_setting(''app.tenant_id'', true)) '
      'WITH CHECK (tenant_id = current_setting(''app.tenant_id'', true))', tbl);
  END LOOP;
END $rls$;
