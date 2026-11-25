-- IDP-Cargo FIN V2.97 / PostgreSQL 16. Run only as the migrator role, never at service start. FIN's rules read the
-- settings F1, F2, F3, F6 and F7 (step C1b-1 of the configuration of the rules), and every decision records the
-- version of the settings it used: a preview, a submitted apportionment and a pool closure. The settings themselves
-- are in fin/000017; this migration only gives the decisions a place to record the version, and lets an attribution
-- of a house that left carry no voucher when it moves no amount (F3 ONLY_WHEN_AMOUNT_MOVES).
--
-- Added: a column settings_version (integer, at least 0, NULL for a row written before this migration) on
-- pool_apportionment_preview, pool_apportionment and consol_cost_pool_closure, each added without a default so no
-- table is rewritten. The closure's version is kept as written by the trigger of fin/000007,
-- consol_cost_pool_closure_rules (a closure changes only in the four columns of the reopening, once). Replaced: the
-- column check on pool_apportionment_attribution.voucher by a table check of the same name that allows the empty
-- voucher (the empty string, not a blank one) when the loss and the exception cost are both 0 (every other rule of
-- the latest definition, fin/000016, is kept: the voucher is not NULL and is at most 500 characters once trimmed);
-- and pool_apportionment_preview_check, with every rule of its latest definition (fin/000016) and now the settings
-- version of an apportionment equal to its preview's (an apportionment of a preview that has a version records the same
-- one, and one with no version is refused; only a preview written before this migration has a NULL version, which the
-- check does not compare). No row changes and nothing else is dropped. The ALTER TABLEs
-- take an ACCESS EXCLUSIVE lock briefly; the new voucher check is validated against the existing rows, which all hold
-- a voucher of 1 to 500 characters (reasoned from the old check, not measured). The columns are granted to the
-- application role by the workflow's grant of SELECT and INSERT on every table of cargo_fin; the closure's UPDATE is
-- the one it already had, on the columns of the reopening only.
--
-- What the schema does not know, because only the domain keeps it: which setting is in force for a legal entity at a
-- time (the ledger of fin/000017 holds the change sets, and FIN reads it through the scope chain), the thresholds and
-- who is barred by them (F1 and F2), that a voucher is needed only when F3 asks for it and that a correction always has
-- one (the database cannot know the setting or tell a correction: it allows the empty voucher whatever F3 says, and
-- only when the amounts are 0), what a profit view counts (F6), whether a pending cause holds back the close (F7), and
-- that the version recorded is the one in force at the time of the decision (the database cannot tell which version was
-- in force; it holds the apportionment's version equal to its preview's, which FIN's fingerprint makes the version in
-- force at the submission).

ALTER TABLE cargo_fin.pool_apportionment_preview ADD COLUMN settings_version integer CONSTRAINT pool_preview_settings_version CHECK (settings_version >= 0);
ALTER TABLE cargo_fin.pool_apportionment ADD COLUMN settings_version integer CONSTRAINT pool_apportionment_settings_version CHECK (settings_version >= 0);
ALTER TABLE cargo_fin.consol_cost_pool_closure ADD COLUMN settings_version integer CONSTRAINT pool_closure_settings_version CHECK (settings_version >= 0);

-- The voucher of an attribution: at most 500 characters once trimmed, and the empty string only when the attribution
-- moves no amount (loss 0 and exception cost 0). A voucher is NOT NULL as before. The database cannot know F3: with
-- EVERY_ATTRIBUTION FIN asks for a voucher on every attribution, and the database lets through the empty one of an
-- attribution that moves nothing. It cannot tell a correction either (an attribution that replaces a different one in
-- force, which FIN always asks to give its reason and its voucher, whatever F3 says): a row has no memory of the one it
-- replaces, so only the domain keeps that.
ALTER TABLE cargo_fin.pool_apportionment_attribution DROP CONSTRAINT pool_apportionment_attribution_voucher_check,
  ADD CONSTRAINT pool_apportionment_attribution_voucher_check CHECK (length(btrim(voucher)) <= 500
    AND (length(btrim(voucher)) >= 1 OR (voucher = '' AND loss_minor = 0 AND exception_minor = 0)));

-- At commit, an apportionment that names a preview, as fin/000016 has it, and now of the preview's settings version.
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
          x->>'voucher', coalesce(x->>'reason', ''), (x->>'departed_at')::timestamptz, (x->>'carried')::boolean
        FROM jsonb_array_elements(v.figures) l CROSS JOIN LATERAL jsonb_array_elements(coalesce(l->'attributions', '[]'::jsonb)) x
       EXCEPT
       SELECT ax.line_no, ax.shipment_id, ax.job_id, ax.loss_minor, ax.loss_local_minor, ax.exception_minor, ax.exception_local_minor, coalesce(ax.party, ''),
          coalesce(ax.party_reference, ''), ax.voucher, coalesce(ax.reason, ''), ax.departed_at, ax.carried
        FROM cargo_fin.pool_apportionment_attribution ax WHERE ax.tenant_id = t AND ax.pool_id = id AND ax.apportionment_no = ano)
      UNION ALL
      (SELECT ax.line_no, ax.shipment_id, ax.job_id, ax.loss_minor, ax.loss_local_minor, ax.exception_minor, ax.exception_local_minor, coalesce(ax.party, ''),
          coalesce(ax.party_reference, ''), ax.voucher, coalesce(ax.reason, ''), ax.departed_at, ax.carried
        FROM cargo_fin.pool_apportionment_attribution ax WHERE ax.tenant_id = t AND ax.pool_id = id AND ax.apportionment_no = ano
       EXCEPT
       SELECT (l->>'line_no')::integer, x->>'shipment_id', x->>'job_id', (x->>'loss_minor')::bigint, (x->>'loss_local_minor')::bigint,
          (x->>'exception_minor')::bigint, (x->>'exception_local_minor')::bigint, coalesce(x->>'party', ''), coalesce(x->>'party_reference', ''),
          x->>'voucher', coalesce(x->>'reason', ''), (x->>'departed_at')::timestamptz, (x->>'carried')::boolean
        FROM jsonb_array_elements(v.figures) l CROSS JOIN LATERAL jsonb_array_elements(coalesce(l->'attributions', '[]'::jsonb)) x))
    INTO ok;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: apportionment % of pool %/% is not what its preview % computed', ano, t, id, a.preview_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_apportionment_preview_matches';
  END IF;
END $fn$;
