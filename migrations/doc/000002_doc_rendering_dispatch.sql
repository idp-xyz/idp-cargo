-- IDP-Cargo DOC V0.2 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- DocumentDispatch with the P5 records (DOC candidate 24, decisions N1-DOC OD-4 A, OD-5 A; privacy decision P5): every
-- copy of a document version that leaves the tenant is a dispatch to one recipient over one channel, with the copy's
-- SHA-256 and template, the version's status the copy shows, and one row per contact and role with its decision. No
-- row holds a name, phone, e-mail or consent evidence: contacts are named by key (kernel grammar: ORG:<party>:<contact id>,
-- COM:<order>:<role>:<party version> or SHA:<digest>). A dispatch changes only by its outcomes, SENT and FAILED final,
-- UNKNOWN followed by SENT or FAILED. No foreign key leaves cargo_doc.
CREATE TABLE cargo_doc.document_dispatch (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 dispatch_id text NOT NULL CHECK (length(btrim(dispatch_id)) BETWEEN 1 AND 128),
 document_id text NOT NULL,
 version_no integer NOT NULL,
 kind text NOT NULL CHECK (kind IN ('HAWB','MAWB')),
 version_status text NOT NULL CHECK (version_status IN ('DRAFT','APPROVED','ISSUED')),
 recipient_party_id text NOT NULL CHECK (length(btrim(recipient_party_id)) BETWEEN 1 AND 128),
 recipient_country char(2) NOT NULL CHECK (recipient_country ~ '^[A-Z]{2}$'),
 recipient_role text NOT NULL CHECK (recipient_role IN ('ORDERING_PARTY','SHIPPER','CONSIGNEE','NOTIFY_PARTY','DESTINATION_AGENT','CARRIER',
   'CUSTOMS_BROKER')),
 ordering_party_id text CHECK (length(btrim(ordering_party_id)) BETWEEN 1 AND 128),
 channel text NOT NULL CHECK (channel IN ('EMAIL_PDF','PRINT_HANDOVER','PORTAL')),
 copy_sha256 char(64) NOT NULL CHECK (copy_sha256 ~ '^[0-9a-f]{64}$'),
 template text NOT NULL CHECK (length(btrim(template)) BETWEEN 1 AND 64),
 register_year integer NOT NULL CHECK (register_year BETWEEN 2000 AND 9999),
 register_state text NOT NULL CHECK (register_state IN ('OK','WARNING','BLOCKED')),
 state text NOT NULL CHECK (state IN ('PREPARED','SENT','FAILED','UNKNOWN')),
 prepared_by text NOT NULL CHECK (length(btrim(prepared_by)) > 0),
 prepared_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, dispatch_id),
 FOREIGN KEY (tenant_id, document_id, version_no) REFERENCES cargo_doc.document_version (tenant_id, document_id, version_no),
 -- a draft goes to the ordering party only, as COM reported it
 CONSTRAINT document_dispatch_draft CHECK (version_status = 'ISSUED' OR recipient_role = 'ORDERING_PARTY'),
 CONSTRAINT document_dispatch_ordering_party CHECK (recipient_role <> 'ORDERING_PARTY' OR (ordering_party_id IS NOT NULL AND recipient_party_id = ordering_party_id))
);
CREATE INDEX document_dispatch_by_document ON cargo_doc.document_dispatch (tenant_id, document_id);
CREATE INDEX document_dispatch_by_time ON cargo_doc.document_dispatch (tenant_id, prepared_at);

