-- IDP-Cargo RATE V0.5 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Decision R6 (MD-27, OD-1 to OD-3, OD-8): the charge-code catalogue, rate cards with versions, entries, lines and
-- breaks, and rate snapshots. Parties are ORG party ids with the revision ORG confirmed, locations UN/LOCODEs resolved
-- against the location reference data; there are no cross-schema foreign keys.

-- The starter list every tenant's catalogue starts with (version 0 of a code); RATE's application reads it.
CREATE TABLE cargo_rate.charge_code_starter (
 code text PRIMARY KEY CHECK (code ~ '^[A-Z][A-Z0-9_]{1,15}$'),
 name_zh text NOT NULL,
 name_en text NOT NULL,
 charge_group text NOT NULL,
 calculator text NOT NULL,
 rate_unit text,
 modes text[] NOT NULL
);
INSERT INTO cargo_rate.charge_code_starter VALUES
 ('AIR_FREIGHT','空运费','Air freight','FREIGHT','PER_UNIT','KG_CHARGEABLE','{AIR}'),
 ('FUEL_SURCHARGE','燃油附加费','Fuel surcharge','SURCHARGE','PER_UNIT','KG_CHARGEABLE','{AIR}'),
 ('SECURITY_CHARGE','安检附加费','Security surcharge','SURCHARGE','PER_UNIT','KG_CHARGEABLE','{AIR}'),
 ('THC','起运地码头操作费','Origin terminal handling','ORIGIN','PER_UNIT','KG_CHARGEABLE','{AIR,SEA}'),
 ('HANDLING','操作费','Handling','ORIGIN','FLAT',NULL,'{AIR,SEA}'),
 ('CFS_HANDLING','拼箱站操作费','CFS handling','ORIGIN','PER_UNIT','FREIGHT_TONNE','{SEA}'),
 ('DOC_FEE','文件费','Documentation fee','DOCUMENTATION','FLAT',NULL,'{AIR,SEA}'),
 ('CUSTOMS_CLEARING','报关费','Customs clearance','CUSTOMS','FLAT',NULL,'{AIR,SEA}'),
 ('PICKUP','提货费','Pickup','ORIGIN','FLAT',NULL,'{AIR,SEA}'),
 ('OCEAN_FREIGHT','海运费','Ocean freight','FREIGHT','PER_UNIT','FREIGHT_TONNE','{SEA}'),
 ('DEST_CHARGES','目的港代收费用','Destination charges collected for agents','DESTINATION','FLAT',NULL,'{AIR,SEA}'),
 ('DISCOUNT','折扣','Discount','DISCOUNT','FLAT',NULL,'{AIR,SEA}');

-- A tenant's code versions: each change writes the next version; none changes once written.
CREATE TABLE cargo_rate.charge_code_version (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 code text NOT NULL CHECK (code ~ '^[A-Z][A-Z0-9_]{1,15}$'),
 version integer NOT NULL CHECK (version > 0),
 name_zh text NOT NULL CHECK (length(btrim(name_zh)) BETWEEN 1 AND 100),
 name_en text NOT NULL CHECK (length(btrim(name_en)) BETWEEN 1 AND 100),
 charge_group text NOT NULL CHECK (charge_group IN ('FREIGHT','ORIGIN','DESTINATION','DOCUMENTATION','CUSTOMS','SURCHARGE','DISBURSEMENT','DISCOUNT')),
 calculator text NOT NULL CHECK (calculator IN ('FLAT','PER_UNIT','PERCENT_OF')),
 rate_unit text CHECK (rate_unit IN ('KG_CHARGEABLE','KG_GROSS','CBM','FREIGHT_TONNE','PIECE','DOCUMENT')),
 modes text[] NOT NULL CHECK (cardinality(modes) BETWEEN 1 AND 2 AND modes <@ ARRAY['AIR','SEA']),
 active boolean NOT NULL,
 set_by text NOT NULL CHECK (length(btrim(set_by)) > 0),
 set_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, code, version),
 CHECK ((calculator = 'PER_UNIT') = (rate_unit IS NOT NULL))
);

