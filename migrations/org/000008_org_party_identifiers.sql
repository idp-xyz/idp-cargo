-- IDP-Cargo ORG V0.4 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Owner decision O1 of 2026-11-10 (benchmark against CargoWise and Chinese rules): a party's identity gains typed
-- identifiers (MD-06) and an English name; an address may carry the organization's phone and e-mail.
-- Customs declarations name Chinese parties by their 18-character unified social credit code (GB 32100) and
-- AEO operators by country and code; manifests require the shipper's code and phone.

-- Migration 000001 created party_identifier as an encrypted candidate. No code ever wrote it, and V0.4
-- replaces it. It is dropped only when empty; the owner is exempt from row level security for the check.
ALTER TABLE cargo_org.party_identifier NO FORCE ROW LEVEL SECURITY;
DO $empty$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_org.party_identifier) THEN
    RAISE EXCEPTION 'cargo_org: party_identifier holds rows of the V2.3 candidate; convert them before ORG V0.4'
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
END $empty$;
DROP TABLE cargo_org.party_identifier;

-- GB 32100: 17 characters of the set 0-9 and A-Y without I, O, S, V and Z, weighted by 3^(i-1) mod 31, and a
-- check character that makes the sum a multiple of 31. Same rule as domain.ValidUSCC.
CREATE FUNCTION cargo_org.uscc_valid(code text) RETURNS boolean LANGUAGE plpgsql IMMUTABLE STRICT AS $fn$
DECLARE
  cs constant text := '0123456789ABCDEFGHJKLMNPQRTUWXY';
  s integer := 0;
  w integer := 1;
BEGIN
  IF code !~ '^[0-9A-HJ-NP-RTUWXY]{18}$' THEN
    RETURN false;
  END IF;
  FOR i IN 1..17 LOOP
    s := s + (strpos(cs, substr(code, i, 1)) - 1) * w;
    w := (w * 3) % 31;
  END LOOP;
  RETURN substr(code, 18, 1) = substr(cs, ((31 - s % 31) % 31) + 1, 1);
END $fn$;

-- One row per identifier a party was given, or per recorded absence (none), at the party revision that
-- recorded it. Retiring sets the retirement columns once; nothing else changes and nothing is deleted.
CREATE TABLE cargo_org.party_identifier (
  tenant_id text NOT NULL,
  party_id text NOT NULL,
  identifier_id char(64) NOT NULL CHECK (identifier_id ~ '^[0-9a-f]{64}$'),
  identifier_type text NOT NULL CHECK (identifier_type IN ('CN_USCC','CN_CUSTOMS','AEO','EU_EORI','US_EIN','REGISTRATION')),
  issuing_country char(2) NOT NULL CHECK (issuing_country ~ '^[A-Z]{2}$'),
  value text CHECK (value ~ '^[0-9A-Z][0-9A-Z-]{0,34}$'),
  none boolean NOT NULL,
  evidence_ref text NOT NULL CHECK (char_length(btrim(evidence_ref)) BETWEEN 1 AND 500),
  recorded_by text NOT NULL CHECK (length(btrim(recorded_by)) > 0),
  recorded_at timestamptz NOT NULL,
  recorded_revision bigint NOT NULL CHECK (recorded_revision > 0),
  retired_by text CHECK (length(btrim(retired_by)) > 0),
  retired_at timestamptz,
  retire_reason text CHECK (char_length(btrim(retire_reason)) BETWEEN 1 AND 500),
  retired_revision bigint,
  PRIMARY KEY (tenant_id, party_id, identifier_id),
  FOREIGN KEY (tenant_id, party_id) REFERENCES cargo_org.business_party (tenant_id, party_id),
  CONSTRAINT party_identifier_none CHECK (none = (value IS NULL)),
  CONSTRAINT party_identifier_retirement CHECK ((retired_at IS NULL) = (retired_by IS NULL)
    AND (retired_at IS NULL) = (retire_reason IS NULL) AND (retired_at IS NULL) = (retired_revision IS NULL)
    AND (retired_revision IS NULL OR retired_revision > recorded_revision)),
  CONSTRAINT party_identifier_country CHECK ((identifier_type NOT IN ('CN_USCC','CN_CUSTOMS') OR issuing_country = 'CN')
    AND (identifier_type <> 'US_EIN' OR issuing_country = 'US')),
  CONSTRAINT party_identifier_form CHECK (none OR CASE identifier_type
    WHEN 'CN_USCC' THEN cargo_org.uscc_valid(value)
    WHEN 'CN_CUSTOMS' THEN value ~ '^[0-9A-Z]{10}$'
    WHEN 'AEO' THEN value ~ '^[A-Z]{2}[0-9A-Z]{1,30}$' AND left(value, 2) = issuing_country
    WHEN 'EU_EORI' THEN value ~ '^[A-Z]{2}[0-9A-Z]{1,15}$' AND left(value, 2) = issuing_country
    WHEN 'US_EIN' THEN value ~ '^[0-9]{9}$'
    ELSE true END)
);
-- One active entry per type and issuing country of a party; one active holder of a value in a tenant.
CREATE UNIQUE INDEX party_identifier_one_active ON cargo_org.party_identifier (tenant_id, party_id, identifier_type, issuing_country)
  WHERE retired_at IS NULL;
CREATE UNIQUE INDEX party_identifier_value_active ON cargo_org.party_identifier (tenant_id, identifier_type, issuing_country, value)
  WHERE retired_at IS NULL AND value IS NOT NULL;

CREATE FUNCTION cargo_org.retire_identifier_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.retired_at IS NOT NULL OR NEW.retired_at IS NULL
     OR (NEW.tenant_id, NEW.party_id, NEW.identifier_id, NEW.identifier_type, NEW.issuing_country, NEW.value, NEW.none,
         NEW.evidence_ref, NEW.recorded_by, NEW.recorded_at, NEW.recorded_revision)
        IS DISTINCT FROM (OLD.tenant_id, OLD.party_id, OLD.identifier_id, OLD.identifier_type, OLD.issuing_country, OLD.value, OLD.none,
         OLD.evidence_ref, OLD.recorded_by, OLD.recorded_at, OLD.recorded_revision) THEN
    RAISE EXCEPTION 'cargo_org: identifier % of party %/% can only be retired, once', OLD.identifier_type, OLD.tenant_id, OLD.party_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER party_identifier_retire_only BEFORE UPDATE OR DELETE ON cargo_org.party_identifier
  FOR EACH ROW EXECUTE FUNCTION cargo_org.retire_identifier_only();

ALTER TABLE cargo_org.party_identifier ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_org.party_identifier FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_org.party_identifier
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- The party's name in English as international documents print it: printable ASCII, trimmed, up to 200.
ALTER TABLE cargo_org.business_party
  ADD COLUMN english_name text CONSTRAINT business_party_english_name CHECK (english_name ~ '^[ -~]{1,200}$' AND english_name = btrim(english_name));

-- The organization's phone and e-mail at an address; a residential address carries neither (they would be
-- a person's; persons are contacts).
ALTER TABLE cargo_org.party_address_version
  ADD COLUMN phone text CHECK (char_length(phone) BETWEEN 3 AND 40),
  ADD COLUMN email text CHECK (char_length(email) BETWEEN 3 AND 254),
  ADD CONSTRAINT party_address_version_residential_contact CHECK (NOT residential OR (phone IS NULL AND email IS NULL));
