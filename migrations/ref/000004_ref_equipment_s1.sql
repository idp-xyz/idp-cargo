-- IDP-Cargo reference data / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The S1 equipment subset (PB8; M1: development and internal acceptance on subset data): the three ISO 6346
-- general-purpose size-types the PHY design names (22G1, 42G1, 45G1). Maximum gross is the ISO 668 rating of these
-- series (30,480 kg, as carriers publish for their 20ft and 40ft standard containers); tare and internal capacity are
-- the assessor's typical values from carrier specifications (Hapag-Lloyd's container specification, as secondary
-- copies reproduce it, and DHL's container reference: about 33 m3 for a 20ft standard); they differ by construction
-- series by tens of kilograms and tenths of a cubic metre, so they are defaults, never a container's own plate
-- data. The compiled values (code,description,teu,max_gross_kg,tare_kg,capacity_cm3, one line each, as below) have
-- SHA-256 35d14b2eea60dd161722a7044b4594171f99cd9251c4fabac8b46c7bcfc4e8f8. To be verified against carrier data, or
-- replaced by the full import, before the pilot (E3). ULD types come with ULD build-up.

INSERT INTO cargo_ref.equipment_edition (edition, source, source_digest, published, note, is_current) VALUES ('EQUIPMENT-2026-11-S1',
  'Assessor S1 compilation: ISO 668 ratings; typical tare and capacity from carrier container specifications',
  'sha256:35d14b2eea60dd161722a7044b4594171f99cd9251c4fabac8b46c7bcfc4e8f8', DATE '2026-11-13',
  'S1 subset: 22G1, 42G1, 45G1; tare and capacity are typical defaults, to be verified before E3', true);

INSERT INTO cargo_ref.container_size_type (edition, code, description, teu, max_gross_kg, tare_kg, capacity_cm3) VALUES
  ('EQUIPMENT-2026-11-S1', '22G1', '20ft x 8ft 6in general purpose container', 1, 30480, 2250, 33200000),
  ('EQUIPMENT-2026-11-S1', '42G1', '40ft x 8ft 6in general purpose container', 2, 30480, 3750, 67700000),
  ('EQUIPMENT-2026-11-S1', '45G1', '40ft x 9ft 6in high cube general purpose container', 2, 30480, 3900, 76300000);
