-- IDP-Cargo FIN V2.91 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A pool frozen while a container is reworked, and every apportionment previewed before it is submitted (step 5 of "a
-- house leaves a sealed container"). A preview is an immutable record of the figures FIN computed for some of a pool's
-- lines and of the fingerprint of what it was computed on (the text and its SHA-256), written with its pool while the
-- pool is OPEN. The apportionment that submits a preview names it: a preview is submitted at most once, by an
-- apportionment of its own pool that carries its basis revision and stage, its lines and its shares, and is not made
-- before it. An apportionment made before this migration names no preview.
--
-- Added: one table, pool_apportionment_preview, with the deferred trigger every record of a pool has
-- (check_pool_record_at_commit of fin/000007); on pool_apportionment one column (preview_no), one foreign key, one
-- unique constraint and one deferred trigger, with two functions. The consistency function of a pool,
-- cargo_fin.consol_cost_pool_consistent_check, is replaced by the definition of fin/000006 (the latest there is) with
-- the numbering of the previews added to the rules it had. No row changes and nothing is dropped. The column is added
-- without a default, so no table is rewritten. The ALTER TABLE takes an ACCESS EXCLUSIVE lock on pool_apportionment,
-- builds the unique index and checks the foreign key against its rows (none of them names a preview), as fin/000012 did
-- on its tables. The table and the column are granted to the application role by the workflow's grant of SELECT and
-- INSERT on every table of cargo_fin; a preview is never updated.
--
-- What the schema does not know, because PHY's facts are not in it: that the fingerprint is the one PHY's basis and the
-- pool answer now (FIN compares them when it submits), that the person submitting did not ask for a rework that has
-- ended and was not carried by an earlier submission, that no apportionment is made while a container is in rework, and
-- that an apportionment made after this migration comes from a preview at all.

CREATE TABLE cargo_fin.pool_apportionment_preview (
 tenant_id text NOT NULL,
 pool_id text NOT NULL,
 preview_no integer NOT NULL CHECK (preview_no > 0),
 basis_revision bigint NOT NULL CHECK (basis_revision > 0),
 stage text NOT NULL CHECK (stage IN ('ACTUAL','PLANNED')),
 -- the lines it divided, each with its cost, its method and its shares, as the apportionment that submits it writes
 -- them
 figures jsonb NOT NULL,
 fingerprint text NOT NULL CHECK (length(fingerprint) > 0),
 fingerprint_sha256 char(64) NOT NULL,
 made_by text NOT NULL CHECK (length(btrim(made_by)) > 0),
 made_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, pool_id, preview_no),
 FOREIGN KEY (tenant_id, pool_id) REFERENCES cargo_fin.consol_cost_pool (tenant_id, pool_id),
 CONSTRAINT pool_preview_figures CHECK (jsonb_typeof(figures) = 'array' AND jsonb_array_length(figures) > 0),
 CONSTRAINT pool_preview_fingerprint_sha256 CHECK (fingerprint_sha256 = encode(sha256(convert_to(fingerprint, 'UTF8')), 'hex'))
);

