-- IDP-Cargo COM V0.4 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Owner decision F1: FUL plans parts of one obligation in several shipments, so several FUL shipments may
-- each hold a reservation on one obligation version. COM still refuses to revoke an obligation, or to cancel
-- its order, while any reservation on it is in force (check_obligation_within_line is unchanged). COM does
-- not know the parts; FUL keeps them within the obligation. Existing rows already satisfy the new index.
DROP INDEX cargo_com.obligation_reservation_one_in_force;
CREATE UNIQUE INDEX obligation_reservation_one_per_shipment ON cargo_com.obligation_reservation (tenant_id, obligation_id, shipment_id)
  WHERE released_at IS NULL;
COMMENT ON INDEX cargo_com.obligation_reservation_one_per_shipment IS
  'A shipment holds at most one reservation in force per obligation; several shipments may hold one (F1).';
