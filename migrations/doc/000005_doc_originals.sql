-- IDP-Cargo DOC (Transport Documents) V0.3, originals and the house sea waybill / PostgreSQL 16.
-- Run only as the migrator role, never at service start.
-- An HBL is EXPRESS or ORIGINALS. ORIGINALS records one to nine original bills, each a series of immutable records.
-- Surrendering every original of the current version at origin (telex release) moves the document to SURRENDERED;
-- the version stays issued. A house sea waybill is the other house document of one sea shipment: a named
-- consignee and no originals. 000001 through 000004 are unchanged.

ALTER TABLE cargo_doc.transport_document DROP CONSTRAINT transport_document_state_check;
ALTER TABLE cargo_doc.transport_document ADD CONSTRAINT transport_document_state_check
  CHECK (state IN ('DRAFT','APPROVED','ISSUED','AMENDING','SURRENDERED','CANCELLED'));

ALTER TABLE cargo_doc.transport_document DROP CONSTRAINT transport_document_kind_check;
ALTER TABLE cargo_doc.transport_document ADD CONSTRAINT transport_document_kind_check
  CHECK (kind IN ('HAWB','MAWB','HBL','HOUSE_SEA_WAYBILL'));
ALTER TABLE cargo_doc.transport_document DROP CONSTRAINT transport_document_house;
ALTER TABLE cargo_doc.transport_document ADD CONSTRAINT transport_document_house
  CHECK ((kind IN ('HAWB','HBL','HOUSE_SEA_WAYBILL')) = (shipment_id IS NOT NULL));
-- One sea house document that is not cancelled per shipment: an HBL or a house sea waybill, not both.
DROP INDEX cargo_doc.transport_document_one_hbl;
CREATE UNIQUE INDEX transport_document_one_sea_house ON cargo_doc.transport_document (tenant_id, shipment_id)
  WHERE kind IN ('HBL','HOUSE_SEA_WAYBILL') AND state <> 'CANCELLED';

ALTER TABLE cargo_doc.issued_number DROP CONSTRAINT issued_number_kind_check;
ALTER TABLE cargo_doc.issued_number ADD CONSTRAINT issued_number_kind_check
  CHECK (kind IN ('HAWB','MAWB','HBL','HOUSE_SEA_WAYBILL'));
ALTER TABLE cargo_doc.issuance_authority DROP CONSTRAINT issuance_authority_kind_check;
ALTER TABLE cargo_doc.issuance_authority ADD CONSTRAINT issuance_authority_kind_check
  CHECK (kind IN ('HAWB','MAWB','HBL','HOUSE_SEA_WAYBILL'));
ALTER TABLE cargo_doc.document_dispatch DROP CONSTRAINT document_dispatch_kind_check;
ALTER TABLE cargo_doc.document_dispatch ADD CONSTRAINT document_dispatch_kind_check
  CHECK (kind IN ('HAWB','MAWB','HBL','HOUSE_SEA_WAYBILL'));

-- A version's content has the member originals exactly when it is released as ORIGINALS; it is then one digit from 1
-- to 9, and the content is an HBL's. A missing member is no NULL that lets a version through: every version written
-- before 000005 is released CONSIGNEE_NAMED or EXPRESS and has none, so it holds.
ALTER TABLE cargo_doc.document_version ADD CONSTRAINT document_version_originals CHECK (
  CASE WHEN coalesce((content::jsonb)->>'release_type', '') = 'ORIGINALS'
    THEN coalesce((content::jsonb) ? 'originals' AND jsonb_typeof((content::jsonb)->'originals') = 'number'
      AND (content::jsonb)->>'originals' ~ '^[1-9]$' AND (content::jsonb)->>'kind' = 'HBL', false)
    ELSE NOT coalesce((content::jsonb) ? 'originals', false)
  END);

