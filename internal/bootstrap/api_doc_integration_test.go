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
)

// DOC through HTTP (stage 3, V2.109), on PostgreSQL, with the job roles' tokens: the owner's two boxes sealed, loaded
// and OM-1 finalized in process; then through HTTP the HBL of H04 drafted by the documentation staff, refused to its
// own drafter without the permission by name and approved by a colleague, the order's parties locked for it through
// COM's route, issued over the missing credit account by a person who holds doc:OverrideCreditCheck by name (the
// documentation staff alone is refused), rendered as PDF, listed for the shipment, the movement's pending items read;
// the finance staff's draft refused.
func TestDOCThroughHTTPWithTheJobRolesOnPostgres(t *testing.T) {
	d := newSeaLineDay(t, "cargo-doc-api-")
	d.must("FUL takes the evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H05"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H05"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	d.visit["CV-1"], d.visit["CV-2"] = d.cvOf("CV-1").Revision, d.cvOf("CV-2").Revision
	d.must("seal A", d.boxSeal("CV-1", "seal-x-1", 17))
	d.must("seal B", d.boxSeal("CV-2", "seal-x-2", 17))
	for _, v := range []string{"CV-1", "CV-2"} {
		d.must("stuffing done "+v, d.boxStuffingDone(v, 18))
	}
	d.must("VGM A", d.boxVGM("CV-1", 2900, 19))
	d.must("VGM B", d.boxVGM("CV-2", 3000, 19))
	d.masterBill(18)
	d.visit["CV-1"], d.visit["CV-2"] = d.cvOf("CV-1").Revision, d.cvOf("CV-2").Revision
	d.load("CV-1", 22)
	d.load("CV-2", 22)
	d.finalizeOf("OM-1", "fin-1", 22)
	order := func() int64 {
		p, err := d.com.GetOrderParties(d.ctx, comapi.GetOrderParties{TenantID: d.tenant, OrderID: "O-H04"})
		d.must("order parties", err)
		return p.Revision
	}
	for _, p := range []comapi.OrderParty{{Role: comapi.PartyShipper, PartyID: "shipper-h04"}, {Role: comapi.PartyConsignee, PartyID: "consignee-de"}} {
		_, err := d.com.SetOrderParty(d.ctx, comapi.SetOrderParty{CommandID: "set-" + p.Role, TenantID: d.tenant, ActorID: "cs-1", OrderID: "O-H04",
			ExpectedRevision: order(), Party: p, OccurredAt: d.hours(23)})
		d.must("set "+p.Role, err)
	}

	cfg := APIConfig{TokenKey: []byte("api-test-key-of-thirty-two-bytes!!"), TokenIssuer: "idp-cargo-test", TokenAudience: "idp-cargo-api",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MarginFloorBP: 2000}
	h, err := d.platform.API(cfg)
	d.must("API", err)
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := apiClient{t: t, srv: srv}
	token := func(actor string, roles ...string) string {
		t.Helper()
		perms, err := RolePermissions(roles, nil)
		d.must("roles", err)
		tok, err := MintDevToken(cfg, d.tenant, actor, perms, time.Hour)
		d.must("token", err)
		return tok
	}
	dora, dan, lead, finance := token("dora", "docs"), token("dan", "docs"), token("lee", "docs", "finance-manager"), token("finn", "finance")
	occurred := fmt.Sprintf(`"occurred_at":%q`, d.hours(24).Format(time.RFC3339))
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

	draft := `{"document_id":"DOC-HBL-H04","kind":"HBL","movement_id":"OM-1","shipment_id":"SH-H04","payment_indicator":"PREPAID","release":"EXPRESS",` + occurred + `}`
	c.expect("the finance staff drafts", post(finance, "d-0", "/v1/documents", "", draft), 403, "FORBIDDEN")
	want("draft", post(dora, "d-1", "/v1/documents", "", draft), 201, `"1"`)
	doc := want("read it", c.do("GET", "/v1/documents/DOC-HBL-H04", finance, ""), 200, `"1"`)
	if doc.body["state"] != "DRAFT" || doc.body["movement_id"] != "OM-1" {
		t.Fatalf("the draft: %v", doc.body)
	}
	c.expect("its drafter approves without the permission by name", post(dora, "d-2", "/v1/documents/DOC-HBL-H04/approve", `"1"`, `{`+occurred+`}`), 403, "FORBIDDEN")
	want("a colleague approves", post(dan, "d-3", "/v1/documents/DOC-HBL-H04/approve", `"1"`, `{`+occurred+`}`), 200, `"2"`)
	lock := want("the order", c.do("GET", "/v1/orders/O-H04", dora, ""), 200, "")
	want("lock the parties", post(dora, "l-1", "/v1/orders/O-H04/party-locks", lock.header.Get("ETag"), `{"holder_id":"DOC-HBL-H04",`+occurred+`}`), 200, "")
	issue := `{"credit_override_reason":"the controller approved the credit",` + occurred + `}`
	c.expect("the documentation staff overrides the credit check", post(dora, "d-4", "/v1/documents/DOC-HBL-H04/issue", `"2"`, issue), 403, "FORBIDDEN")
	want("issue", post(lead, "d-5", "/v1/documents/DOC-HBL-H04/issue", `"2"`, issue), 200, `"3"`)
	pdf := want("the PDF", c.do("GET", "/v1/documents/DOC-HBL-H04/versions/0/pdf", finance, ""), 200, "")
	if s, _ := pdf.body["pdf"].(string); len(s) < 100 || !strings.HasPrefix(s, "JVBER") {
		t.Fatalf("the PDF: %d bytes", len(s))
	}
	if docs := want("the shipment's documents", c.do("GET", "/v1/shipments/SH-H04/documents", finance, ""), 200, ""); len(docs.body["items"].([]any)) != 1 {
		t.Fatalf("the shipment's documents: %v", docs.body)
	}
	want("the movement's pending items", c.do("GET", "/v1/movements/OM-1/pending-documents", dora, ""), 200, "")
}
