-- IDP-Cargo FIN with RATE V0.5 part 3b-2 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A job priced by its accepted quote on a planned shipment that carries that quote only takes the quote re-evaluated on
-- the shipment's cargo (N1-RATE OD-7 A: per-unit lines at the quote's frozen rates, lump sums as quoted); its basis
-- names the plan version RATE re-evaluated it on. A basis of a quote as quoted names none.

ALTER TABLE cargo_fin.job_basis DROP CONSTRAINT job_basis_purpose;
ALTER TABLE cargo_fin.job_basis ADD CONSTRAINT job_basis_purpose CHECK (CASE purpose
  WHEN 'QUOTE_ACCEPTED' THEN quote_id IS NOT NULL AND quote_revision IS NOT NULL AND order_id IS NOT NULL AND (plan_version IS NULL OR plan_version > 0)
  ELSE quote_id IS NULL AND quote_revision IS NULL AND order_id IS NULL AND plan_version IS NOT NULL AND plan_version > 0 END) NOT VALID;
ALTER TABLE cargo_fin.job_basis VALIDATE CONSTRAINT job_basis_purpose;
