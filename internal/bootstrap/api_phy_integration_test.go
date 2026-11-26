//go:build integration

package bootstrap

import (
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
)

// PHY through HTTP (stage 3, V2.107), on PostgreSQL, with the job roles' tokens: a sea consolidation created by the
// operator and read by the finance staff, listed by its origin given as text, a container planned, a planned house
// attached; the house's cargo received into a lot at the CFS by the warehouse, measured and its receipt completed;
// the refusals of a role without the permission and of another tenant. The house is planned in process.
func TestPHYThroughHTTPWithTheJobRolesOnPostgres(t *testing.T) {
	p := openPlatform(t)
	cfg := APIConfig{TokenKey: []byte("api-test-key-of-thirty-two-bytes!!"), TokenIssuer: "idp-cargo-test", TokenAudience: "idp-cargo-api",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MarginFloorBP: 2000}
	h, err := p.API(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := apiClient{t: t, srv: srv}
	tenant, other := uniqueTenant("cargo-phy-api-a-"), uniqueTenant("cargo-phy-api-b-")
	seedORG(t, p, tenant, "party-a")
	orgModule, err := p.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := p.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := p.COM(orgModule, rateModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	fulModule, _, err := p.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	departs := at.Add(72 * time.Hour)
	slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}.
		planSea(t, "party-a", "A", comapi.ContainerLCL, fulapi.ContainerLCL)
	token := func(tenant, actor string, roles ...string) string {
		t.Helper()
		perms, err := RolePermissions(roles, nil)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := MintDevToken(cfg, tenant, actor, perms, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	ops, warehouse, finance := token(tenant, "otto", "ops"), token(tenant, "walt", "warehouse"), token(tenant, "finn", "finance")
	when := func(h int) string { return at.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }
	occurred := func(h int) string { return fmt.Sprintf(`"occurred_at":%q`, when(h)) }
	post := func(tok, key, path, ifMatch, body string) apiResponse {
		t.Helper()
		headers := []string{"Idempotency-Key", key}
		if ifMatch != "" {
			headers = append(headers, "If-Match", ifMatch)
		}
		return c.do("POST", path, tok, body, headers...)
	}
	want := func(name string, r apiResponse, status int, etag string) apiResponse {
		t.Helper()
		if r.status != status || (etag != "" && r.header.Get("ETag") != etag) {
			t.Fatalf("%s: %d %s %v, want %d %s", name, r.status, r.header.Get("ETag"), r.body, status, etag)
		}
		return r
	}

	consolidation := fmt.Sprintf(`{"consolidation_id":"C-1","mode":"SEA","kind":"SEA_LCL_OWN","operating_legal_entity_id":"LE",`+
		`"origin":{"scheme":"UNLOCODE","code":"CNSHA"},"destination":{"scheme":"UNLOCODE","code":"DEHAM"},"planned_departure":%q,"house_cut_off":%q,%s}`,
		departs.Add(6*time.Hour).Format(time.RFC3339), departs.Add(-6*time.Hour).Format(time.RFC3339), occurred(8))
	c.expect("the finance staff creates a consolidation", post(finance, "c-0", "/v1/consolidations", "", consolidation), 403, "FORBIDDEN")
	want("create the consolidation", post(ops, "c-1", "/v1/consolidations", "", consolidation), 201, `"1"`)
	got := want("the finance staff reads it", c.do("GET", "/v1/consolidations/C-1", finance, ""), 200, `"1"`)
	if !strings.Contains(fmt.Sprint(got.body), "SEA_LCL_OWN") {
		t.Fatalf("the consolidation: %v", got.body)
	}
	open := want("open ones from Shanghai (the code as frozen, CNSHG)", c.do("GET", "/v1/consolidations?origin=CNSHG&mode=SEA", ops, ""), 200, "")
	if items := open.body["items"].([]any); len(items) != 1 || items[0].(map[string]any)["consolidation_id"] != "C-1" {
		t.Fatalf("from Shanghai: %v", open.body)
	}
	if none := want("open ones from Pudong", c.do("GET", "/v1/consolidations?origin=IATA:PVG", ops, ""), 200, ""); len(none.body["items"].([]any)) != 0 {
		t.Fatalf("from Pudong: %v", none.body)
	}
	c.expect("another tenant", c.do("GET", "/v1/consolidations/C-1", token(other, "otto", "ops"), ""), 404, "NOT_FOUND")
	want("plan a container", post(ops, "c-2", "/v1/consolidations/C-1/load-units", `"1"`, `{"kind":"CONTAINER","type_code":"22G1",`+occurred(9)+`}`), 200, `"2"`)
	want("attach the house", post(ops, "c-3", "/v1/consolidations/C-1/houses", "", `{"shipment_id":"SH-A","expected_membership_revision":0,`+occurred(10)+`}`), 200, "")

	// the warehouse receives the house's cargo at the CFS
	receipt := `{"shipment_id":"SH-A","facility":{"location":{"scheme":"UNLOCODE","code":"CNSHG"},"code":"SHA-CFS1"},"packs":6,"package_type":"CT",` +
		`"condition":"GOOD","evidence":{"source":"CFS tally","reference":"T-1","occurred_at":"` + when(11) + `"},` + occurred(11) + `}`
	c.expect("the finance staff receives cargo", post(finance, "l-0", "/v1/cargo-lots/L-A/receipts", `"0"`, receipt), 403, "FORBIDDEN")
	want("receive", post(warehouse, "l-1", "/v1/cargo-lots/L-A/receipts", `"0"`, receipt), 200, `"1"`)
	measure := `{"gross_weight":{"unit":"KGM","amount":300},"volume":{"unit":"MTQ","amount":2},"method":"SCALE","evidence":{"source":"scale","reference":"W-1","occurred_at":"` +
		when(12) + `"},` + occurred(12) + `}`
	want("measure", post(warehouse, "l-2", "/v1/cargo-lots/L-A/measurements", `"1"`, measure), 200, `"2"`)
	lot := want("the warehouse reads the lot", c.do("GET", "/v1/cargo-lots/L-A", warehouse, ""), 200, "")
	if !strings.Contains(fmt.Sprint(lot.body), "SH-A") {
		t.Fatalf("the lot: %v", lot.body)
	}
	want("the open discrepancies", c.do("GET", "/v1/discrepancies?consolidation_id=C-1", ops, ""), 200, "")
}
