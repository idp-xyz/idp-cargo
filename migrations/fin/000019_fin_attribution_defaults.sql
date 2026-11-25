-- IDP-Cargo FIN V2.98 / PostgreSQL 16. Run only as the migrator role, never at service start. FIN's rules read the
-- settings F4, F8 and F9 (step C1b-2 of the configuration of the rules): an apportionment in one step that names no
-- preview, an attribution made by default to the cargo that remains, and a confirmed attribution carried by the
-- proportion of its line's cost. The settings themselves are in fin/000017; this migration gives the rows what they
-- need to say which of these made them, and keeps the rules about the rows that do not depend on a setting.
--
-- Added: two columns on pool_apportionment_attribution, by_default and by_ratio (boolean, not NULL, default false,
-- which PostgreSQL adds without rewriting the table: false for every row written before). by_default marks an
-- attribution nobody stated, made by F8 DEFAULT_TO_REMAINING; its check (pool_attribution_by_default) allows it only
-- with a loss of 0, an exception cost of 0, no party and the empty voucher (the empty voucher is allowed by fin/000018
-- when both amounts are 0, and the local amounts of an attribution of nothing are 0 by fin/000016). by_ratio marks a
-- carried attribution whose loss and exception cost F9 KEEP_RATIO scaled; its check (pool_attribution_by_ratio) allows
-- it only on a carried row. Both checks are validated against the existing rows, which are all false (reasoned, not
-- measured). Changed: check_attribution_at_commit of fin/000016, with every rule it had and one more, which it checks
-- last: an attribution of an apportionment that names no preview must be carried (pool_attribution_stated_by_preview).
-- An apportionment made in one step, or written by a raw statement, names no preview and states no attribution; the
-- database checks only that such a row is carried, which is how it tells it from one a person stated in a preview. It
-- cannot state one without calling it carried: that a carried row equals the attribution in force, or is the right
-- proportion of it, only the domain keeps. An apportionment before fin/000015 names no preview either, and none of them
-- has an attribution (reasoned, not run: attributions came with fin/000016, after every apportionment was previewed).
-- And pool_apportionment_preview_check, with every rule of its latest definition (fin/000018) and now the two marks of
-- an attribution compared with the preview's figures, read as false when a preview written before this migration lacks
-- them. No row changes and nothing is dropped. The ALTER TABLE takes an ACCESS EXCLUSIVE lock briefly. The columns are
-- granted to the application role by the workflow's grant of SELECT and INSERT on every table of cargo_fin.
--
-- What the schema does not know, because only the domain keeps it: which setting is in force for a legal entity at a
-- time, so it cannot tell that an apportionment in one step needs F4 ONE_STEP_WITHOUT_OPENED_OR_LEFT (it only knows the
-- apportionment names no preview, and a preview-less apportionment is allowed whatever F4 says), nor that it was
-- refused while a former share was pending, a rework was uncarried or a carried attribution was to be stated again;
-- that a default attribution is made only where F8 DEFAULT_TO_REMAINING says and none is in force (it allows a row
-- marked by default whatever F8 says, as long as it moves no amount); that F9 was in force, or that the amounts of a
-- row carried by ratio are the right proportion of the amounts stated by the apportionment that stated them (it knows
-- only that the row is carried).

ALTER TABLE cargo_fin.pool_apportionment_attribution
  ADD COLUMN by_default boolean NOT NULL DEFAULT false,
  ADD COLUMN by_ratio boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT pool_attribution_by_default CHECK (NOT by_default OR (loss_minor = 0 AND exception_minor = 0 AND party IS NULL AND voucher = '')),
  ADD CONSTRAINT pool_attribution_by_ratio CHECK (NOT by_ratio OR carried);

-- At commit, an attribution as fin/000016 has it, and now one stated only in an apportionment that names a preview.
CREATE OR REPLACE FUNCTION cargo_fin.check_attribution_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id
       AND p.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: % is written with its pool', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fin_records_with_parent';
  END IF;
  PERFORM cargo_fin.require_read_committed();
  PERFORM cargo_fin.pool_open_check(NEW.tenant_id, NEW.pool_id);
  PERFORM cargo_fin.pool_line_shares_check(NEW.tenant_id, NEW.pool_id, NEW.apportionment_no, NEW.line_no);
  PERFORM 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id FOR NO KEY UPDATE;
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.freight_job j JOIN cargo_fin.consol_cost_pool p
       ON p.tenant_id = j.tenant_id AND p.pool_id = NEW.pool_id
       WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id AND (j.state = 'OPEN' OR NEW.loss_minor = 0)
         AND j.legal_entity_id = p.legal_entity_id AND j.shipment_id = NEW.shipment_id) THEN
    RAISE EXCEPTION 'cargo_fin: attribution of shipment % on line % of pool %/% apportionment % needs the job of the pool''s legal entity for the shipment, OPEN to bear a loss',
      NEW.shipment_id, NEW.line_no, NEW.tenant_id, NEW.pool_id, NEW.apportionment_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_attribution_job_open';
  END IF;
  IF NOT NEW.carried AND EXISTS (SELECT 1 FROM cargo_fin.pool_apportionment a WHERE a.tenant_id = NEW.tenant_id AND a.pool_id = NEW.pool_id
       AND a.apportionment_no = NEW.apportionment_no AND a.preview_no IS NULL) THEN
    RAISE EXCEPTION 'cargo_fin: attribution of shipment % on line % of pool %/% apportionment % is stated, and an apportionment that names no preview states none (it only carries)',
      NEW.shipment_id, NEW.line_no, NEW.tenant_id, NEW.pool_id, NEW.apportionment_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_attribution_stated_by_preview';
  END IF;
  RETURN NULL;
