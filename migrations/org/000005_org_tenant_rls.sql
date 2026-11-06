-- Defense-in-depth policies; NOT an authentication or complete authorization implementation.
-- `app.tenant_id` must be SET LOCAL by trusted service inside each transaction, and
-- trusted application DB role must be NONOWNER/NOBYPASSRLS; do not expose raw DB credentials.
-- FORCE RLS also covers accidental table-owner access, but superuser/BYPASSRLS still bypass.
DO $rls$
DECLARE tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY[
    'business_party','legal_entity','legal_entity_branch','party_identifier',
    'party_contact','party_address','party_role','partner_merge_case',
    'party_alias','partner_external_ref','command_receipt','org_audit'
  ] LOOP
    EXECUTE format('ALTER TABLE cargo_org.%I ENABLE ROW LEVEL SECURITY',tbl);
    EXECUTE format('ALTER TABLE cargo_org.%I FORCE ROW LEVEL SECURITY',tbl);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON cargo_org.%I USING (tenant_id = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id = current_setting(''app.tenant_id'',true))',tbl
    );
  END LOOP;
END $rls$;
-- Security acceptance tests MUST verify SELECT/UPDATE/INSERT/DELETE are denied for
-- missing, wrong and tampered tenant scope; admin/grant design still requires review.
-- No tenant policy applies to reference_code_edition; restrict write to steward role,
-- and allow SELECT only through a deliberately granted read-only reference-data role.
