package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManifestOrdersContextDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "migrations")
	manifest := filepath.Join(root, "manifest", "contexts.txt")
	write(t, manifest, "# order\nalpha\n\nbeta numbering\n")
	entries, err := readManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0] != (entry{Dir: filepath.Join(root, "alpha")}) || entries[1] != (entry{Dir: filepath.Join(root, "beta"), Numbering: true}) {
		t.Fatalf("entries = %+v", entries)
	}
	for _, bad := range []string{"alpha\nalpha\n", "../org\n", "# nothing\n", "alpha options\n", "alpha numbering numbering\n"} {
		write(t, manifest, bad)
		if _, err := readManifest(manifest); err == nil {
			t.Errorf("manifest %q must be rejected", bad)
		}
	}
}

func TestMigrationFilesCarryTheirContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rate")
	write(t, filepath.Join(dir, "000001_rate_quotes.sql"), "SELECT 1;\r\n")
	items, err := loadDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "000001_rate_quotes" || items[0].SQL != "SELECT 1;\n" {
		t.Fatalf("items = %+v", items)
	}
	write(t, filepath.Join(dir, "000002_org_identity.sql"), "SELECT 2;")
	if _, err := loadDirectory(dir); err == nil {
		t.Fatal("a file named for another context must be rejected")
	}
	if _, err := loadDirectory(filepath.Join(t.TempDir(), "org")); err == nil {
		t.Fatal("ORG needs its six original migrations")
	}
}

func TestRepositoryManifestLoads(t *testing.T) {
	entries, err := readManifest(filepath.Join("..", "..", "migrations", "manifest", "contexts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := load(entries)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range items {
		ids = append(ids, m.ID)
		if (strings.HasPrefix(m.Ledger, "platform:") != strings.Contains(m.ID, ":")) || m.Ledger == "" {
			t.Fatalf("ledger id %q of %q", m.Ledger, m.ID)
		}
	}
	want := []string{"000001_ref_locations", "000002_ref_locations_s1_subset", "000003_ref_equipment", "000004_ref_equipment_s1", "000001_org_identity", "000002_org_roles", "000003_org_merge", "000004_org_audit_reference",
		"000005_org_tenant_rls", "000006_org_receipt_revision", "000007_org_party_address_book", "000008_org_party_identifiers", "000009_org_contact_consent_access", "000010_org_carrier_codes", "000011_org_contact_transfer_register", "000001_rate_quotes", "000002_rate_quote_v03", "000003_rate_service_level_list", "000004_rate_cards", "000005_rate_pricing_basis", "000006_rate_quote_lines", "000007_rate_agreements", "000001_com_orders",
		"000002_com_obligation_lifecycle", "000003_com_reservation_per_shipment", "000004_com_order_correction_policy_terms", "000005_com_order_status_derived", "000006_com_order_parties", "000007_com_order_closure_contact_retention", "000008_com_contact_consent_access", "000009_com_order_party_lock", "000010_com_agreement_orders", "000011_com_container_mode",
		"000001_ful_shipments", "000002_ful_shipment_parts", "000003_ful_location_resolution", "000004_ful_consol_attachment", "000005_ful_load_evidence",
		"000006_ful_goods_and_screening", "000007_ful_cost_driver", "000008_ful_container_mode", "000001_phy_consolidation_membership", "000002_phy_cargo_lots_tender", "000003_phy_load_units", "000001_book_carrier_bookings",
		"cargo_air:platform_numbering_0001_numbering_sequence", "000001_air_movement_waybills", "000002_air_measured_facts", "000003_air_doc_facts",
		"000004_air_hawb_numbering", "000001_doc_transport_documents", "000002_doc_rendering_dispatch", "000003_doc_credit_decision", "cargo_fin:platform_numbering_0001_numbering_sequence", "000001_fin_jobs_credit", "000002_fin_billing", "000003_fin_numbering", "000004_fin_returned_estimates", "000005_fin_settlement", "000006_fin_consol_pool", "000007_fin_creditor_invoices", "000008_fin_creditor_payments", "000009_fin_rated_estimates", "000010_fin_quote_reevaluation", "000011_fin_fapiao"}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Fatalf("migration order = %v, want %v", ids, want)
	}
}
