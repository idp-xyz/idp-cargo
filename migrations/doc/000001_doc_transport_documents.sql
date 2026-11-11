-- IDP-Cargo DOC V0.1 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Transport documents of the N1 air line (DOC candidate 24, decisions N1-DOC OD-1, OD-2, OD-3, OD-7, OD-10): the
-- HAWB of a FUL shipment and the MAWB of a consolidation, built from AIR's AirDocFacts v1. A document is a series
-- of immutable versions, each kept as the JSON written with its SHA-256; approvals, issues and discards are rows
-- beside the versions. An issued number belongs to its shipment (HAWB) or movement (MAWB) under its legal entity for
-- ever. Issuance authorities (OD-10 A) change only by being withdrawn, once. No foreign key leaves cargo_doc.
CREATE SCHEMA IF NOT EXISTS cargo_doc;

CREATE TABLE cargo_doc.transport_document (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 document_id text NOT NULL CHECK (length(btrim(document_id)) BETWEEN 1 AND 128),
 kind text NOT NULL CHECK (kind IN ('HAWB','MAWB')),
 movement_id text NOT NULL CHECK (length(btrim(movement_id)) BETWEEN 1 AND 128),
 consolidation_id text NOT NULL CHECK (length(btrim(consolidation_id)) BETWEEN 1 AND 128),
 shipment_id text CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 number text NOT NULL CHECK (length(btrim(number)) BETWEEN 1 AND 32),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 state text NOT NULL CHECK (state IN ('DRAFT','APPROVED','ISSUED','AMENDING','CANCELLED')),
 cancel_reason text CHECK (length(btrim(cancel_reason)) BETWEEN 1 AND 500),
 cancelled_by text CHECK (length(btrim(cancelled_by)) > 0),
 cancelled_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, document_id),
 CONSTRAINT transport_document_house CHECK ((kind = 'HAWB') = (shipment_id IS NOT NULL)),
 CONSTRAINT transport_document_cancellation CHECK ((state = 'CANCELLED') = (cancelled_at IS NOT NULL)
   AND (cancelled_by IS NULL) = (cancelled_at IS NULL) AND (cancel_reason IS NULL) = (cancelled_at IS NULL))
);
-- One document of a kind that is not cancelled per shipment (HAWB) and per consolidation (MAWB).
CREATE UNIQUE INDEX transport_document_one_house ON cargo_doc.transport_document (tenant_id, shipment_id)
  WHERE kind = 'HAWB' AND state <> 'CANCELLED';
CREATE UNIQUE INDEX transport_document_one_master ON cargo_doc.transport_document (tenant_id, consolidation_id)
  WHERE kind = 'MAWB' AND state <> 'CANCELLED';
CREATE INDEX transport_document_by_consolidation ON cargo_doc.transport_document (tenant_id, consolidation_id);