-- An original bill of an issued version: serial 1..n, and its records numbered from 1. The record with the highest
-- number is the original's status. The first record is its issue; every later one carries evidence.
CREATE TABLE cargo_doc.original (
 tenant_id text NOT NULL,
 document_id text NOT NULL,
 version_no integer NOT NULL,
 serial integer NOT NULL CHECK (serial BETWEEN 1 AND 9),
 record_no integer NOT NULL CHECK (record_no > 0),
 status text NOT NULL CHECK (status IN ('ISSUED','RETURNED','SURRENDERED_AT_ORIGIN')),
 actor text NOT NULL CHECK (length(btrim(actor)) > 0),
 at timestamptz NOT NULL,
 evidence text CHECK (length(btrim(evidence)) BETWEEN 1 AND 500),
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, document_id, version_no, serial, record_no),
 FOREIGN KEY (tenant_id, document_id, version_no) REFERENCES cargo_doc.document_version_issue (tenant_id, document_id, version_no),
 CONSTRAINT original_first_record CHECK (record_no > 1 OR status = 'ISSUED'),
 CONSTRAINT original_evidence CHECK (record_no = 1 OR evidence IS NOT NULL)
);
CREATE TRIGGER original_immutable BEFORE UPDATE OR DELETE ON cargo_doc.original
  FOR EACH ROW EXECUTE FUNCTION cargo_doc.reject_row_change();

-- A record is written for an original of an HBL's version issued last, whose content says ORIGINALS and counts the
-- serial; it is the original's next record, and follows the one before: ISSUED to RETURNED or
-- SURRENDERED_AT_ORIGIN, RETURNED to ISSUED (handed back when an amendment is abandoned) or to
-- SURRENDERED_AT_ORIGIN (surrendered with the rest of the set). Writers of one document wait for each other on its
-- row, which a record is written with.
CREATE FUNCTION cargo_doc.check_original_record() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  doc_kind text;
  current_no integer;
  body jsonb;
  last_no integer;
  last_status text;
BEGIN
  SELECT d.kind INTO doc_kind FROM cargo_doc.transport_document d
    WHERE d.tenant_id = NEW.tenant_id AND d.document_id = NEW.document_id;
  SELECT max(i.version_no) INTO current_no FROM cargo_doc.document_version_issue i
    WHERE i.tenant_id = NEW.tenant_id AND i.document_id = NEW.document_id;
  SELECT v.content::jsonb INTO body FROM cargo_doc.document_version v
    WHERE v.tenant_id = NEW.tenant_id AND v.document_id = NEW.document_id AND v.version_no = NEW.version_no;
  SELECT o.record_no, o.status INTO last_no, last_status FROM cargo_doc.original o
    WHERE o.tenant_id = NEW.tenant_id AND o.document_id = NEW.document_id AND o.version_no = NEW.version_no AND o.serial = NEW.serial
    ORDER BY o.record_no DESC LIMIT 1;
  IF doc_kind IS DISTINCT FROM 'HBL' OR current_no IS DISTINCT FROM NEW.version_no OR body->>'release_type' IS DISTINCT FROM 'ORIGINALS'
     OR NEW.serial > coalesce((body->>'originals')::int, 0) OR NEW.record_no <> coalesce(last_no, 0) + 1
     OR (last_status = 'ISSUED' AND NEW.status NOT IN ('RETURNED','SURRENDERED_AT_ORIGIN'))
     OR (last_status = 'RETURNED' AND NEW.status NOT IN ('ISSUED','SURRENDERED_AT_ORIGIN')) OR last_status = 'SURRENDERED_AT_ORIGIN' THEN
    RAISE EXCEPTION 'cargo_doc: record % (%) of original %/%/%/% does not follow', NEW.record_no, NEW.status, NEW.tenant_id, NEW.document_id,
      NEW.version_no, NEW.serial USING ERRCODE = 'check_violation', CONSTRAINT = 'original_record_follows';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER original_record_follows BEFORE INSERT ON cargo_doc.original
  FOR EACH ROW EXECUTE FUNCTION cargo_doc.check_original_record();
-- 000001's rule for versions and their records: written by a transaction that writes the document row.
CREATE CONSTRAINT TRIGGER original_with_document AFTER INSERT ON cargo_doc.original DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION cargo_doc.check_written_with_document();