CREATE TRIGGER pool_apportionment_preview_immutable BEFORE UPDATE OR DELETE ON cargo_fin.pool_apportionment_preview
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
ALTER TABLE cargo_fin.pool_apportionment_preview ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.pool_apportionment_preview FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.pool_apportionment_preview USING (tenant_id = current_setting('app.tenant_id',true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id',true));

ALTER TABLE cargo_fin.pool_apportionment ADD COLUMN preview_no integer,
  ADD CONSTRAINT pool_apportionment_preview_same_pool FOREIGN KEY (tenant_id, pool_id, preview_no)
    REFERENCES cargo_fin.pool_apportionment_preview (tenant_id, pool_id, preview_no),
  ADD CONSTRAINT pool_apportionment_one_per_preview UNIQUE (tenant_id, pool_id, preview_no);

-- At commit, a preview: written with its pool, while the pool is OPEN, and the pool consistent (as every pool record,
-- fin/000007).
CREATE CONSTRAINT TRIGGER pool_apportionment_preview_with_pool AFTER INSERT ON cargo_fin.pool_apportionment_preview
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_pool_record_at_commit();

-- The pool's consistency as fin/000006 has it (its functional currency and book are its policy's; its lines, each
-- line's versions and its apportionments are numbered from 1 without gaps and every line has a version; a version's
-- local amount is its amount in the functional currency or converted at an ESTIMATE entry in force), and now its
-- previews numbered from 1 without gaps.
CREATE OR REPLACE FUNCTION cargo_fin.consol_cost_pool_consistent_check(t text, id text) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  p cargo_fin.consol_cost_pool;
  ok boolean;
BEGIN
  SELECT * INTO p FROM cargo_fin.consol_cost_pool WHERE tenant_id = t AND pool_id = id;
  SELECT EXISTS (SELECT 1 FROM cargo_fin.finance_policy fp WHERE fp.tenant_id = p.tenant_id AND fp.legal_entity_id = p.legal_entity_id
       AND fp.functional_currency = p.functional_currency AND fp.book_code = p.book_code)
     AND (SELECT count(*) = coalesce(max(line_no), 0) FROM cargo_fin.pool_cost_line l WHERE l.tenant_id = p.tenant_id AND l.pool_id = p.pool_id)
     AND (SELECT count(*) = coalesce(max(apportionment_no), 0) FROM cargo_fin.pool_apportionment a
       WHERE a.tenant_id = p.tenant_id AND a.pool_id = p.pool_id)
     AND (SELECT count(*) = coalesce(max(preview_no), 0) FROM cargo_fin.pool_apportionment_preview v
       WHERE v.tenant_id = p.tenant_id AND v.pool_id = p.pool_id)
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.pool_cost_line l WHERE l.tenant_id = p.tenant_id AND l.pool_id = p.pool_id
       AND (SELECT count(*) <> coalesce(max(version_no), 0) OR count(*) = 0 FROM cargo_fin.pool_cost_version v
         WHERE v.tenant_id = l.tenant_id AND v.pool_id = l.pool_id AND v.line_no = l.line_no))
     AND NOT EXISTS (SELECT 1 FROM cargo_fin.pool_cost_version v WHERE v.tenant_id = p.tenant_id AND v.pool_id = p.pool_id
       AND CASE WHEN v.currency = p.functional_currency OR v.rate IS NULL
           THEN v.rate IS NOT NULL OR v.local_minor <> v.amount_minor OR (v.currency <> p.functional_currency AND v.amount_minor <> 0)
         ELSE NOT EXISTS (SELECT 1 FROM cargo_fin.exchange_rate_entry e WHERE e.tenant_id = p.tenant_id AND e.legal_entity_id = p.legal_entity_id
           AND e.entry_no = v.rate_entry_no AND e.rate_type = v.rate_type AND e.rate_type = 'ESTIMATE' AND e.from_currency = v.currency AND e.rate = v.rate
           AND e.scale = v.scale AND e.from_digits = v.from_digits AND e.to_digits = v.to_digits AND e.valid_from <= v.recorded_at
           AND v.recorded_at < e.valid_until
           AND v.local_minor = round(v.amount_minor::numeric * e.rate * power(10::numeric, e.to_digits - e.from_digits - e.scale))) END)
    INTO ok;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: consol cost pool %/% is inconsistent at commit', p.tenant_id, p.pool_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consol_cost_pool_consistent';
  END IF;
END $fn$;

-- At commit, an apportionment that names a preview: of the preview's basis revision and stage, not made before it, and
-- carrying exactly its lines (line, line version, method, reason and the cost it divided) and its shares (house, job,
-- whether used, quantity, unit and amounts). Its own conservation and cost are checked by the rules of fin/000007.
CREATE FUNCTION cargo_fin.pool_apportionment_preview_check(t text, id text, ano integer) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  a cargo_fin.pool_apportionment;
  v cargo_fin.pool_apportionment_preview;
  ok boolean;
