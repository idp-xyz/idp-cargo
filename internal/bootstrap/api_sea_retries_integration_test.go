//go:build integration

package bootstrap

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// SEA's routes authorize a command before they answer its replay, and hold its command id while it commits, as
// RATE's do and as every context's routes must (V2.53 to V2.55). An actor whose permission is gone is not answered
// the receipt of a command it once ran; a principal without the permission is answered alike for a key in use and
// for one that is not; and the same command sent eight times at once commits once and answers one receipt eight
// times.
func TestHTTPSeaRetriesOnPostgres(t *testing.T) {
	platform := openPlatform(t)
	cfg := APIConfig{TokenKey: []byte("api-test-key-of-thirty-two-bytes!!"), TokenIssuer: "idp-cargo-test", TokenAudience: "idp-cargo-api",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MarginFloorBP: 2000}
	h, err := platform.API(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := apiClient{t: t, srv: srv}
	mint := func(tenant, actor string, perms ...string) string {
		t.Helper()
		token, err := MintDevToken(cfg, tenant, actor, perms, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond).Format(time.RFC3339Nano)
	body := `{"legal_entity_id":"LE","template":"SHAL{seq:7}","first_number":1,"occurred_at":"` + at + `"}`
	other := strings.Replace(body, `"first_number":1`, `"first_number":2`, 1)
	post := func(token, b, key string) apiResponse {
		t.Helper()
		return c.do("POST", "/v1/hbl-series", token, b, "Idempotency-Key", key)
	}

	tenant := uniqueTenant("cargo-sea-retry-a-")
	seedORG(t, platform, tenant, "party-l")
	ops := mint(tenant, "ops-1", "sea:RegisterHblSeries", "sea:Read")
	first := post(ops, body, "series")
	if first.status != 201 || first.body["subject_id"] != "LE" || first.body["command_id"] != "series" || first.header.Get("ETag") != `"1"` {
		t.Fatalf("registered: %d %v %v", first.status, first.header, first.body)
	}
	// the actor still allowed is answered its receipt, and another body under the key is another command
	if again := post(ops, body, "series"); again.status != 201 || !reflect.DeepEqual(again.body, first.body) {
		t.Fatalf("the replay: %d %v, want %v", again.status, again.body, first.body)
	}
	c.expect("another body under the key", post(ops, other, "series"), 409, "IDEMPOTENCY_MISMATCH")
	// the same actor, its permission gone: neither the receipt nor whether the body matches
	revoked := mint(tenant, "ops-1", "sea:Read")
	c.expect("a replay without the permission", post(revoked, body, "series"), 403, "FORBIDDEN")
	c.expect("another body without the permission", post(revoked, other, "series"), 403, "FORBIDDEN")
	// another actor that never held it: a key in use and a key not in use are answered alike
	reader := mint(tenant, "cs-1", "sea:Read")
	c.expect("another actor, the key in use", post(reader, body, "series"), 403, "FORBIDDEN")
	c.expect("another actor, a key not in use", post(reader, body, "series-unused"), 403, "FORBIDDEN")
	// every SEA permission is the permission
	if again := post(mint(tenant, "ops-1", "sea:*"), body, "series"); again.status != 201 || !reflect.DeepEqual(again.body, first.body) {
		t.Fatalf("the replay with sea:*: %d %v", again.status, again.body)
	}

	// The same command eight times at once, in a tenant of its own.
	second := uniqueTenant("cargo-sea-retry-b-")
	seedORG(t, platform, second, "party-l")
	ops2 := mint(second, "ops-1", "sea:RegisterHblSeries", "sea:Read")
	responses := make([]apiResponse, 8)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range responses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses[i] = c.do("POST", "/v1/hbl-series", ops2, body, "Idempotency-Key", "series-at-once")
		}()
	}
	close(start)
	wg.Wait()
	for i, r := range responses {
		if r.status != 201 || r.header.Get("Location") != "/v1/hbl-series/LE" || r.header.Get("ETag") != `"1"` || !reflect.DeepEqual(r.body, responses[0].body) ||
			r.body["command_id"] != "series-at-once" || r.body["new_revision"] != 1.0 {
			t.Fatalf("request %d of eight: %d %v %v, want the receipt %v", i, r.status, r.header, r.body, responses[0].body)
		}
	}
	if s := c.do("GET", "/v1/hbl-series/LE", ops2, ""); s.status != 200 || s.body["template"] != "SHAL{seq:7}" || s.body["first_number"] != 1.0 {
		t.Fatalf("the series registered once: %d %v", s.status, s.body)
	}
	if again := post(ops2, body, "series-at-once"); again.status != 201 || !reflect.DeepEqual(again.body, responses[0].body) {
		t.Fatalf("sent once more: %d %v", again.status, again.body)
	}
	// a second registration under another key is another command, and is refused: the series exists once
	c.expect("a second registration", post(ops2, body, "series-again"), 409, "VERSION_CONFLICT")
}