END $fn$;

-- At commit, an apportionment that names a preview, as fin/000018 has it, and now the two marks of its attributions.
CREATE OR REPLACE FUNCTION cargo_fin.pool_apportionment_preview_check(t text, id text, ano integer) RETURNS void LANGUAGE plpgsql AS $fn$
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
    AND (v.settings_version IS NULL OR a.settings_version IS NOT DISTINCT FROM v.settings_version)
    AND NOT EXISTS (
      (SELECT (l->>'line_no')::integer, (l->>'line_version')::integer, l->>'method', coalesce(l->>'reason', ''), (l->>'amount_minor')::bigint,
          (l->>'local_minor')::bigint, coalesce((l->>'to_allocate_minor')::bigint, 0), coalesce((l->>'to_allocate_local_minor')::bigint, 0)
        FROM jsonb_array_elements(v.figures) l
       EXCEPT
       SELECT al.line_no, al.line_version, al.method, coalesce(al.reason, ''), al.amount_minor, al.local_minor, al.to_allocate_minor, al.to_allocate_local_minor
        FROM cargo_fin.pool_apportionment_line al WHERE al.tenant_id = t AND al.pool_id = id AND al.apportionment_no = ano)
      UNION ALL
      (SELECT al.line_no, al.line_version, al.method, coalesce(al.reason, ''), al.amount_minor, al.local_minor, al.to_allocate_minor, al.to_allocate_local_minor
        FROM cargo_fin.pool_apportionment_line al WHERE al.tenant_id = t AND al.pool_id = id AND al.apportionment_no = ano
       EXCEPT
       SELECT (l->>'line_no')::integer, (l->>'line_version')::integer, l->>'method', coalesce(l->>'reason', ''), (l->>'amount_minor')::bigint,
          (l->>'local_minor')::bigint, coalesce((l->>'to_allocate_minor')::bigint, 0), coalesce((l->>'to_allocate_local_minor')::bigint, 0)
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
    AND NOT EXISTS (
      (SELECT (l->>'line_no')::integer, x->>'shipment_id', x->>'job_id', (x->>'loss_minor')::bigint, (x->>'loss_local_minor')::bigint,
          (x->>'exception_minor')::bigint, (x->>'exception_local_minor')::bigint, coalesce(x->>'party', ''), coalesce(x->>'party_reference', ''),
          x->>'voucher', coalesce(x->>'reason', ''), (x->>'departed_at')::timestamptz, (x->>'carried')::boolean,
          coalesce((x->>'by_default')::boolean, false), coalesce((x->>'by_ratio')::boolean, false)
        FROM jsonb_array_elements(v.figures) l CROSS JOIN LATERAL jsonb_array_elements(coalesce(l->'attributions', '[]'::jsonb)) x
       EXCEPT
       SELECT ax.line_no, ax.shipment_id, ax.job_id, ax.loss_minor, ax.loss_local_minor, ax.exception_minor, ax.exception_local_minor, coalesce(ax.party, ''),
          coalesce(ax.party_reference, ''), ax.voucher, coalesce(ax.reason, ''), ax.departed_at, ax.carried, ax.by_default, ax.by_ratio
        FROM cargo_fin.pool_apportionment_attribution ax WHERE ax.tenant_id = t AND ax.pool_id = id AND ax.apportionment_no = ano)
      UNION ALL
      (SELECT ax.line_no, ax.shipment_id, ax.job_id, ax.loss_minor, ax.loss_local_minor, ax.exception_minor, ax.exception_local_minor, coalesce(ax.party, ''),
          coalesce(ax.party_reference, ''), ax.voucher, coalesce(ax.reason, ''), ax.departed_at, ax.carried, ax.by_default, ax.by_ratio
        FROM cargo_fin.pool_apportionment_attribution ax WHERE ax.tenant_id = t AND ax.pool_id = id AND ax.apportionment_no = ano
       EXCEPT
       SELECT (l->>'line_no')::integer, x->>'shipment_id', x->>'job_id', (x->>'loss_minor')::bigint, (x->>'loss_local_minor')::bigint,
          (x->>'exception_minor')::bigint, (x->>'exception_local_minor')::bigint, coalesce(x->>'party', ''), coalesce(x->>'party_reference', ''),
          x->>'voucher', coalesce(x->>'reason', ''), (x->>'departed_at')::timestamptz, (x->>'carried')::boolean,
          coalesce((x->>'by_default')::boolean, false), coalesce((x->>'by_ratio')::boolean, false)
        FROM jsonb_array_elements(v.figures) l CROSS JOIN LATERAL jsonb_array_elements(coalesce(l->'attributions', '[]'::jsonb)) x))
    INTO ok;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: apportionment % of pool %/% is not what its preview % computed', ano, t, id, a.preview_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_apportionment_preview_matches';
  END IF;
END $fn$;
