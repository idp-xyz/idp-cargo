-- IDP-Cargo FIN V0.7 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- Fapiao records (FB6): a record covers issued debit notes of one legal entity and one Chinese enterprise debtor and
-- freezes the buyer data of the debtor's billing profile version. The national e-invoice platform issues the fapiao; an
-- authorized operator records its number, date and evidence (ISSUED), or its failure (FAILED), and later the red-letter
-- fapiao that reverses it (RED_LETTERED). A debit note is covered by at most one record that is requested or issued, and
-- no credit note reverses a line of it the fapiao states (all but pass-through disbursements) while it is. Accounting
-- records, kept 30 years and never erased.

CREATE TABLE cargo_fin.fapiao_record (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 fapiao_id text NOT NULL CHECK (length(btrim(fapiao_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL,
 debtor_party_id text NOT NULL,
 debtor_revision bigint NOT NULL CHECK (debtor_revision > 0),
 fapiao_type text NOT NULL CHECK (fapiao_type IN ('ORDINARY','SPECIAL')),
 currency char(3) NOT NULL CHECK (currency = 'CNY'),
 profile_version integer NOT NULL CHECK (profile_version > 0),
 buyer_title text NOT NULL CHECK (length(btrim(buyer_title)) BETWEEN 1 AND 300),
 taxpayer_number text NOT NULL CHECK (taxpayer_number ~ '^[0-9A-HJ-NP-RTUW-Y]{18}$'),
 buyer_address text,
 buyer_phone text,
 bank_name text,
 bank_account text,
 gross_minor bigint NOT NULL CHECK (gross_minor > 0 AND gross_minor <= 1000000000000000),
 vat_minor bigint NOT NULL CHECK (vat_minor >= 0 AND vat_minor <= gross_minor),
 state text NOT NULL CHECK (state IN ('REQUESTED','ISSUED','FAILED','RED_LETTERED')),
 requested_by text NOT NULL CHECK (length(btrim(requested_by)) > 0),
 requested_at timestamptz NOT NULL,
 entered_at timestamptz NOT NULL,
 issued_number text CHECK (issued_number ~ '^[0-9]{20}$'),
 issued_on date,
 issued_evidence_ref text CHECK (issued_evidence_ref ~ '^[A-Za-z0-9]([A-Za-z0-9 ._:/#-]{0,198}[A-Za-z0-9._:/#-])?$'),
 issued_by text CHECK (length(btrim(issued_by)) > 0),
 issued_at timestamptz,
 issued_entered_at timestamptz,
 failed_reason text CHECK (length(btrim(failed_reason)) BETWEEN 1 AND 500),
 failed_by text CHECK (length(btrim(failed_by)) > 0),
 failed_at timestamptz,
 failed_entered_at timestamptz,
 red_number text CHECK (red_number ~ '^[0-9]{20}$'),
 red_on date,
 red_evidence_ref text CHECK (red_evidence_ref ~ '^[A-Za-z0-9]([A-Za-z0-9 ._:/#-]{0,198}[A-Za-z0-9._:/#-])?$'),
 red_confirmation_ref text CHECK (red_confirmation_ref ~ '^[A-Za-z0-9]([A-Za-z0-9 ._:/#-]{0,198}[A-Za-z0-9._:/#-])?$'),
 red_reason text CHECK (length(btrim(red_reason)) BETWEEN 1 AND 500),
 red_by text CHECK (length(btrim(red_by)) > 0),
 red_at timestamptz,
 red_entered_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id, fapiao_id),
 FOREIGN KEY (tenant_id, debtor_party_id, profile_version) REFERENCES cargo_fin.billing_profile_version (tenant_id, debtor_party_id, version_no),
 CONSTRAINT fapiao_record_state CHECK (
   ((state IN ('ISSUED','RED_LETTERED')) = (issued_number IS NOT NULL)) AND (issued_number IS NULL) = (issued_on IS NULL)
   AND (issued_number IS NULL) = (issued_evidence_ref IS NULL) AND (issued_number IS NULL) = (issued_by IS NULL)
   AND (issued_number IS NULL) = (issued_at IS NULL) AND (issued_number IS NULL) = (issued_entered_at IS NULL)
   AND ((state = 'FAILED') = (failed_reason IS NOT NULL)) AND (failed_reason IS NULL) = (failed_by IS NULL)
   AND (failed_reason IS NULL) = (failed_at IS NULL) AND (failed_reason IS NULL) = (failed_entered_at IS NULL)
   AND ((state = 'RED_LETTERED') = (red_number IS NOT NULL)) AND (red_number IS NULL) = (red_on IS NULL)
   AND (red_number IS NULL) = (red_evidence_ref IS NULL) AND (red_number IS NULL) = (red_reason IS NULL)
   AND (red_number IS NULL) = (red_by IS NULL) AND (red_number IS NULL) = (red_at IS NULL) AND (red_number IS NULL) = (red_entered_at IS NULL)
   AND (red_number IS NULL) = (red_confirmation_ref IS NULL)
   AND (red_number IS NULL OR (red_number <> issued_number AND red_on >= issued_on AND red_at >= issued_at))
   AND (issued_at IS NULL OR issued_at >= requested_at) AND (failed_at IS NULL OR failed_at >= requested_at)),
 CONSTRAINT fapiao_record_bank CHECK ((bank_name IS NULL) = (bank_account IS NULL))
);
CREATE INDEX fapiao_record_by_debtor ON cargo_fin.fapiao_record (tenant_id, legal_entity_id, debtor_party_id);

-- The debit notes a record covers; released once the record failed or was red-lettered.
CREATE TABLE cargo_fin.fapiao_covered_note (
 tenant_id text NOT NULL,
 fapiao_id text NOT NULL,
 invoice_id text NOT NULL,
 released boolean NOT NULL DEFAULT false,
 PRIMARY KEY (tenant_id, fapiao_id, invoice_id),
 FOREIGN KEY (tenant_id, fapiao_id) REFERENCES cargo_fin.fapiao_record (tenant_id, fapiao_id),
 FOREIGN KEY (tenant_id, invoice_id) REFERENCES cargo_fin.debtor_invoice (tenant_id, invoice_id)
);
-- a debit note is covered by one record that is requested or issued
CREATE UNIQUE INDEX fapiao_covered_note_once ON cargo_fin.fapiao_covered_note (tenant_id, invoice_id) WHERE NOT released;

-- What a covered debit note puts on the fapiao per tax treatment and rate (0 for none), in CNY: its lines not reversed,
-- pass-through disbursements left off.
CREATE TABLE cargo_fin.fapiao_covered_amount (
 tenant_id text NOT NULL,
 fapiao_id text NOT NULL,
 invoice_id text NOT NULL,
 treatment text NOT NULL CHECK (treatment IN ('EXEMPT_INTL_FREIGHT_FORWARDING','TAXABLE')),
 rate_bp integer NOT NULL CHECK (CASE treatment WHEN 'TAXABLE' THEN rate_bp BETWEEN 1 AND 3000 ELSE rate_bp = 0 END),
 local_minor bigint NOT NULL CHECK (local_minor <> 0 AND abs(local_minor) <= 1000000000000000),
 PRIMARY KEY (tenant_id, fapiao_id, invoice_id, treatment, rate_bp),
 FOREIGN KEY (tenant_id, fapiao_id, invoice_id) REFERENCES cargo_fin.fapiao_covered_note (tenant_id, fapiao_id, invoice_id)
);

-- The platform's fapiao numbers a tenant recorded, issued and red-letter alike: each once (fully digital e-invoice
-- numbers are unique nationwide).
CREATE TABLE cargo_fin.fapiao_number (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 number text NOT NULL CHECK (number ~ '^[0-9]{20}$'),
 fapiao_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('ISSUED','RED_LETTER')),
 PRIMARY KEY (tenant_id, number),
 UNIQUE (tenant_id, fapiao_id, kind),
 FOREIGN KEY (tenant_id, fapiao_id) REFERENCES cargo_fin.fapiao_record (tenant_id, fapiao_id)
);

ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check
    CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile','DebtorReceipt','ConsolCostPool','CreditorInvoice',
      'CreditorPayment','FapiaoRecord'));

