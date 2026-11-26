-- IDP-Cargo FIN V2.102 (batch B) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- The after-the-fact review of the LEAN profile: each week, the attributions confirmed in it that have no second person,
-- no voucher or were made by default (F8) are listed for finance's head, who marks the week reviewed with a note. The
-- list itself is a query over the apportionments (it needs no table); this migration holds only the mark.
--
-- Added: one table, after_the_fact_review: one row per legal entity and ISO week, immutable (rows are only inserted),
-- with row security enabled and forced and written at READ COMMITTED like every table of cargo_fin. A row says who
-- reviewed the week, the note (1 to 2000 characters), when, and the size and the SHA-256 digest of the list at that
-- time, so that a list that changes later can be told from the one that was reviewed. Changed: the result types of
-- command_receipt gain 'AfterTheFactReview'. No row changes and nothing is dropped; the ALTER TABLE takes an ACCESS
-- EXCLUSIVE lock on command_receipt for its brief check of the existing rows (reasoned, not measured).
--
-- What the database refuses: a second review of one legal entity and week (the primary key); a legal entity with no
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
-- the list at the time, and that a list still matches its digest is read by the query), that the reviewer holds the
-- permissions ReviewAfterTheFact and ReadProfit, and that the actor is the token's actor.

CREATE TABLE cargo_fin.after_the_fact_review (
 tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) BETWEEN 1 AND 128),
 legal_entity_id text NOT NULL,
 week text NOT NULL,
 week_start timestamptz NOT NULL,
 reviewed_by text NOT NULL CHECK (length(btrim(reviewed_by)) BETWEEN 1 AND 128),
 note text NOT NULL CHECK (length(btrim(note)) BETWEEN 1 AND 2000),
 list_size integer NOT NULL CHECK (list_size >= 0),
 list_digest char(64) NOT NULL CHECK (list_digest ~ '^[0-9a-f]{64}$'),
 reviewed_at timestamptz NOT NULL,
 PRIMARY KEY (tenant_id, legal_entity_id, week),
 FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES cargo_fin.finance_policy (tenant_id, legal_entity_id),
 CONSTRAINT after_the_fact_review_week_form CHECK (week ~ '^[0-9]{4}-W(0[1-9]|[1-4][0-9]|5[0-3])$'),
 CONSTRAINT after_the_fact_review_week_start CHECK (
   week_start = date_trunc('week', week_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
   AND week = to_char(week_start AT TIME ZONE 'UTC', 'IYYY-"W"IW')),
 CONSTRAINT after_the_fact_review_week_begun CHECK (reviewed_at >= week_start),
 CONSTRAINT after_the_fact_review_week_ended CHECK ((reviewed_at AT TIME ZONE 'UTC') >= (week_start AT TIME ZONE 'UTC') + interval '7 days')
);

ALTER TABLE cargo_fin.command_receipt DROP CONSTRAINT command_receipt_result_type_check,
  ADD CONSTRAINT command_receipt_result_type_check
    CHECK (result_type IN ('FinancePolicy','FreightJob','CreditAccount','BillingProfile','DebtorReceipt','ConsolCostPool','CreditorInvoice',
      'CreditorPayment','FapiaoRecord','FinSettings','CostReviewRequest','CostReviewResolution','AfterTheFactReview'));

CREATE TRIGGER after_the_fact_review_immutable BEFORE UPDATE OR DELETE ON cargo_fin.after_the_fact_review
  FOR EACH ROW EXECUTE FUNCTION cargo_fin.reject_row_change();
CREATE TRIGGER after_the_fact_review_read_committed BEFORE INSERT OR UPDATE OR DELETE ON cargo_fin.after_the_fact_review
  FOR EACH STATEMENT EXECUTE FUNCTION cargo_fin.writes_read_committed();
ALTER TABLE cargo_fin.after_the_fact_review ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_fin.after_the_fact_review FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_fin.after_the_fact_review
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
