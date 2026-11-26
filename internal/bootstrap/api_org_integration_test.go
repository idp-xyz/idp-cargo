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

// ORG through HTTP alone (stage 3, V2.106), on PostgreSQL, with tokens of the default job roles: the forwarder's legal
// entity registered and verified, a shipper registered, activated and approved for a role, its address book kept by
// the operator, its contacts read only with the permission by name (the documentation staff can, the administrator's
// "org:*" cannot) and every read in the privacy officer's log, an erasure, a consignee suspended and archived; the
// replays, versions, refusals and another tenant. Nothing is written to the database directly.
func TestORGThroughHTTPWithTheJobRolesOnPostgres(t *testing.T) {
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
	tenant, other := uniqueTenant("cargo-org-api-a-"), uniqueTenant("cargo-org-api-b-")
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
	admin, ops, docs, privacy, finance := token(tenant, "ada", "admin"), token(tenant, "otto", "ops"), token(tenant, "dora", "docs"),
		token(tenant, "pia", "privacy"), token(tenant, "finn", "finance")
	stranger := token(other, "ada", "admin")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	occurred := fmt.Sprintf(`"occurred_at":%q`, at.Format(time.RFC3339))
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

	// 1. The forwarder's own company: registered (DRAFT), verified; only the administrator may.
	le := `{"legal_entity_id":"LE-SH","registered_name":"上海货代有限公司","jurisdiction":"CN","evidence_ref":"business licence 91310000",` + occurred + `}`
	c.expect("the operator registers a legal entity", post(ops, "le-0", "/v1/legal-entities", "", le), 403, "FORBIDDEN")
	first := want("register the legal entity", post(admin, "le-1", "/v1/legal-entities", "", le), 201, `"1"`)
	if first.body["legal_entity_id"] != "LE-SH" || first.body["command_id"] != "le-1" {
		t.Fatalf("the receipt: %v", first.body)
	}
	want("replayed", post(admin, "le-1", "/v1/legal-entities", "", le), 201, `"1"`)
	c.expect("the key with another body", post(admin, "le-1", "/v1/legal-entities", "", strings.Replace(le, "LE-SH", "LE-BJ", 1)), 409, "IDEMPOTENCY_MISMATCH")
	c.expect("the same id again", post(admin, "le-2", "/v1/legal-entities", "", le), 409, "")
	c.expect("a tenant in the body", post(admin, "le-3", "/v1/legal-entities", "", strings.Replace(le, `{`, `{"tenant_id":"x",`, 1)), 400, "MALFORMED_REQUEST")
	c.expect("no business time", post(admin, "le-4", "/v1/legal-entities", "", `{"legal_entity_id":"LE-X","registered_name":"X","jurisdiction":"CN","evidence_ref":"e"}`), 400, "OCCURRED_AT_REQUIRED")
	// a role for a legal entity not verified is refused
	want("register the shipper", post(admin, "p-1", "/v1/parties", "", `{"party_id":"shipper-a","legal_name":"上海甲贸易有限公司","country_code":"CN",`+occurred+`}`), 201, `"1"`)
	want("activate the shipper", post(admin, "p-2", "/v1/parties/shipper-a/activate", `"1"`, `{"evidence_ref":"business licence 9131 checked",`+occurred+`}`), 200, `"2"`)
	role := `{"legal_entity_id":"LE-SH","role_code":"SHIPPER","valid_from":"` + at.Add(-time.Hour).Format(time.RFC3339) + `","evidence_ref":"KYC-2026-11",` + occurred + `}`
	c.expect("a role for a legal entity not verified", post(admin, "role-0", "/v1/parties/shipper-a/roles", `"2"`, role), 422, "")
	c.expect("verify at a stale revision", post(admin, "le-v0", "/v1/legal-entities/LE-SH/verify", `"7"`, `{`+occurred+`}`), 409, "")
	want("verify", post(admin, "le-v1", "/v1/legal-entities/LE-SH/verify", `"1"`, `{`+occurred+`}`), 200, `"2"`)
	c.expect("verify twice", post(admin, "le-v2", "/v1/legal-entities/LE-SH/verify", `"2"`, `{`+occurred+`}`), 422, "")
	if got := want("the legal entity", c.do("GET", "/v1/legal-entities/LE-SH", finance, ""), 200, `"2"`); got.body["status"] != "VERIFIED" || got.body["registered_name"] != "上海货代有限公司" {
		t.Fatalf("the legal entity: %v", got.body)
	}
	if list := want("the legal entities", c.do("GET", "/v1/legal-entities", finance, ""), 200, ""); list.status != 200 {
		t.Fatal(list.body)
	}

	// 2. The shipper: approved for the role, an English name and an identifier; found in the lists.
	want("approve the role", post(admin, "role-1", "/v1/parties/shipper-a/roles", `"2"`, role), 200, `"3"`)
	c.expect("the operator approves a role", post(ops, "role-2", "/v1/parties/shipper-a/roles", `"3"`, role), 403, "FORBIDDEN")
	want("English name", post(admin, "en-1", "/v1/parties/shipper-a/english-name", `"3"`, `{"english_name":"Shanghai Jia Trading Co., Ltd.","evidence_ref":"licence"}`), 200, `"4"`)
	want("an identifier", post(admin, "id-1", "/v1/parties/shipper-a/identifiers", `"4"`, `{"type":"CN_CUSTOMS","country":"CN","value":"3105960ABC","evidence_ref":"customs registration"}`), 200, `"5"`)
	party := want("the shipper", c.do("GET", "/v1/parties/shipper-a", ops, ""), 200, `"5"`)
	if party.body["status"] != "ACTIVE" || party.body["english_name"] != "Shanghai Jia Trading Co., Ltd." || len(party.body["identifiers"].([]any)) != 1 {
		t.Fatalf("the shipper: %v", party.body)
	}
	want("register the consignee", post(admin, "p-3", "/v1/parties", "", `{"party_id":"consignee-de","legal_name":"Hamburg Import GmbH","country_code":"DE",`+occurred+`}`), 201, `"1"`)
	shippers := want("the shippers", c.do("GET", "/v1/parties?role_code=SHIPPER", ops, ""), 200, "")
	if ps := shippers.body["parties"].([]any); len(ps) != 1 || ps[0].(map[string]any)["party_id"] != "shipper-a" || len(ps[0].(map[string]any)["roles"].([]any)) != 1 {
		t.Fatalf("the shippers: %v", shippers.body)
	}
	byName := want("by name", c.do("GET", "/v1/parties?name_contains=hamburg", ops, ""), 200, "")
	if ps := byName.body["parties"].([]any); len(ps) != 1 || ps[0].(map[string]any)["party_id"] != "consignee-de" {
		t.Fatalf("by name: %v", byName.body)
	}
	page := want("a page of one", c.do("GET", "/v1/parties?limit=1", ops, ""), 200, "")
	if page.body["next"] != "consignee-de" {
		t.Fatalf("the page: %v", page.body)
	}
	c.expect("an unknown parameter", c.do("GET", "/v1/parties?tenant_id="+tenant, ops, ""), 400, "MALFORMED_REQUEST")
	c.expect("another tenant", c.do("GET", "/v1/parties/shipper-a", stranger, ""), 404, "NOT_FOUND")

	// 3. The address book, kept by the operator; the contacts, read by name only.
	address := `{"address_id":"HQ","address":{"short_code":"HQ","purposes":["OFFICE"],"name":"上海甲贸易有限公司","lines":["浦东新区世纪大道 100 号"],"city":"Shanghai","country_code":"CN"},"default_for":["OFFICE"]}`
	want("an address", post(ops, "addr-1", "/v1/parties/shipper-a/addresses", `"0"`, address), 200, `"1"`)
	want("a contact", post(ops, "ct-1", "/v1/parties/shipper-a/contacts", `"1"`, `{"contact_id":"C-1","contact":{"purposes":["DOCUMENTS"],"name":"王丽","email":"wang.li@example.com"},"default_for":["DOCUMENTS"]}`), 200, `"2"`)
	c.expect("the finance staff adds an address", post(finance, "addr-2", "/v1/parties/shipper-a/addresses", `"2"`, address), 403, "FORBIDDEN")
	if got := want("the addresses", c.do("GET", "/v1/parties/shipper-a/addresses", finance, ""), 200, `"2"`); len(got.body["addresses"].([]any)) != 1 {
		t.Fatalf("the addresses: %v", got.body)
	}
	contacts := want("the documentation staff reads the contacts", c.do("GET", "/v1/parties/shipper-a/contacts?purpose=DOCUMENTS", docs, ""), 200, "")
	if cs := contacts.body["contacts"].([]any); len(cs) != 1 || cs[0].(map[string]any)["email"] != "wang.li@example.com" {
		t.Fatalf("the contacts: %v", contacts.body)
	}
	c.expect("the administrator's org:* reads the contacts", c.do("GET", "/v1/parties/shipper-a/contacts?purpose=DOCUMENTS", admin, ""), 403, "FORBIDDEN")
	c.expect("the finance staff reads the contacts", c.do("GET", "/v1/parties/shipper-a/contacts?purpose=DOCUMENTS", finance, ""), 403, "FORBIDDEN")
	window := "?since=" + at.Add(-24*time.Hour).Format(time.RFC3339) + "&until=" + time.Now().UTC().Add(time.Hour).Format(time.RFC3339) + "&limit=100"
	log := want("the privacy officer reads the log", c.do("GET", "/v1/parties/shipper-a/contact-access"+window, privacy, ""), 200, "")
	if body := fmt.Sprint(log.body); !strings.Contains(body, "dora") {
		t.Fatalf("the log: %v", log.body)
	}
	c.expect("the administrator reads the log", c.do("GET", "/v1/parties/shipper-a/contact-access"+window, admin, ""), 403, "FORBIDDEN")
	c.expect("the operator erases", post(ops, "erase-0", "/v1/parties/shipper-a/contacts/C-1/erase", `"2"`, `{"reason":"the person asked"}`), 403, "FORBIDDEN")
	c.expect("the administrator's org:* erases", post(admin, "erase-1", "/v1/parties/shipper-a/contacts/C-1/erase", `"2"`, `{"reason":"the person asked"}`), 403, "FORBIDDEN")
	want("the privacy officer erases", post(privacy, "erase-2", "/v1/parties/shipper-a/contacts/C-1/erase", `"2"`, `{"reason":"the person asked"}`), 200, `"3"`)
	erased := want("read again", c.do("GET", "/v1/parties/shipper-a/contacts?purpose=DOCUMENTS", docs, ""), 200, "")
	if strings.Contains(fmt.Sprint(erased.body), "wang.li@example.com") {
		t.Fatalf("erased: %v", erased.body)
	}

	// 4. The consignee: activated, suspended, archived (no role).
	want("activate the consignee", post(admin, "p-4", "/v1/parties/consignee-de/activate", `"1"`, `{"evidence_ref":"Handelsregister HRB 1",`+occurred+`}`), 200, `"2"`)
	c.expect("archive an active party", post(admin, "p-5", "/v1/parties/consignee-de/archive", `"2"`, `{"reason":"no business",`+occurred+`}`), 422, "")
	want("suspend", post(admin, "p-6", "/v1/parties/consignee-de/suspend", `"2"`, `{"reason":"no business",`+occurred+`}`), 200, `"3"`)
	want("archive", post(admin, "p-7", "/v1/parties/consignee-de/archive", `"3"`, `{"reason":"no business",`+occurred+`}`), 200, `"4"`)
	if got := want("archived", c.do("GET", "/v1/parties/consignee-de", ops, ""), 200, `"4"`); got.body["status"] != "ARCHIVED" {
		t.Fatalf("the consignee: %v", got.body)
	}

	// 5. The contact transfer register and the onboardings, read by those who may.
	want("the register", c.do("GET", fmt.Sprintf("/v1/contact-transfers/%d", at.Year()), docs, ""), 200, "")
	want("the consultant reads the onboardings", c.do("GET", "/v1/onboardings", token(tenant, "cora", "consultant"), ""), 200, "")
	c.expect("the finance staff reads the onboardings", c.do("GET", "/v1/onboardings", finance, ""), 403, "FORBIDDEN")
	c.expect("the operator proposes an onboarding", post(ops, "onb-0", "/v1/onboardings", "", `{"onboarding_id":"ON-1","profile":"STANDARD"}`), 403, "FORBIDDEN")
	c.expect("a profile that is none", post(token(tenant, "cora", "consultant"), "onb-1", "/v1/onboardings", "", `{"onboarding_id":"ON-1","profile":"LAX"}`), 422, "")
}