-- A record keeps what it was requested on; only its issue, failure and red letter are added, in order, with its
-- revision and update time. Covered notes are only released, once the record failed or was red-lettered; amounts and
-- numbers never change. Nothing is deleted.
CREATE FUNCTION cargo_fin.fapiao_record_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  moves text[] := ARRAY['state','issued_number','issued_on','issued_evidence_ref','issued_by','issued_at','issued_entered_at','failed_reason',
    'failed_by','failed_at','failed_entered_at','red_number','red_on','red_evidence_ref','red_confirmation_ref','red_reason','red_by','red_at',
    'red_entered_at','revision','updated_at'];
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_fin: fapiao records are never deleted' USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'REQUESTED' THEN
      RAISE EXCEPTION 'cargo_fin: a fapiao record is requested first' USING ERRCODE = 'check_violation', CONSTRAINT = 'fapiao_record_state';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - moves IS DISTINCT FROM to_jsonb(OLD) - moves
     OR (NEW.state <> OLD.state AND (OLD.state, NEW.state) NOT IN (('REQUESTED','ISSUED'),('REQUESTED','FAILED'),('ISSUED','RED_LETTERED')))
     OR (OLD.issued_number IS NOT NULL AND (NEW.issued_number, NEW.issued_on, NEW.issued_evidence_ref, NEW.issued_by, NEW.issued_at, NEW.issued_entered_at)
       IS DISTINCT FROM (OLD.issued_number, OLD.issued_on, OLD.issued_evidence_ref, OLD.issued_by, OLD.issued_at, OLD.issued_entered_at))
     OR (OLD.state IN ('FAILED','RED_LETTERED') AND to_jsonb(NEW) - ARRAY['revision','updated_at'] IS DISTINCT FROM to_jsonb(OLD) - ARRAY['revision','updated_at']) THEN
    RAISE EXCEPTION 'cargo_fin: fapiao record %/% changes only from REQUESTED to ISSUED or FAILED and from ISSUED to RED_LETTERED', OLD.tenant_id,
      OLD.fapiao_id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER fapiao_record_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.fapiao_record
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.fapiao_record_rules();

