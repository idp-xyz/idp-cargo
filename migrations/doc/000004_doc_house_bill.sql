-- IDP-Cargo DOC (Transport Documents) V0.3, the house bill / PostgreSQL 16.
-- Run only as the migrator role, never at service start.
-- An HBL is the house document of one sea shipment, built from OceanDocFacts v1. Its number is the HBL SEA drew.
-- 000001 through 000003 are unchanged.

ALTER TABLE cargo_doc.transport_document DROP CONSTRAINT transport_document_kind_check;
ALTER TABLE cargo_doc.transport_document ADD CONSTRAINT transport_document_kind_check CHECK (kind IN ('HAWB','MAWB','HBL'));
ALTER TABLE cargo_doc.transport_document DROP CONSTRAINT transport_document_house;
ALTER TABLE cargo_doc.transport_document ADD CONSTRAINT transport_document_house CHECK ((kind IN ('HAWB','HBL')) = (shipment_id IS NOT NULL));
CREATE UNIQUE INDEX transport_document_one_hbl ON cargo_doc.transport_document (tenant_id, shipment_id) WHERE kind = 'HBL' AND state <> 'CANCELLED';

ALTER TABLE cargo_doc.issued_number DROP CONSTRAINT issued_number_kind_check;
ALTER TABLE cargo_doc.issued_number ADD CONSTRAINT issued_number_kind_check CHECK (kind IN ('HAWB','MAWB','HBL'));
ALTER TABLE cargo_doc.issuance_authority DROP CONSTRAINT issuance_authority_kind_check;
ALTER TABLE cargo_doc.issuance_authority ADD CONSTRAINT issuance_authority_kind_check CHECK (kind IN ('HAWB','MAWB','HBL'));
ALTER TABLE cargo_doc.document_dispatch DROP CONSTRAINT document_dispatch_kind_check;
ALTER TABLE cargo_doc.document_dispatch ADD CONSTRAINT document_dispatch_kind_check CHECK (kind IN ('HAWB','MAWB','HBL'));

-- A house bill rolls to another movement with its shipment, as a HAWB does.
CREATE OR REPLACE FUNCTION cargo_doc.reject_document_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','cancel_reason','cancelled_by','cancelled_at','revision','updated_at'];
BEGIN
  IF OLD.kind IN ('HAWB','HBL') THEN
    changeable := changeable || ARRAY['movement_id','consolidation_id'];
  END IF;
  IF TG_OP = 'DELETE' OR OLD.state = 'CANCELLED' OR to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_doc: document %/% changes only its state, and never once cancelled', OLD.tenant_id, OLD.document_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;

-- The rest is 000001's. An HBL's issued number belongs to its shipment, as a HAWB's does.
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
           AND n.number = d.number AND n.subject_id = CASE WHEN d.kind IN ('HAWB','HBL') THEN d.shipment_id ELSE d.movement_id END)
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
