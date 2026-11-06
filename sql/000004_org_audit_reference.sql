CREATE TABLE cargo_org.partner_external_ref (
 tenant_id text NOT NULL,source_system text NOT NULL,external_party_id text NOT NULL,
 party_id text NOT NULL,verified_at timestamptz,source_evidence_ref text,
 PRIMARY KEY(tenant_id,source_system,external_party_id),
 FOREIGN KEY(tenant_id,party_id) REFERENCES cargo_org.business_party(tenant_id,party_id)
);
-- Versioned external codes: a code can change meaning across editions.
CREATE TABLE cargo_org.reference_code_edition (
 catalog text NOT NULL,source text NOT NULL,edition text NOT NULL,code text NOT NULL,
 label text NOT NULL, valid_from timestamptz, valid_until timestamptz,
 deprecated boolean NOT NULL DEFAULT false, source_digest text NOT NULL,
 PRIMARY KEY(catalog,source,edition,code),
 CHECK(valid_until IS NULL OR valid_from IS NULL OR valid_from < valid_until)
);
-- Reference catalog is a controlled read model owned by the relevant reference steward;
-- global read policies and write grants are explicitly separate from tenant-owned ORG tables.
CREATE TABLE cargo_org.command_receipt (
 tenant_id text NOT NULL,command_id text NOT NULL,request_sha256 char(64) NOT NULL,
 result_type text NOT NULL,result_id text NOT NULL,
 completed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,command_id),
 CHECK(request_sha256 ~ '^[0-9a-f]{64}$')
);
CREATE TABLE cargo_org.org_audit (
 tenant_id text NOT NULL,audit_id text NOT NULL,aggregate_type text NOT NULL,
 aggregate_id text NOT NULL,command_id text NOT NULL,actor_id text NOT NULL,
 action text NOT NULL, evidence_ref text NOT NULL, occurred_at timestamptz NOT NULL DEFAULT now(),
 prior_revision bigint,new_revision bigint,
 PRIMARY KEY(tenant_id,audit_id)
);
CREATE INDEX org_audit_subject_idx ON cargo_org.org_audit(tenant_id,aggregate_type,aggregate_id,occurred_at DESC);
-- command_receipt INSERT+business write+outbox enqueue+audit must use one Bento transaction.
-- Distinct same-CommandID payload hashes MUST reject; uncertain commit => read receipt.