BEGIN
  SELECT * INTO a FROM cargo_fin.pool_apportionment WHERE tenant_id = t AND pool_id = id AND apportionment_no = ano;
  IF a.preview_no IS NULL THEN
    RETURN;
  END IF;
  SELECT * INTO v FROM cargo_fin.pool_apportionment_preview WHERE tenant_id = t AND pool_id = id AND preview_no = a.preview_no;
  SELECT a.stage = v.stage AND a.basis_revision = v.basis_revision AND a.made_at >= v.made_at
    AND NOT EXISTS (
      (SELECT (l->>'line_no')::integer, (l->>'line_version')::integer, l->>'method', coalesce(l->>'reason', ''), (l->>'amount_minor')::bigint,
          (l->>'local_minor')::bigint
        FROM jsonb_array_elements(v.figures) l
       EXCEPT
       SELECT al.line_no, al.line_version, al.method, coalesce(al.reason, ''), al.amount_minor, al.local_minor
        FROM cargo_fin.pool_apportionment_line al WHERE al.tenant_id = t AND al.pool_id = id AND al.apportionment_no = ano)
      UNION ALL
      (SELECT al.line_no, al.line_version, al.method, coalesce(al.reason, ''), al.amount_minor, al.local_minor
        FROM cargo_fin.pool_apportionment_line al WHERE al.tenant_id = t AND al.pool_id = id AND al.apportionment_no = ano
       EXCEPT
       SELECT (l->>'line_no')::integer, (l->>'line_version')::integer, l->>'method', coalesce(l->>'reason', ''), (l->>'amount_minor')::bigint,
          (l->>'local_minor')::bigint
        FROM jsonb_array_elements(v.figures) l))
    AND NOT EXISTS (
      (SELECT (l->>'line_no')::integer, (s->>'share_no')::integer, s->>'shipment_id', s->>'job_id', (s->>'used')::boolean, (s->>'quantity')::bigint,
          coalesce(s->>'unit', ''), (s->>'amount_minor')::bigint, (s->>'local_minor')::bigint
        FROM jsonb_array_elements(v.figures) l CROSS JOIN LATERAL jsonb_array_elements(l->'shares') s
       EXCEPT
       SELECT sh.line_no, sh.share_no, sh.shipment_id, sh.job_id, sh.used, sh.quantity, coalesce(sh.unit, ''), sh.amount_minor, sh.local_minor
        FROM cargo_fin.pool_apportionment_share sh WHERE sh.tenant_id = t AND sh.pool_id = id AND sh.apportionment_no = ano)
      UNION ALL
      (SELECT sh.line_no, sh.share_no, sh.shipment_id, sh.job_id, sh.used, sh.quantity, coalesce(sh.unit, ''), sh.amount_minor, sh.local_minor
        FROM cargo_fin.pool_apportionment_share sh WHERE sh.tenant_id = t AND sh.pool_id = id AND sh.apportionment_no = ano
       EXCEPT
       SELECT (l->>'line_no')::integer, (s->>'share_no')::integer, s->>'shipment_id', s->>'job_id', (s->>'used')::boolean, (s->>'quantity')::bigint,
          coalesce(s->>'unit', ''), (s->>'amount_minor')::bigint, (s->>'local_minor')::bigint
        FROM jsonb_array_elements(v.figures) l CROSS JOIN LATERAL jsonb_array_elements(l->'shares') s))
    INTO ok;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: apportionment % of pool %/% is not what its preview % computed', ano, t, id, a.preview_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_apportionment_preview_matches';
  END IF;
END $fn$;
CREATE FUNCTION cargo_fin.check_apportionment_preview_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM cargo_fin.pool_apportionment_preview_check(NEW.tenant_id, NEW.pool_id, NEW.apportionment_no);
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER pool_apportionment_preview_matches AFTER INSERT ON cargo_fin.pool_apportionment
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_apportionment_preview_at_commit();