CREATE TABLE cargo_rate.rate_card (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 rate_card_id text NOT NULL CHECK (length(btrim(rate_card_id)) BETWEEN 1 AND 128),
 side text NOT NULL CHECK (side IN ('SELL','BUY')),
 scope text NOT NULL CHECK (scope IN ('TARIFF','CUSTOMER','SUPPLIER')),
 legal_entity_id text NOT NULL CHECK (length(btrim(legal_entity_id)) BETWEEN 1 AND 128),
 party_id text CHECK (length(btrim(party_id)) BETWEEN 1 AND 128),
 party_revision bigint CHECK (party_revision > 0),
 name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
 created_by text NOT NULL CHECK (length(btrim(created_by)) > 0),
 created_at timestamptz NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, rate_card_id),
 CONSTRAINT rate_card_scope CHECK (CASE scope WHEN 'SUPPLIER' THEN side = 'BUY' ELSE side = 'SELL' END
   AND (scope = 'TARIFF') = (party_id IS NULL) AND (party_id IS NULL) = (party_revision IS NULL))
);

CREATE TABLE cargo_rate.rate_card_version (
 tenant_id text NOT NULL,
 rate_card_id text NOT NULL,
 version_no integer NOT NULL CHECK (version_no > 0),
 status text NOT NULL CHECK (status IN ('DRAFT','PUBLISHED','WITHDRAWN')),
 valid_from timestamptz NOT NULL,
 valid_until timestamptz,
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 source_ref text CHECK (length(source_ref) <= 200),
 filing_ref text CHECK (length(btrim(filing_ref)) BETWEEN 1 AND 100),
 prepared_by text NOT NULL CHECK (length(btrim(prepared_by)) > 0),
 prepared_at timestamptz NOT NULL,
 published_by text,
 published_at timestamptz,
 approved_by text,
 self_approved boolean,
 withdrawn_by text,
 withdrawn_at timestamptz,
 withdraw_reason text CHECK (length(btrim(withdraw_reason)) BETWEEN 1 AND 500),
 PRIMARY KEY (tenant_id, rate_card_id, version_no),
 FOREIGN KEY (tenant_id, rate_card_id) REFERENCES cargo_rate.rate_card (tenant_id, rate_card_id),
 CHECK (valid_until IS NULL OR valid_from < valid_until),
 CONSTRAINT rate_card_version_state CHECK (
   (published_at IS NULL) = (published_by IS NULL) AND (published_at IS NULL) = (status = 'DRAFT')
   AND (withdrawn_at IS NULL) = (withdrawn_by IS NULL) AND (withdrawn_at IS NULL) = (withdraw_reason IS NULL)
   AND (withdrawn_at IS NULL) = (status <> 'WITHDRAWN')
   AND (approved_by IS NULL) = (self_approved IS NULL) AND (approved_by IS NULL OR published_at IS NOT NULL)
   AND (self_approved IS NULL OR self_approved = (approved_by = prepared_by)))
);
-- A card has at most one DRAFT version at a time.
CREATE UNIQUE INDEX rate_card_one_draft ON cargo_rate.rate_card_version (tenant_id, rate_card_id) WHERE status = 'DRAFT';

