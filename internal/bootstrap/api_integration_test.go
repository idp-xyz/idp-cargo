//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
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

// RATE's cards through the HTTP API with the real ORG and location data: a SELL tariff and a co-loader's BUY card
// opened, drafted, filled and published, each by the permission its side and step need (the pricing manager approves
// the tariff, and a replay asks it again), rated into a snapshot, read back and withdrawn.
func TestHTTPRateCardsOnPostgres(t *testing.T) {
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
	tenant := uniqueTenant("cargo-api-cards-")
	seedORG(t, p, tenant, "coloader")
	org, err := p.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	if _, err = org.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-coloader", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "coloader", ExpectedRevision: 1, RoleCode: "CO_LOADER", ValidFrom: at, ValidUntil: at.Add(365 * 24 * time.Hour),
		EvidenceRef: "co-loading agreement", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	mint := func(actor string, perms ...string) string {
		t.Helper()
		token, err := MintDevToken(cfg, tenant, actor, perms, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	pricing := mint("pricing-1", "rate:Read", "rate:MaintainSellRates")
	buyer := mint("buyer-1", "rate:Read", "rate:MaintainBuyRates")
	manager := mint("pricing-manager", "rate:Read", "rate:MaintainSellRates", "rate:ApproveTariff")
	sales := mint("sales-1", "rate:Read", "rate:EvaluateRates")
	auditor := mint("auditor-1", "rate:Read", "rate:ReadBuyRates")
	reader := mint("reader-1", "rate:Read")
	when := func(t time.Time) string { return `"occurred_at":"` + t.Format(time.RFC3339) + `"` }
	ok := func(name string, r apiResponse, status int, etag string) apiResponse {
		t.Helper()
		if r.status != status || (etag != "" && r.header.Get("ETag") != etag) {
			t.Fatalf("%s: %d %v %v", name, r.status, r.header, r.body)
		}
		return r
	}
	lcl := func(rate int) string {
		return fmt.Sprintf(`{"entries":[{"mode":"SEA","container_mode":"LCL","origin":"CNSHA","destination":"DEHAM","service_level":"STANDARD",
		  "commodity":"GENERAL","lines":[{"charge_code":"OCEAN_FREIGHT","currency":"USD","calculator":"PER_UNIT","unit":"FREIGHT_TONNE","rate":%d},
		  {"charge_code":"CFS_HANDLING","currency":"CNY","calculator":"PER_UNIT","unit":"FREIGHT_TONNE","rate":12000,"minimum":15000}]}],%s}`, rate, when(at))
	}
	c.expect("a SELL card by a buyer", c.do("POST", "/v1/rate-cards", buyer, `{"rate_card_id":"T-X","side":"SELL","scope":"TARIFF",
		"legal_entity_id":"LE","name":"x",`+when(at)+`}`, "Idempotency-Key", "card-x"), 403, "FORBIDDEN")
	for _, card := range []struct{ id, side, scope, party, token string }{
		{"T-SEA", "SELL", "TARIFF", "", pricing}, {"S-SHA-HAM", "BUY", "SUPPLIER", "coloader", buyer}} {
		r := ok("create "+card.id, c.do("POST", "/v1/rate-cards", card.token, fmt.Sprintf(`{"rate_card_id":%q,"side":%q,"scope":%q,
			"legal_entity_id":"LE","party_id":%q,"name":%q,%s}`, card.id, card.side, card.scope, card.party, card.id, when(at)),
			"Idempotency-Key", "card-"+card.id), 201, `"1"`)
		if r.header.Get("Location") != "/v1/rate-cards/"+card.id {
			t.Fatalf("Location %q", r.header.Get("Location"))
		}
		ok("draft "+card.id, c.do("POST", "/v1/rate-cards/"+card.id+"/versions", card.token, `{"valid_from":"`+at.Format(time.RFC3339)+
			`","currency":"USD","source_ref":"tariff 2026-11",`+when(at)+`}`, "Idempotency-Key", "draft-"+card.id, "If-Match", `"1"`), 201, `"2"`)
		rate := 5000
		if card.side == "BUY" {
			rate = 3500
		}
		ok("entries "+card.id, c.do("PUT", "/v1/rate-cards/"+card.id+"/versions/1/entries", card.token, lcl(rate), "Idempotency-Key",
			"set-"+card.id, "If-Match", `"2"`), 200, `"3"`)
	}
	c.expect("a draft without If-Match", c.do("POST", "/v1/rate-cards/T-SEA/versions", pricing, `{"valid_from":"`+at.Format(time.RFC3339)+
		`","currency":"USD",`+when(at)+`}`, "Idempotency-Key", "draft-x"), 428, "PRECONDITION_REQUIRED")
	c.expect("a draft at a stale revision", c.do("POST", "/v1/rate-cards/T-SEA/versions", pricing, `{"valid_from":"`+at.Format(time.RFC3339)+
		`","currency":"USD",`+when(at)+`}`, "Idempotency-Key", "draft-y", "If-Match", `"1"`), 409, "")
	c.expect("a BUY card drafted by a SELL maintainer", c.do("POST", "/v1/rate-cards/S-SHA-HAM/versions", pricing, `{"valid_from":"`+
		at.Format(time.RFC3339)+`","currency":"USD",`+when(at)+`}`, "Idempotency-Key", "draft-z", "If-Match", `"3"`), 403, "FORBIDDEN")
	c.expect("entries of a version the card does not have", c.do("PUT", "/v1/rate-cards/T-SEA/versions/9/entries", pricing, lcl(5000),
		"Idempotency-Key", "set-9", "If-Match", `"3"`), 404, "NOT_FOUND")
	c.expect("the create replayed by one who lost the permission", c.do("POST", "/v1/rate-cards", reader, `{"rate_card_id":"T-SEA","side":"SELL",
		"scope":"TARIFF","legal_entity_id":"LE","party_id":"","name":"T-SEA",`+when(at)+`}`, "Idempotency-Key", "card-T-SEA"), 403, "FORBIDDEN")
	v := ok("version", c.do("GET", "/v1/rate-cards/T-SEA/versions/1", pricing, ""), 200, `"3"`)
	entries, _ := v.body["entries"].([]any)
	if len(entries) != 1 || v.body["status"] != "DRAFT" {
		t.Fatalf("the draft: %v", v.body)
	}
	if e := entries[0].(map[string]any); e["origin"] != "CNSHG" || e["edition"] == "" || len(e["lines"].([]any)) != 2 ||
		e["lines"].([]any)[1].(map[string]any)["no"] != 2.0 {
		t.Fatalf("CNSHA for SEA resolves to the port CNSHG, lines numbered: %v", e)
	}
	c.expect("a tariff published by its preparer without approval", c.do("POST", "/v1/rate-cards/T-SEA/versions/1/publish", pricing, "{"+when(at.Add(time.Minute))+"}",
		"Idempotency-Key", "publish-T-SEA-0", "If-Match", `"3"`), 403, "FORBIDDEN")
	publish := "{" + when(at.Add(time.Minute)) + "}"
	ok("the manager publishes", c.do("POST", "/v1/rate-cards/T-SEA/versions/1/publish", manager, publish, "Idempotency-Key", "publish-T-SEA",
		"If-Match", `"3"`), 200, `"4"`)
	ok("the publication replayed", c.do("POST", "/v1/rate-cards/T-SEA/versions/1/publish", manager, publish, "Idempotency-Key", "publish-T-SEA",
		"If-Match", `"3"`), 200, `"4"`)
	c.expect("the publication replayed by one who may not approve", c.do("POST", "/v1/rate-cards/T-SEA/versions/1/publish", pricing, publish,
		"Idempotency-Key", "publish-T-SEA", "If-Match", `"3"`), 403, "FORBIDDEN")
	ok("the co-loader's card published", c.do("POST", "/v1/rate-cards/S-SHA-HAM/versions/1/publish", buyer, publish, "Idempotency-Key",
		"publish-S", "If-Match", `"3"`), 200, `"4"`)
	c.expect("entries of a published version", c.do("PUT", "/v1/rate-cards/T-SEA/versions/1/entries", pricing, lcl(6000), "Idempotency-Key",
		"set-again", "If-Match", `"4"`), 422, "")
	for _, bad := range []string{"0", "01", "x"} {
		c.expect("version "+bad, c.do("GET", "/v1/rate-cards/T-SEA/versions/"+bad, pricing, ""), 404, "NOT_FOUND")
	}
	card := ok("the tariff", c.do("GET", "/v1/rate-cards/T-SEA", pricing, ""), 200, `"4"`)
	if vs, _ := card.body["versions"].([]any); len(vs) != 1 || vs[0].(map[string]any)["status"] != "PUBLISHED" ||
		vs[0].(map[string]any)["approved_by"] != "pricing-manager" {
		t.Fatalf("the tariff: %v", card.body)
	}
	if cards, _ := ok("SELL cards", c.do("GET", "/v1/rate-cards?side=SELL", pricing, ""), 200, "").body["rate_cards"].([]any); len(cards) != 1 {
		t.Fatalf("SELL cards: %v", cards)
	}
	c.expect("a bad valid_at", c.do("GET", "/v1/rate-cards?valid_at=yesterday", pricing, ""), 400, "MALFORMED_REQUEST")
	c.expect("a bad side", c.do("GET", "/v1/rate-cards?side=buy", pricing, ""), 400, "MALFORMED_REQUEST")
	// supplier costs: rate:Read shows SELL cards only; ReadBuyRates or MaintainBuyRates shows BUY cards
	if cards, _ := ok("cards a reader sees", c.do("GET", "/v1/rate-cards", reader, ""), 200, "").body["rate_cards"].([]any); len(cards) != 1 ||
		cards[0].(map[string]any)["side"] != "SELL" {
		t.Fatalf("a reader sees SELL cards only: %v", cards)
	}
	if cards, _ := ok("cards an auditor sees", c.do("GET", "/v1/rate-cards", auditor, ""), 200, "").body["rate_cards"].([]any); len(cards) != 2 {
		t.Fatalf("an auditor sees both: %v", cards)
	}
	c.expect("BUY cards listed by a reader", c.do("GET", "/v1/rate-cards?side=BUY", reader, ""), 403, "FORBIDDEN")
	c.expect("a BUY card read by a reader", c.do("GET", "/v1/rate-cards/S-SHA-HAM", reader, ""), 403, "FORBIDDEN")
	c.expect("a BUY version read by a reader", c.do("GET", "/v1/rate-cards/S-SHA-HAM/versions/1", reader, ""), 403, "FORBIDDEN")
	ok("a BUY version read by its buyer", c.do("GET", "/v1/rate-cards/S-SHA-HAM/versions/1", buyer, ""), 200, `"4"`)
	c.expect("cards read without rate:Read", c.do("GET", "/v1/rate-cards", mint("ops-1"), ""), 403, "FORBIDDEN")
	c.expect("an unknown card", c.do("GET", "/v1/rate-cards/NO-CARD", pricing, ""), 404, "NOT_FOUND")
	stranger, err := MintDevToken(cfg, uniqueTenant("cargo-api-other-"), "pricing-9", []string{"rate:*"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.expect("another tenant's card", c.do("GET", "/v1/rate-cards/T-SEA", stranger, ""), 404, "NOT_FOUND")

	// a tariff its preparer publishes with ApproveOwnTariff, flagged; replayed after the permission is withdrawn: refused
	own := mint("pricing-manager-2", "rate:Read", "rate:MaintainSellRates", "rate:ApproveTariff", "rate:ApproveOwnTariff")
	ok("create T-AIR", c.do("POST", "/v1/rate-cards", own, `{"rate_card_id":"T-AIR","side":"SELL","scope":"TARIFF","legal_entity_id":"LE",
		"name":"T-AIR",`+when(at)+`}`, "Idempotency-Key", "card-T-AIR"), 201, `"1"`)
	ok("draft T-AIR", c.do("POST", "/v1/rate-cards/T-AIR/versions", own, `{"valid_from":"`+at.Format(time.RFC3339)+`","currency":"USD",`+
		when(at)+`}`, "Idempotency-Key", "draft-T-AIR", "If-Match", `"1"`), 201, `"2"`)
	air := `{"entries":[{"mode":"AIR","container_mode":"LOOSE","origin":"CNPVG","destination":"DEFRA","service_level":"STANDARD","commodity":"GENERAL",
		"lines":[{"charge_code":"AIR_FREIGHT","currency":"CNY","calculator":"PER_UNIT","unit":"KG_CHARGEABLE","rate":3000,"minimum":30000,
		"breaks":[{"from":45000,"rate":2500},{"from":100000,"rate":2000}]}]}],` + when(at) + `}`
	ok("entries T-AIR", c.do("PUT", "/v1/rate-cards/T-AIR/versions/1/entries", own, air, "Idempotency-Key", "set-T-AIR", "If-Match", `"2"`),
		200, `"3"`)
	if lines := ok("T-AIR's entries", c.do("GET", "/v1/rate-cards/T-AIR/versions/1", own, ""), 200, `"3"`).body["entries"].([]any)[0].(map[string]any)["lines"].([]any); len(lines[0].(map[string]any)["breaks"].([]any)) != 2 {
		t.Fatalf("breaks: %v", lines)
	}
	ok("self-approved", c.do("POST", "/v1/rate-cards/T-AIR/versions/1/publish", own, publish, "Idempotency-Key", "publish-T-AIR", "If-Match", `"3"`),
		200, `"4"`)
	if vs := ok("T-AIR", c.do("GET", "/v1/rate-cards/T-AIR", own, ""), 200, `"4"`).body["versions"].([]any); vs[0].(map[string]any)["self_approved"] != true {
		t.Fatalf("not flagged: %v", vs)
	}
	c.expect("the self-approval replayed without ApproveOwnTariff", c.do("POST", "/v1/rate-cards/T-AIR/versions/1/publish",
		mint("pricing-manager-2", "rate:Read", "rate:MaintainSellRates", "rate:ApproveTariff"), publish, "Idempotency-Key", "publish-T-AIR",
		"If-Match", `"3"`), 403, "FORBIDDEN")

	// 0.8 t and 2.5 m3: 2.5 W/M; SELL 125.00 USD and CNY 300.00 CFS; BUY 87.50 USD
	evaluate := `{"snapshot_id":"RS-LCL","purpose":"QUOTE_LINE","legal_entity_id":"LE","sides":["SELL","BUY"],"mode":"SEA","container_mode":"LCL",
		"origin":"CNSHA","destination":"DEHAM","service_level":"STANDARD","commodity":"GENERAL","cargo":{"pieces":4,"gross_grams":800000,
		"volume_cm3":2500000},` + when(at.Add(time.Hour)) + `}`
	c.expect("an evaluation without permission", c.do("POST", "/v1/rate-evaluations", pricing, evaluate, "Idempotency-Key", "evaluate-0"), 403, "FORBIDDEN")
	if r := ok("evaluated", c.do("POST", "/v1/rate-evaluations", sales, evaluate, "Idempotency-Key", "evaluate"), 201, ""); r.header.Get("Location") !=
		"/v1/rate-snapshots/RS-LCL" {
		t.Fatalf("Location %q", r.header.Get("Location"))
	}
	ok("the evaluation replayed", c.do("POST", "/v1/rate-evaluations", sales, evaluate, "Idempotency-Key", "evaluate"), 201, "")
	c.expect("the evaluation replayed by one who lost the permission", c.do("POST", "/v1/rate-evaluations", mint("sales-1", "rate:Read"), evaluate,
		"Idempotency-Key", "evaluate"), 403, "FORBIDDEN")
	// sales sees the SELL side of the snapshot only; the auditor sees the costs
	if s := ok("the snapshot for sales", c.do("GET", "/v1/rate-snapshots/RS-LCL", sales, ""), 200, ""); len(s.body["lines"].([]any)) != 2 ||
		len(s.body["candidates"].([]any)) != 1 || s.body["customer"] != nil {
		t.Fatalf("sales sees SELL only: %v", s.body)
	}
	s := ok("the snapshot", c.do("GET", "/v1/rate-snapshots/RS-LCL", auditor, ""), 200, "")
	want := map[string]float64{"SELL OCEAN_FREIGHT": 12_500, "SELL CFS_HANDLING": 30_000, "BUY OCEAN_FREIGHT": 8_750, "BUY CFS_HANDLING": 30_000}
	lines, _ := s.body["lines"].([]any)
	if len(lines) != 4 || s.body["origin"] != "CNSHG" {
		t.Fatalf("the snapshot: %v", s.body)
	}
	for _, l := range lines {
		m := l.(map[string]any)
		if amount := m["amount"].(map[string]any); amount["minor"] != want[m["side"].(string)+" "+m["charge_code"].(string)] || m["quantity"] != 2500.0 {
			t.Errorf("line %v", m)
		}
	}
	c.expect("an unknown snapshot", c.do("GET", "/v1/rate-snapshots/RS-NONE", sales, ""), 404, "NOT_FOUND")
	c.expect("another tenant's snapshot", c.do("GET", "/v1/rate-snapshots/RS-LCL", stranger, ""), 404, "NOT_FOUND")
	// a lane no card prices: gaps, without amounts
	ok("a lane without cards", c.do("POST", "/v1/rate-evaluations", sales, strings.Replace(strings.Replace(evaluate, `"RS-LCL"`, `"RS-GAP"`, 1),
		`"DEHAM"`, `"USNYC"`, 1), "Idempotency-Key", "evaluate-gap"), 201, "")
	gaps := ok("the gaps", c.do("GET", "/v1/rate-snapshots/RS-GAP", auditor, ""), 200, "").body["lines"].([]any)
	if len(gaps) == 0 {
		t.Fatal("no gap lines")
	}
	for _, l := range gaps {
		if m := l.(map[string]any); m["gap"] == "" || m["amount"] != nil {
			t.Fatalf("a gap with an amount: %v", m)
		}
	}
	ok("withdrawn", c.do("POST", "/v1/rate-cards/T-SEA/versions/1/withdraw", pricing, `{"reason":"replaced by the 2027 tariff",`+
		when(time.Now().UTC())+`}`, "Idempotency-Key", "withdraw", "If-Match", `"4"`), 200, `"5"`)
	if vs := ok("the tariff withdrawn", c.do("GET", "/v1/rate-cards/T-SEA", pricing, ""), 200, `"5"`).body["versions"].([]any); vs[0].(map[string]any)["status"] !=
		"WITHDRAWN" {
		t.Fatalf("withdrawn: %v", vs)
	}
}

// Quotes through the HTTP API with the real ORG: a manual quote below the margin floor waits for approval, its
// costs and margin shown to the approver only; its author may not approve it; approved, accepted and read as
// QuoteSnapshotV3 without costs; another expired after its validity; replays answered only to who may still act; the
// same create sent eight times at once commits once.
func TestHTTPQuotesOnPostgres(t *testing.T) {
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
	tenant := uniqueTenant("cargo-api-quotes-")
	seedORG(t, p, tenant, "party-a")
	org, err := p.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err = org.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-a", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "party-a", ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: now.Add(-72 * time.Hour), ValidUntil: now.Add(365 * 24 * time.Hour),
		EvidenceRef: "customer contract", OccurredAt: now.Add(-72 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	mint := func(actor string, perms ...string) string {
		t.Helper()
		token, err := MintDevToken(cfg, tenant, actor, perms, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	sales := mint("sales-1", "rate:Read", "rate:CreateQuote", "rate:ExpireQuote")
	salesApprover := mint("sales-1", "rate:Read", "rate:ApproveQuotePrice")
	approver := mint("manager-1", "rate:Read", "rate:ApproveQuotePrice")
	acceptor := mint("sales-2", "rate:Read", "rate:AcceptQuote")
	when := func(t time.Time) string { return `"occurred_at":"` + t.Format(time.RFC3339) + `"` }
	ok := func(name string, r apiResponse, status int, etag string) apiResponse {
		t.Helper()
		if r.status != status || (etag != "" && r.header.Get("ETag") != etag) {
			t.Fatalf("%s: %d %v %v", name, r.status, r.header, r.body)
		}
		return r
	}
	quote := func(id string, from, until, at time.Time) string {
		return fmt.Sprintf(`{"quote_id":%q,"legal_entity_id":"LE","customer_party_id":"party-a","currency":"USD","lines":[{"charge_code":"AIR_FREIGHT",
		  "sell":{"currency":"USD","minor":100000},"cost":{"currency":"USD","minor":90000}}],"terms":{"service_level":"STANDARD"},
		  "valid_from":%q,"valid_until":%q,%s}`, id, from.Format(time.RFC3339), until.Format(time.RFC3339), when(at))
	}
	if levels, _ := ok("service levels", c.do("GET", "/v1/service-levels", sales, ""), 200, "").body["service_levels"].([]any); len(levels) == 0 {
		t.Fatal("no service levels")
	}
	q1 := quote("Q-1", now.Add(-time.Hour), now.Add(7*24*time.Hour), now.Add(-time.Minute))
	c.expect("a quote by one who may not create", c.do("POST", "/v1/quotes", approver, q1, "Idempotency-Key", "quote-0"), 403, "FORBIDDEN")
	c.expect("a tenant in the body", c.do("POST", "/v1/quotes", sales, strings.Replace(q1, `"quote_id"`, `"tenant_id":"x","quote_id"`, 1),
		"Idempotency-Key", "quote-x"), 400, "MALFORMED_REQUEST")
	responses := make([]apiResponse, 8)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range responses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses[i] = c.do("POST", "/v1/quotes", sales, q1, "Idempotency-Key", "quote-1")
		}()
	}
	close(start)
	wg.Wait()
	for i, r := range responses {
		if r.status != 201 || r.header.Get("Location") != "/v1/quotes/Q-1" || r.header.Get("ETag") != `"1"` || r.body["subject_id"] != "Q-1" {
			t.Fatalf("create %d: %d %v %v", i, r.status, r.header, r.body)
		}
	}
	// the creator reads the quote without costs; the approver sees costs and the margin of 10%, below the floor of 20%
	mine := ok("the quote", c.do("GET", "/v1/quotes/Q-1", sales, ""), 200, `"1"`).body
	if mine["status"] != "AWAITING_APPROVAL" || mine["cost_total"] != nil || mine["margin_bp"] != nil ||
		mine["lines"].([]any)[0].(map[string]any)["cost"] != nil || mine["approval_reasons"].([]any)[0] != "BELOW_MARGIN_FLOOR" {
		t.Fatalf("the creator's view: %v", mine)
	}
	theirs := ok("the quote for the approver", c.do("GET", "/v1/quotes/Q-1", approver, ""), 200, `"1"`).body
	if theirs["margin_bp"] != 1000.0 || theirs["margin_floor_bp"] != 2000.0 || theirs["cost_total"].(map[string]any)["minor"] != 90000.0 {
		t.Fatalf("the approver's view: %v", theirs)
	}
	c.expect("a snapshot before acceptance", c.do("GET", "/v1/quotes/Q-1/snapshot", sales, ""), 422, "")
	approve := `{"reason_ref":"strategic account 2026-11",` + when(now) + `}`
	c.expect("its author approving without ApproveOwnQuotePrice", c.do("POST", "/v1/quotes/Q-1/approve-price", salesApprover, approve,
		"Idempotency-Key", "approve-own", "If-Match", `"1"`), 422, "")
	c.expect("an approval by one who may not", c.do("POST", "/v1/quotes/Q-1/approve-price", sales, approve, "Idempotency-Key", "approve-x",
		"If-Match", `"1"`), 403, "FORBIDDEN")
	ok("approved", c.do("POST", "/v1/quotes/Q-1/approve-price", approver, approve, "Idempotency-Key", "approve", "If-Match", `"1"`), 200, `"2"`)
	ok("the approval replayed", c.do("POST", "/v1/quotes/Q-1/approve-price", approver, approve, "Idempotency-Key", "approve", "If-Match", `"1"`),
		200, `"2"`)
	c.expect("the approval replayed by one who lost the permission", c.do("POST", "/v1/quotes/Q-1/approve-price", mint("manager-1", "rate:Read"),
		approve, "Idempotency-Key", "approve", "If-Match", `"1"`), 403, "FORBIDDEN")
	accept := `{"evidence_kind":"EMAIL","evidence_ref":"mail 2026-11-13 #12",` + when(now) + `}`
	c.expect("an acceptance at a stale revision", c.do("POST", "/v1/quotes/Q-1/accept", acceptor, accept, "Idempotency-Key", "accept-0",
		"If-Match", `"1"`), 409, "")
	ok("accepted", c.do("POST", "/v1/quotes/Q-1/accept", acceptor, accept, "Idempotency-Key", "accept", "If-Match", `"2"`), 200, `"3"`)
	snapshot := c.do("GET", "/v1/quotes/Q-1/snapshot", sales, "")
	if snapshot.status != 200 || snapshot.body["total"].(map[string]any)["minor"] != 100000.0 || snapshot.body["price_approved_by"] != "manager-1" ||
		snapshot.body["kind"] != "ONE_OFF" {
		t.Fatalf("the snapshot: %d %v", snapshot.status, snapshot.body)
	}
	raw, _ := json.Marshal(snapshot.body)
	if strings.Contains(string(raw), "cost") || strings.Contains(string(raw), "margin") {
		t.Fatalf("a snapshot with costs: %s", raw)
	}
	if q := ok("the accepted quote", c.do("GET", "/v1/quotes/Q-1", sales, ""), 200, `"3"`).body; q["status"] != "ACCEPTED" ||
		q["acceptance"].(map[string]any)["evidence_kind"] != "EMAIL" || q["approval"].(map[string]any)["approved_by"] != "manager-1" {
		t.Fatalf("the accepted quote: %v", q)
	}
	// a quote whose validity has passed is expired
	q2 := quote("Q-2", now.Add(-3*time.Hour), now.Add(-time.Hour), now.Add(-2*time.Hour))
	ok("a short quote", c.do("POST", "/v1/quotes", sales, q2, "Idempotency-Key", "quote-2"), 201, `"1"`)
	ok("expired", c.do("POST", "/v1/quotes/Q-2/expire", sales, "{"+when(now)+"}", "Idempotency-Key", "expire-2", "If-Match", `"1"`), 200, `"2"`)
	c.expect("an expiry replayed by one who lost the permission", c.do("POST", "/v1/quotes/Q-2/expire", mint("sales-1", "rate:Read"),
		"{"+when(now)+"}", "Idempotency-Key", "expire-2", "If-Match", `"1"`), 403, "FORBIDDEN")
	c.expect("an unknown quote", c.do("GET", "/v1/quotes/NO-QUOTE", sales, ""), 404, "NOT_FOUND")
	stranger, err := MintDevToken(cfg, uniqueTenant("cargo-api-other-"), "sales-9", []string{"rate:*"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.expect("another tenant's quote", c.do("GET", "/v1/quotes/Q-1", stranger, ""), 404, "NOT_FOUND")
	c.expect("quotes read without rate:Read", c.do("GET", "/v1/quotes/Q-1", mint("ops-1"), ""), 403, "FORBIDDEN")
}
