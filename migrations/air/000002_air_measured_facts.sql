-- IDP-Cargo AIR V0.2 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A house air waybill entry freezes PHY's measures once FUL relays them (decision N1-PHY OD-2, option A): basis
-- RECEIVED (the lots' latest measures once every lot completed receipt) or TENDERED (what was tendered in the
-- consolidation, at the lots' latest measures), with FUL's evidence version and the security state of the
-- cargo; PLANNED entries keep FUL's plan. Existing entries stay PLANNED, evidence version 0.

ALTER TABLE cargo_air.house_waybill
  DROP CONSTRAINT house_waybill_basis_check,
  ADD CONSTRAINT house_waybill_basis_check CHECK (basis IN ('PLANNED','RECEIVED','TENDERED')),
  ADD COLUMN evidence_version integer NOT NULL DEFAULT 0 CHECK (evidence_version >= 0),
  ADD COLUMN security_status text CHECK (security_status IN ('PENDING','CLEARED','FAILED')),
  ADD CONSTRAINT house_waybill_measured CHECK (basis = 'PLANNED' OR evidence_version > 0);
