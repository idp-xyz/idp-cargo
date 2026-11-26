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
)

// FIN's pools and settings and SEA's settings through HTTP (stage 3, V2.108), on PostgreSQL, with the job roles'
// tokens: the owner's pool P-1 (set up in process) read by the finance staff and refused to the operator, the document
// fee previewed by one person and submitted by another, its effect and C-1's economics read; a loosening of FIN's
// settings proposed by the consultant (a body naming an onboarding refused) and approved by the administrator, the
// settings read; SEA's catalogue read, and a roll of a container SEA does not know refused.
func TestFINPoolsAndTheSettingsThroughHTTPWithTheJobRolesOnPostgres(t *testing.T) {
	d := newSeaLineDay(t, "cargo-fin-pool-api-")
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
	finn, mia, ops, cora, ada := token("finn", "finance"), token("mia", "finance-manager"), token("otto", "ops"), token("cora", "consultant"), token("ada", "admin")
	occurred := fmt.Sprintf(`"occurred_at":%q`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
	post := func(tok, key, path, ifMatch, body string) apiResponse {
		t.Helper()
		headers := []string{"Idempotency-Key", key}
		if ifMatch != "" {
			headers = append(headers, "If-Match", ifMatch)
		}
		return c.do("POST", path, tok, body, headers...)
	}
	want := func(name string, got apiResponse, status int, etag string) apiResponse {
		t.Helper()
		if got.status != status || (etag != "" && got.header.Get("ETag") != etag) {
			t.Fatalf("%s: %d %s %v, want %d %s", name, got.status, got.header.Get("ETag"), got.body, status, etag)
		}
		return got
	}

	// the pool
	pool := want("the finance staff reads the pool", c.do("GET", "/v1/fin/pools/P-1", finn, ""), 200, "")
	c.expect("the operator reads the pool's costs", c.do("GET", "/v1/fin/pools/P-1", ops, ""), 403, "FORBIDDEN")
	preview := want("preview the document fee", post(finn, "pv-1", "/v1/fin/pools/P-1/previews", pool.header.Get("ETag"), `{"line_nos":[3],`+occurred+`}`), 200, "")

	no := fmt.Sprint(preview.body["version"])
	if v, ok := preview.body["version"].(float64); ok {
		no = fmt.Sprint(int(v))
	}
	want("its effect", c.do("GET", "/v1/fin/pools/P-1/previews/"+no+"/effect", finn, ""), 200, "")
	want("another person submits", post(mia, "sb-1", "/v1/fin/pools/P-1/previews/"+no+"/submit", preview.header.Get("ETag"), `{`+occurred+`}`), 200, "")
	after := want("the pool again", c.do("GET", "/v1/fin/pools/P-1", finn, ""), 200, "")
	if as, _ := after.body["apportionments"].([]any); len(as) != 1 {
		t.Fatalf("the apportionments: %v", after.body["apportionments"])
	}
	want("C-1's economics", c.do("GET", "/v1/fin/consolidations/C-1/profit", finn, ""), 200, "")

	// FIN's settings: proposed by the consultant, approved by the administrator
	change := `{"change_id":"F1-1","entries":[{"scope_kind":"TENANT","key":"fin.F1","value":"UNRESTRICTED"}],"reason":"the customer's practice"}`
	c.expect("the finance manager proposes", post(mia, "sc-0", "/v1/fin/settings-changes", "", change), 403, "FORBIDDEN")
	c.expect("a body naming an onboarding", post(cora, "sc-1", "/v1/fin/settings-changes", "", strings.Replace(change, `{"change_id"`, `{"onboarding_id":"ON-1","change_id"`, 1)), 400, "MALFORMED_REQUEST")
	want("the consultant proposes", post(cora, "sc-2", "/v1/fin/settings-changes", "", change), 201, "")
	c.expect("the consultant approves", post(cora, "sc-3", "/v1/fin/settings-changes/F1-1/approve", "", `{}`), 403, "FORBIDDEN")
	want("the administrator approves", post(ada, "sc-4", "/v1/fin/settings-changes/F1-1/approve", "", `{}`), 200, "")
	settings := want("the settings", c.do("GET", "/v1/fin/settings?scope_kind=TENANT", finn, ""), 200, "")
	if !strings.Contains(fmt.Sprint(settings.body), "UNRESTRICTED") {
		t.Fatalf("the settings: %v", settings.body)
	}

	// SEA's settings catalogue, and the roll of a container SEA does not know
	want("SEA's catalogue", c.do("GET", "/v1/sea-settings-catalogue", ops, ""), 200, "")
	c.expect("roll an unknown container", post(ops, "roll-0", "/v1/container-visits/CV-NONE/roll", `"1"`, `{"to_movement_id":"OM-2","reason":"rolled",`+occurred+`}`), 404, "NOT_FOUND")
}
