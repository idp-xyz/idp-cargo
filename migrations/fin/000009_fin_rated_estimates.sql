-- IDP-Cargo FIN with RATE V0.5 part 2 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Rated estimates (FB2 and FB3 with RATE's PricingBasis v1): a job takes a SHIPMENT_ESTIMATE basis as RATED charge
-- lines, a consol cost pool a CONSOL_COST_ESTIMATE basis as RATED cost lines. A job is priced by its accepted quotes or
-- by one rated estimate, never both; a pool takes one rated estimate; a basis is applied once.

ALTER TABLE cargo_fin.job_basis ADD COLUMN purpose text NOT NULL DEFAULT 'QUOTE_ACCEPTED'
  CHECK (purpose IN ('QUOTE_ACCEPTED','SHIPMENT_ESTIMATE'));
ALTER TABLE cargo_fin.job_basis ADD COLUMN plan_version integer;
ALTER TABLE cargo_fin.job_basis ALTER COLUMN quote_id DROP NOT NULL, ALTER COLUMN quote_revision DROP NOT NULL, ALTER COLUMN order_id DROP NOT NULL;
ALTER TABLE cargo_fin.job_basis ADD CONSTRAINT job_basis_purpose CHECK (CASE purpose
  WHEN 'QUOTE_ACCEPTED' THEN quote_id IS NOT NULL AND quote_revision IS NOT NULL AND order_id IS NOT NULL AND plan_version IS NULL
  ELSE quote_id IS NULL AND quote_revision IS NULL AND order_id IS NULL AND plan_version > 0 END);
-- a rated basis is applied to one live job, once
CREATE UNIQUE INDEX job_basis_estimate_once ON cargo_fin.job_basis (tenant_id, basis_id) WHERE live AND purpose = 'SHIPMENT_ESTIMATE';

-- A job's live bases are all of one purpose, and at most one is a rated estimate; checked with the job row locked.
CREATE FUNCTION cargo_fin.job_basis_one_kind() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id FOR NO KEY UPDATE;
  IF NEW.live AND EXISTS (SELECT 1 FROM cargo_fin.job_basis b WHERE b.tenant_id = NEW.tenant_id AND b.job_id = NEW.job_id AND b.live
       AND b.basis_no <> NEW.basis_no AND (b.purpose <> NEW.purpose OR NEW.purpose = 'SHIPMENT_ESTIMATE')) THEN
    RAISE EXCEPTION 'cargo_fin: job %/% is priced by its accepted quotes or by one rated estimate', NEW.tenant_id, NEW.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'job_basis_one_kind';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER job_basis_one_kind BEFORE INSERT ON cargo_fin.job_basis FOR EACH ROW EXECUTE FUNCTION cargo_fin.job_basis_one_kind();

-- Charge lines from a rated estimate are RATED, from an accepted quote QUOTE; each names its basis and basis line.
ALTER TABLE cargo_fin.charge_line DROP CONSTRAINT charge_line_source_check;
ALTER TABLE cargo_fin.charge_line ADD CONSTRAINT charge_line_source_check CHECK (source IN ('QUOTE','RATED','MANUAL'));
ALTER TABLE cargo_fin.charge_line DROP CONSTRAINT charge_line_source;
ALTER TABLE cargo_fin.charge_line ADD CONSTRAINT charge_line_source CHECK (CASE source
  WHEN 'MANUAL' THEN basis_no IS NULL AND quote_line_no IS NULL AND reason IS NOT NULL
  ELSE basis_no IS NOT NULL AND quote_line_no IS NOT NULL AND reason IS NULL END);
COMMENT ON COLUMN cargo_fin.charge_line.quote_line_no IS 'The line of the basis the charge line comes from: a quote line, or a rated basis line.';
CREATE FUNCTION cargo_fin.charge_line_of_its_basis() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.source <> 'MANUAL' AND NOT EXISTS (SELECT 1 FROM cargo_fin.job_basis b WHERE b.tenant_id = NEW.tenant_id AND b.job_id = NEW.job_id
       AND b.basis_no = NEW.basis_no AND b.purpose = CASE NEW.source WHEN 'QUOTE' THEN 'QUOTE_ACCEPTED' ELSE 'SHIPMENT_ESTIMATE' END) THEN
    RAISE EXCEPTION 'cargo_fin: a % line of job %/% comes from a basis of its purpose', NEW.source, NEW.tenant_id, NEW.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'charge_line_of_its_basis';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER charge_line_of_its_basis BEFORE INSERT ON cargo_fin.charge_line FOR EACH ROW EXECUTE FUNCTION cargo_fin.charge_line_of_its_basis();

-- Pool cost lines from a rated estimate are RATED and name the basis and its line; a pool takes one rated estimate and a
-- basis line is applied once.
ALTER TABLE cargo_fin.pool_cost_line ADD COLUMN source text NOT NULL DEFAULT 'MANUAL' CHECK (source IN ('MANUAL','RATED'));
ALTER TABLE cargo_fin.pool_cost_line ADD COLUMN basis_id text CHECK (length(btrim(basis_id)) BETWEEN 1 AND 128);
ALTER TABLE cargo_fin.pool_cost_line ADD COLUMN basis_line_no integer CHECK (basis_line_no > 0);
ALTER TABLE cargo_fin.pool_cost_line ADD CONSTRAINT pool_cost_line_source
  CHECK ((source = 'RATED') = (basis_id IS NOT NULL) AND (basis_id IS NULL) = (basis_line_no IS NULL));
CREATE UNIQUE INDEX pool_cost_line_basis_once ON cargo_fin.pool_cost_line (tenant_id, basis_id, basis_line_no) WHERE basis_id IS NOT NULL;
CREATE FUNCTION cargo_fin.pool_one_estimate() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.source = 'RATED' AND EXISTS (SELECT 1 FROM cargo_fin.pool_cost_line l WHERE l.tenant_id = NEW.tenant_id AND l.pool_id = NEW.pool_id
       AND l.source = 'RATED' AND l.basis_id <> NEW.basis_id) THEN
    RAISE EXCEPTION 'cargo_fin: pool %/% takes one rated estimate', NEW.tenant_id, NEW.pool_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_one_estimate';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER pool_one_estimate BEFORE INSERT ON cargo_fin.pool_cost_line FOR EACH ROW EXECUTE FUNCTION cargo_fin.pool_one_estimate();
