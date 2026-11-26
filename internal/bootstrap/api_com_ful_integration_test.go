//go:build integration

package bootstrap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// COM and FUL through HTTP (stage 3, V2.107), on PostgreSQL, with the job roles' tokens: an order recorded on an
// accepted quote, read by the finance staff, allocated to an obligation, its shipper set, its parties read with their
// contacts only by name (the documentation staff can, the administrator's com:Read cannot) and every read in the
// privacy officer's log; the obligation reserved for a shipment, which is created, planned and confirmed, and read by
// the warehouse. The quote and the parties' roles are set up in process; the HTTP-only run from the quote is V2.113.
func TestCOMAndFULThroughHTTPWithTheJobRolesOnPostgres(t *testing.T) {
	ctx := context.Background()
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
	tenant, other := uniqueTenant("cargo-com-api-a-"), uniqueTenant("cargo-com-api-b-")
	seedORG(t, p, tenant, "party-a", "shipper-a")
	at := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)
	orgModule, err := p.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := p.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range []struct{ party, role string }{{"party-a", "CUSTOMER"}, {"shipper-a", "SHIPPER"}} {
		if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: fmt.Sprint("role-", i), TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
			PartyID: r.party, ExpectedRevision: 1, RoleCode: r.role, ValidFrom: at.Add(-time.Hour), EvidenceRef: "contract", OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote-a", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-A", LegalEntityID: "LE",
		CustomerPartyID: "party-a", Currency: "USD", Lines: []rateapi.QuoteLine{{ChargeCode: "OCEAN_FREIGHT", Sell: usd(100_000), Cost: usd(50_000)}},
		Validity: kernel.ValidityInterval{From: at.Add(-time.Hour), Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err = rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-a", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-A", ExpectedRevision: 1,
		EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail", OccurredAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
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
	ops, docs, admin, privacy, finance, warehouse := token(tenant, "otto", "ops"), token(tenant, "dora", "docs"), token(tenant, "ada", "admin"),
		token(tenant, "pia", "privacy"), token(tenant, "finn", "finance"), token(tenant, "walt", "warehouse")
	hour := func(h int) string {
		return fmt.Sprintf(`"occurred_at":%q`, at.Add(time.Duration(h)*time.Hour).Format(time.RFC3339))
	}
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

	// the order
	order := `{"order_id":"O-A","quote_id":"Q-A","customer_party_id":"party-a","transport":{"mode":"SEA","container_mode":"LCL"},` +
		`"lines":[{"description":"Cartons of furniture parts","packs":6,"package_type":"CT","gross_weight":{"unit":"KGM","amount":300}}],` + hour(1) + `}`
	c.expect("the finance staff records an order", post(finance, "o-0", "/v1/orders", "", order), 403, "FORBIDDEN")
	want("record the order", post(ops, "o-1", "/v1/orders", "", order), 201, `"1"`)
	got := want("the finance staff reads it", c.do("GET", "/v1/orders/O-A", finance, ""), 200, `"1"`)
	if got.body["status"] != "RECORDED" || got.body["customer_party_id"] != "party-a" || len(got.body["lines"].([]any)) != 1 {
		t.Fatalf("the order: %v", got.body)
	}
	c.expect("another tenant", c.do("GET", "/v1/orders/O-A", token(other, "otto", "ops"), ""), 404, "NOT_FOUND")
	want("allocate", post(ops, "o-2", "/v1/orders/O-A/obligations", `"1"`, `{"line_no":1,"obligation_id":"OB-A","packs":6,"gross_weight":{"unit":"KGM","amount":300},`+hour(2)+`}`), 200, `"2"`)
	// the shipper's office and contact in ORG, through ORG's routes
	want("the shipper's office", post(ops, "a-1", "/v1/parties/shipper-a/addresses", `"0"`, `{"address_id":"HQ","address":{"short_code":"HQ","purposes":["OFFICE"],`+
		`"name":"Shanghai Jia Trading","lines":["100 Century Avenue"],"city":"Shanghai","country_code":"CN"},"default_for":["OFFICE"]}`), 200, `"1"`)
	want("the shipper's contact", post(ops, "a-2", "/v1/parties/shipper-a/contacts", `"1"`, `{"contact_id":"C-1","contact":{"purposes":["DOCUMENTS"],`+
		`"name":"Wang Li","email":"wang.li@example.com"},"default_for":["DOCUMENTS"]}`), 200, `"2"`)
	want("the shipper", post(ops, "o-3", "/v1/orders/O-A/parties", `"2"`, `{"party":{"role":"SHIPPER","party_id":"shipper-a"},`+hour(2)+`}`), 200, `"3"`)

	// the parties, with their contacts by name only, and the log of the reads
	want("the administrator reads the parties", c.do("GET", "/v1/orders/O-A/parties", admin, ""), 200, `"3"`)
	c.expect("the administrator's com:Read reads the contacts", c.do("GET", "/v1/orders/O-A/parties?with_contacts=true&purpose=DOCUMENTS", admin, ""), 403, "FORBIDDEN")
	want("the documentation staff reads the contacts", c.do("GET", "/v1/orders/O-A/parties?with_contacts=true&purpose=DOCUMENTS", docs, ""), 200, `"3"`)
	window := "?since=" + at.Add(-time.Hour).Format(time.RFC3339) + "&until=" + time.Now().UTC().Add(time.Hour).Format(time.RFC3339) + "&limit=100"
	log := want("the privacy officer reads the log", c.do("GET", "/v1/orders/O-A/contact-access"+window, privacy, ""), 200, "")
	if !strings.Contains(fmt.Sprint(log.body), "dora") {
		t.Fatalf("the log: %v", log.body)
	}
	c.expect("the operator reads the log", c.do("GET", "/v1/orders/O-A/contact-access"+window, ops, ""), 403, "FORBIDDEN")

	// the shipment
	want("reserve the obligation", post(ops, "r-1", "/v1/obligations/OB-A/reservations", "", `{"obligation_version":1,"shipment_id":"SH-A",`+hour(3)+`}`), 200, "")
	want("create the shipment", post(ops, "s-1", "/v1/shipments", "", `{"shipment_id":"SH-A","obligation_id":"OB-A","obligation_version":1,`+hour(4)+`}`), 201, `"1"`)
	departs := at.Add(72 * time.Hour)
	plan := fmt.Sprintf(`{"transport_mode":"SEA","container_mode":"LCL","origin":{"scheme":"UNLOCODE","code":"CNSHA"},"destination":{"scheme":"UNLOCODE","code":"DEHAM"},`+
		`"departure_window":{"from":%q,"until":%q},"reason":"vessel booked",`+hour(5)+`}`, departs.Format(time.RFC3339), departs.Add(48*time.Hour).Format(time.RFC3339))
	c.expect("the warehouse plans", post(warehouse, "s-2", "/v1/shipments/SH-A/plan", `"1"`, plan), 403, "FORBIDDEN")
	want("plan", post(ops, "s-3", "/v1/shipments/SH-A/plan", `"1"`, plan), 200, `"2"`)
	c.expect("confirm at a stale revision", post(ops, "s-4", "/v1/shipments/SH-A/confirm", `"1"`, `{`+hour(6)+`}`), 409, "")
	want("confirm", post(ops, "s-5", "/v1/shipments/SH-A/confirm", `"2"`, `{`+hour(6)+`}`), 200, `"3"`)
	shipment := want("the warehouse reads the shipment", c.do("GET", "/v1/shipments/SH-A", warehouse, ""), 200, `"3"`)
	if shipment.body["status"] != "PLANNED" || !strings.Contains(fmt.Sprint(shipment.body["parts"]), "OB-A") || shipment.body["transport_mode"] != "SEA" {
		t.Fatalf("the shipment: %v", shipment.body)
	}
	got = want("the order again", c.do("GET", "/v1/orders/O-A", finance, ""), 200, "")
	if rs := got.body["reservations"].([]any); len(rs) != 1 || rs[0].(map[string]any)["shipment_id"] != "SH-A" {
		t.Fatalf("the reservations: %v", got.body)
	}
}
