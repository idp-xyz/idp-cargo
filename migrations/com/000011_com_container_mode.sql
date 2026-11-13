-- IDP-Cargo COM / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The container mode an order requests (decision N1-PHY OD-9, option A: requested on the COM order with the transport
-- mode, like C11, and planned in FUL): LCL or FCL on SEA, LOOSE on AIR, none on ROAD or RAIL yet. It is a column of
-- the transport version, so a change is a new version with its reason (ChangeOrderTransport), refused while DOC holds
-- the order's parties locked. Orders recorded before it request none; nothing is converted.

ALTER TABLE cargo_com.order_transport
  ADD COLUMN container_mode text CHECK (container_mode IN ('LCL','FCL','LOOSE')),
  ADD CONSTRAINT order_transport_container_mode CHECK (container_mode IS NULL
    OR (transport_mode = 'SEA' AND container_mode IN ('LCL','FCL')) OR (transport_mode = 'AIR' AND container_mode = 'LOOSE')) NOT VALID;
ALTER TABLE cargo_com.order_transport VALIDATE CONSTRAINT order_transport_container_mode;