CREATE FUNCTION cargo_fin.fapiao_covered_note_rules() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'DELETE' OR (TG_OP = 'UPDATE' AND ((NEW.tenant_id, NEW.fapiao_id, NEW.invoice_id) IS DISTINCT FROM (OLD.tenant_id, OLD.fapiao_id, OLD.invoice_id)
       OR OLD.released OR NOT NEW.released
       OR NOT EXISTS (SELECT 1 FROM cargo_fin.fapiao_record r WHERE r.tenant_id = NEW.tenant_id AND r.fapiao_id = NEW.fapiao_id
         AND r.state IN ('FAILED','RED_LETTERED')))) THEN
    RAISE EXCEPTION 'cargo_fin: a covered debit note is only released, once its fapiao record failed or was red-lettered'
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'INSERT' AND NEW.released THEN
    RAISE EXCEPTION 'cargo_fin: a debit note is covered when the record is requested' USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER fapiao_covered_note_rules BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.fapiao_covered_note
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.fapiao_covered_note_rules();
CREATE FUNCTION cargo_fin.reject_fapiao_change() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'cargo_fin: % rows never change once written', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $fn$;
CREATE TRIGGER fapiao_covered_amount_immutable BEFORE UPDATE OR DELETE ON cargo_fin.fapiao_covered_amount
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_fapiao_change();
CREATE TRIGGER fapiao_number_immutable BEFORE UPDATE OR DELETE ON cargo_fin.fapiao_number
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_fapiao_change();

