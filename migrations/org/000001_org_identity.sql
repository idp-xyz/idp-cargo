-- IDP-Cargo ORG P0 / PostgreSQL 16 / migration candidate v1.1.
-- Run ONLY under consumer-controlled migration identity, not service startup.
-- PostgreSQL major 16; not executed against a live instance here.
CREATE SCHEMA IF NOT EXISTS cargo_org;
CREATE TABLE cargo_org.business_party (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 party_id text NOT NULL CHECK (length(btrim(party_id)) BETWEEN 1 AND 128),
 legal_name text NOT NULL CHECK (length(btrim(legal_name)) BETWEEN 1 AND 500),
 registration_country char(2) NOT NULL CHECK (registration_country ~ '^[A-Z]{2}$'),
 status text NOT NULL CHECK (status IN ('DRAFT','ACTIVE','SUSPENDED','ARCHIVED')),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,party_id)
);
CREATE TABLE cargo_org.legal_entity (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 registered_name text NOT NULL CHECK (length(btrim(registered_name)) > 0),
 jurisdiction char(2) NOT NULL CHECK (jurisdiction ~ '^[A-Z]{2}$'),
 registration_evidence_ref text NOT NULL CHECK (length(btrim(registration_evidence_ref)) > 0),
 status text NOT NULL CHECK (status IN ('DRAFT','VERIFIED','SUSPENDED')),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,legal_entity_id)
);
CREATE TABLE cargo_org.legal_entity_branch (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 branch_id text NOT NULL,
 branch_name text NOT NULL CHECK(length(btrim(branch_name))>0),
 evidence_ref text NOT NULL CHECK(length(btrim(evidence_ref))>0),
 approved_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id,legal_entity_id,branch_id),
 FOREIGN KEY (tenant_id,legal_entity_id) REFERENCES cargo_org.legal_entity(tenant_id,legal_entity_id)
);
CREATE TABLE cargo_org.party_identifier (
 tenant_id text NOT NULL,party_id text NOT NULL,
 id_scheme text NOT NULL,id_country char(2) NOT NULL,
 identifier_hmac bytea NOT NULL CHECK(octet_length(identifier_hmac)=32),
 identifier_ciphertext bytea NOT NULL,
 verified_evidence_ref text NOT NULL,
 verified_at timestamptz,
 PRIMARY KEY(tenant_id,party_id,id_scheme,id_country,identifier_hmac),
 UNIQUE(tenant_id,id_scheme,id_country,identifier_hmac),
 FOREIGN KEY(tenant_id,party_id) REFERENCES cargo_org.business_party(tenant_id,party_id)
);
-- Raw identifiers require application-layer encryption and keyed HMAC.
-- KMS policy, rotation, erasure and legal provenance remain separate acceptance gates.
CREATE TABLE cargo_org.party_contact (
 tenant_id text NOT NULL,party_id text NOT NULL, contact_id text NOT NULL,
 contact_kind text NOT NULL CHECK(contact_kind IN('EMAIL','PHONE','PERSON')),
 ciphertext bytea NOT NULL, consent_ref text,valid_from timestamptz NOT NULL,
 valid_until timestamptz,
 PRIMARY KEY(tenant_id,party_id,contact_id),
 FOREIGN KEY(tenant_id,party_id) REFERENCES cargo_org.business_party(tenant_id,party_id),
 CHECK(valid_until IS NULL OR valid_from<valid_until)
);
CREATE TABLE cargo_org.party_address (
 tenant_id text NOT NULL,party_id text NOT NULL,address_id text NOT NULL,
 address_type text NOT NULL CHECK(address_type IN('REGISTERED','OPERATING','BILLING','SHIPPING')),
 ciphertext bytea NOT NULL,country_code char(2) NOT NULL,
 source_ref text NOT NULL,valid_from timestamptz NOT NULL,valid_until timestamptz,
 PRIMARY KEY(tenant_id,party_id,address_id),
 FOREIGN KEY(tenant_id,party_id) REFERENCES cargo_org.business_party(tenant_id,party_id),
 CHECK(valid_until IS NULL OR valid_from<valid_until)
);
