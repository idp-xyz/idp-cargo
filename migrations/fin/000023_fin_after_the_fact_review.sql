-- IDP-Cargo FIN V2.102 (batches B, D, E and F) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The after-the-fact review of the LEAN profile: each week, the attributions confirmed in it that have no second person,
-- no voucher or were made by default (F8) are listed for finance's head, who marks the week reviewed with a note. The
-- list itself is a query over the apportionments (it needs no table); this migration holds only the mark.
--
-- Added: two tables, each immutable (rows are only inserted), with row security enabled and forced and written at READ COMMITTED
-- like every table of cargo_fin. after_the_fact_review has one row per review of a legal entity's ISO week. A row says
-- who reviewed the week, the conclusion (ACCEPTED or NOT_ACCEPTED), the note (1 to 2000 characters when given, and given for
-- NOT_ACCEPTED), when, and the size and the SHA-256 digest of the list at that time, so that a list that changes later can be told from the one that was reviewed. A week may be reviewed again when its
-- list changed after the latest review (a command stamped inside the week can commit after the week's end, so that its line
-- is listed and was never reviewed): each review has its own number in the key, from 1 without a gap, and its own size and
-- digest, and after_the_fact_review_line, its child table, has the lines the review covers, that is the list at that time (batch
-- F), so that each line of an ended week says which reviews cover it and a line that no review covers is found; the trigger after_the_fact_review_order refuses a number that is not the next one and a review whose size and digest
-- are those of the latest review of the week (there is nothing new to review). Changed: the result types of
-- command_receipt gain 'AfterTheFactReview'. No row changes and nothing is dropped; the ALTER TABLE takes an ACCESS
-- EXCLUSIVE lock on command_receipt for its brief check of the existing rows (reasoned, not measured).
--
-- What the database refuses: a review whose number is not the next of its legal entity and week, counted from 1 (the primary
-- key and the trigger), or whose list is the one the latest review of the week signed (the trigger); a legal entity with no
-- finance policy (a foreign key); a week that is not written YYYY-Www with a week from 01 to 53, or one that does not
-- exist (week 53 of a year that has 52, which to_char reads as week 01 of the next year); a week_start that is not
-- that week's Monday at 00:00 UTC; a review made before its week began, or before it has ended (a week is reviewed at or
-- after the next Monday at 00:00 UTC, in UTC whatever the session's time zone, so that the list the review signs is the
-- whole week's, not the part that has run); a note of less than 1 or more than 2000
-- characters; a reviewer of less than 1 or more than 128 characters; a negative list size; a digest that is not a
-- SHA-256 in lower-case hexadecimal; a row changed or removed.
--
-- What the schema does not know, and only the domain keeps: that the reviewer is neither the submitter nor the
-- previewer of any line of the week's list (the list is a query over other tables, and a check at commit that read it
-- would take a lock on every apportionment of the week), what the list is (so that the size and the digest are those of
-- the list at the time, and that a list still matches its digest is read by the query), that the bar on the reviewer is
-- applied to every line of the list of each review, that the reviewer holds the
-- permissions ReviewAfterTheFact and ReadProfit, and that the actor is the token's actor.

CREATE TABLE cargo_fin.after_the_fact_review (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL,
 week text NOT NULL,
 review_no integer NOT NULL CONSTRAINT after_the_fact_review_no CHECK (review_no >= 1),
 week_start timestamptz NOT NULL,
 reviewed_by text NOT NULL CHECK (length(btrim(reviewed_by)) BETWEEN 1 AND 128),
 conclusion text NOT NULL CONSTRAINT after_the_fact_review_conclusion CHECK (conclusion IN ('ACCEPTED','NOT_ACCEPTED')),
 note text CONSTRAINT after_the_fact_review_note_length CHECK (note IS NULL OR char_length(btrim(note)) BETWEEN 1 AND 2000),
 list_size integer NOT NULL CHECK (list_size >= 0),
 list_digest char(64) NOT NULL CHECK (list_digest ~ '^[0-9a-f]{64}$'),
 reviewed_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, week, review_no),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 CONSTRAINT after_the_fact_review_week_form CHECK (week ~ '^[0-9]{4}-W(0[1-9]|[1-4][0-9]|5[0-3])$'),
 CONSTRAINT after_the_fact_review_week_start CHECK (
   week_start = date_trunc('week', week_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
   AND week = to_char(week_start AT TIME ZONE 'UTC', 'IYYY-"W"IW')),
 CONSTRAINT after_the_fact_review_week_begun CHECK (reviewed_at >= week_start),
 CONSTRAINT after_the_fact_review_week_ended CHECK ((reviewed_at AT TIME ZONE 'UTC') >= (week_start AT TIME ZONE 'UTC') + interval '7 days'),
 CONSTRAINT after_the_fact_review_note_required CHECK (conclusion <> 'NOT_ACCEPTED' OR note IS NOT NULL),
 CONSTRAINT after_the_fact_review_for_lines UNIQUE (tenant_id, legal_entity_id, week, review_no, week_start)
);