CREATE TABLE cargo_doc.dispatch_contact (
 tenant_id text NOT NULL,
 dispatch_id text NOT NULL,
 contact_no integer NOT NULL CHECK (contact_no > 0),
 contact_key text NOT NULL CHECK (contact_key ~ '^(ORG:[A-Za-z0-9._-]{1,128}:[A-Za-z0-9._-]{1,128}|COM:[A-Za-z0-9._-]{1,128}:(SHIPPER|CONSIGNEE|NOTIFY_PARTY):[1-9][0-9]{0,9}|SHA:[0-9a-f]{64})$'),
 party_role text NOT NULL CHECK (party_role IN ('SHIPPER','CONSIGNEE','NOTIFY_PARTY')),
 party_id text CHECK (length(btrim(party_id)) BETWEEN 1 AND 128),
 order_id text NOT NULL CHECK (length(btrim(order_id)) BETWEEN 1 AND 128),
 source text NOT NULL CHECK (source IN ('ORG','ORDER')),
 classification text NOT NULL CHECK (classification IN ('NOT_PROVIDED','PROVISION','CROSS_BORDER')),
 legal_path text NOT NULL CHECK (legal_path IN ('NONE','CONSENT','CONSENT_WITH_THRESHOLD_EXEMPTION','STANDARD_CONTRACT','CERTIFICATION')),
 status text NOT NULL CHECK (status IN ('INCLUDED','OMITTED')),
 omit_reason text CHECK (omit_reason IN ('NO_CONSENT_RECORDED','NO_CONSENT_TO_PROVIDE','NO_CONSENT_TO_TRANSFER','ERASED')),
 consent_notice_version text CHECK (length(btrim(consent_notice_version)) BETWEEN 1 AND 64),
 consent_provide_to_others boolean,
 consent_cross_border boolean,
 consent_basis text CHECK (length(btrim(consent_basis)) BETWEEN 1 AND 64),
 consent_org_revision bigint CHECK (consent_org_revision >= 0),
 PRIMARY KEY (tenant_id, dispatch_id, contact_no),
 UNIQUE (tenant_id, dispatch_id, contact_key, party_role),
 FOREIGN KEY (tenant_id, dispatch_id) REFERENCES cargo_doc.document_dispatch (tenant_id, dispatch_id),
 CONSTRAINT dispatch_contact_omission CHECK ((status = 'OMITTED') = (omit_reason IS NOT NULL)),
 CONSTRAINT dispatch_contact_consent CHECK ((consent_provide_to_others IS NULL) = (consent_cross_border IS NULL)),
 -- the path a contact goes by follows its classification and the consent recorded
 CONSTRAINT dispatch_contact_path CHECK (CASE
   WHEN status = 'OMITTED' OR classification = 'NOT_PROVIDED' THEN legal_path = 'NONE'
   WHEN classification = 'PROVISION' THEN legal_path = 'CONSENT' AND consent_provide_to_others IS TRUE
   ELSE legal_path IN ('CONSENT_WITH_THRESHOLD_EXEMPTION','STANDARD_CONTRACT','CERTIFICATION') AND consent_provide_to_others IS TRUE
     AND consent_cross_border IS TRUE
   END IS TRUE)
);

CREATE TABLE cargo_doc.dispatch_outcome (
 tenant_id text NOT NULL,
 dispatch_id text NOT NULL,
 outcome_no integer NOT NULL CHECK (outcome_no > 0),
 outcome text NOT NULL CHECK (outcome IN ('SENT','FAILED','UNKNOWN')),
 reference text CHECK (length(btrim(reference)) BETWEEN 1 AND 500),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, dispatch_id, outcome_no),
 FOREIGN KEY (tenant_id, dispatch_id) REFERENCES cargo_doc.document_dispatch (tenant_id, dispatch_id),
 CONSTRAINT dispatch_outcome_reference CHECK (outcome = 'UNKNOWN' OR reference IS NOT NULL)
);

ALTER TABLE cargo_doc.command_receipt DROP CONSTRAINT command_receipt_result_type_check;
ALTER TABLE cargo_doc.command_receipt ADD CONSTRAINT command_receipt_result_type_check
  CHECK (result_type IN ('TransportDocument','IssuanceAuthority','DocumentDispatch'));

