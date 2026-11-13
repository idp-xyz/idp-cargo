-- IDP-Cargo FUL / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The container mode a shipment plan version plans (decision N1-PHY OD-9, option A: requested on the COM order and
-- planned in FUL): LCL or FCL on SEA, LOOSE on AIR, none on ROAD or RAIL yet. LoadIntent v1 carries it to PHY, whose
-- LCL consolidations refuse FCL houses. Plan versions written before it plan none; nothing is converted.

ALTER TABLE cargo_ful.shipment_plan_version
  ADD COLUMN container_mode text CHECK (container_mode IN ('LCL','FCL','LOOSE')),
  ADD CONSTRAINT shipment_plan_version_container_mode CHECK (container_mode IS NULL
    OR (coalesce(transport_mode, '') = 'SEA' AND container_mode IN ('LCL','FCL'))
    OR (coalesce(transport_mode, '') = 'AIR' AND container_mode = 'LOOSE')) NOT VALID;
ALTER TABLE cargo_ful.shipment_plan_version VALIDATE CONSTRAINT shipment_plan_version_container_mode;
