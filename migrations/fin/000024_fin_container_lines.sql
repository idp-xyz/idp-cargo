-- IDP-Cargo FIN V2.104 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A cost line of one container (PLAN stage 2, "按箱自动圈定"; the owner's acceptance criteria of 2026-11-26 in
-- 验收标准_V2.104_按箱自动圈定.md): "the container number decides who takes part, the cost line how it is divided, the
-- warehouse's actuals when it takes effect".
--   pool_cost_version.load_unit_no: the container of the consolidation a version of a line belongs to, 1 to 999; NULL is
--     the whole consolidation, as every earlier row reads. Moving a line to another container is a revision.
--   pool_apportionment_share.in_unit: on a line of one container, whether the house had packs loaded in that container at
--     the basis the apportionment read; NULL on a line of the whole consolidation. A share used is in the container.
-- Two nullable columns without a default (no table is rewritten, no row changes) and their rules. Only the domain keeps
-- that the container exists and is sealed (PHY's basis), and that in_unit and the quantities are what PHY answered.

ALTER TABLE cargo_fin.pool_cost_version ADD COLUMN load_unit_no integer,
  ADD CONSTRAINT pool_cost_version_load_unit CHECK (load_unit_no IS NULL OR load_unit_no BETWEEN 1 AND 999);

ALTER TABLE cargo_fin.pool_apportionment_share ADD COLUMN in_unit boolean,
  ADD CONSTRAINT pool_apportionment_share_in_unit CHECK (in_unit IS NOT FALSE OR NOT used);

-- A version names a container only in a sea LCL pool (the forwarder's own containers; co-loads have none, which the
-- domain refuses at the apportionment), and not on a line whose default method is CONTAINER_COUNT or TEU.
CREATE FUNCTION cargo_fin.check_cost_version_unit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.load_unit_no IS NULL THEN
    RETURN NULL;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id AND p.mode = 'SEA_LCL') THEN
    RAISE EXCEPTION 'cargo_fin: version % of line % of pool %/% names container %, and the pool is not a sea LCL pool', NEW.version_no, NEW.line_no, NEW.tenant_id,
      NEW.pool_id, NEW.load_unit_no USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_cost_version_unit_sea';
  END IF;
  IF EXISTS (SELECT 1 FROM cargo_fin.pool_cost_line l WHERE l.tenant_id = NEW.tenant_id AND l.pool_id = NEW.pool_id AND l.line_no = NEW.line_no
       AND l.method IN ('CONTAINER_COUNT','TEU')) THEN
    RAISE EXCEPTION 'cargo_fin: line % of pool %/% is divided by container count or TEU by default and cannot belong to one container', NEW.line_no, NEW.tenant_id,
      NEW.pool_id USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_cost_version_unit_method';
  END IF;
  RETURN NULL;
END $fn$;
CREATE TRIGGER pool_cost_version_unit AFTER INSERT ON cargo_fin.pool_cost_version FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_cost_version_unit();

-- An apportionment line of a version of one container is not divided by CONTAINER_COUNT or TEU, and its shares say
-- whether each house was in the container; a line of the whole consolidation has shares that do not.
CREATE FUNCTION cargo_fin.check_apportionment_line_unit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.method IN ('CONTAINER_COUNT','TEU') AND EXISTS (SELECT 1 FROM cargo_fin.pool_cost_version v WHERE v.tenant_id = NEW.tenant_id AND v.pool_id = NEW.pool_id
       AND v.line_no = NEW.line_no AND v.version_no = NEW.line_version AND v.load_unit_no IS NOT NULL) THEN
    RAISE EXCEPTION 'cargo_fin: line % of pool %/% apportionment % belongs to one container and is divided by %', NEW.line_no, NEW.tenant_id, NEW.pool_id,
      NEW.apportionment_no, NEW.method USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_apportionment_line_unit_method';
  END IF;
  RETURN NULL;
END $fn$;
CREATE TRIGGER pool_apportionment_line_unit AFTER INSERT ON cargo_fin.pool_apportionment_line FOR EACH ROW
  EXECUTE FUNCTION cargo_fin.check_apportionment_line_unit();

CREATE FUNCTION cargo_fin.check_share_in_unit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  unit_no integer;
BEGIN
  SELECT v.load_unit_no INTO unit_no FROM cargo_fin.pool_apportionment_line al JOIN cargo_fin.pool_cost_version v
      ON v.tenant_id = al.tenant_id AND v.pool_id = al.pool_id AND v.line_no = al.line_no AND v.version_no = al.line_version
    WHERE al.tenant_id = NEW.tenant_id AND al.pool_id = NEW.pool_id AND al.apportionment_no = NEW.apportionment_no AND al.line_no = NEW.line_no;
  IF (unit_no IS NULL) <> (NEW.in_unit IS NULL) THEN
    RAISE EXCEPTION 'cargo_fin: share % of line % of pool %/% apportionment % says whether its house is in the container exactly when the line belongs to one',
      NEW.share_no, NEW.line_no, NEW.tenant_id, NEW.pool_id, NEW.apportionment_no USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_share_in_unit_of_line';
  END IF;
  RETURN NULL;
END $fn$;
CREATE TRIGGER pool_apportionment_share_in_unit AFTER INSERT ON cargo_fin.pool_apportionment_share FOR EACH ROW
  EXECUTE FUNCTION cargo_fin.check_share_in_unit();
