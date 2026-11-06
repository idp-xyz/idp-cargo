-- Requires migration principal to CREATE EXTENSION btree_gist; verify privileges preflight.
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE cargo_org.party_role (
 tenant_id text NOT NULL,party_id text NOT NULL,role_id text NOT NULL,
 legal_entity_id text NOT NULL,role_code text NOT NULL,
 valid_from timestamptz NOT NULL,valid_until timestamptz,
 approval_state text NOT NULL CHECK(approval_state IN ('PENDING','APPROVED','REVOKED')),
 evidence_ref text NOT NULL CHECK(length(btrim(evidence_ref))>0),
 approved_by text,approved_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision > 0),
 PRIMARY KEY (tenant_id,party_id,role_id),
 FOREIGN KEY (tenant_id,party_id) REFERENCES cargo_org.business_party(tenant_id,party_id),
 FOREIGN KEY (tenant_id,legal_entity_id) REFERENCES cargo_org.legal_entity(tenant_id,legal_entity_id),
 CHECK(valid_until IS NULL OR valid_from<valid_until),
 CHECK(approval_state <> 'APPROVED' OR (approved_by IS NOT NULL AND approved_at IS NOT NULL))
);
ALTER TABLE cargo_org.party_role ADD CONSTRAINT party_role_approved_no_overlap
 EXCLUDE USING gist (
  tenant_id WITH =, party_id WITH =, legal_entity_id WITH =, role_code WITH =,
  tstzrange(valid_from,COALESCE(valid_until,'infinity'::timestamptz),'[)') WITH &&
 ) WHERE (approval_state='APPROVED');
CREATE INDEX party_role_lookup_idx ON cargo_org.party_role(tenant_id,party_id,legal_entity_id,role_code);
-- Approval must use verified, non-suspended LegalEntity and active BusinessParty:
-- enforce through command-handler SELECT+lock and same tx, not this declarative DDL alone.
