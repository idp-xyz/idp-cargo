-- IDP-Cargo ORG V0.6 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Carrier codes as party identifiers, for BOOK and AIR (N1): an airline's two-character IATA designator,
-- its three-digit air waybill prefix, and a carrier's SCAC. International bodies issue them, so their issuing
-- country is ZZ (ISO 3166-1 user-assigned), and the existing index keeps one active holder of a value per
-- tenant. Existing identifiers satisfy the new checks unchanged.
ALTER TABLE cargo_org.party_identifier
  DROP CONSTRAINT party_identifier_identifier_type_check,
  ADD CONSTRAINT party_identifier_identifier_type_check CHECK (identifier_type IN
    ('CN_USCC','CN_CUSTOMS','AEO','EU_EORI','US_EIN','REGISTRATION','IATA_AIRLINE','IATA_AWB_PREFIX','SCAC')),
  DROP CONSTRAINT party_identifier_country,
  ADD CONSTRAINT party_identifier_country CHECK ((identifier_type NOT IN ('CN_USCC','CN_CUSTOMS') OR issuing_country = 'CN')
    AND (identifier_type <> 'US_EIN' OR issuing_country = 'US')
    AND ((identifier_type IN ('IATA_AIRLINE','IATA_AWB_PREFIX','SCAC')) = (issuing_country = 'ZZ'))),
  DROP CONSTRAINT party_identifier_form,
  ADD CONSTRAINT party_identifier_form CHECK (none OR CASE identifier_type
    WHEN 'CN_USCC' THEN cargo_org.uscc_valid(value)
    WHEN 'CN_CUSTOMS' THEN value ~ '^[0-9A-Z]{10}$'
    WHEN 'AEO' THEN value ~ '^[A-Z]{2}[0-9A-Z]{1,30}$' AND left(value, 2) = issuing_country
    WHEN 'EU_EORI' THEN value ~ '^[A-Z]{2}[0-9A-Z]{1,15}$' AND left(value, 2) = issuing_country
    WHEN 'US_EIN' THEN value ~ '^[0-9]{9}$'
    WHEN 'IATA_AIRLINE' THEN value ~ '^([A-Z][0-9A-Z]|[0-9][A-Z])$'
    WHEN 'IATA_AWB_PREFIX' THEN value ~ '^[0-9]{3}$'
    WHEN 'SCAC' THEN value ~ '^[A-Z]{2,4}$'
    ELSE true END);
