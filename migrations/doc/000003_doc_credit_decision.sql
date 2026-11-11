-- IDP-Cargo DOC V0.3 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A house document issued on FIN's credit decision (CreditStatus v1, FIN -> DOC, C7) names the decision it checked.
-- Documents issued before carry no FIN_OK issue (FIN was not built), so existing rows pass.
ALTER TABLE cargo_doc.document_version_issue
  ADD CONSTRAINT document_version_issue_decision CHECK (credit_basis <> 'FIN_OK' OR credit_decision_id IS NOT NULL);