CREATE TABLE cargo_doc.document_version (
 tenant_id text NOT NULL,
 document_id text NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 content text NOT NULL,
 content_sha256 char(64) NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
 source_movement_id text NOT NULL,
 source_version integer NOT NULL CHECK (source_version > 0),
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 drafted_by text NOT NULL CHECK (length(btrim(drafted_by)) > 0),
 drafted_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, document_id, version_no),
 FOREIGN KEY (tenant_id, document_id) REFERENCES cargo_doc.transport_document (tenant_id, document_id),
 CONSTRAINT document_version_content CHECK (content_sha256 = encode(sha256(convert_to(content, 'UTF8')), 'hex')
   AND jsonb_typeof(content::jsonb) = 'object' AND (content::jsonb)->>'document_id' = document_id
   AND ((content::jsonb)->'source'->>'version')::int = source_version AND (content::jsonb)->'source'->>'movement_id' = source_movement_id),
 CONSTRAINT document_version_reason CHECK (version_no = 1 OR reason IS NOT NULL)
);
CREATE TABLE cargo_doc.document_version_approval (
 tenant_id text NOT NULL,
 document_id text NOT NULL,
 version_no integer NOT NULL,
 approved_by text NOT NULL CHECK (length(btrim(approved_by)) > 0),
 approved_at timestamptz NOT NULL,
 self_approved boolean NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, document_id, version_no),
 FOREIGN KEY (tenant_id, document_id, version_no) REFERENCES cargo_doc.document_version (tenant_id, document_id, version_no)
);
CREATE TABLE cargo_doc.document_version_issue (
 tenant_id text NOT NULL,
 document_id text NOT NULL,
 version_no integer NOT NULL,
 issued_by text NOT NULL CHECK (length(btrim(issued_by)) > 0),
 issued_at timestamptz NOT NULL,
 authority_id text NOT NULL CHECK (length(btrim(authority_id)) BETWEEN 1 AND 128),
 authority_version bigint NOT NULL CHECK (authority_version > 0),
 credit_basis text NOT NULL CHECK (credit_basis IN ('FIN_OK','OVERRIDE','NOT_REQUIRED')),
 credit_decision_id text,
 credit_override_by text,
 credit_override_reason text CHECK (length(btrim(credit_override_reason)) BETWEEN 1 AND 500),
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, document_id, version_no),
 FOREIGN KEY (tenant_id, document_id, version_no) REFERENCES cargo_doc.document_version (tenant_id, document_id, version_no),
 CONSTRAINT document_version_issue_credit CHECK ((credit_basis = 'OVERRIDE') = (credit_override_reason IS NOT NULL)
   AND (credit_override_by IS NULL) = (credit_override_reason IS NULL))
);
CREATE TABLE cargo_doc.document_version_discard (
 tenant_id text NOT NULL,
 document_id text NOT NULL,
 version_no integer NOT NULL,
 discarded_by text NOT NULL CHECK (length(btrim(discarded_by)) > 0),
 discarded_at timestamptz NOT NULL,
 reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, document_id, version_no),
 FOREIGN KEY (tenant_id, document_id, version_no) REFERENCES cargo_doc.document_version (tenant_id, document_id, version_no)
);
-- An issued number belongs to the shipment (HAWB) or movement (MAWB) it was first issued for, under its legal entity,
-- for ever (G08-04; AIR keeps HAWB numbers per legal entity, N1-HAWB): a later document of the same shipment issues
-- it again, another shipment never.
CREATE TABLE cargo_doc.issued_number (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('HAWB','MAWB')),
 number text NOT NULL,
 subject_id text NOT NULL CHECK (length(btrim(subject_id)) BETWEEN 1 AND 128),
 first_document_id text NOT NULL,
 first_issued_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, kind, number),
 FOREIGN KEY (tenant_id, first_document_id) REFERENCES cargo_doc.transport_document (tenant_id, document_id)
);