ALTER TABLE cargo_doc.original ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_doc.original FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_doc.original USING (tenant_id = current_setting('app.tenant_id',true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id',true));

CREATE OR REPLACE FUNCTION cargo_doc.reject_document_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','cancel_reason','cancelled_by','cancelled_at','revision','updated_at'];
BEGIN
  IF OLD.kind IN ('HAWB','HBL','HOUSE_SEA_WAYBILL') THEN
    changeable := changeable || ARRAY['movement_id','consolidation_id'];
  END IF;
  IF TG_OP = 'DELETE' OR OLD.state = 'CANCELLED' OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_doc: document %/% changes only its state, and never once cancelled', OLD.tenant_id, OLD.document_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- 000004's check, and the originals of the version issued last (as many as its content counts when the document is
-- an HBL and the content says ORIGINALS, else none): every one of them has records; ISSUED: none surrendered; SURRENDERED: all are surrendered, and the
-- latest version is the issued one or the draft of an amendment that was abandoned, as for ISSUED; AMENDING: all
-- returned; CANCELLED: none still out. In every state, every original of an issued version that is no longer the last
-- issued one is RETURNED: DOC amends only once all are back, so no older version has an original out or surrendered.
CREATE OR REPLACE FUNCTION cargo_doc.check_document_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
  current_no integer;
  wanted integer := 0;
  recorded integer;
  out_n integer;
  returned_n integer;
  surrendered_n integer;
  older_not_returned boolean;
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
           AND n.number = d.number AND n.subject_id = CASE WHEN d.kind IN ('HAWB','HBL','HOUSE_SEA_WAYBILL') THEN d.shipment_id ELSE d.movement_id END),
         (SELECT max(i.version_no) FROM cargo_doc.document_version_issue i WHERE i.tenant_id = d.tenant_id AND i.document_id = d.document_id)
    INTO last_approved, last_issued, last_discarded, any_issued, issued_unapproved, holds_number, current_no;
  IF current_no IS NOT NULL AND d.kind = 'HBL' THEN
    SELECT CASE WHEN (v.content::jsonb)->>'release_type' = 'ORIGINALS' THEN coalesce(((v.content::jsonb)->>'originals')::int, 0) ELSE 0 END INTO wanted
      FROM cargo_doc.document_version v WHERE v.tenant_id = d.tenant_id AND v.document_id = d.document_id AND v.version_no = current_no;
  END IF;
  SELECT count(*), count(*) FILTER (WHERE s.status = 'ISSUED'), count(*) FILTER (WHERE s.status = 'RETURNED'),
         count(*) FILTER (WHERE s.status = 'SURRENDERED_AT_ORIGIN') INTO recorded, out_n, returned_n, surrendered_n
    FROM (SELECT DISTINCT ON (o.serial) o.status FROM cargo_doc.original o
          WHERE o.tenant_id = d.tenant_id AND o.document_id = d.document_id AND o.version_no = current_no
          ORDER BY o.serial, o.record_no DESC) s;
  SELECT current_no IS NOT NULL AND EXISTS (SELECT 1
    FROM (SELECT DISTINCT ON (o.version_no, o.serial) o.status FROM cargo_doc.original o
          WHERE o.tenant_id = d.tenant_id AND o.document_id = d.document_id AND o.version_no < current_no
          ORDER BY o.version_no, o.serial, o.record_no DESC) s
    WHERE s.status <> 'RETURNED') INTO older_not_returned;
  ok := versions > 0 AND versions = last_no AND NOT issued_unapproved AND (NOT any_issued OR holds_number) AND recorded = wanted
    AND NOT older_not_returned AND CASE d.state
    WHEN 'DRAFT' THEN NOT last_approved AND NOT any_issued AND NOT last_discarded
    WHEN 'APPROVED' THEN last_approved AND NOT any_issued AND NOT last_discarded
    WHEN 'ISSUED' THEN any_issued AND (last_issued OR last_discarded) AND surrendered_n = 0
    WHEN 'AMENDING' THEN any_issued AND NOT last_issued AND NOT last_discarded AND returned_n = wanted
    WHEN 'SURRENDERED' THEN any_issued AND (last_issued OR last_discarded) AND wanted > 0 AND surrendered_n = wanted
    WHEN 'CANCELLED' THEN out_n = 0
    ELSE false END;
  IF NOT ok THEN
    RAISE EXCEPTION 'cargo_doc: document %/% is inconsistent at commit (state %)', d.tenant_id, d.document_id, d.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'transport_document_consistent';
  END IF;
  RETURN NULL;
END $fn$;