CREATE TABLE cargo_rate.rate_entry (
 tenant_id text NOT NULL,
 rate_card_id text NOT NULL,
 version_no integer NOT NULL,
 entry_no integer NOT NULL CHECK (entry_no BETWEEN 1 AND 500),
 mode text NOT NULL,
 container_mode text NOT NULL,
 origin text NOT NULL CHECK (origin ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 destination text NOT NULL CHECK (destination ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 via text CHECK (via ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
 edition text NOT NULL CHECK (length(btrim(edition)) > 0),
 carrier_party_id text CHECK (length(btrim(carrier_party_id)) BETWEEN 1 AND 128),
 service_level text NOT NULL CHECK (service_level ~ '^[A-Z][A-Z0-9_]{1,15}$'),
 commodity text NOT NULL CHECK (commodity = 'GENERAL'),
 transit_days integer NOT NULL CHECK (transit_days BETWEEN 0 AND 365),
 frequency text NOT NULL CHECK (length(frequency) <= 60),
 valid_from timestamptz,
 valid_until timestamptz,
 PRIMARY KEY (tenant_id, rate_card_id, version_no, entry_no),
 FOREIGN KEY (tenant_id, rate_card_id, version_no) REFERENCES cargo_rate.rate_card_version (tenant_id, rate_card_id, version_no),
 CHECK ((mode, container_mode) IN (('AIR','LOOSE'),('SEA','LCL'))),
 CHECK (origin <> destination AND (via IS NULL OR (via <> origin AND via <> destination))),
 CHECK ((valid_from IS NULL) = (valid_until IS NULL) AND (valid_from IS NULL OR valid_from < valid_until))
);

CREATE TABLE cargo_rate.rate_line (
 tenant_id text NOT NULL,
 rate_card_id text NOT NULL,
 version_no integer NOT NULL,
 entry_no integer NOT NULL,
 line_no integer NOT NULL CHECK (line_no BETWEEN 1 AND 100),
 charge_code text NOT NULL CHECK (charge_code ~ '^[A-Z][A-Z0-9_]{1,15}$'),
 currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 calculator text NOT NULL CHECK (calculator IN ('FLAT','PER_UNIT','PERCENT_OF')),
 rate_unit text CHECK (rate_unit IN ('KG_CHARGEABLE','KG_GROSS','CBM','FREIGHT_TONNE','PIECE','DOCUMENT')),
 rate_minor bigint NOT NULL CHECK (rate_minor >= 0),
 rate_scale smallint NOT NULL CHECK (rate_scale BETWEEN 0 AND 6),
 minimum_minor bigint NOT NULL CHECK (minimum_minor >= 0),
 maximum_minor bigint CHECK (maximum_minor > 0),
 quantity_step bigint CHECK (quantity_step > 0),
 min_quantity bigint CHECK (min_quantity >= 0),
 volume_factor bigint CHECK (volume_factor > 0),
 percent_bp bigint CHECK (percent_bp BETWEEN 1 AND 1000000),
 base_code text CHECK (base_code ~ '^[A-Z][A-Z0-9_]{1,15}$'),
 call_for_pricing boolean NOT NULL,
 PRIMARY KEY (tenant_id, rate_card_id, version_no, entry_no, line_no),
 UNIQUE (tenant_id, rate_card_id, version_no, entry_no, charge_code),
 FOREIGN KEY (tenant_id, rate_card_id, version_no, entry_no) REFERENCES cargo_rate.rate_entry (tenant_id, rate_card_id, version_no, entry_no),
 CHECK (maximum_minor IS NULL OR maximum_minor >= minimum_minor),
 CONSTRAINT rate_line_calculator CHECK (CASE calculator
   WHEN 'FLAT' THEN rate_unit IS NULL AND quantity_step IS NULL AND min_quantity IS NULL AND volume_factor IS NULL AND percent_bp IS NULL AND base_code IS NULL
   WHEN 'PER_UNIT' THEN rate_unit IS NOT NULL AND quantity_step IS NOT NULL AND min_quantity IS NOT NULL AND percent_bp IS NULL AND base_code IS NULL
     AND (rate_unit IN ('KG_CHARGEABLE','FREIGHT_TONNE')) = (volume_factor IS NOT NULL)
   ELSE rate_unit IS NULL AND quantity_step IS NULL AND min_quantity IS NULL AND volume_factor IS NULL AND rate_minor = 0
     AND base_code IS NOT NULL AND base_code <> charge_code AND (percent_bp IS NOT NULL OR call_for_pricing) END),
 CHECK (NOT call_for_pricing OR (rate_minor = 0 AND minimum_minor = 0 AND maximum_minor IS NULL AND percent_bp IS NULL))
);

CREATE TABLE cargo_rate.rate_break (
 tenant_id text NOT NULL,
 rate_card_id text NOT NULL,
 version_no integer NOT NULL,
 entry_no integer NOT NULL,
 line_no integer NOT NULL,
 from_quantity bigint NOT NULL CHECK (from_quantity > 0),
 rate_minor bigint NOT NULL CHECK (rate_minor >= 0),
 PRIMARY KEY (tenant_id, rate_card_id, version_no, entry_no, line_no, from_quantity),
 FOREIGN KEY (tenant_id, rate_card_id, version_no, entry_no, line_no)
   REFERENCES cargo_rate.rate_line (tenant_id, rate_card_id, version_no, entry_no, line_no)
);

CREATE TABLE cargo_rate.rate_snapshot (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 snapshot_id text NOT NULL CHECK (length(btrim(snapshot_id)) BETWEEN 1 AND 128),
 purpose text NOT NULL CHECK (purpose IN ('QUOTE_LINE','SHIPMENT_ESTIMATE','CONSOL_COST_ESTIMATE')),
 legal_entity_id text NOT NULL,
 sides text[] NOT NULL CHECK (sides IN ('{SELL}','{BUY}','{SELL,BUY}')),
 customer_party_id text,
 customer_revision bigint CHECK (customer_revision > 0),
 carrier_party_id text,
 mode text NOT NULL,
 container_mode text NOT NULL,
 origin text NOT NULL,
 destination text NOT NULL,
 via text,
 edition text NOT NULL,
 service_level text NOT NULL,
 commodity text NOT NULL,
 business_at timestamptz NOT NULL,
 pieces bigint NOT NULL CHECK (pieces >= 0),
 gross_grams bigint NOT NULL CHECK (gross_grams >= 0),
 volume_cm3 bigint NOT NULL CHECK (volume_cm3 >= 0),
 documents bigint NOT NULL CHECK (documents >= 0),
 subject_kind text CHECK (subject_kind IN ('QUOTE','SHIPMENT','CONSOLIDATION')),
 subject_id text,
 subject_version bigint NOT NULL CHECK (subject_version >= 0),
 engine_version text NOT NULL,
 evaluated_by text NOT NULL CHECK (length(btrim(evaluated_by)) > 0),
 recorded_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, snapshot_id),
 CHECK ((customer_party_id IS NULL) = (customer_revision IS NULL) AND (subject_kind IS NULL) = (subject_id IS NULL))
);
CREATE TABLE cargo_rate.rate_snapshot_candidate (
 tenant_id text NOT NULL,
 snapshot_id text NOT NULL,
 candidate_no integer NOT NULL CHECK (candidate_no > 0),
 side text NOT NULL CHECK (side IN ('SELL','BUY')),
 rate_card_id text NOT NULL,
 version_no integer NOT NULL,
 entry_no integer NOT NULL,
 taken boolean NOT NULL,
 reason text NOT NULL,
 totals jsonb NOT NULL DEFAULT '[]',
 PRIMARY KEY (tenant_id, snapshot_id, candidate_no),
 FOREIGN KEY (tenant_id, snapshot_id) REFERENCES cargo_rate.rate_snapshot (tenant_id, snapshot_id)
);
CREATE TABLE cargo_rate.rate_snapshot_line (
 tenant_id text NOT NULL,
 snapshot_id text NOT NULL,
 line_no integer NOT NULL CHECK (line_no > 0),
 side text NOT NULL CHECK (side IN ('SELL','BUY')),
 charge_code text,
 rate_card_id text,
 version_no integer,
 entry_no integer,
 rate_line_no integer,
 calculator text,
 rate_unit text,
 quantity bigint NOT NULL,
 charged bigint NOT NULL,
 band_from bigint NOT NULL,
 rate_minor bigint NOT NULL,
 rate_scale smallint NOT NULL,
 percent_bp bigint NOT NULL,
 base_amount bigint NOT NULL,
 minimum_applied boolean NOT NULL,
 maximum_applied boolean NOT NULL,
 currency char(3),
 amount_minor bigint NOT NULL CHECK (amount_minor >= 0),
 gap text CHECK (gap IN ('NO_RATE','CALL_FOR_PRICING','BASE_MISSING','BASE_CURRENCY')),
 PRIMARY KEY (tenant_id, snapshot_id, line_no),
 FOREIGN KEY (tenant_id, snapshot_id) REFERENCES cargo_rate.rate_snapshot (tenant_id, snapshot_id),
 -- a gap has no amount: RATE never prices a missing rate at zero
 CHECK (gap IS NULL OR amount_minor = 0),
 CHECK ((gap IS NOT DISTINCT FROM 'NO_RATE') = (charge_code IS NULL) AND (charge_code IS NULL) = (rate_card_id IS NULL)
   AND (charge_code IS NULL) = (currency IS NULL))
);

-- What never changes: code versions, snapshots, and card headers but for their revision.
CREATE FUNCTION cargo_rate.reject_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_rate: % rows never change once written', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
DO $immutable$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['charge_code_version','rate_snapshot','rate_snapshot_candidate','rate_snapshot_line'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON cargo_rate.%I FOR EACH ROW EXECUTE FUNCTION cargo_rate.reject_change()',
      tbl || '_immutable', tbl);
  END LOOP;
END $immutable$;
-- A code's versions are written in sequence: version n follows version n-1 (version 1 a starter code or a new one).
CREATE FUNCTION cargo_rate.charge_code_version_in_sequence() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.version > 1 AND NOT EXISTS (SELECT 1 FROM cargo_rate.charge_code_version v WHERE v.tenant_id = NEW.tenant_id AND v.code = NEW.code
       AND v.version = NEW.version - 1) THEN
    RAISE EXCEPTION 'cargo_rate: version % of charge code %/% does not follow version %', NEW.version, NEW.tenant_id, NEW.code, NEW.version - 1
      USING ERRCODE = 'check_violation', CONSTRAINT = 'charge_code_version_sequence';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER charge_code_version_sequence BEFORE INSERT ON cargo_rate.charge_code_version
  FOR EACH ROW EXECUTE FUNCTION cargo_rate.charge_code_version_in_sequence();
CREATE FUNCTION cargo_rate.rate_card_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR (NEW.tenant_id, NEW.rate_card_id, NEW.side, NEW.scope, NEW.legal_entity_id, NEW.party_id, NEW.party_revision, NEW.name,
       NEW.created_by, NEW.created_at) IS DISTINCT FROM (OLD.tenant_id, OLD.rate_card_id, OLD.side, OLD.scope, OLD.legal_entity_id, OLD.party_id,
       OLD.party_revision, OLD.name, OLD.created_by, OLD.created_at) OR NEW.revision <> OLD.revision + 1 THEN
    RAISE EXCEPTION 'cargo_rate: rate card %/% changes only its revision, by one', OLD.tenant_id, OLD.rate_card_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER rate_card_rules BEFORE UPDATE OR DELETE ON cargo_rate.rate_card FOR EACH ROW EXECUTE FUNCTION cargo_rate.rate_card_rules();

-- A version is written with its card, DRAFT; it is then published once and withdrawn once. What it states never
-- changes once published; a DRAFT changes only who prepared it.
CREATE FUNCTION cargo_rate.rate_card_version_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'DRAFT' THEN
      RAISE EXCEPTION 'cargo_rate: a version of rate card %/% is written as a DRAFT', NEW.tenant_id, NEW.rate_card_id USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' OR (NEW.tenant_id, NEW.rate_card_id, NEW.version_no, NEW.valid_from, NEW.valid_until, NEW.currency, NEW.source_ref, NEW.filing_ref)
       IS DISTINCT FROM (OLD.tenant_id, OLD.rate_card_id, OLD.version_no, OLD.valid_from, OLD.valid_until, OLD.currency, OLD.source_ref, OLD.filing_ref)
     OR (OLD.status <> 'DRAFT' AND (NEW.prepared_by, NEW.prepared_at) IS DISTINCT FROM (OLD.prepared_by, OLD.prepared_at))
     OR (OLD.published_at IS NOT NULL AND (NEW.published_by, NEW.published_at, NEW.approved_by, NEW.self_approved)
       IS DISTINCT FROM (OLD.published_by, OLD.published_at, OLD.approved_by, OLD.self_approved))
     OR (OLD.withdrawn_at IS NOT NULL AND (NEW.status, NEW.withdrawn_by, NEW.withdrawn_at, NEW.withdraw_reason)
       IS DISTINCT FROM (OLD.status, OLD.withdrawn_by, OLD.withdrawn_at, OLD.withdraw_reason))
     OR (NEW.status <> OLD.status AND (OLD.status, NEW.status) NOT IN (('DRAFT','PUBLISHED'),('PUBLISHED','WITHDRAWN'))) THEN
    RAISE EXCEPTION 'cargo_rate: version %/%/% changes only from DRAFT to PUBLISHED to WITHDRAWN', OLD.tenant_id, OLD.rate_card_id, OLD.version_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER rate_card_version_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_rate.rate_card_version
  FOR EACH ROW EXECUTE FUNCTION cargo_rate.rate_card_version_rules();

-- Entries, lines and breaks are written and replaced only while their version is a DRAFT; the version row is held
-- FOR SHARE, so a publication waits for them and they wait for a publication.
CREATE FUNCTION cargo_rate.rate_content_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE r record; st text;
BEGIN
  IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION 'cargo_rate: % rows are replaced, not changed', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT v.status INTO st FROM cargo_rate.rate_card_version v
    WHERE v.tenant_id = r.tenant_id AND v.rate_card_id = r.rate_card_id AND v.version_no = r.version_no FOR SHARE;
  IF st IS DISTINCT FROM 'DRAFT' THEN
    RAISE EXCEPTION 'cargo_rate: version %/%/% is %; its rates change only by a new version', r.tenant_id, r.rate_card_id, r.version_no, st
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN r;
END $fn$;
DO $content$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['rate_entry','rate_line','rate_break'] LOOP
    EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON cargo_rate.%I FOR EACH ROW EXECUTE FUNCTION cargo_rate.rate_content_rules()',
      tbl || '_while_draft', tbl);
  END LOOP;
END $content$;

-- At commit, with the card row locked first (so publications of one card take turns) and at READ COMMITTED (so the
-- check reads what committed before it): no two PUBLISHED versions of a card are valid at the same instant; a
-- published version has entries, each with lines; a SELL tariff is approved; a CUSTOMER version is not published
-- here (it is published by accepting its AGREEMENT quote, RATE V0.5 part 3).
CREATE FUNCTION cargo_rate.check_rate_card_version_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE c record;
BEGIN
  IF current_setting('transaction_isolation') <> 'read committed' THEN
    RAISE EXCEPTION 'cargo_rate: RATE''s commit checks run at READ COMMITTED, not %', current_setting('transaction_isolation')
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rate_read_committed';
  END IF;
  SELECT k.scope INTO c FROM cargo_rate.rate_card k WHERE k.tenant_id = NEW.tenant_id AND k.rate_card_id = NEW.rate_card_id FOR NO KEY UPDATE;
  IF NOT FOUND OR EXISTS (SELECT 1 FROM cargo_rate.rate_card_version v WHERE v.tenant_id = NEW.tenant_id AND v.rate_card_id = NEW.rate_card_id
       AND v.status = 'PUBLISHED' AND EXISTS (SELECT 1 FROM cargo_rate.rate_card_version o WHERE o.tenant_id = v.tenant_id
         AND o.rate_card_id = v.rate_card_id AND o.version_no <> v.version_no AND o.status = 'PUBLISHED'
         AND tstzrange(o.valid_from, o.valid_until) && tstzrange(v.valid_from, v.valid_until))) THEN
    RAISE EXCEPTION 'cargo_rate: published versions of rate card %/% overlap', NEW.tenant_id, NEW.rate_card_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rate_version_overlap';
  END IF;
  IF NEW.status = 'PUBLISHED' AND (c.scope = 'CUSTOMER' OR (c.scope = 'TARIFF') <> (NEW.approved_by IS NOT NULL)
       OR NOT EXISTS (SELECT 1 FROM cargo_rate.rate_entry e WHERE e.tenant_id = NEW.tenant_id AND e.rate_card_id = NEW.rate_card_id
         AND e.version_no = NEW.version_no)
       OR EXISTS (SELECT 1 FROM cargo_rate.rate_entry e WHERE e.tenant_id = NEW.tenant_id AND e.rate_card_id = NEW.rate_card_id
         AND e.version_no = NEW.version_no AND NOT EXISTS (SELECT 1 FROM cargo_rate.rate_line l WHERE l.tenant_id = e.tenant_id
           AND l.rate_card_id = e.rate_card_id AND l.version_no = e.version_no AND l.entry_no = e.entry_no))) THEN
    RAISE EXCEPTION 'cargo_rate: version %/%/% is published without entries, lines or the approval its card needs', NEW.tenant_id, NEW.rate_card_id,
      NEW.version_no USING ERRCODE = 'check_violation', CONSTRAINT = 'rate_version_publishable';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER rate_card_version_consistent AFTER INSERT OR UPDATE ON cargo_rate.rate_card_version
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_rate.check_rate_card_version_at_commit();

-- Tenant isolation as in 000001. Code versions, cards and snapshots are written with their command receipt.
DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['charge_code_version','rate_card','rate_card_version','rate_entry','rate_line','rate_break','rate_snapshot',
      'rate_snapshot_candidate','rate_snapshot_line'] LOOP
    EXECUTE format('ALTER TABLE cargo_rate.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_rate.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_rate.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
  END LOOP;
END $rls$;

-- At commit, a row of any RATE table with a tenant is of the tenant the transaction is bound to, as in cargo_fin
-- (fin/000008): a transaction that rebinds app.tenant_id before committing cannot make the commit checks read nothing.
-- Roles that bypass row-level security (the migrator) are not bound. The trigger's name sorts before the other checks.
CREATE FUNCTION cargo_rate.tenant_scope_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF current_setting('app.tenant_id', true) IS DISTINCT FROM NEW.tenant_id AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles r
       WHERE r.rolname = current_user AND (r.rolsuper OR r.rolbypassrls)) THEN
    RAISE EXCEPTION 'cargo_rate: a row of % of tenant % is committed while the transaction is bound to tenant %', TG_TABLE_NAME, NEW.tenant_id,
      current_setting('app.tenant_id', true) USING ERRCODE = 'check_violation', CONSTRAINT = 'rate_tenant_scope';
  END IF;
  RETURN NULL;
END $fn$;
DO $scope$
DECLARE tbl text;
BEGIN
  FOR tbl IN SELECT c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
      WHERE n.nspname = 'cargo_rate' AND c.relkind = 'r' AND EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = c.oid
        AND a.attname = 'tenant_id' AND NOT a.attisdropped) ORDER BY c.relname LOOP
    EXECUTE format('CREATE CONSTRAINT TRIGGER %I AFTER INSERT OR UPDATE ON cargo_rate.%I DEFERRABLE INITIALLY DEFERRED FOR EACH ROW '
      'EXECUTE FUNCTION cargo_rate.tenant_scope_at_commit()', '_tenant_scope', tbl);
  END LOOP;
END $scope$;

ALTER TABLE cargo_rate.command_receipt ADD CONSTRAINT command_receipt_result_type
  CHECK (result_type IN ('Quote','ChargeCode','RateCard','RateSnapshot'));
