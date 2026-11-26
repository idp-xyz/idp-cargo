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

// BOOK and AIR through HTTP alone (stage 3, V2.108), on PostgreSQL, with the job roles' tokens: the airline's codes and
// CARRIER role recorded by the administrator through ORG's routes, an air consolidation through PHY's, a booking
// drafted, sent, listed as awaiting the carrier, confirmed with its MAWB, found by the MAWB and listed for the
// consolidation; an air movement opened on it, a HAWB series registered, the movement found by its MAWB and
// cancelled. The refusals of a role without the permission and of another tenant.
func TestBOOKAndAIRThroughHTTPWithTheJobRolesOnPostgres(t *testing.T) {
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
	tenant, other := uniqueTenant("cargo-book-api-a-"), uniqueTenant("cargo-book-api-b-")
	seedORG(t, p, tenant, "airline-mu")
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
	admin, ops, docs, finance := token(tenant, "ada", "admin"), token(tenant, "otto", "ops"), token(tenant, "dora", "docs"), token(tenant, "finn", "finance")
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	etd := at.Add(72 * time.Hour)
	ts := func(t time.Time) string { return t.Format(time.RFC3339) }
	occurred := func(h int) string { return fmt.Sprintf(`"occurred_at":%q`, ts(at.Add(time.Duration(h)*time.Minute))) }
	evidence := func(h int) string {
		return fmt.Sprintf(`{"sha256":%q,"storage_id":"mail/line.eml","media_type":"message/rfc822","captured_at":%q}`, strings.Repeat("6b", 32),
			ts(at.Add(time.Duration(h)*time.Minute)))
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

	// ORG: the airline's codes and CARRIER role; PHY: the air consolidation
	want("the designator", post(admin, "mu-1", "/v1/parties/airline-mu/identifiers", `"1"`, `{"type":"IATA_AIRLINE","country":"ZZ","value":"MU","evidence_ref":"IATA directory"}`), 200, `"2"`)
	want("the prefix", post(admin, "mu-2", "/v1/parties/airline-mu/identifiers", `"2"`, `{"type":"IATA_AWB_PREFIX","country":"ZZ","value":"781","evidence_ref":"IATA directory"}`), 200, `"3"`)
	want("the carrier role", post(admin, "mu-3", "/v1/parties/airline-mu/roles", `"3"`, fmt.Sprintf(`{"legal_entity_id":"LE","role_code":"CARRIER","valid_from":%q,"evidence_ref":"carrier contract",%s}`,
		ts(at.Add(-time.Hour)), occurred(0))), 200, `"4"`)
	want("the consolidation", post(ops, "c-1", "/v1/consolidations", "", fmt.Sprintf(`{"consolidation_id":"C-1","mode":"AIR","kind":"AIR_CONSOL","operating_legal_entity_id":"LE",`+
		`"origin":{"scheme":"IATA","code":"PVG"},"destination":{"scheme":"IATA","code":"JFK"},"planned_departure":%q,"house_cut_off":%q,%s}`, ts(etd), ts(etd.Add(-12*time.Hour)), occurred(1))), 201, `"1"`)

	// BOOK: the booking from its draft to the carrier's confirmation
	draft := fmt.Sprintf(`{"booking_id":"BK-1","kind":"AIR_CONSOL","legal_entity_id":"LE","carrier_party_id":"airline-mu","consolidation_id":"C-1",`+
		`"request":{"legs":[{"loading":{"scheme":"IATA","code":"PVG"},"discharge":{"scheme":"UNLOCODE","code":"USJFK"},"flight":"MU587","departure_date":%q}],`+
		`"capacity":{"pieces":30,"gross_weight":{"unit":"KGM","amount":1500}},"handling_codes":["GEN"]},%s}`, etd.Format(time.DateOnly), occurred(2))
	c.expect("the finance staff drafts a booking", post(finance, "b-0", "/v1/bookings", "", draft), 403, "FORBIDDEN")
	want("draft", post(ops, "b-1", "/v1/bookings", "", draft), 201, `"1"`)
	want("sent", post(ops, "b-2", "/v1/bookings/BK-1/sent", `"1"`, `{"channel":"CARRIER_PORTAL","evidence":`+evidence(3)+`,`+occurred(3)+`}`), 200, `"2"`)
	awaiting := want("awaiting the carrier", c.do("GET", "/v1/bookings-awaiting-carrier?sent_before="+ts(time.Now().UTC().Add(time.Hour))+"&limit=10", docs, ""), 200, "")
	if !strings.Contains(fmt.Sprint(awaiting.body), "BK-1") {
		t.Fatalf("awaiting: %v", awaiting.body)
	}
	confirmation := fmt.Sprintf(`{"answer":{"channel":"EMAIL","evidence":%s,"carrier_time":%q,"references":[{"type":"MAWB_NUMBER","value":"781-12345675"},`+
		`{"type":"BOOKING_NUMBER","value":"mu-bk-0815"}]},"lines":[{"capacity":{"pieces":30,"gross_weight":{"unit":"KGM","amount":1500}},"legs":[{"loading":{"scheme":"IATA","code":"PVG"},`+
		`"discharge":{"scheme":"IATA","code":"JFK"},"flight":"MU587","etd":%q,"eta":%q,"cut_offs":[{"kind":"CARGO_ACCEPTANCE","at":%q,"time_zone":"Asia/Shanghai"}]}]}],%s}`,
		evidence(4), ts(at.Add(4*time.Minute)), ts(etd), ts(etd.Add(14*time.Hour)), ts(etd.Add(-5*time.Hour)), occurred(4))
	c.expect("confirm at a stale revision", post(ops, "b-3", "/v1/bookings/BK-1/confirmation", `"1"`, confirmation), 409, "")
	want("confirm", post(ops, "b-4", "/v1/bookings/BK-1/confirmation", `"2"`, confirmation), 200, `"3"`)
	byMAWB := want("by the MAWB", c.do("GET", "/v1/booking-references?carrier_party_id=airline-mu&type=MAWB_NUMBER&value=78112345675", docs, ""), 200, `"3"`)
	if byMAWB.body["booking_id"] != "BK-1" || byMAWB.body["status"] != "CONFIRMED" {
		t.Fatalf("by the MAWB: %v", byMAWB.body)
	}
	if held := want("the consolidation's bookings", c.do("GET", "/v1/bookings?consolidation_id=C-1", finance, ""), 200, ""); len(held.body["items"].([]any)) != 1 {
		t.Fatalf("held: %v", held.body)
	}
	want("the cut-offs due", c.do("GET", "/v1/cut-offs?from="+ts(at)+"&until="+ts(etd)+"&limit=10", ops, ""), 200, "")
	c.expect("another tenant", c.do("GET", "/v1/bookings/BK-1", token(other, "otto", "ops"), ""), 404, "NOT_FOUND")

	// AIR: the movement on the confirmed booking, a HAWB series
	c.expect("the documentation staff opens a movement", post(docs, "m-0", "/v1/air-movements", "", `{"movement_id":"AM-1","consolidation_id":"C-1","booking_id":"BK-1",`+occurred(5)+`}`), 403, "FORBIDDEN")
	want("open the movement", post(ops, "m-1", "/v1/air-movements", "", `{"movement_id":"AM-1","consolidation_id":"C-1","booking_id":"BK-1",`+occurred(5)+`}`), 201, `"1"`)
	want("the HAWB series", post(ops, "m-2", "/v1/hawb-series/LE", "", `{"template":"SHA{seq:7}","first_number":1,`+occurred(6)+`}`), 200, "")
	want("read the series", c.do("GET", "/v1/hawb-series/LE", docs, ""), 200, "")
	movement := want("read the movement", c.do("GET", "/v1/air-movements/AM-1", docs, ""), 200, `"1"`)
	if !strings.Contains(fmt.Sprint(movement.body), "MU587") {
		t.Fatalf("the movement: %v", movement.body)
	}
	if found := want("by its MAWB", c.do("GET", "/v1/air-movements?mawb=781-12345675", docs, ""), 200, ""); found.body["movement_id"] != "AM-1" {
		t.Fatalf("by its MAWB: %v", found.body)
	}
	want("cancel", post(ops, "m-3", "/v1/air-movements/AM-1/cancel", `"1"`, `{"reason":"the shipper withdrew",`+occurred(7)+`}`), 200, `"2"`)
}
