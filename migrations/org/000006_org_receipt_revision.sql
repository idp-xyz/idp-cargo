-- IDP-Cargo V2.4 additive migration candidate; ORIGINAL migrations 1-5 retained unchanged.
-- No destructive update or reset of a production command receipt is permitted.
ALTER TABLE cargo_org.command_receipt
    ADD COLUMN result_revision bigint NOT NULL DEFAULT 0 CHECK (result_revision >= 0);
-- Legacy V2.3 receipts have unknown revision (=0): they cannot be blindly replayed
-- as proof of a completed mutation. Approval recovery must explicitly reject
-- such records for manual/controlled reconciliation, not manufacture success.
COMMENT ON COLUMN cargo_org.command_receipt.result_revision
IS 'Revision >0 for V2.4 authoritative receipts; 0 legacy requires reconciliation';
