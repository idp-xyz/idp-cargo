//go:build integration

package bootstrap

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type apiClient struct {
	t   *testing.T
	srv *httptest.Server
}

type apiResponse struct {
	status int
	header http.Header
	body   map[string]any
}

func (c apiClient) do(method, path, token, body string, header ...string) apiResponse {
	c.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, r)
	if err != nil {
		c.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := apiResponse{status: resp.StatusCode, header: resp.Header, body: map[string]any{}}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &out.body); err != nil {
			c.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
	}
	return out
}

func (c apiClient) expect(name string, r apiResponse, status int, code string) {
	c.t.Helper()
	if r.status != status || (code != "" && r.body["code"] != code) {
		c.t.Fatalf("%s: %d %v, want %d %s", name, r.status, r.body, status, code)
	}
}

// The HTTP API end to end on PostgreSQL 16: cargo-api's handler with HS256 tokens, RATE's charge-code catalogue read
// and maintained with the principal's tenant and actor, Idempotency-Key replays, If-Match versions, permissions,
// tenant isolation and the record window.
func TestHTTPChargeCodesOnPostgres(t *testing.T) {
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
	tenant, other := uniqueTenant("cargo-api-a-"), uniqueTenant("cargo-api-b-")
	mint := func(tenant, actor string, perms ...string) string {
		t.Helper()
		token, err := MintDevToken(cfg, tenant, actor, perms, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	maintainer := mint(tenant, "pricing-1", "rate:Read", "rate:MaintainChargeCodes")
	reader := mint(tenant, "sales-1", "rate:Read")
	nobody := mint(tenant, "ops-1")
	stranger := mint(other, "pricing-9", "rate:*")

	c.expect("no token", c.do("GET", "/v1/charge-codes", "", ""), 401, "UNAUTHENTICATED")
	c.expect("a read without permission", c.do("GET", "/v1/charge-codes", nobody, ""), 403, "FORBIDDEN")
	list := c.do("GET", "/v1/charge-codes", reader, "")
	if codes, _ := list.body["charge_codes"].([]any); list.status != 200 || len(codes) < 10 {
		t.Fatalf("the starter list: %d %v", list.status, list.body)
	}
	got := c.do("GET", "/v1/charge-codes/AIR_FREIGHT", reader, "")
	if got.status != 200 || got.header.Get("ETag") != `"0"` || got.body["unit"] != "KG_CHARGEABLE" || got.body["version"] != 0.0 {
		t.Fatalf("a starter code: %d %v %v", got.status, got.header, got.body)
	}
	c.expect("an unknown code", c.do("GET", "/v1/charge-codes/NO_SUCH_CODE", reader, ""), 404, "NOT_FOUND")

	at := `"occurred_at":"` + time.Now().UTC().Add(-time.Minute).Format(time.RFC3339) + `"`
	define := `{"code":"WAREHOUSE_FEE","name_zh":"仓储费","name_en":"Warehouse fee","group":"ORIGIN","calculator":"FLAT","modes":["AIR","SEA"],` + at + `}`
	c.expect("a define without permission", c.do("POST", "/v1/charge-codes", reader, define, "Idempotency-Key", "define-0"), 403, "FORBIDDEN")
	c.expect("a define without Idempotency-Key", c.do("POST", "/v1/charge-codes", maintainer, define), 400, "IDEMPOTENCY_KEY_REQUIRED")
	created := c.do("POST", "/v1/charge-codes", maintainer, define, "Idempotency-Key", "define-1")
	if created.status != 201 || created.header.Get("Location") != "/v1/charge-codes/WAREHOUSE_FEE" || created.header.Get("ETag") != `"1"` ||
		created.body["command_id"] != "define-1" || created.body["subject_id"] != "WAREHOUSE_FEE" || created.body["new_revision"] != 1.0 {
		t.Fatalf("defined: %d %v %v", created.status, created.header, created.body)
	}
	if again := c.do("POST", "/v1/charge-codes", maintainer, define, "Idempotency-Key", "define-1"); again.status != 201 ||
		again.body["new_revision"] != 1.0 {
		t.Fatalf("replayed: %d %v", again.status, again.body)
	}
	c.expect("the key reused for another body", c.do("POST", "/v1/charge-codes", maintainer, strings.Replace(define, "仓储费", "仓库费", 1),
		"Idempotency-Key", "define-1"), 409, "IDEMPOTENCY_MISMATCH")
	if twice := c.do("POST", "/v1/charge-codes", maintainer, define, "Idempotency-Key", "define-2"); twice.status != 409 && twice.status != 422 {
		t.Fatalf("a code defined twice: %d %v", twice.status, twice.body)
	}
	c.expect("no business time", c.do("POST", "/v1/charge-codes", maintainer, strings.Replace(define, ","+at, "", 1), "Idempotency-Key", "define-6"),
		400, "OCCURRED_AT_REQUIRED")
	c.expect("a bad code", c.do("POST", "/v1/charge-codes", maintainer, `{"code":"bad","name_zh":"x","name_en":"x","group":"ORIGIN",
		"calculator":"FLAT","modes":["AIR"],`+at+`}`, "Idempotency-Key", "define-3"), 422, "")
	c.expect("an unknown field", c.do("POST", "/v1/charge-codes", maintainer, strings.Replace(define, `"code"`, `"tenant_id":"x","code"`, 1),
		"Idempotency-Key", "define-4"), 400, "MALFORMED_REQUEST")
	c.expect("a business time outside the record window", c.do("POST", "/v1/charge-codes", maintainer,
		strings.Replace(strings.Replace(define, `"code":"WAREHOUSE_FEE"`, `"code":"OLD_FEE"`, 1), at, `"occurred_at":"2020-01-01T00:00:00Z"`, 1),
		"Idempotency-Key", "define-5"), 422, "")

	revise := `{"name_zh":"仓储费","name_en":"Warehousing","group":"ORIGIN","calculator":"FLAT","modes":["AIR","SEA"],` + at + `}`
	c.expect("a revision without If-Match", c.do("POST", "/v1/charge-codes/WAREHOUSE_FEE/revise", maintainer, revise, "Idempotency-Key", "revise-0"),
		428, "PRECONDITION_REQUIRED")
	c.expect("a stale If-Match", c.do("POST", "/v1/charge-codes/WAREHOUSE_FEE/revise", maintainer, revise, "Idempotency-Key", "revise-1", "If-Match", `"0"`),
		409, "")
	if r := c.do("POST", "/v1/charge-codes/WAREHOUSE_FEE/revise", maintainer, revise, "Idempotency-Key", "revise-2", "If-Match", `"1"`); r.status != 200 ||
		r.header.Get("ETag") != `"2"` {
		t.Fatalf("revised: %d %v %v", r.status, r.header, r.body)
	}
	c.expect("a deactivation without a body", c.do("POST", "/v1/charge-codes/WAREHOUSE_FEE/deactivate", maintainer, "", "Idempotency-Key", "off-0",
		"If-Match", `"2"`), 415, "UNSUPPORTED_MEDIA_TYPE")
	if r := c.do("POST", "/v1/charge-codes/WAREHOUSE_FEE/deactivate", maintainer, "{"+at+"}", "Idempotency-Key", "off-1", "If-Match", `"2"`); r.status != 200 ||
		r.header.Get("ETag") != `"3"` {
		t.Fatalf("deactivated: %d %v", r.status, r.body)
	}
	listed := func(query string) bool {
		r := c.do("GET", "/v1/charge-codes"+query, reader, "")
		codes, _ := r.body["charge_codes"].([]any)
		for _, code := range codes {
			if m, _ := code.(map[string]any); m["code"] == "WAREHOUSE_FEE" {
				return true
			}
		}
		return false
	}
	if listed("") || !listed("?include_inactive=true") {
		t.Fatal("an inactive code is listed only when asked")
	}
	c.expect("a bad include_inactive", c.do("GET", "/v1/charge-codes?include_inactive=maybe", reader, ""), 400, "MALFORMED_REQUEST")
	if r := c.do("POST", "/v1/charge-codes/WAREHOUSE_FEE/reactivate", maintainer, `{"occurred_at":"`+time.Now().UTC().Format(time.RFC3339)+`"}`,
		"Idempotency-Key", "on-1", "If-Match", `"3"`); r.status != 200 || !listed("") {
		t.Fatalf("reactivated: %d %v", r.status, r.body)
	}
	if r := c.do("GET", "/v1/charge-codes/WAREHOUSE_FEE", reader, ""); r.body["name_en"] != "Warehousing" || r.body["set_by"] != "pricing-1" ||
		r.header.Get("ETag") != `"4"` {
		t.Fatalf("the code: %v %v", r.header, r.body)
	}
	// a starter code revised at version 0
	if r := c.do("POST", "/v1/charge-codes/DOC_FEE/revise", maintainer, `{"name_zh":"文件费","name_en":"Documentation fee","group":"DOCUMENTATION",
		"calculator":"FLAT","modes":["AIR","SEA"],`+at+`}`, "Idempotency-Key", "revise-doc", "If-Match", `"0"`); r.status != 200 || r.header.Get("ETag") != `"1"` {
		t.Fatalf("a starter code revised: %d %v", r.status, r.body)
	}
	// another tenant sees its own catalogue only
	c.expect("another tenant's code", c.do("GET", "/v1/charge-codes/WAREHOUSE_FEE", stranger, ""), 404, "NOT_FOUND")
	if r := c.do("GET", "/v1/charge-codes/DOC_FEE", stranger, ""); r.header.Get("ETag") != `"0"` {
		t.Fatalf("another tenant's starter code: %v", r.header)
	}
	expired, err := MintDevToken(cfg, tenant, "pricing-1", []string{"rate:*"}, -time.Hour)
	if err == nil {
		t.Fatalf("an expired token minted: %s", expired)
	}
	other2 := cfg
	other2.TokenKey = []byte("another-key-of-thirty-two-bytes!!!")
	forged, err := MintDevToken(other2, tenant, "pricing-1", []string{"*"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.expect("a token of another key", c.do("GET", "/v1/charge-codes", forged, ""), 401, "UNAUTHENTICATED")
	bad := cfg
	bad.TokenKey = []byte("short")
	if _, err = p.API(bad); err == nil {
		t.Fatal("an API with a short key")
	}
}

// The same command sent eight times at once (a client retrying before the first answer) commits once and every
// request answers the same receipt; a replay is authorized as the command was, and a key in use tells another actor
// nothing.
func TestHTTPRetriesOnPostgres(t *testing.T) {
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
	tenant := uniqueTenant("cargo-api-r-")
	mint := func(actor string, perms ...string) string {
		t.Helper()
		token, err := MintDevToken(cfg, tenant, actor, perms, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	maintainer := mint("pricing-1", "rate:Read", "rate:MaintainChargeCodes")
	at := `"occurred_at":"` + time.Now().UTC().Add(-time.Minute).Format(time.RFC3339) + `"`
	define := `{"code":"RACE_FEE","name_zh":"并发费","name_en":"Race fee","group":"ORIGIN","calculator":"FLAT","modes":["AIR"],` + at + `}`
	revise := `{"name_zh":"并发费","name_en":"Race fee, revised","group":"ORIGIN","calculator":"FLAT","modes":["AIR"],` + at + `}`
	for _, step := range []struct {
		name, path, body, key, ifMatch string
		status                         int
		revision                       float64
	}{
		{"define", "/v1/charge-codes", define, "race-define", "", 201, 1},
		{"revise", "/v1/charge-codes/RACE_FEE/revise", revise, "race-revise", `"1"`, 200, 2},
	} {
		responses := make([]apiResponse, 8)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range responses {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				header := []string{"Idempotency-Key", step.key}
				if step.ifMatch != "" {
					header = append(header, "If-Match", step.ifMatch)
				}
				responses[i] = c.do("POST", step.path, maintainer, step.body, header...)
			}()
		}
		close(start)
		wg.Wait()
		for i, r := range responses {
			if r.status != step.status || r.body["new_revision"] != step.revision || r.body["command_id"] != step.key {
				t.Fatalf("%s, request %d: %d %v", step.name, i, r.status, r.body)
			}
		}
	}
	if r := c.do("GET", "/v1/charge-codes/RACE_FEE", maintainer, ""); r.header.Get("ETag") != `"2"` {
		t.Fatalf("committed more than once: %v %v", r.header, r.body)
	}
	// the actor's permission withdrawn: its replay is refused, not answered
	c.expect("a replay without the permission", c.do("POST", "/v1/charge-codes", mint("pricing-1", "rate:Read"), define, "Idempotency-Key",
		"race-define"), 403, "FORBIDDEN")
	// another actor without the permission learns nothing of the key: 403, as for an unused key
	c.expect("a used key by an actor not allowed", c.do("POST", "/v1/charge-codes", mint("ops-1"), strings.Replace(define, "并发费", "x", 1),
		"Idempotency-Key", "race-define"), 403, "FORBIDDEN")
	// a body of the wrong type is described without Go's type names
	r := c.do("POST", "/v1/charge-codes", maintainer, `{"code":7}`, "Idempotency-Key", "race-type")
	if detail, _ := r.body["detail"].(string); r.status != 400 || strings.Contains(detail, "Go") || strings.Contains(detail, "struct") {
		t.Fatalf("a wrong type: %d %v", r.status, r.body)
	}
}