-- A dispatch changes only its state, revision and updated_at, and only while PREPARED or UNKNOWN.
CREATE FUNCTION cargo_doc.reject_dispatch_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.state NOT IN ('PREPARED','UNKNOWN') OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_doc: dispatch %/% changes only by its outcomes, until sent or failed', OLD.tenant_id, OLD.dispatch_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER document_dispatch_rules BEFORE UPDATE OR DELETE ON cargo_doc.document_dispatch
  FOR EACH ROW EXECUTE FUNCTION cargo_doc.reject_dispatch_change();
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['dispatch_contact','dispatch_outcome'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_doc.%I FOR EACH ROW EXECUTE FUNCTION cargo_doc.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- At commit, for every dispatch a transaction writes: outcomes numbered from 1 without gaps, each but the last
-- UNKNOWN; the state is the last outcome, or PREPARED; the kind is the document's; an issued copy shows a version
-- that was issued; every contact is classified for the recipient (NOT_PROVIDED only for the ordering party getting
-- back a contact it gave on the order or its own; else PROVISION in mainland China and CROSS_BORDER abroad) and one
-- person is left off for every role or for none. The row is read as it stands at commit.
CREATE FUNCTION cargo_doc.check_dispatch_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  x cargo_doc.document_dispatch;
  outcomes integer;
  last_no integer;
  last_outcome text;
  early_final boolean;
  ok boolean;
BEGIN
  SELECT * INTO x FROM cargo_doc.document_dispatch WHERE tenant_id = NEW.tenant_id AND dispatch_id = NEW.dispatch_id;
  SELECT count(*), coalesce(max(outcome_no), 0) INTO outcomes, last_no FROM cargo_doc.dispatch_outcome o
    WHERE o.tenant_id = x.tenant_id AND o.dispatch_id = x.dispatch_id;
  SELECT o.outcome INTO last_outcome FROM cargo_doc.dispatch_outcome o
    WHERE o.tenant_id = x.tenant_id AND o.dispatch_id = x.dispatch_id AND o.outcome_no = last_no;
  SELECT EXISTS (SELECT 1 FROM cargo_doc.dispatch_outcome o WHERE o.tenant_id = x.tenant_id AND o.dispatch_id = x.dispatch_id
    AND o.outcome_no < last_no AND o.outcome <> 'UNKNOWN') INTO early_final;
  ok := outcomes = last_no AND NOT early_final AND x.state = coalesce(last_outcome, 'PREPARED')
    AND EXISTS (SELECT 1 FROM cargo_doc.transport_document d WHERE d.tenant_id = x.tenant_id AND d.document_id = x.document_id AND d.kind = x.kind)
    AND (x.version_status <> 'ISSUED' OR EXISTS (SELECT 1 FROM cargo_doc.document_version_issue i WHERE i.tenant_id = x.tenant_id
      AND i.document_id = x.document_id AND i.version_no = x.version_no))
    AND NOT EXISTS (SELECT 1 FROM cargo_doc.dispatch_contact c WHERE c.tenant_id = x.tenant_id AND c.dispatch_id = x.dispatch_id
      AND c.classification <> CASE
        WHEN x.recipient_party_id = x.ordering_party_id AND (c.source = 'ORDER' OR c.party_id = x.ordering_party_id) THEN 'NOT_PROVIDED'
        WHEN x.recipient_country = 'CN' THEN 'PROVISION' ELSE 'CROSS_BORDER' END)
    AND NOT EXISTS (SELECT 1 FROM cargo_doc.dispatch_contact c JOIN cargo_doc.dispatch_contact o ON o.tenant_id = c.tenant_id
      AND o.dispatch_id = c.dispatch_id AND o.contact_key = c.contact_key WHERE c.tenant_id = x.tenant_id AND c.dispatch_id = x.dispatch_id
      AND c.status <> o.status);
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_doc: dispatch %/% is inconsistent at commit (state %)', x.tenant_id, x.dispatch_id, x.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'document_dispatch_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER document_dispatch_consistent AFTER INSERT OR UPDATE ON cargo_doc.document_dispatch
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_doc.check_dispatch_at_commit();

-- At commit, a dispatch's contacts are written by the transaction that prepares it, and its outcomes by one that
-- writes the dispatch row: nothing is added to a dispatch behind its back.
CREATE FUNCTION cargo_doc.check_written_with_dispatch() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_doc.document_dispatch x WHERE x.tenant_id = NEW.tenant_id AND x.dispatch_id = NEW.dispatch_id
       AND x.xmin = pg_current_xact_id()::xid AND (TG_TABLE_NAME <> 'dispatch_contact' OR x.revision = 1)) THEN
    RAISE EXCEPTION 'cargo_doc: % of dispatch %/% is written with the dispatch', TG_TABLE_NAME, NEW.tenant_id, NEW.dispatch_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dispatch_records_with_dispatch';
  END IF;
  RETURN NULL;
END $fn$;
DO $with$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['dispatch_contact','dispatch_outcome'] LOOP
    EXECUTE format('CREATE CONSTRAINT TRIGGER %I AFTER INSERT ON cargo_doc.%I DEFERRABLE INITIALLY DEFERRED FOR EACH ROW '
      'EXECUTE FUNCTION cargo_doc.check_written_with_dispatch()', tbl || '_with_dispatch', tbl);
  END LOOP;
END $with$;

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['document_dispatch','dispatch_contact','dispatch_outcome'] LOOP
    EXECUTE format('ALTER TABLE cargo_doc.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_doc.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_doc.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
