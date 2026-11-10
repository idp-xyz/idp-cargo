-- IDP-Cargo FUL V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Decision F2, option 4: every plan version that names locations freezes the UN/LOCODEs it resolved, for AIR
-- the IATA airport codes, and the edition of the location reference data (cargo_ref) used. T1: plan versions
-- and confirmations keep when FUL recorded them, next to their business time.
-- Rows written before V0.3 have no edition, IATA codes or recording time and stay as they are; rows written
-- since (entered_at set) must be resolved. No foreign key reaches cargo_ref: the edition is a frozen label.

ALTER TABLE cargo_ful.shipment_plan_version
  ADD COLUMN origin_iata char(3) CHECK (origin_iata ~ '^[A-Z]{3}$'),
  ADD COLUMN destination_iata char(3) CHECK (destination_iata ~ '^[A-Z]{3}$'),
  ADD COLUMN location_edition text CHECK (location_edition ~ '^[A-Z0-9][A-Z0-9._-]{0,63}$'),
  ADD COLUMN entered_at timestamptz,
  ADD CONSTRAINT shipment_plan_version_iata_air CHECK ((origin_iata IS NULL AND destination_iata IS NULL) OR transport_mode = 'AIR'),
  ADD CONSTRAINT shipment_plan_version_iata_location CHECK ((origin_iata IS NULL OR origin_code IS NOT NULL)
    AND (destination_iata IS NULL OR destination_code IS NOT NULL)),
  ADD CONSTRAINT shipment_plan_version_resolved CHECK (entered_at IS NULL
    OR ((origin_code IS NULL AND destination_code IS NULL) OR location_edition IS NOT NULL)
    AND (transport_mode IS DISTINCT FROM 'AIR' OR ((origin_code IS NULL) = (origin_iata IS NULL) AND (destination_code IS NULL) = (destination_iata IS NULL))));

ALTER TABLE cargo_ful.shipment_plan_confirmation
  ADD COLUMN entered_at timestamptz;