-- At commit, as a record is written or moves on: requested on the debtor's latest billing profile version, of a
-- CN_ENTERPRISE buyer, which it equals, in the legal entity's functional currency, with covered notes whose amounts give
-- its totals (the VAT of each taxable rate rounded half up once); a SPECIAL fapiao without exempt amounts; issued on or
-- after the covered notes' dates; issued and red-letter numbers recorded with the move; a failed or red-lettered record
-- releases its notes.
CREATE FUNCTION cargo_fin.check_fapiao_record_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE gross bigint; vat bigint;
BEGIN
  IF TG_OP = 'INSERT' THEN
    SELECT coalesce(sum(s), 0), coalesce(sum(CASE WHEN treatment = 'TAXABLE' AND s > 0 THEN (s * rate_bp + (10000 + rate_bp) / 2) / (10000 + rate_bp) ELSE 0 END), 0)
      INTO gross, vat FROM (SELECT a.treatment, a.rate_bp, sum(a.local_minor)::bigint s FROM cargo_fin.fapiao_covered_amount a
        WHERE a.tenant_id = NEW.tenant_id AND a.fapiao_id = NEW.fapiao_id GROUP BY a.treatment, a.rate_bp) g;
    IF NOT EXISTS (SELECT 1 FROM cargo_fin.billing_profile_version v WHERE v.tenant_id = NEW.tenant_id AND v.debtor_party_id = NEW.debtor_party_id
         AND v.version_no = NEW.profile_version AND v.buyer_kind = 'CN_ENTERPRISE' AND v.title = NEW.buyer_title AND v.taxpayer_number = NEW.taxpayer_number
         AND v.registered_address IS NOT DISTINCT FROM NEW.buyer_address AND v.registered_phone IS NOT DISTINCT FROM NEW.buyer_phone
         AND v.bank_name IS NOT DISTINCT FROM NEW.bank_name AND v.bank_account IS NOT DISTINCT FROM NEW.bank_account)
       OR EXISTS (SELECT 1 FROM cargo_fin.billing_profile_version v WHERE v.tenant_id = NEW.tenant_id AND v.debtor_party_id = NEW.debtor_party_id
         AND v.version_no > NEW.profile_version)
       OR NOT EXISTS (SELECT 1 FROM cargo_fin.finance_policy p WHERE p.tenant_id = NEW.tenant_id AND p.legal_entity_id = NEW.legal_entity_id
         AND p.functional_currency = NEW.currency)
       OR NOT EXISTS (SELECT 1 FROM cargo_fin.fapiao_covered_note c WHERE c.tenant_id = NEW.tenant_id AND c.fapiao_id = NEW.fapiao_id)
       OR gross <> NEW.gross_minor OR vat <> NEW.vat_minor
       OR EXISTS (SELECT 1 FROM (SELECT a.treatment, a.rate_bp, sum(a.local_minor)::bigint s FROM cargo_fin.fapiao_covered_amount a
         WHERE a.tenant_id = NEW.tenant_id AND a.fapiao_id = NEW.fapiao_id GROUP BY a.treatment, a.rate_bp) g
         WHERE g.s < 0 OR (NEW.fapiao_type = 'SPECIAL' AND g.treatment = 'EXEMPT_INTL_FREIGHT_FORWARDING' AND g.s <> 0)) THEN
      RAISE EXCEPTION 'cargo_fin: fapiao record %/% is not its billing profile version, covered notes and totals', NEW.tenant_id, NEW.fapiao_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'fapiao_record_consistent';
    END IF;
  END IF;
  IF (NEW.issued_number IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_fin.fapiao_number n WHERE n.tenant_id = NEW.tenant_id
        AND n.legal_entity_id = NEW.legal_entity_id AND n.number = NEW.issued_number AND n.fapiao_id = NEW.fapiao_id AND n.kind = 'ISSUED'))
     OR (NEW.red_number IS NOT NULL AND NOT EXISTS (SELECT 1 FROM cargo_fin.fapiao_number n WHERE n.tenant_id = NEW.tenant_id
        AND n.legal_entity_id = NEW.legal_entity_id AND n.number = NEW.red_number AND n.fapiao_id = NEW.fapiao_id AND n.kind = 'RED_LETTER'))
     OR (NEW.state IN ('FAILED','RED_LETTERED') AND EXISTS (SELECT 1 FROM cargo_fin.fapiao_covered_note c WHERE c.tenant_id = NEW.tenant_id
        AND c.fapiao_id = NEW.fapiao_id AND NOT c.released))
     OR (NEW.issued_on IS NOT NULL AND EXISTS (SELECT 1 FROM cargo_fin.fapiao_covered_note c JOIN cargo_fin.debtor_invoice n
        ON n.tenant_id = c.tenant_id AND n.invoice_id = c.invoice_id WHERE c.tenant_id = NEW.tenant_id AND c.fapiao_id = NEW.fapiao_id
        AND n.invoice_date > NEW.issued_on)) THEN
    RAISE EXCEPTION 'cargo_fin: fapiao record %/% is issued on or after its notes, records its numbers and releases its notes as it moves on',
      NEW.tenant_id, NEW.fapiao_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'fapiao_record_consistent';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER fapiao_record_consistent AFTER INSERT OR UPDATE ON cargo_fin.fapiao_record
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_fapiao_record_at_commit();

