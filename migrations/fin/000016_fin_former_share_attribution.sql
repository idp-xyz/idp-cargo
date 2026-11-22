-- IDP-Cargo FIN V2.93 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Finance attributes a departed house's former share of the original consolidation's costs (step 6b of "a house leaves
-- a sealed container"). An apportionment of a cost line may now divide the line's cost into more than the shares of the
-- live houses: the attributions of the houses that left the consolidation (a loss, which the house's own job bears, and
-- an exception cost, which stays on the consolidation, with the party responsible for it or PENDING) and, when no house
-- is live, an amount still to be allocated. The shares, the losses, the exception costs and the amount to be allocated
-- add up to the line's cost, in the line's currency and in the functional currency.
--
-- Added: the table pool_apportionment_attribution (one row for each house of an apportionment line; immutable; row
-- security enabled and forced; written with its pool at READ COMMITTED like every table of cargo_fin) with its deferred
-- trigger; on pool_apportionment_line two columns, to_allocate_minor and to_allocate_local_minor (columns, not rows: a
-- line has one such amount or none, it is part of the line's own figures like its cost, and the close and the preview
-- check read it beside the line's other figures), each NOT NULL DEFAULT 0 with a check, and one check that ties the
-- two. Replaced, each with every rule of its latest earlier definition: pool_line_shares_check (fin/000012: the shares
-- are numbered without a gap, a line that is not RETAINED has a used share, the sums are the line's amount and local
-- amount and a RETAINED line has no share), check_job_pool_shares_at_commit (fin/000012: a cancelled job holds no
-- current used share), consol_cost_pool_closure_check (fin/000007) and pool_apportionment_preview_check (fin/000015).
-- No row changes and nothing is dropped. The ALTER TABLE takes an ACCESS EXCLUSIVE lock on pool_apportionment_line,
-- checks its rows against the new constraints (each holds for the default 0) and does not rewrite the table: the
-- default is a constant (reasoned from PostgreSQL's rule for ADD COLUMN with a constant default, not measured). The
-- table and the columns are granted to the application role by the workflow's grant of SELECT and INSERT on every table
-- of cargo_fin; nothing is ever updated.
--
-- What the schema does not know, because PHY's facts are not in it: that a shipment of an attribution is a house PHY
-- lists as having left the consolidation, that it held a former share on the line or an attribution in force, the
-- default loss FIN shows (the former share), that an attribution a preview did not state is the one in force carried
-- forward, who may confirm it (neither the person who asked for the rework nor the person who ended the house's place
-- in the consolidation), that a line's former share was attributed when the pool closes, and that a to-be-allocated
-- amount exists only while no house is live. A loss in force is the loss of the attribution under the latest
-- apportionment that covers its line.

ALTER TABLE cargo_fin.pool_apportionment_line
  ADD COLUMN to_allocate_minor bigint NOT NULL DEFAULT 0 CONSTRAINT pool_apportionment_line_to_allocate CHECK (to_allocate_minor >= 0),
  ADD COLUMN to_allocate_local_minor bigint NOT NULL DEFAULT 0 CONSTRAINT pool_apportionment_line_to_allocate_local CHECK (to_allocate_local_minor >= 0),
  ADD CONSTRAINT pool_apportionment_line_to_allocate_pair CHECK (to_allocate_minor > 0 OR to_allocate_local_minor = 0);

CREATE TABLE cargo_fin.pool_apportionment_attribution (
 tenant_id text NOT NULL,
 pool_id text NOT NULL,
 apportionment_no integer NOT NULL,
 line_no integer NOT NULL,
 shipment_id text NOT NULL CHECK (length(btrim(shipment_id)) BETWEEN 1 AND 128),
 -- the job of the shipment and the pool's legal entity: it bears the loss
 job_id text NOT NULL,
 loss_minor bigint NOT NULL CHECK (loss_minor >= 0),
 loss_local_minor bigint NOT NULL CHECK (loss_local_minor >= 0),
 exception_minor bigint NOT NULL CHECK (exception_minor >= 0),
 exception_local_minor bigint NOT NULL CHECK (exception_local_minor >= 0),
 party text CHECK (party IN ('CARRIER','WAREHOUSE','OWN_OPERATIONS','PENDING')),
 party_reference text CHECK (length(btrim(party_reference)) BETWEEN 1 AND 500),
 voucher text NOT NULL CHECK (length(btrim(voucher)) BETWEEN 1 AND 500),
 reason text CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
 -- the house's departure, the business date of the attribution
 departed_at timestamptz NOT NULL,
 -- true when the apportionment only carries forward what an earlier one of the line stated
 carried boolean NOT NULL,
 PRIMARY KEY (tenant_id, pool_id, apportionment_no, line_no, shipment_id),
 FOREIGN KEY (tenant_id, pool_id, apportionment_no, line_no)
   REFERENCES cargo_fin.pool_apportionment_line (tenant_id, pool_id, apportionment_no, line_no),
 FOREIGN KEY (tenant_id, job_id) REFERENCES cargo_fin.freight_job (tenant_id, job_id),
 CONSTRAINT pool_attribution_party CHECK ((exception_minor > 0) = (party IS NOT NULL)),
 CONSTRAINT pool_attribution_locals CHECK ((loss_minor > 0 OR loss_local_minor = 0) AND (exception_minor > 0 OR exception_local_minor = 0))
);
CREATE INDEX pool_apportionment_attribution_by_job ON cargo_fin.pool_apportionment_attribution (tenant_id, job_id);

CREATE TRIGGER pool_apportionment_attribution_immutable BEFORE UPDATE OR DELETE ON cargo_fin.pool_apportionment_attribution
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
CREATE TRIGGER pool_apportionment_attribution_read_committed BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.pool_apportionment_attribution
  FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed();
ALTER TABLE cargo_fin.pool_apportionment_attribution ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.pool_apportionment_attribution FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.pool_apportionment_attribution USING (tenant_id = current_setting('app.tenant_id',true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id',true));

-- The conservation rule as fin/000012 has it, now with the attributions and the amount to be allocated: the shares of a
-- line that is not RETAINED are numbered from 1 without a gap and, with the losses, the exception costs and the amount
-- to be allocated, sum exactly to the amount and the local amount the line divides. A line has a used share, or no
-- share at all (no house is live) and then either an amount to be allocated or an attribution; an amount to be
-- allocated and a share are never together. A RETAINED line has no share and nothing to be allocated, and its
-- attributions are not more than its cost: what remains is the cost the consolidation retains.
CREATE OR REPLACE FUNCTION cargo_fin.pool_line_shares_check(t text, pool text, ano integer, line integer) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  al cargo_fin.pool_apportionment_line;
  v cargo_fin.pool_cost_version;
  att record;
  ok boolean;
BEGIN
  SELECT * INTO al FROM cargo_fin.pool_apportionment_line WHERE tenant_id = t AND pool_id = pool AND apportionment_no = ano AND line_no = line;
  SELECT * INTO v FROM cargo_fin.pool_cost_version
    WHERE tenant_id = t AND pool_id = pool AND line_no = al.line_no AND version_no = al.line_version;
  SELECT count(*) AS n, coalesce(sum(a.loss_minor + a.exception_minor), 0) AS taken, coalesce(sum(a.loss_local_minor + a.exception_local_minor), 0) AS taken_local
    INTO att FROM cargo_fin.pool_apportionment_attribution a
    WHERE a.tenant_id = t AND a.pool_id = pool AND a.apportionment_no = ano AND a.line_no = line;
  SELECT CASE WHEN al.method = 'RETAINED' THEN count(*) = 0 AND al.amount_minor IS NOT NULL AND al.to_allocate_minor = 0 AND al.to_allocate_local_minor = 0
        AND att.taken <= al.amount_minor AND att.taken_local <= al.local_minor
      ELSE count(*) = coalesce(max(s.share_no), 0)
        AND ((count(*) >= 1 AND count(*) FILTER (WHERE s.used) >= 1 AND al.to_allocate_minor = 0 AND al.to_allocate_local_minor = 0)
          OR (count(*) = 0 AND (al.to_allocate_minor > 0 OR att.n > 0)))
        AND coalesce(sum(s.amount_minor), 0) + att.taken + al.to_allocate_minor = coalesce(al.amount_minor, v.amount_minor)
        AND coalesce(sum(s.local_minor), 0) + att.taken_local + al.to_allocate_local_minor = coalesce(al.local_minor, v.local_minor) END
    INTO ok FROM cargo_fin.pool_apportionment_share s
    WHERE s.tenant_id = t AND s.pool_id = pool AND s.apportionment_no = ano AND s.line_no = line;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: shares, losses, exception costs and the amount to be allocated of pool %/% apportionment % line % do not conserve its amount, or none takes part',
      t, pool, ano, line USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_apportionment_conserves';
  END IF;
END $fn$;

-- At commit, an attribution: written with its pool, while the pool is OPEN, conserving its line, and its job, locked as
-- FIN's job commands lock it (so a cancellation racing an apportionment takes turns with it), the job of its shipment
-- and the pool's legal entity, OPEN when the attribution has a loss to bear.
CREATE FUNCTION cargo_fin.check_attribution_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
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
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER pool_apportionment_attribution_consistent AFTER INSERT ON cargo_fin.pool_apportionment_attribution
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_attribution_at_commit();

-- At commit, a job: once CANCELLED it holds no current pool share that is used (one under the latest apportionment
-- covering its line), as fin/000012 has it, and bears no loss in force (one of an attribution under that
-- apportionment). The first is a house still taking part in a line, or a house that left whose former share no
-- apportionment attributed; finance attributes it, or the line is apportioned again leaving the house out. The second
-- is a re-routing loss; finance changes the attribution first (a correction of the line, with its loss 0).
CREATE OR REPLACE FUNCTION cargo_fin.check_job_pool_shares_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.state = 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_fin.pool_apportionment_share sh
       WHERE sh.tenant_id = NEW.tenant_id AND sh.job_id = NEW.job_id AND sh.used
         AND sh.apportionment_no = (SELECT max(l2.apportionment_no) FROM cargo_fin.pool_apportionment_line l2
           WHERE l2.tenant_id = sh.tenant_id AND l2.pool_id = sh.pool_id AND l2.line_no = sh.line_no)) THEN
    RAISE EXCEPTION 'cargo_fin: freight job %/% bears current pool shares; apportion those lines leaving its house out first, or, for a house that left the consolidation, have finance attribute its former share first (reopen the pool if it is closed)', NEW.tenant_id, NEW.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_share_job_open';
  END IF;
  IF NEW.state = 'CANCELLED' AND EXISTS (SELECT 1 FROM cargo_fin.pool_apportionment_attribution ax
       WHERE ax.tenant_id = NEW.tenant_id AND ax.job_id = NEW.job_id AND ax.loss_minor > 0
         AND ax.apportionment_no = (SELECT max(l2.apportionment_no) FROM cargo_fin.pool_apportionment_line l2
           WHERE l2.tenant_id = ax.tenant_id AND l2.pool_id = ax.pool_id AND l2.line_no = ax.line_no)) THEN
    RAISE EXCEPTION 'cargo_fin: freight job %/% bears a re-routing loss in force; have finance change the attribution first (reopen the pool if it is closed, then apportion the line again with the loss changed, with a reason and a voucher)', NEW.tenant_id, NEW.job_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'pool_loss_job_open';
  END IF;
  RETURN NULL;
END $fn$;

-- At commit, a pool's closures are numbered and open as fin/000007 has it, and a CLOSED pool's every line is matched
-- final or released, by invoices neither disputed nor awaiting approval, and its latest apportionment (if any) divided
-- the line's final cost on the ACTUAL basis unless that cost is zero; and now that apportionment leaves nothing to be
-- allocated. Whether a former share of a house that left was attributed is checked by FIN before it closes the pool.
CREATE OR REPLACE FUNCTION cargo_fin.consol_cost_pool_closure_check(t text, id text) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
  p cargo_fin.consol_cost_pool;
  ok boolean;
BEGIN
  SELECT * INTO p FROM cargo_fin.consol_cost_pool WHERE tenant_id = t AND pool_id = id;
  IF p.state = 'CLOSED' THEN
    PERFORM cargo_fin.require_read_committed();
  END IF;
  SELECT count(*) = coalesce(max(c.closure_no), 0)
      AND coalesce(bool_and(c.reopened_at IS NOT NULL OR c.closure_no = (SELECT max(x.closure_no) FROM cargo_fin.consol_cost_pool_closure x
        WHERE x.tenant_id = c.tenant_id AND x.pool_id = c.pool_id)), true)
      AND (p.state = 'CLOSED') = coalesce(bool_or(c.reopened_at IS NULL), false)
    INTO ok FROM cargo_fin.consol_cost_pool_closure c WHERE c.tenant_id = p.tenant_id AND c.pool_id = p.pool_id;
  IF ok AND p.state = 'CLOSED' THEN
    SELECT NOT EXISTS (SELECT 1 FROM cargo_fin.pool_cost_line l
        CROSS JOIN LATERAL cargo_fin.accrual_latest(l.tenant_id, 'POOL_COST', l.pool_id, l.line_no) v
        CROSS JOIN LATERAL cargo_fin.accrual_consumed(l.tenant_id, 'POOL_COST', l.pool_id, l.line_no) c
        LEFT JOIN LATERAL (SELECT al.*, a.stage FROM cargo_fin.pool_apportionment_line al JOIN cargo_fin.pool_apportionment a
            ON a.tenant_id = al.tenant_id AND a.pool_id = al.pool_id AND a.apportionment_no = al.apportionment_no
            JOIN cargo_fin.pool_cost_version av ON av.tenant_id = al.tenant_id AND av.pool_id = al.pool_id AND av.line_no = al.line_no
              AND av.version_no = al.line_version
            WHERE al.tenant_id = l.tenant_id AND al.pool_id = l.pool_id AND al.line_no = l.line_no
            ORDER BY al.apportionment_no DESC LIMIT 1) la ON true
        WHERE l.tenant_id = p.tenant_id AND l.pool_id = p.pool_id
          AND ((v.status <> 'RELEASED' AND NOT c.final) OR c.held
            OR (la.apportionment_no IS NOT NULL AND (la.to_allocate_minor <> 0 OR la.to_allocate_local_minor <> 0))
            OR CASE WHEN la.apportionment_no IS NULL THEN c.amount <> 0 OR c.local <> 0
              ELSE coalesce(la.amount_minor, (SELECT av.amount_minor FROM cargo_fin.pool_cost_version av WHERE av.tenant_id = la.tenant_id
                  AND av.pool_id = la.pool_id AND av.line_no = la.line_no AND av.version_no = la.line_version)) <> c.amount
                OR coalesce(la.local_minor, (SELECT av.local_minor FROM cargo_fin.pool_cost_version av WHERE av.tenant_id = la.tenant_id
                  AND av.pool_id = la.pool_id AND av.line_no = la.line_no AND av.version_no = la.line_version)) <> c.local
                OR ((c.amount <> 0 OR c.local <> 0) AND la.stage <> 'ACTUAL') END))
      INTO ok;
  END IF;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_fin: consol cost pool %/% is % without its costs final', p.tenant_id, p.pool_id, p.state
      USING ERRCODE = 'check_violation', CONSTRAINT = 'consol_cost_pool_closable';
  END IF;
END $fn$;

-- At commit, an apportionment that names a preview, as fin/000015 has it: of the preview's basis revision and stage,
-- not made before it, carrying exactly its lines (line, line version, method, reason, the cost it divided and now the
-- amount to be allocated) and its shares (house, job, whether used, quantity, unit and amounts), and now exactly its
-- attributions (house, job, loss, exception cost, party, reference, voucher, reason, the departure and whether
-- carried). A preview made before this migration has no attributions and nothing to be allocated in its figures. Its
-- own conservation and cost are checked by the rules above and those of fin/000007.
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
