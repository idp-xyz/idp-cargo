-- IDP-Cargo FIN V0.9 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The methods by container count and by TEU (FB3, plan stage 2b-3). CONTAINER_COUNT (unit CNT, containers) and TEU (unit
-- DTE, tenths of a TEU) are accepted on cost lines and on apportionment lines. Three check constraints are replaced by
-- wider ones, keeping every value they accepted; no table is rewritten, no row changes, and no function, column or grant
-- changes. Each ALTER TABLE takes an ACCESS EXCLUSIVE lock on its table and checks the existing rows against the new
-- constraint (one scan per table; the pool tables are small), as fin/000012 did.

ALTER TABLE cargo_fin.pool_cost_line DROP CONSTRAINT pool_cost_line_method_check,
  ADD CONSTRAINT pool_cost_line_method_check
    CHECK (method IN ('CHARGEABLE','VOLUME','PIECES','MANUAL','GROSS_WEIGHT','SHIPMENT','CONTAINER_COUNT','TEU'));
ALTER TABLE cargo_fin.pool_apportionment_line DROP CONSTRAINT pool_apportionment_line_method_check,
  ADD CONSTRAINT pool_apportionment_line_method_check
    CHECK (method IN ('CHARGEABLE','VOLUME','PIECES','MANUAL','RETAINED','GROSS_WEIGHT','SHIPMENT','CONTAINER_COUNT','TEU'));

-- Grams, cubic centimetres, pieces, milli freight tonnes, shipments, containers or tenths of a TEU; NULL on a MANUAL
-- share and on a share not used.
ALTER TABLE cargo_fin.pool_apportionment_share DROP CONSTRAINT pool_apportionment_share_unit_check,
  ADD CONSTRAINT pool_apportionment_share_unit_check CHECK (unit IN ('GRM','CMQ','PCS','MWM','SHP','CNT','DTE'));