-- As a covered note or its amounts are written, at once: of a record written in this transaction, requested.
CREATE FUNCTION cargo_fin.check_fapiao_part() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.fapiao_record r WHERE r.tenant_id = NEW.tenant_id AND r.fapiao_id = NEW.fapiao_id
       AND r.state = 'REQUESTED' AND r.xmin = pg_current_xact_id()::xid AND r.created_at = now() AND r.revision = 1) THEN
    RAISE EXCEPTION 'cargo_fin: % of fapiao record %/% is written with the record', TG_TABLE_NAME, NEW.tenant_id, NEW.fapiao_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'fapiao_written_once';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER fapiao_covered_note_written_once BEFORE INSERT ON cargo_fin.fapiao_covered_note FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_fapiao_part();
CREATE TRIGGER fapiao_covered_amount_written_once BEFORE INSERT ON cargo_fin.fapiao_covered_amount FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_fapiao_part();

-- A number is written with the move that records it.
CREATE FUNCTION cargo_fin.check_fapiao_number() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.fapiao_record r WHERE r.tenant_id = NEW.tenant_id AND r.fapiao_id = NEW.fapiao_id
       AND r.legal_entity_id = NEW.legal_entity_id AND r.xmin = pg_current_xact_id()::xid
       AND CASE NEW.kind WHEN 'ISSUED' THEN r.issued_number = NEW.number AND r.state = 'ISSUED' ELSE r.red_number = NEW.number END) THEN
    RAISE EXCEPTION 'cargo_fin: fapiao number % is recorded with its record %/%', NEW.number, NEW.tenant_id, NEW.fapiao_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'fapiao_written_once';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER fapiao_number_with_record BEFORE INSERT ON cargo_fin.fapiao_number FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_fapiao_number();

