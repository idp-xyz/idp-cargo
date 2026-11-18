-- IDP-Cargo DOC (Transport Documents) V0.4, the carrier's documents / PostgreSQL 16.
-- Run only as the migrator role, never at service start.
-- The carrier's master bill of lading or master sea waybill is issued by the carrier, not by DOC. DOC records it from
-- OceanDocFacts v1 with a reference to the evidence of the carrier's issuance, and a second person verifies the record
-- (DOC records that a second person did; it does not check the evidence). A recorded document is an aggregate of its
-- own: it has no versions, approvals, originals, rendering or dispatch. The tables of 000001 through 000005 are not
-- touched; only the receipt's list of result types is extended, at the end. A co-loader's bill is recorded as an MBL
-- with the co-loader as its issuer. For a tenant and a carrier, a number is on at most one document that is not
-- cancelled; a cancelled record frees it.

CREATE TABLE cargo_doc.carrier_document (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 document_id text NOT NULL CHECK (length(btrim(document_id)) BETWEEN 1 AND 128),
 kind text NOT NULL CHECK (kind IN ('MBL','MASTER_SEA_WAYBILL')),
 movement_id text NOT NULL CHECK (length(btrim(movement_id)) BETWEEN 1 AND 128),
 consolidation_id text NOT NULL CHECK (length(btrim(consolidation_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 carrier_party_id text NOT NULL CHECK (length(btrim(carrier_party_id)) BETWEEN 1 AND 128),
 carrier_role text NOT NULL CHECK (carrier_role IN ('CARRIER','CO_LOADER')),
 number text NOT NULL CHECK (length(btrim(number)) BETWEEN 1 AND 35 AND number = btrim(number)),
 content text NOT NULL,
 content_sha256 char(64) NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
 source_version integer NOT NULL CHECK (source_version > 0),
 recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0 AND recorded_by = btrim(recorded_by)),
 recorded_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 state text NOT NULL CHECK (state IN ('ISSUED','CANCELLED')),
 verified_by text CHECK (length(btrim(verified_by)) > 0 AND verified_by = btrim(verified_by)),
 verified_at timestamptz,
 verify_entered_at timestamptz,
 cancel_reason text CHECK (length(btrim(cancel_reason)) BETWEEN 1 AND 500),
 cancelled_by text CHECK (length(btrim(cancelled_by)) > 0 AND cancelled_by = btrim(cancelled_by)),
 cancelled_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, document_id),
 -- the content is one JSON object, the bytes its digest covers, and says what the columns say
 CONSTRAINT carrier_document_content CHECK (coalesce(
   content_sha256 = encode(sha256(convert_to(content, 'UTF8')), 'hex') AND jsonb_typeof(content::jsonb) = 'object'
   AND (content::jsonb)->>'document_id' = document_id AND (content::jsonb)->>'kind' = kind AND (content::jsonb)->>'number' = number
   AND (content::jsonb)->'carrier'->>'party_id' = carrier_party_id AND (content::jsonb)->'carrier'->>'role' = carrier_role
   AND (content::jsonb)->>'legal_entity_id' = legal_entity_id AND (content::jsonb)->>'consolidation_id' = consolidation_id
   AND (content::jsonb)->'source'->>'movement_id' = movement_id AND (content::jsonb)->'source'->'version' = to_jsonb(source_version), false)),
 -- the kind, the carrier's role and the issuer's capacity go together
 CONSTRAINT carrier_document_capacity CHECK (coalesce(
   CASE WHEN kind = 'MBL' AND carrier_role = 'CARRIER' THEN (content::jsonb)->>'issuer_capacity' = 'CARRIER'
     WHEN kind = 'MBL' AND carrier_role = 'CO_LOADER' THEN (content::jsonb)->>'issuer_capacity' = 'CO_LOADER'
     WHEN kind = 'MASTER_SEA_WAYBILL' AND carrier_role = 'CARRIER' THEN (content::jsonb)->>'issuer_capacity' = 'CARRIER'
     ELSE false END, false)),
 -- an MBL is released as ORIGINALS, which count one digit from 1 to 9, or EXPRESS, which has none; a master sea
 -- waybill is SEA_WAYBILL and has none
 CONSTRAINT carrier_document_release CHECK (coalesce(
   CASE WHEN kind = 'MBL' THEN
       ((content::jsonb)->>'release_type' = 'ORIGINALS' AND jsonb_typeof((content::jsonb)->'originals') = 'number'
         AND (content::jsonb)->>'originals' ~ '^[1-9]$')
       OR ((content::jsonb)->>'release_type' = 'EXPRESS' AND NOT ((content::jsonb) ? 'originals'))
     ELSE (content::jsonb)->>'release_type' = 'SEA_WAYBILL' AND NOT ((content::jsonb) ? 'originals') END, false)),
 -- a verification is a person, a time and the time DOC entered it, together; it is by a second person, not before the record
 CONSTRAINT carrier_document_verified_together CHECK ((verified_by IS NULL) = (verified_at IS NULL) AND (verified_at IS NULL) = (verify_entered_at IS NULL)),
 CONSTRAINT carrier_document_second_person CHECK (verified_by <> recorded_by),
 CONSTRAINT carrier_document_verified_after_record CHECK (verified_at >= recorded_at),
 -- the times are in order: a cancellation is not dated before the record or the verification (null passes)
 CONSTRAINT carrier_document_cancelled_after_record CHECK (cancelled_at >= recorded_at),
 CONSTRAINT carrier_document_cancelled_after_verification CHECK (cancelled_at >= verified_at),
 -- the cancellation is written exactly when the document is CANCELLED
 CONSTRAINT carrier_document_cancellation CHECK ((state = 'CANCELLED') = (cancel_reason IS NOT NULL) AND (cancel_reason IS NULL) = (cancelled_by IS NULL)
   AND (cancelled_by IS NULL) = (cancelled_at IS NULL))
);
-- One document that is not cancelled per movement, and one per carrier and number.
CREATE UNIQUE INDEX carrier_document_one_live ON cargo_doc.carrier_document (tenant_id, movement_id) WHERE state <> 'CANCELLED';
CREATE UNIQUE INDEX carrier_document_one_number ON cargo_doc.carrier_document (tenant_id, carrier_party_id, number) WHERE state <> 'CANCELLED';

-- A row is inserted as a fresh record: ISSUED, revision 1, not verified, not cancelled.
CREATE FUNCTION cargo_doc.check_carrier_document_insert() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.state <> 'ISSUED' OR NEW.revision <> 1 OR NEW.verified_by IS NOT NULL OR NEW.verified_at IS NOT NULL OR NEW.verify_entered_at IS NOT NULL
     OR NEW.cancel_reason IS NOT NULL OR NEW.cancelled_by IS NOT NULL OR NEW.cancelled_at IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_doc: carrier document %/% is inserted ISSUED, at revision 1, not verified and not cancelled', NEW.tenant_id, NEW.document_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'carrier_document_inserted_fresh';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER carrier_document_inserted_fresh BEFORE INSERT ON cargo_doc.carrier_document
  FOR EACH ROW EXECUTE FUNCTION cargo_doc.check_carrier_document_insert();

-- A carrier document is never deleted. It changes only its state, its verification, its cancellation, its revision and
-- its update time; its revision goes up by exactly one; nothing changes once it is cancelled; a verification, once
-- written, is never changed or removed. (A state moves only from ISSUED to CANCELLED: the state check allows no third
-- state, and nothing changes once cancelled.)
CREATE FUNCTION cargo_doc.reject_carrier_document_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  changeable text[] := ARRAY['state','verified_by','verified_at','verify_entered_at','cancel_reason','cancelled_by','cancelled_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_doc: carrier document %/% is never deleted', OLD.tenant_id, OLD.document_id
      USING ERRCODE = 'restrict_violation', CONSTRAINT = 'carrier_document_never_deleted';
  END IF;
  IF OLD.state = 'CANCELLED' THEN
    RAISE EXCEPTION 'cargo_doc: carrier document %/% changes no more once cancelled', OLD.tenant_id, OLD.document_id
      USING ERRCODE = 'restrict_violation', CONSTRAINT = 'carrier_document_cancelled_is_final';
  END IF;
  IF to_jsonb(OLD) - changeable IS DISTINCT FROM to_jsonb(NEW) - changeable THEN
    RAISE EXCEPTION 'cargo_doc: carrier document %/% changes only its state, verification, cancellation and revision', OLD.tenant_id, OLD.document_id
      USING ERRCODE = 'restrict_violation', CONSTRAINT = 'carrier_document_changes';
  END IF;
  IF NEW.revision IS DISTINCT FROM OLD.revision + 1 THEN
    RAISE EXCEPTION 'cargo_doc: carrier document %/% revision goes up by one', OLD.tenant_id, OLD.document_id
      USING ERRCODE = 'restrict_violation', CONSTRAINT = 'carrier_document_revision_step';
  END IF;
  IF OLD.verified_by IS NOT NULL AND (NEW.verified_by IS DISTINCT FROM OLD.verified_by OR NEW.verified_at IS DISTINCT FROM OLD.verified_at
     OR NEW.verify_entered_at IS DISTINCT FROM OLD.verify_entered_at) THEN
    RAISE EXCEPTION 'cargo_doc: the verification of carrier document %/% is never changed or removed', OLD.tenant_id, OLD.document_id
      USING ERRCODE = 'restrict_violation', CONSTRAINT = 'carrier_document_verification_is_final';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER carrier_document_rules BEFORE UPDATE OR DELETE ON cargo_doc.carrier_document
  FOR EACH ROW EXECUTE FUNCTION cargo_doc.reject_carrier_document_change();

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['carrier_document'] LOOP
    EXECUTE format('ALTER TABLE cargo_doc.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_doc.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_doc.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;

-- The latest definition of the receipt's result types is 000002's; this keeps every value it has and adds the carrier
-- document. It is the only change to a table of an earlier migration.
ALTER TABLE cargo_doc.command_receipt DROP CONSTRAINT command_receipt_result_type_check;
ALTER TABLE cargo_doc.command_receipt ADD CONSTRAINT command_receipt_result_type_check
  CHECK (result_type IN ('TransportDocument','IssuanceAuthority','DocumentDispatch','CarrierDocument'));
