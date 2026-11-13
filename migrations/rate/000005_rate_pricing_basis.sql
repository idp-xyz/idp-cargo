-- IDP-Cargo RATE V0.5 part 2 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- PricingBasis v1 of purposes SHIPMENT_ESTIMATE and CONSOL_COST_ESTIMATE (RATE -> FIN): issued from a rate snapshot in
-- the snapshot's transaction, immutable, and equal to it line for line. QUOTE_ACCEPTED bases are still built from the
-- accepted quote when asked.

-- Snapshot lines name the party of the card that priced them (a supplier, a customer, none for a tariff); lines of
-- earlier snapshots have none.
ALTER TABLE cargo_rate.rate_snapshot_line ADD COLUMN party_id text CHECK (length(btrim(party_id)) BETWEEN 1 AND 128);

CREATE TABLE cargo_rate.pricing_basis (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 basis_id text NOT NULL CHECK (length(btrim(basis_id)) BETWEEN 1 AND 128),
 purpose text NOT NULL CHECK (purpose IN ('SHIPMENT_ESTIMATE','CONSOL_COST_ESTIMATE')),
 legal_entity_id text NOT NULL,
 customer_party_id text,
 customer_revision bigint CHECK (customer_revision > 0),
 subject_kind text NOT NULL CHECK (subject_kind IN ('SHIPMENT','CONSOLIDATION')),
 subject_id text NOT NULL CHECK (length(btrim(subject_id)) BETWEEN 1 AND 128),
 subject_version bigint NOT NULL CHECK (subject_version >= 0),
 snapshot_id text NOT NULL,
 issued_by text NOT NULL CHECK (length(btrim(issued_by)) > 0),
 issued_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, basis_id),
 UNIQUE (tenant_id, snapshot_id),
 FOREIGN KEY (tenant_id, snapshot_id) REFERENCES cargo_rate.rate_snapshot (tenant_id, snapshot_id),
 CONSTRAINT pricing_basis_purpose CHECK (CASE purpose
   WHEN 'SHIPMENT_ESTIMATE' THEN subject_kind = 'SHIPMENT' AND customer_party_id IS NOT NULL AND customer_revision IS NOT NULL
   ELSE subject_kind = 'CONSOLIDATION' AND customer_party_id IS NULL AND customer_revision IS NULL END)
);
CREATE TABLE cargo_rate.pricing_basis_line (
 tenant_id text NOT NULL,
 basis_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 charge_code text NOT NULL CHECK (charge_code ~ '^[A-Z][A-Z0-9_]{1,15}$'),
 sell_currency char(3),
 sell_minor bigint CHECK (sell_minor >= 0),
 sell_line_no integer,
 cost_currency char(3),
 cost_minor bigint CHECK (cost_minor >= 0),
 cost_line_no integer,
 creditor_party_id text,
 PRIMARY KEY (tenant_id, basis_id, line_no),
 UNIQUE (tenant_id, basis_id, charge_code),
 FOREIGN KEY (tenant_id, basis_id) REFERENCES cargo_rate.pricing_basis (tenant_id, basis_id),
 CHECK ((sell_minor IS NULL) = (sell_currency IS NULL) AND (sell_minor IS NULL) = (sell_line_no IS NULL)
   AND (cost_minor IS NULL) = (cost_currency IS NULL) AND (cost_minor IS NULL) = (cost_line_no IS NULL)
   AND (sell_minor IS NOT NULL OR cost_minor IS NOT NULL) AND (creditor_party_id IS NULL) = (cost_minor IS NULL))
);
CREATE TABLE cargo_rate.pricing_basis_gap (
 tenant_id text NOT NULL,
 basis_id text NOT NULL,
 gap_no integer NOT NULL CHECK (gap_no > 0),
 side text NOT NULL CHECK (side IN ('SELL','BUY')),
 charge_code text,
 gap text NOT NULL CHECK (gap IN ('NO_RATE','CALL_FOR_PRICING','BASE_MISSING','BASE_CURRENCY','NO_QUANTITY')),
 PRIMARY KEY (tenant_id, basis_id, gap_no),
 FOREIGN KEY (tenant_id, basis_id) REFERENCES cargo_rate.pricing_basis (tenant_id, basis_id)
);

DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['pricing_basis','pricing_basis_line','pricing_basis_gap'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_rate.%I FOR EACH ROW EXECUTE FUNCTION cargo_rate.reject_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;

-- At commit: a basis is issued in its snapshot's transaction, from a snapshot of its purpose (with the sides the
-- purpose rates), legal entity, customer and subject; its lines and gaps are written with it; every line equals the
-- snapshot lines it names (SELL and BUY of its charge code, the BUY line's party its creditor), every snapshot line is
-- in the basis, as a line or a gap, and the basis has no gap the snapshot lacks. It runs when the basis, its lines or
-- gaps, or the snapshot's lines are written, so making some of these checks immediate cannot slip a change past it.
CREATE FUNCTION cargo_rate.pricing_basis_consistent_check(t text, id text) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE ok boolean;
BEGIN
  SELECT b.xmin = pg_current_xact_id()::xid AND s.xmin = pg_current_xact_id()::xid AND s.purpose = b.purpose
    AND s.sides = CASE b.purpose WHEN 'SHIPMENT_ESTIMATE' THEN '{SELL,BUY}'::text[] ELSE '{BUY}'::text[] END
    AND s.legal_entity_id = b.legal_entity_id AND s.customer_party_id IS NOT DISTINCT FROM b.customer_party_id
    AND s.customer_revision IS NOT DISTINCT FROM b.customer_revision AND s.subject_kind = b.subject_kind AND s.subject_id = b.subject_id
    AND s.subject_version = b.subject_version
    -- every basis line is its snapshot lines
    AND NOT EXISTS (SELECT 1 FROM cargo_rate.pricing_basis_line l WHERE l.tenant_id = t AND l.basis_id = id AND (
      (l.sell_line_no IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_rate.rate_snapshot_line x WHERE x.tenant_id = t AND x.snapshot_id = b.snapshot_id
        AND x.line_no = l.sell_line_no AND x.side = 'SELL' AND x.gap IS NULL AND x.charge_code = l.charge_code AND x.currency = l.sell_currency
        AND x.amount_minor = l.sell_minor))
      OR (l.cost_line_no IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_rate.rate_snapshot_line x WHERE x.tenant_id = t AND x.snapshot_id = b.snapshot_id
        AND x.line_no = l.cost_line_no AND x.side = 'BUY' AND x.gap IS NULL AND x.charge_code = l.charge_code AND x.currency = l.cost_currency
        AND x.amount_minor = l.cost_minor AND x.party_id IS NOT DISTINCT FROM l.creditor_party_id))))
    -- every snapshot line is in the basis
    AND NOT EXISTS (SELECT 1 FROM cargo_rate.rate_snapshot_line x WHERE x.tenant_id = t AND x.snapshot_id = b.snapshot_id AND (
      (x.gap IS NULL AND NOT EXISTS (SELECT 1 FROM cargo_rate.pricing_basis_line l WHERE l.tenant_id = t AND l.basis_id = id
        AND x.line_no IN (l.sell_line_no, l.cost_line_no)))
      OR (x.gap IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_rate.pricing_basis_gap g WHERE g.tenant_id = t AND g.basis_id = id AND g.side = x.side
        AND g.charge_code IS NOT DISTINCT FROM x.charge_code AND g.gap = x.gap))))
    -- and the basis has no gap the snapshot lacks
    AND NOT EXISTS (SELECT 1 FROM cargo_rate.pricing_basis_gap g WHERE g.tenant_id = t AND g.basis_id = id AND NOT EXISTS (
      SELECT 1 FROM cargo_rate.rate_snapshot_line x WHERE x.tenant_id = t AND x.snapshot_id = b.snapshot_id AND x.side = g.side
        AND x.charge_code IS NOT DISTINCT FROM g.charge_code AND x.gap = g.gap))
    AND (SELECT count(*) FROM cargo_rate.pricing_basis_gap g WHERE g.tenant_id = t AND g.basis_id = id)
      = (SELECT count(*) FROM cargo_rate.rate_snapshot_line x WHERE x.tenant_id = t AND x.snapshot_id = b.snapshot_id AND x.gap IS NOT NULL)
    AND (SELECT count(*) = coalesce(max(g.gap_no), 0) FROM cargo_rate.pricing_basis_gap g WHERE g.tenant_id = t AND g.basis_id = id)
    AND (SELECT count(*) = coalesce(max(l.line_no), 0) FROM cargo_rate.pricing_basis_line l WHERE l.tenant_id = t AND l.basis_id = id)
  INTO ok
  FROM cargo_rate.pricing_basis b JOIN cargo_rate.rate_snapshot s ON s.tenant_id = b.tenant_id AND s.snapshot_id = b.snapshot_id
  WHERE b.tenant_id = t AND b.basis_id = id;
  IF ok IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'cargo_rate: pricing basis %/% is not its snapshot as issued', t, id USING ERRCODE = 'check_violation',
      CONSTRAINT = 'pricing_basis_consistent';
  END IF;