-- The lines a review covers: the list at the time of the review.
CREATE TABLE cargo_fin.after_the_fact_review_line (
 tenant_id text NOT NULL,
 legal_entity_id text NOT NULL,
 week text NOT NULL,
 review_no integer NOT NULL,
 week_start timestamptz NOT NULL,
 pool_id text NOT NULL,
 apportionment_no integer NOT NULL,
 line_no integer NOT NULL,
 shipment_id text NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, week, review_no, pool_id, apportionment_no, line_no, shipment_id),
 FOREIGN KEY (tenant_id, legal_entity_id, week, review_no, week_start)
   REFERENCES cargo_fin.after_the_fact_review (tenant_id, legal_entity_id, week, review_no, week_start),
 FOREIGN KEY (tenant_id, pool_id, apportionment_no, line_no, shipment_id)
   REFERENCES cargo_fin.pool_apportionment_attribution (tenant_id, pool_id, apportionment_no, line_no, shipment_id)
);
CREATE INDEX after_the_fact_review_line_by_line ON cargo_fin.after_the_fact_review_line (tenant_id, pool_id, apportionment_no, line_no, shipment_id);

ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check
    CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile','DebtorReceipt','ConsolCostPool','CreditorInvoice',
      'CreditorPayment','FapiaoRecord','FinSettings','CostReviewRequest','CostReviewResolution','AfterTheFactReview'));

CREATE FUNCTION cargo_fin.check_after_the_fact_review() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE latest cargo_fin.after_the_fact_review;
BEGIN
  SELECT * INTO latest FROM cargo_fin.after_the_fact_review r WHERE r.tenant_id = NEW.tenant_id AND r.legal_entity_id = NEW.legal_entity_id
    AND r.week = NEW.week ORDER BY r.review_no DESC LIMIT 1;
  IF NEW.review_no <> coalesce(latest.review_no, 0) + 1 THEN
    RAISE EXCEPTION 'cargo_fin: review % of %/% week % is not the next review of the week (%)', NEW.review_no, NEW.tenant_id, NEW.legal_entity_id, NEW.week,
      coalesce(latest.review_no, 0) + 1 USING ERRCODE = 'check_violation', CONSTRAINT = 'after_the_fact_review_numbered';
  END IF;
  IF latest.review_no IS NOT NULL AND NEW.list_size = latest.list_size AND NEW.list_digest = latest.list_digest THEN
    RAISE EXCEPTION 'cargo_fin: week % of legal entity % was reviewed (review %) with this very list; there is nothing new to review', NEW.week, NEW.legal_entity_id,
      latest.review_no USING ERRCODE = 'check_violation', CONSTRAINT = 'after_the_fact_review_list_changed';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER after_the_fact_review_order BEFORE INSERT ON cargo_fin.after_the_fact_review
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_after_the_fact_review();
CREATE TRIGGER after_the_fact_review_immutable BEFORE UPDATE OR DELETE ON cargo_fin.after_the_fact_review
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
CREATE TRIGGER after_the_fact_review_read_committed BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.after_the_fact_review
  FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed();
