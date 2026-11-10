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
	write(t, manifest, "# order\nalpha\n\nbeta\n")
	dirs, err := readManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "alpha") + "," + filepath.Join(root, "beta"); strings.Join(dirs, ",") != want {
		t.Fatalf("dirs = %v, want %s", dirs, want)
	}
	for _, bad := range []string{"alpha\nalpha\n", "../org\n", "# nothing\n"} {
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
	dirs, err := readManifest(filepath.Join("..", "..", "migrations", "manifest", "contexts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range dirs {
		items, err := loadDirectory(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range items {
			ids = append(ids, m.ID)
		}
	}
	want := []string{"000001_ref_locations", "000002_ref_locations_s1_subset", "000001_org_identity", "000002_org_roles", "000003_org_merge", "000004_org_audit_reference",
		"000005_org_tenant_rls", "000006_org_receipt_revision", "000007_org_party_address_book", "000008_org_party_identifiers", "000001_rate_quotes", "000002_rate_quote_v03", "000003_rate_service_level_list", "000001_com_orders",
		"000002_com_obligation_lifecycle", "000003_com_reservation_per_shipment", "000004_com_order_correction_policy_terms", "000005_com_order_status_derived", "000006_com_order_parties", "000007_com_order_closure_contact_retention",
		"000001_ful_shipments", "000002_ful_shipment_parts", "000003_ful_location_resolution"}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Fatalf("migration order = %v, want %v", ids, want)
	}
}