-- At commit, with the debit note's job locked first (so that its credit notes and fapiao records take turns): a covered
-- note is an ISSUED debit note of the record's legal entity and debtor, and puts on the fapiao per treatment and rate
-- exactly what its lines not reversed bill, pass-through disbursements left off, and something.
CREATE FUNCTION cargo_fin.check_fapiao_covered_note_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id
    AND j.job_id = (SELECT n.job_id FROM cargo_fin.debtor_invoice n WHERE n.tenant_id = NEW.tenant_id AND n.invoice_id = NEW.invoice_id) FOR NO KEY UPDATE;
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n JOIN cargo_fin.fapiao_record r ON r.tenant_id = n.tenant_id AND r.fapiao_id = NEW.fapiao_id
       WHERE n.tenant_id = NEW.tenant_id AND n.invoice_id = NEW.invoice_id AND n.kind = 'DEBIT_NOTE' AND n.state = 'ISSUED'
         AND n.legal_entity_id = r.legal_entity_id AND n.debtor_party_id = r.debtor_party_id)
     OR NOT EXISTS (SELECT 1 FROM cargo_fin.fapiao_covered_amount a WHERE a.tenant_id = NEW.tenant_id AND a.fapiao_id = NEW.fapiao_id
         AND a.invoice_id = NEW.invoice_id)
     OR EXISTS (
       (SELECT l.treatment, coalesce(l.rate_bp, 0), sum(l.local_minor)::bigint FROM cargo_fin.debtor_invoice_line l
         WHERE l.tenant_id = NEW.tenant_id AND (l.invoice_id = NEW.invoice_id OR l.credits_invoice_id = NEW.invoice_id)
           AND l.treatment <> 'PASS_THROUGH_DISBURSEMENT'
         GROUP BY l.treatment, coalesce(l.rate_bp, 0) HAVING sum(l.local_minor) <> 0
        EXCEPT SELECT a.treatment, a.rate_bp, a.local_minor FROM cargo_fin.fapiao_covered_amount a
         WHERE a.tenant_id = NEW.tenant_id AND a.fapiao_id = NEW.fapiao_id AND a.invoice_id = NEW.invoice_id)
       UNION ALL
       (SELECT a.treatment, a.rate_bp, a.local_minor FROM cargo_fin.fapiao_covered_amount a
         WHERE a.tenant_id = NEW.tenant_id AND a.fapiao_id = NEW.fapiao_id AND a.invoice_id = NEW.invoice_id
        EXCEPT SELECT l.treatment, coalesce(l.rate_bp, 0), sum(l.local_minor)::bigint FROM cargo_fin.debtor_invoice_line l
         WHERE l.tenant_id = NEW.tenant_id AND (l.invoice_id = NEW.invoice_id OR l.credits_invoice_id = NEW.invoice_id)
           AND l.treatment <> 'PASS_THROUGH_DISBURSEMENT'
         GROUP BY l.treatment, coalesce(l.rate_bp, 0) HAVING sum(l.local_minor) <> 0)) THEN
    RAISE EXCEPTION 'cargo_fin: debit note %/% is not an issued note of the record''s legal entity and debtor, or contributes other amounts',
      NEW.tenant_id, NEW.invoice_id USING ERRCODE = 'check_violation', CONSTRAINT = 'fapiao_covers_issued_notes';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER fapiao_covers_issued_notes AFTER INSERT ON cargo_fin.fapiao_covered_note
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_fapiao_covered_note_at_commit();

-- At commit, with the job locked first: no credit note line reverses a line a fapiao states (all but pass-through
-- disbursements) of a debit note a requested or issued fapiao record covers (the red-letter fapiao is recorded first,
-- CN e-invoice rules).
CREATE FUNCTION cargo_fin.check_credit_line_fapiao_at_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.credits_invoice_id IS NULL OR NEW.treatment = 'PASS_THROUGH_DISBURSEMENT' THEN
    RETURN NULL;
  END IF;
  PERFORM 1 FROM cargo_fin.freight_job j WHERE j.tenant_id = NEW.tenant_id AND j.job_id = NEW.job_id FOR NO KEY UPDATE;
  IF EXISTS (SELECT 1 FROM cargo_fin.fapiao_covered_note c WHERE c.tenant_id = NEW.tenant_id AND c.invoice_id = NEW.credits_invoice_id AND NOT c.released) THEN
    RAISE EXCEPTION 'cargo_fin: credit note %/% reverses a line of debit note % that a fapiao record covers', NEW.tenant_id, NEW.invoice_id,
      NEW.credits_invoice_id USING ERRCODE = 'check_violation', CONSTRAINT = 'fapiao_covers_debit_note';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER debtor_invoice_line_fapiao AFTER INSERT ON cargo_fin.debtor_invoice_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_credit_line_fapiao_at_commit();

DO $tables$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['fapiao_record','fapiao_covered_note','fapiao_covered_amount','fapiao_number'] LOOP
    EXECUTE format('ALTER TABLE cargo_fin.%I ENABLE ROW LEVEL SECURITY', tbl);
    EXECUTE format('ALTER TABLE cargo_fin.%I FORCE ROW LEVEL SECURITY', tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_fin.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))', tbl
    );
    EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.%I FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed()',
      tbl || '_read_committed', tbl);
    EXECUTE format('CREATE CONSTRAINT TRIGGER %I AFTER INSERT OR UPDATE ON cargo_fin.%I DEFERRABLE INITIALLY DEFERRED FOR EACH ROW '
      'EXECUTE FUNCTION cargo_fin.tenant_scope_at_commit()', '_tenant_scope', tbl);
  END LOOP;
END $tables$;