END $fn$;
CREATE FUNCTION cargo_rate.check_pricing_basis_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE id text;
BEGIN
  IF TG_TABLE_NAME = 'rate_snapshot_line' THEN
    SELECT b.basis_id INTO id FROM cargo_rate.pricing_basis b WHERE b.tenant_id = NEW.tenant_id AND b.snapshot_id = NEW.snapshot_id;
    IF id IS NULL THEN
      RETURN NULL;
    END IF;
  ELSE
    id := NEW.basis_id;
  END IF;
  PERFORM cargo_rate.pricing_basis_consistent_check(NEW.tenant_id, id);
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER pricing_basis_consistent AFTER INSERT ON cargo_rate.pricing_basis
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_pricing_basis_at_commit();
CREATE CONSTRAINT TRIGGER pricing_basis_line_consistent AFTER INSERT ON cargo_rate.pricing_basis_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_pricing_basis_at_commit();
CREATE CONSTRAINT TRIGGER pricing_basis_gap_consistent AFTER INSERT ON cargo_rate.pricing_basis_gap
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_pricing_basis_at_commit();
CREATE CONSTRAINT TRIGGER rate_snapshot_line_of_its_basis AFTER INSERT ON cargo_rate.rate_snapshot_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_pricing_basis_at_commit();

-- A snapshot line names the party of the card that priced it (rate/000004's check of a snapshot's parts, extended):
-- a supplier's, a customer's, none for a tariff.
CREATE OR REPLACE FUNCTION cargo_rate.check_snapshot_part_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_rate.rate_snapshot s WHERE s.tenant_id = NEW.tenant_id AND s.snapshot_id = NEW.snapshot_id
       AND s.xmin = pg_current_xact_id()::xid AND NEW.side = ANY (s.sides)) THEN
    RAISE EXCEPTION 'cargo_rate: % of snapshot %/% is written with it, for a side it was asked for', TG_TABLE_NAME, NEW.tenant_id, NEW.snapshot_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rate_snapshot_written_once';
  END IF;
  IF TG_TABLE_NAME = 'rate_snapshot_line' AND NEW.rate_card_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_rate.rate_card c
       WHERE c.tenant_id = NEW.tenant_id AND c.rate_card_id = NEW.rate_card_id AND c.party_id IS NOT DISTINCT FROM (to_jsonb(NEW) ->> 'party_id')) THEN
    RAISE EXCEPTION 'cargo_rate: line % of snapshot %/% names a card and party that do not go together', NEW.line_no, NEW.tenant_id, NEW.snapshot_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rate_snapshot_written_once';
  END IF;
  RETURN NULL;
END $fn$;

DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['pricing_basis','pricing_basis_line','pricing_basis_gap'] LOOP
    EXECUTE format('ALTER TABLE cargo_rate.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_rate.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_rate.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
    -- the tenant guard of rate/000004, run before the other checks
    EXECUTE format('CREATE CONSTRAINT TRIGGER %I AFTER INSERT OR UPDATE ON cargo_rate.%I DEFERRABLE INITIALLY DEFERRED FOR EACH ROW '
      'EXECUTE FUNCTION cargo_rate.tenant_scope_at_commit()', '_tenant_scope', tbl);
  END LOOP;
END $rls$;

ALTER TABLE cargo_rate.command_receipt DROP CONSTRAINT command_receipt_result_type;
ALTER TABLE cargo_rate.command_receipt ADD CONSTRAINT command_receipt_result_type
  CHECK (result_type IN ('Quote','ChargeCode','RateCard','RateSnapshot','PricingBasis'));