CREATE TABLE cargo_doc.issuance_authority (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 authority_id text NOT NULL CHECK (length(btrim(authority_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 kind text NOT NULL CHECK (kind IN ('HAWB','MAWB')),
 issue_place text NOT NULL CHECK (issue_place ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 airline_party_id text CHECK (length(btrim(airline_party_id)) BETWEEN 1 AND 128),
 qualification text NOT NULL CHECK (length(btrim(qualification)) BETWEEN 1 AND 500),
 valid_from timestamptz NOT NULL,
 valid_until timestamptz,
 evidence_ref text NOT NULL CHECK (length(btrim(evidence_ref)) BETWEEN 1 AND 500),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 withdrawn_by text CHECK (length(btrim(withdrawn_by)) > 0),
 withdrawn_at timestamptz,
 withdraw_reason text CHECK (length(btrim(withdraw_reason)) BETWEEN 1 AND 500),
 withdraw_entered_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 PRIMARY KEY (tenant_id, authority_id),
 CONSTRAINT issuance_authority_airline CHECK ((kind = 'MAWB') = (airline_party_id IS NOT NULL)),
 CONSTRAINT issuance_authority_validity CHECK (valid_until IS NULL OR valid_until > valid_from),
 CONSTRAINT issuance_authority_withdrawal CHECK ((withdrawn_at IS NULL) = (withdrawn_by IS NULL) AND (withdrawn_at IS NULL) = (withdraw_reason IS NULL)
   AND (withdrawn_at IS NULL) = (withdraw_entered_at IS NULL) AND (withdrawn_at IS NULL OR withdrawn_at >= recorded_at))
);
CREATE INDEX issuance_authority_lookup ON cargo_doc.issuance_authority (tenant_id, legal_entity_id, kind, issue_place);

CREATE TABLE cargo_doc.command_receipt (
 tenant_id text NOT NULL,
 command_id text NOT NULL,
 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 result_type text NOT NULL CHECK (result_type IN ('TransportDocument','IssuanceAuthority')),
 result_id text NOT NULL,
 result_revision bigint NOT NULL CHECK (result_revision > 0),
 result_version integer CHECK (result_version > 0),
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, command_id)
);

-- A document changes only its state, cancellation, revision and updated_at, and never once cancelled; a HAWB also
-- its movement and consolidation, when its shipment is rolled to another master.
CREATE FUNCTION cargo_doc.reject_document_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','cancel_reason','cancelled_by','cancelled_at','revision','updated_at'];
BEGIN
  IF OLD.kind = 'HAWB' THEN
    changeable := changeable || ARRAY['movement_id','consolidation_id'];
  END IF;
  IF TG_OP = 'DELETE' OR OLD.state = 'CANCELLED' OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_doc: document %/% changes only its state, and never once cancelled', OLD.tenant_id, OLD.document_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER transport_document_rules BEFORE UPDATE OR DELETE ON cargo_doc.transport_document
  FOR EACH ROW EXECUTE FUNCTION cargo_doc.reject_document_change();

-- An authority is only withdrawn, once.
CREATE FUNCTION cargo_doc.withdraw_authority_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['withdrawn_by','withdrawn_at','withdraw_reason','withdraw_entered_at','revision'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.withdrawn_at IS NOT NULL OR NEW.withdrawn_at IS NULL
     OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_doc: issuance authority %/% can only be withdrawn, once', OLD.tenant_id, OLD.authority_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER issuance_authority_rules BEFORE UPDATE OR DELETE ON cargo_doc.issuance_authority
  FOR EACH ROW EXECUTE FUNCTION cargo_doc.withdraw_authority_only();

CREATE FUNCTION cargo_doc.reject_row_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_doc: % rows are immutable', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['document_version','document_version_approval','document_version_issue','document_version_discard','issued_number'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_doc.%I FOR EACH ROW EXECUTE FUNCTION cargo_doc.reject_row_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- At commit, for every document a transaction writes: versions numbered from 1 without gaps; every issued version
-- approved; the state matches the latest version and what was issued (DRAFT: nothing approved or issued yet on
-- the latest and nothing issued ever; APPROVED: the latest approved, nothing issued; ISSUED: something issued and
-- the latest issued or discarded; AMENDING: something issued and the latest neither issued nor discarded); an
-- issued document's number belongs to its shipment or movement. The row is read as it stands at commit.
CREATE FUNCTION cargo_doc.check_document_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  d cargo_doc.transport_document;
  versions integer;
  last_no integer;
  last_approved boolean;
  last_issued boolean;
  last_discarded boolean;
  any_issued boolean;
  issued_unapproved boolean;
  holds_number boolean;
  ok boolean;
BEGIN
  SELECT * INTO d FROM cargo_doc.transport_document WHERE tenant_id = NEW.tenant_id AND document_id = NEW.document_id;
  SELECT count(*), coalesce(max(version_no), 0) INTO versions, last_no FROM cargo_doc.document_version v
    WHERE v.tenant_id = d.tenant_id AND v.document_id = d.document_id;
  SELECT EXISTS (SELECT 1 FROM cargo_doc.document_version_approval a WHERE a.tenant_id = d.tenant_id AND a.document_id = d.document_id AND a.version_no = last_no),
         EXISTS (SELECT 1 FROM cargo_doc.document_version_issue i WHERE i.tenant_id = d.tenant_id AND i.document_id = d.document_id AND i.version_no = last_no),
         EXISTS (SELECT 1 FROM cargo_doc.document_version_discard x WHERE x.tenant_id = d.tenant_id AND x.document_id = d.document_id AND x.version_no = last_no),
         EXISTS (SELECT 1 FROM cargo_doc.document_version_issue i WHERE i.tenant_id = d.tenant_id AND i.document_id = d.document_id),
         EXISTS (SELECT 1 FROM cargo_doc.document_version_issue i WHERE i.tenant_id = d.tenant_id AND i.document_id = d.document_id
           AND NOT EXISTS (SELECT 1 FROM cargo_doc.document_version_approval a WHERE a.tenant_id = i.tenant_id AND a.document_id = i.document_id
             AND a.version_no = i.version_no)),
         EXISTS (SELECT 1 FROM cargo_doc.issued_number n WHERE n.tenant_id = d.tenant_id AND n.legal_entity_id = d.legal_entity_id AND n.kind = d.kind
           AND n.number = d.number AND n.subject_id = CASE d.kind WHEN 'HAWB' THEN d.shipment_id ELSE d.movement_id END)
    INTO last_approved, last_issued, last_discarded, any_issued, issued_unapproved, holds_number;
  ok := versions > 0 AND versions = last_no AND NOT issued_unapproved AND (NOT any_issued OR holds_number) AND CASE d.state
    WHEN 'DRAFT' THEN NOT last_approved AND NOT any_issued AND NOT last_discarded
    WHEN 'APPROVED' THEN last_approved AND NOT any_issued AND NOT last_discarded
    WHEN 'ISSUED' THEN any_issued AND (last_issued OR last_discarded)
    WHEN 'AMENDING' THEN any_issued AND NOT last_issued AND NOT last_discarded
    ELSE true END;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_doc: document %/% is inconsistent at commit (state %)', d.tenant_id, d.document_id, d.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'transport_document_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER transport_document_consistent AFTER INSERT OR UPDATE ON cargo_doc.transport_document
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_doc.check_document_at_commit();

-- At commit, a version and its approval, issue or discard are written by a transaction that writes the
-- document row: nothing is added to a document behind its back.
CREATE FUNCTION cargo_doc.check_written_with_document() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_doc.transport_document d WHERE d.tenant_id = NEW.tenant_id AND d.document_id = NEW.document_id
       AND d.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_doc: % of document %/% is written with the document', TG_TABLE_NAME, NEW.tenant_id, NEW.document_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'document_records_with_document';
  END IF;
  RETURN NULL;
END $fn$;
DO $with$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['document_version','document_version_approval','document_version_issue','document_version_discard'] LOOP
    EXECUTE format('CREATE CONSTRAINT TRIGGER %I AFTER INSERT ON cargo_doc.%I DEFERRABLE INITIALLY DEFERRED FOR EACH ROW '
      'EXECUTE FUNCTION cargo_doc.check_written_with_document()', tbl || '_with_document', tbl);
  END LOOP;
END $with$;

CREATE FUNCTION cargo_doc.check_number_with_document() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_doc.transport_document d WHERE d.tenant_id = NEW.tenant_id AND d.document_id = NEW.first_document_id
       AND d.xmin = pg_current_xact_id()::xid AND d.legal_entity_id = NEW.legal_entity_id AND d.kind = NEW.kind AND d.number = NEW.number) THEN
    RAISE EXCEPTION 'cargo_doc: issued number %/% is written with the document that issues it', NEW.tenant_id, NEW.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'document_records_with_document';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER issued_number_with_document AFTER INSERT ON cargo_doc.issued_number
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_doc.check_number_with_document();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['transport_document','document_version','document_version_approval','document_version_issue','document_version_discard',
      'issued_number','issuance_authority','command_receipt'] LOOP
    EXECUTE format('ALTER TABLE cargo_doc.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_doc.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_doc.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;
