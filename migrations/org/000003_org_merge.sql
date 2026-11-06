CREATE TABLE cargo_org.partner_merge_case (
 tenant_id text NOT NULL,merge_case_id text NOT NULL,
 source_party_id text NOT NULL,target_party_id text NOT NULL,
 opened_by text NOT NULL,evidence_ref text NOT NULL,
 impact_evidence_ref text,approved_by text,
 source_revision bigint,target_revision bigint,
 status text NOT NULL CHECK(status IN('OPEN','ASSESSED','APPROVED','REJECTED','ALIAS_APPLIED')),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 opened_at timestamptz NOT NULL DEFAULT now(),approved_at timestamptz,applied_at timestamptz,
 PRIMARY KEY(tenant_id,merge_case_id),
 FOREIGN KEY(tenant_id,source_party_id) REFERENCES cargo_org.business_party(tenant_id,party_id),
 FOREIGN KEY(tenant_id,target_party_id) REFERENCES cargo_org.business_party(tenant_id,party_id),
 CHECK(source_party_id<>target_party_id),
 CHECK(status NOT IN('APPROVED','ALIAS_APPLIED') OR
    (impact_evidence_ref IS NOT NULL AND approved_by IS NOT NULL AND approved_by<>opened_by AND approved_at IS NOT NULL AND source_revision IS NOT NULL AND target_revision IS NOT NULL AND source_revision>0 AND target_revision>0)),
 CHECK(status<>'ALIAS_APPLIED' OR applied_at IS NOT NULL)
);
-- Immutable survivor mapping. Never delete signed historical documents or rewrite their party snapshots.
CREATE TABLE cargo_org.party_alias (
 tenant_id text NOT NULL,source_party_id text NOT NULL,target_party_id text NOT NULL,
 merge_case_id text NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,source_party_id),
 UNIQUE(tenant_id,merge_case_id),
 FOREIGN KEY(tenant_id,source_party_id) REFERENCES cargo_org.business_party(tenant_id,party_id),
 FOREIGN KEY(tenant_id,target_party_id) REFERENCES cargo_org.business_party(tenant_id,party_id),
 FOREIGN KEY(tenant_id,merge_case_id) REFERENCES cargo_org.partner_merge_case(tenant_id,merge_case_id),
 CHECK(source_party_id<>target_party_id)
);
-- COMMAND TRANSACTION MUST lock both parties by deterministic key order, recheck revisions,
-- refuse target if itself alias, and atomically insert alias + change case + event Outbox.
-- These cross-row invariants are NOT guaranteed by this SQL alone.
