-- IDP-Cargo FUL V0.4 (fourth part) / PostgreSQL 16. Run only as the migrator role, never at service start.
-- What FIN needs to open a job and price it from the accepted quote (CostDriver v1, kind SHIPMENT_PLANNED): the
-- quote id and revision COM's obligation snapshot named when FUL planned an obligation entry. Entries planned before
-- keep none; FIN prices their orders by hand.

ALTER TABLE cargo_ful.shipment_obligation
  ADD COLUMN quote_id text CHECK (length(btrim(quote_id)) BETWEEN 1 AND 128),
  ADD COLUMN quote_revision bigint CHECK (quote_revision > 0),
  ADD CONSTRAINT shipment_obligation_quote CHECK ((quote_id IS NULL) = (quote_revision IS NULL));
