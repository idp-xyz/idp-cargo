//go:build integration

package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN's routes end to end on PostgreSQL 16 (V2.99), through cargo-api's handler with HS256 tokens: the profit pages,
// a job's profit and its version, a cost review requested with the version read, replayed, refused when stale or
// open, resolved by another person, and read back; permissions, tenant isolation and strict bodies. ORG, RATE, COM and
// FUL run in process; the loss is a cost line FIN records.
func TestHTTPFinProfitAndCostReviewOnPostgres(t *testing.T) {
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	finModule, err := platform.FIN(orgModule, rateModule, fulModule, &DocumentSnapshots{}, phyModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := APIConfig{TokenKey: []byte("api-test-key-of-thirty-two-bytes!!"), TokenIssuer: "idp-cargo-test", TokenAudience: "idp-cargo-api",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MarginFloorBP: 2000}
	h, err := platform.API(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := apiClient{t: t, srv: srv}
	tenant, other := uniqueTenant("cargo-fin-api-a-"), uniqueTenant("cargo-fin-api-b-")
	seedORG(t, platform, tenant, "party-a", "party-b", "line-1")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	_, err = orgModule.ApproveRole(context.Background(), orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE", PartyID: "line-1",
		ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at})
	must("role line-1", err)
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: at.Add(72 * time.Hour)}
	contexts.planSea(t, "party-a", "A", comapi.ContainerLCL, fulapi.ContainerLCL)
	contexts.planSea(t, "party-b", "B", comapi.ContainerLCL, fulapi.ContainerLCL)
	_, err = finModule.OpenFinancePolicy(context.Background(), finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(10)})
	must("policy", err)
	_, err = finModule.RecordExchangeRate(context.Background(), finapi.RecordExchangeRate{CommandID: "fin-rate", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE", ExpectedRevision: 1,
		RateType: finapi.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at, ValidUntil: at.AddDate(0, 1, 0),
		Source: "CFETS central parity", OccurredAt: hours(10)})
	must("rate", err)
	for _, x := range []struct {
		house string
		cost  int64
	}{{"A", 1_200_000}, {"B", 100_000}} {
		job := "JOB-" + x.house
		_, err = finModule.OpenFreightJob(context.Background(), finapi.OpenFreightJob{CommandID: "open-" + x.house, TenantID: tenant, ActorID: "fin-1", JobID: job, ShipmentID: "SH-" + x.house, OccurredAt: hours(10)})
		must("open "+job, err)
		r, err := finModule.ApplyPricingBasis(context.Background(), finapi.ApplyPricingBasis{CommandID: "apply-" + x.house, TenantID: tenant, ActorID: "fin-1", JobID: job, ExpectedRevision: 1,
			QuoteID: "Q-" + x.house, OrderID: "O-" + x.house, OccurredAt: hours(11)})
		must("apply "+job, err)
		_, err = finModule.RecordChargeLine(context.Background(), finapi.RecordChargeLine{CommandID: "line-" + x.house, TenantID: tenant, ActorID: "fin-1", JobID: job, ExpectedRevision: r.NewRevision,
			ChargeCode: "TRUCKING", Cost: &finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: x.cost}}, Reason: "pickup", OccurredAt: hours(12)})
		must("cost of "+job, err)
	}
	mint := func(tenant, actor string, perms ...string) string {
		t.Helper()
		token, err := MintDevToken(cfg, tenant, actor, perms, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	alice := mint(tenant, "alice", "fin:ReadProfit", "fin:RequestCostReview")
	reviewer := mint(tenant, "fin-reviewer", "fin:ReadProfit", "fin:ResolveCostReview")
	reader := mint(tenant, "reader", "fin:ReadProfit")
	nobody := mint(tenant, "nobody", "rate:*")
	everything := mint(tenant, "admin", "fin:*")
	stranger := mint(other, "alice", "fin:*")
	const prefix = "/v1/fin/"

	// permissions and authentication
	for _, route := range [][2]string{{"GET", prefix + "legal-entities/LE/job-profits"}, {"GET", prefix + "jobs/JOB-A/profit"}, {"GET", prefix + "cost-review-requests/CRR-1"},
		{"GET", prefix + "jobs/JOB-A/cost-review-requests"}} {
		c.expect(route[1]+" without a token", c.do(route[0], route[1], "", ""), 401, "UNAUTHENTICATED")
		c.expect(route[1]+" without the permission", c.do(route[0], route[1], nobody, ""), 403, "FORBIDDEN")
	}
	c.expect("a request without the permission", c.do("POST", prefix+"cost-review-requests", reader, `{"job_id":"JOB-A","profit_version":"x","reason":"r"}`, "Idempotency-Key", "k-1"), 403, "FORBIDDEN")
	c.expect("a resolution without the permission", c.do("POST", prefix+"cost-review-requests/CRR-1/resolution", alice, `{"outcome":"NO_CHANGE","note":"n"}`, "Idempotency-Key", "k-2"), 403, "FORBIDDEN")

	// the loss-making jobs
	list := c.do("GET", prefix+"legal-entities/LE/job-profits?loss_only=true&limit=10", reader, "")
	c.expect("losses", list, 200, "")
	rows, _ := list.body["rows"].([]any)
	if len(rows) != 1 || dig(rows, 0, "job_id") != "JOB-A" || dig(rows, 0, "profit", "currency") != "CNY" || dig(rows, 0, "profit_basis") != "SEPARATE" ||
		dig(rows, 0, "revenue", "minor").(float64)-dig(rows, 0, "cost", "minor").(float64) != dig(rows, 0, "profit", "minor").(float64) || dig(rows, 0, "profit", "minor").(float64) >= 0 {
		t.Fatalf("the losses: %v", list.body)
	}
	all := c.do("GET", prefix+"legal-entities/LE/job-profits", everything, "")
	if r, _ := all.body["rows"].([]any); all.status != 200 || len(r) != 2 || list.body["as_of"] == nil {
		t.Fatalf("all jobs: %v", all.body)
	}
	version, _ := dig(rows, 0, "profit_version").(string)
	profit := c.do("GET", prefix+"jobs/JOB-A/profit", reader, "")
	c.expect("the profit", profit, 200, "")
	if profit.body["profit_version"] != version || len(version) != 64 {
		t.Fatalf("the profit: %v, the row's version %s", profit.body, version)
	}
	for _, bad := range []string{"limit=0", "limit=501", "limit=x", "loss_only=yes"} {
		c.expect(bad, c.do("GET", prefix+"legal-entities/LE/job-profits?"+bad, reader, ""), 400, "MALFORMED_REQUEST")
	}
	// a query parameter the route does not define is refused, naming it; a malformed one is refused
	for _, bad := range []string{"tenant_id=other", "actor_id=x", "limit=5&colour=red", "after=has%20space", "after=JOB-A%00"} {
		r := c.do("GET", prefix+"legal-entities/LE/job-profits?"+bad, reader, "")
		c.expect(bad, r, 400, "MALFORMED_REQUEST")
		if strings.HasPrefix(bad, "tenant_id") && !strings.Contains(r.body["detail"].(string), `"tenant_id"`) {
			t.Fatalf("the refusal does not name the parameter: %v", r.body)
		}
	}
	c.expect("a query on the profit", c.do("GET", prefix+"jobs/JOB-A/profit?actor_id=x", reader, ""), 400, "MALFORMED_REQUEST")
	c.expect("a query on a request", c.do("GET", prefix+"cost-review-requests/CRR-1?tenant_id=x", reader, ""), 400, "MALFORMED_REQUEST")
	for _, bad := range []string{"after=CRR-x", "after=CRR-05", "after=JOB-A", "tenant_id=x", "state=OPEN&x=1", "limit=0"} {
		c.expect(bad, c.do("GET", prefix+"jobs/JOB-A/cost-review-requests?"+bad, reader, ""), 400, "MALFORMED_REQUEST")
	}
	c.expect("a path with a control character", c.do("GET", prefix+"jobs/JOB-A%00/profit", reader, ""), 400, "MALFORMED_REQUEST")
	c.expect("a path with a line break", c.do("GET", prefix+"cost-review-requests/CRR-1%0A", reader, ""), 400, "MALFORMED_REQUEST")
	c.expect("another tenant's job", c.do("GET", prefix+"jobs/JOB-A/profit", stranger, ""), 404, "NOT_FOUND")
	if o := c.do("GET", prefix+"legal-entities/LE/job-profits", stranger, ""); o.status != 200 || len(o.body["rows"].([]any)) != 0 {
		t.Fatalf("another tenant sees rows: %v", o.body)
	}

	// the request
	body := `{"job_id":"JOB-A","profit_version":"` + version + `","reason":"the trucking cost looks wrong"}`
	post := func(token, key, b string) apiResponse {
		return c.do("POST", prefix+"cost-review-requests", token, b, "Idempotency-Key", key)
	}
	first := post(alice, "req-a-1", body)
	c.expect("the request", first, 201, "")
	if first.body["request_id"] != "CRR-1" || first.body["command_id"] != "req-a-1" || first.body["new_revision"] != float64(1) || first.body["request_no"] != float64(1) ||
		first.header.Get("Location") != prefix+"cost-review-requests/CRR-1" {
		t.Fatalf("the receipt: %v %v", first.body, first.header)
	}
	if again := post(alice, "req-a-1", body); again.status != 201 || again.body["request_id"] != "CRR-1" || again.body["new_revision"] != float64(1) {
		t.Fatalf("the replay: %v", again.body)
	}
	c.expect("the key with another body", post(alice, "req-a-1", strings.Replace(body, "wrong", "right", 1)), 409, "IDEMPOTENCY_MISMATCH")
	c.expect("a second request", post(alice, "req-a-2", body), 422, "FIN_REVIEW_OPEN")
	c.expect("no key", c.do("POST", prefix+"cost-review-requests", alice, body), 400, "IDEMPOTENCY_KEY_REQUIRED")
	c.expect("a tenant in the body", post(alice, "req-a-3", `{"tenant_id":"x","job_id":"JOB-B","profit_version":"v","reason":"r"}`), 400, "MALFORMED_REQUEST")
	c.expect("an actor in the body", post(alice, "req-a-4", `{"actor_id":"x","job_id":"JOB-B","profit_version":"v","reason":"r"}`), 400, "MALFORMED_REQUEST")
	c.expect("a requester in the body", post(alice, "req-a-5", `{"requested_by":"x","job_id":"JOB-B","profit_version":"v","reason":"r"}`), 400, "MALFORMED_REQUEST")
	for _, x := range [][2]string{{"reason", `"a\u0000b"`}, {"reason", `"a\u001bb"`}, {"job_id", `"JOB-A\u0000"`}, {"profit_version", `"v\u0000"`}} {
		b := map[string]string{"job_id": `"JOB-A"`, "profit_version": `"v"`, "reason": `"r"`}
		b[x[0]] = x[1]
		r := post(alice, "req-nul-"+x[0], `{"job_id":`+b["job_id"]+`,"profit_version":`+b["profit_version"]+`,"reason":`+b["reason"]+`}`)
		c.expect("a control character in "+x[0], r, 400, "MALFORMED_REQUEST")
		if d, _ := r.body["detail"].(string); !strings.Contains(d, `"`+x[0]+`"`) {
			t.Fatalf("the refusal does not name %s: %v", x[0], r.body)
		}
	}
	if r := post(alice, "req-lines", `{"job_id":"JOB-B","profit_version":"v","reason":"two\nlines\tand a tab"}`); r.status == 400 {
		t.Fatalf("a line break in a reason is text: %v", r.body)
	}
	c.expect("a stale version", post(alice, "req-b-1", `{"job_id":"JOB-A","profit_version":"`+strings.Repeat("0", 64)+`","reason":"r"}`), 422, "FIN_REVIEW_STALE")
	c.expect("a job that is not a loss", post(alice, "req-b-2", `{"job_id":"JOB-B","profit_version":"v","reason":"r"}`), 422, "FIN_REVIEW_NOT_A_LOSS")
	c.expect("an unknown job", post(alice, "req-b-3", `{"job_id":"JOB-Z","profit_version":"v","reason":"r"}`), 404, "NOT_FOUND")
	c.expect("an empty reason", post(alice, "req-b-4", `{"job_id":"JOB-A","profit_version":"v","reason":""}`), 422, "")
	resp, err := http.NewRequest("POST", srv.URL+prefix+"cost-review-requests", strings.NewReader(body))
	must("request", err)
	resp.Header.Set("Authorization", "Bearer "+alice)
	resp.Header.Set("Idempotency-Key", "req-a-6")
	r, err := srv.Client().Do(resp)
	must("without a content type", err)
	r.Body.Close()
	if r.StatusCode != 415 {
		t.Fatalf("without a content type: %d", r.StatusCode)
	}

	// the read back and the resolution
	got := c.do("GET", prefix+"cost-review-requests/CRR-1", reader, "")
	c.expect("the request read", got, 200, "")
	if got.body["state"] != "OPEN" || got.body["requested_by"] != "alice" || got.body["job_id"] != "JOB-A" || got.body["profit_version"] != version || got.body["resolution"] != nil ||
		dig(got.body, "profit", "minor") != dig(rows, 0, "profit", "minor") {
		t.Fatalf("the request read: %v", got.body)
	}
	resolve := func(token, key string) apiResponse {
		return c.do("POST", prefix+"cost-review-requests/CRR-1/resolution", token, `{"outcome":"COST_CORRECTED","note":"the trucking cost was corrected"}`, "Idempotency-Key", key)
	}
	resolved := resolve(everything, "res-0")
	c.expect("another person resolves", resolved, 200, "")
	if resolved.header.Get("ETag") != `"2"` || first.header.Get("ETag") != `"1"` {
		t.Fatalf("the ETags: request %q, resolution %q", first.header.Get("ETag"), resolved.header.Get("ETag"))
	}
	c.expect("a control character in the note", c.do("POST", prefix+"cost-review-requests/CRR-1/resolution", reviewer, `{"outcome":"NO_CHANGE","note":"a\u0000b"}`, "Idempotency-Key", "res-nul"), 400, "MALFORMED_REQUEST") // everything is another person: admin
	c.expect("a second resolution", resolve(reviewer, "res-1"), 422, "FIN_REVIEW_RESOLVED")
	done := c.do("GET", prefix+"cost-review-requests/CRR-1", reader, "")
	if done.body["state"] != "RESOLVED" || dig(done.body, "resolution", "resolved_by") != "admin" || dig(done.body, "resolution", "outcome") != "COST_CORRECTED" {
		t.Fatalf("resolved: %v", done.body)
	}
	if replay := resolve(everything, "res-0"); replay.status != 200 || replay.body["request_id"] != "CRR-1" || replay.body["new_revision"] != float64(2) {
		t.Fatalf("the replay of the resolution: %v", replay.body)
	}
	// the requester cannot resolve their own request
	again := post(everything, "req-admin", body)
	c.expect("a request by admin after the resolution", again, 201, "")
	c.expect("admin resolves their own request", c.do("POST", prefix+"cost-review-requests/CRR-2/resolution", everything, `{"outcome":"NO_CHANGE","note":"n"}`, "Idempotency-Key", "res-own"), 422, "FIN_REVIEW_SAME_ACTOR")
	c.expect("a person in the resolution", c.do("POST", prefix+"cost-review-requests/CRR-2/resolution", reviewer, `{"resolved_by":"x","outcome":"NO_CHANGE","note":"n"}`, "Idempotency-Key", "res-x"), 400, "MALFORMED_REQUEST")
	c.expect("the reviewer resolves", c.do("POST", prefix+"cost-review-requests/CRR-2/resolution", reviewer, `{"outcome":"NO_CHANGE","note":"the costs are right"}`, "Idempotency-Key", "res-2"), 200, "")
	c.expect("an unknown request", c.do("GET", prefix+"cost-review-requests/CRR-99", reader, ""), 404, "NOT_FOUND")
	c.expect("another tenant's request", c.do("GET", prefix+"cost-review-requests/CRR-1", stranger, ""), 404, "NOT_FOUND")
	byJob := c.do("GET", prefix+"jobs/JOB-A/cost-review-requests?state=RESOLVED&limit=1", reader, "")
	if reqs, _ := byJob.body["requests"].([]any); byJob.status != 200 || len(reqs) != 1 || byJob.body["next_after"] != "CRR-1" {
		t.Fatalf("by job: %v", byJob.body)
	}
	c.expect("a state that is none", c.do("GET", prefix+"jobs/JOB-A/cost-review-requests?state=DONE", reader, ""), 400, "MALFORMED_REQUEST")
	// the review changed no amount: the profit version is the one the requester saw
	if p := c.do("GET", prefix+"jobs/JOB-A/profit", reader, ""); p.body["profit_version"] != version {
		t.Fatalf("the review changed the profit: %v", p.body)
	}
}