ALTER TABLE cargo_fin.after_the_fact_review ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.after_the_fact_review FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.after_the_fact_review
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- A line of a review: written with its review, of the review's legal entity, stated by an apportionment (not carried forward)
-- entered in the review's week, and listed for a reason, the three reasons the list has.
CREATE FUNCTION cargo_fin.check_after_the_fact_review_line() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  att cargo_fin.pool_apportionment_attribution;
  pool cargo_fin.consol_cost_pool;
  app cargo_fin.pool_apportionment;
  previewer text;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM cargo_fin.after_the_fact_review r WHERE r.tenant_id = NEW.tenant_id AND r.legal_entity_id = NEW.legal_entity_id
       AND r.week = NEW.week AND r.review_no = NEW.review_no AND r.xmin = pg_current_xact_id()::xid) THEN
    RAISE EXCEPTION 'cargo_fin: a line of an after-the-fact review is written with its review' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'after_the_fact_line_with_review';
  END IF;
  SELECT * INTO att FROM cargo_fin.pool_apportionment_attribution x WHERE x.tenant_id = NEW.tenant_id AND x.pool_id = NEW.pool_id
    AND x.apportionment_no = NEW.apportionment_no AND x.line_no = NEW.line_no AND x.shipment_id = NEW.shipment_id;
  SELECT * INTO pool FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id = NEW.tenant_id AND p.pool_id = NEW.pool_id;
  SELECT * INTO app FROM cargo_fin.pool_apportionment a WHERE a.tenant_id = NEW.tenant_id AND a.pool_id = NEW.pool_id AND a.apportionment_no = NEW.apportionment_no;
  IF pool.legal_entity_id IS DISTINCT FROM NEW.legal_entity_id THEN
    RAISE EXCEPTION 'cargo_fin: attribution %/%/%/% is of another legal entity than review % of %', NEW.pool_id, NEW.apportionment_no, NEW.line_no, NEW.shipment_id, NEW.week, NEW.legal_entity_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'after_the_fact_line_entity';
  END IF;
  IF app.entered_at IS NULL OR app.entered_at < NEW.week_start OR app.entered_at >= NEW.week_start + interval '7 days' THEN
    RAISE EXCEPTION 'cargo_fin: attribution %/%/%/% was not stated in week %', NEW.pool_id, NEW.apportionment_no, NEW.line_no, NEW.shipment_id, NEW.week
      USING ERRCODE = 'check_violation', CONSTRAINT = 'after_the_fact_line_week';
  END IF;
  IF att.carried THEN
    RAISE EXCEPTION 'cargo_fin: attribution %/%/%/% is carried forward, not stated by its apportionment, so it is not a line of a review', NEW.pool_id, NEW.apportionment_no, NEW.line_no, NEW.shipment_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'after_the_fact_line_carried';
  END IF;
  SELECT v.made_by INTO previewer FROM cargo_fin.pool_apportionment_preview v WHERE v.tenant_id = app.tenant_id AND v.pool_id = app.pool_id AND v.preview_no = app.preview_no;
  IF NOT (coalesce(previewer = app.made_by, false) OR btrim(att.voucher) = '' OR att.by_default) THEN
    RAISE EXCEPTION 'cargo_fin: attribution %/%/%/% is not on the after-the-fact list: it has a second person, a voucher and was not made by default', NEW.pool_id, NEW.apportionment_no, NEW.line_no, NEW.shipment_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'after_the_fact_line_reason';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER after_the_fact_review_line_check BEFORE INSERT ON cargo_fin.after_the_fact_review_line
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_after_the_fact_review_line();
CREATE TRIGGER after_the_fact_review_line_immutable BEFORE UPDATE OR DELETE ON cargo_fin.after_the_fact_review_line
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
CREATE TRIGGER after_the_fact_review_line_read_committed BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.after_the_fact_review_line
  FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed();
ALTER TABLE cargo_fin.after_the_fact_review_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.after_the_fact_review_line FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.after_the_fact_review_line
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- At commit, a review has as many lines as its list size says.
CREATE FUNCTION cargo_fin.check_after_the_fact_review_lines() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE n integer;
BEGIN
  SELECT count(*) INTO n FROM cargo_fin.after_the_fact_review_line l WHERE l.tenant_id = NEW.tenant_id AND l.legal_entity_id = NEW.legal_entity_id
    AND l.week = NEW.week AND l.review_no = NEW.review_no;
  IF n <> NEW.list_size THEN
    RAISE EXCEPTION 'cargo_fin: review % of %/% week % covers % lines and its list has %', NEW.review_no, NEW.tenant_id, NEW.legal_entity_id, NEW.week, n, NEW.list_size
      USING ERRCODE = 'check_violation', CONSTRAINT = 'after_the_fact_review_lines';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER after_the_fact_review_lines AFTER INSERT ON cargo_fin.after_the_fact_review
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_fin.check_after_the_fact_review_lines();
