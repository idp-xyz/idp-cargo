-- IDP-Cargo FIN V0.8 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Houses that take no part in an apportionment (CargoWise's Is Used) and the methods by gross weight and by shipment
-- (FB3, plan stage 2b-2). A share records whether its house took part in the line: a house left out keeps a share, so
-- that a line still names every live house once, with quantity zero, no unit and amounts zero. A line that is not
-- RETAINED has at least one share that is used, and a job whose only current shares are not used may be cancelled. The
-- methods GROSS_WEIGHT (unit GRM, gross grams) and SHIPMENT (unit SHP, one per house that takes part) are accepted on
-- cost lines and on apportionment lines. Three check constraints are replaced by wider ones and two functions are
-- replaced; no table is rewritten, no row changes and nothing is dropped but those.

-- Every apportionment written before this version divided its line over every house, so every stored share took part:
-- the default is what the rows already say.
ALTER TABLE cargo_fin.pool_apportionment_share ADD COLUMN used boolean NOT NULL DEFAULT true;
ALTER TABLE cargo_fin.pool_apportionment_share ADD CONSTRAINT pool_apportionment_share_not_used
  CHECK (used OR (quantity = 0 AND unit IS NULL AND amount_minor = 0 AND local_minor = 0));

-- The methods as fin/000006 and fin/000007 left them, and the two new ones.
ALTER TABLE cargo_fin.pool_cost_line DROP CONSTRAINT pool_cost_line_method_check,
  ADD CONSTRAINT pool_cost_line_method_check CHECK (method IN ('CHARGEABLE','VOLUME','PIECES','MANUAL','GROSS_WEIGHT','SHIPMENT'));
ALTER TABLE cargo_fin.pool_apportionment_line DROP CONSTRAINT pool_apportionment_line_method_check,
  ADD CONSTRAINT pool_apportionment_line_method_check
    CHECK (method IN ('CHARGEABLE','VOLUME','PIECES','MANUAL','RETAINED','GROSS_WEIGHT','SHIPMENT'));

-- Grams (chargeable or gross), cubic centimetres, pieces, milli freight tonnes, or shipments; NULL on a MANUAL share and on
-- a share not used.
ALTER TABLE cargo_fin.pool_apportionment_share DROP CONSTRAINT pool_apportionment_share_unit_check,
  ADD CONSTRAINT pool_apportionment_share_unit_check CHECK (unit IN ('GRM','CMQ','PCS','MWM','SHP'));

-- The conservation rule as fin/000007 has it, and now at least one share of a line that is not RETAINED is used. The
-- schema does not tie a share's unit or quantity to its line's method for any method, so none is added for the new ones:
-- FIN computes them with PHY's facts, which are not in this schema.
CREATE OR REPLACE FUNCTION cargo_fin.pool_line_shares_check(t text, pool text, ano integer, line integer) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  al cargo_fin.pool_apportionment_line;
  v cargo_fin.pool_cost_version;
  ok boolean;
BEGIN
  SELECT * INTO al FROM cargo_fin.pool_apportionment_line WHERE tenant_id = t AND pool_id = pool AND apportionment_no = ano AND line_no = line;
  SELECT * INTO v FROM cargo_fin.pool_cost_version
    WHERE tenant_id = t AND pool_id = pool AND line_no = al.line_no AND version_no = al.line_version;
  SELECT CASE WHEN al.method = 'RETAINED' THEN count(*) = 0 AND al.amount_minor IS NOT NULL
      ELSE count(*) = coalesce(max(s.share_no), 0) AND count(*) >= 1 AND count(*) FILTER (WHERE s.used) >= 1
        AND coalesce(sum(s.amount_minor), -1) = coalesce(al.amount_minor, v.amount_minor)
        AND coalesce(sum(s.local_minor), -1) = coalesce(al.local_minor, v.local_minor) END
    INTO ok FROM cargo_fin.pool_apportionment_share s
    WHERE s.tenant_id = t AND s.pool_id = pool AND s.apportionment_no = ano AND s.line_no = line;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: shares of pool %/% apportionment % line % do not conserve its amount or none takes part', t, pool, ano, line
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_apportionment_conserves';
  END IF;
END $fn$;

-- At commit, a job: once CANCELLED it holds no current pool share that is used (one under the latest apportionment
-- covering its line). A share not used bears nothing, so a house left out of its lines does not hold its job back; the
-- house is still a live house of the consolidation in PHY, so an apportionment that names it needs an OPEN job for it
-- (pool_share_job_open on the share, as fin/000006 and fin/000007).
CREATE OR REPLACE FUNCTION cargo_fin.check_job_pool_shares_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.state = 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_fin.pool_apportionment_share sh
       WHERE sh.tenant_id = NEW.tenant_id AND sh.job_id = NEW.job_id AND sh.used
         AND sh.apportionment_no = (SELECT max(l2.apportionment_no) FROM cargo_fin.pool_apportionment_line l2
           WHERE l2.tenant_id = sh.tenant_id AND l2.pool_id = sh.pool_id AND l2.line_no = sh.line_no)) THEN
    RAISE EXCEPTION 'cargo_fin: freight job %/% bears current pool shares; apportion those lines leaving its house out first', NEW.tenant_id, NEW.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_share_job_open';
  END IF;
  RETURN NULL;
END $fn$;
