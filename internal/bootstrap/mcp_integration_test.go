//go:build integration

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
	"go.idp.xyz/idp-cargo/internal/kernel"
	mcptransport "go.idp.xyz/idp-cargo/internal/transport/mcp"
	"go.idp.xyz/idp-cargo/internal/transport/mcp/mcptest"
)

// spyAuthorizer is FIN's authorizer as cargo-api has it (the permissions of the principal) that also remembers whom FIN
// was asked about: the tenant and the actor of each query, and the action.
type spyAuthorizer struct {
	mu   sync.Mutex
	seen []string
}

func (s *spyAuthorizer) AuthorizeFinAction(ctx context.Context, tenant, actor, action, subject string) error {
	s.mu.Lock()
	s.seen = append(s.seen, tenant+"|"+actor+"|"+action)
	s.mu.Unlock()
	return permissions{}.AuthorizeFinAction(ctx, tenant, actor, action, subject)
}

func (s *spyAuthorizer) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// lockedBuffer is a log the server writes from its own goroutines while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type mcpReply struct {
	status  int
	rpc     map[string]any
	isError bool
	text    string
	data    map[string]any
	raw     string
}

type mcpClient struct {
	t      *testing.T
	srv    *httptest.Server
	signer *mcptest.Signer
	id     int
}

func (c *mcpClient) send(method, name string, body []byte) mcpReply {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.srv.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header = mcptest.Headers(method, name)
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := mcpReply{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.rpc)
	if res, ok := out.rpc["result"].(map[string]any); ok {
		out.isError, _ = res["isError"].(bool)
		if content, _ := res["content"].([]any); len(content) > 0 {
			out.text, _ = content[0].(map[string]any)["text"].(string)
		}
		out.data, _ = res["structuredContent"].(map[string]any)
	}
	return out
}

// call calls a tool as the member the gateway would sign for.
func (c *mcpClient) call(caller mcptransport.Caller, tool string, args map[string]any) mcpReply {
	c.t.Helper()
	c.id++
	return c.send("tools/call", tool, c.signer.Call(caller, c.id, "tools/call", map[string]any{"name": tool, "arguments": args}))
}

func (c mcpReply) rows(key string) []any {
	rows, _ := c.data[key].([]any)
	return rows
}

// FIN's three tools end to end on PostgreSQL 16 (V2.101), through the MCP server behind the verifier of the gateway's
// assertion, with assertions signed by a key pair this test generates. ORG, RATE, COM, FUL and PHY run in process; the
// losses are cost lines FIN records. It shows each tool with the real FIN, that a tenant cannot see another's jobs,
// that a legal entity a role may not see is refused, that a job of such a legal entity is a job that does not exist,
// and that FIN is asked about the member the assertion names and no one else.
func TestMCPFinToolsOnPostgres(t *testing.T) {
	platform := openPlatform(t)
	ctx := context.Background()
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
	tenant, other := uniqueTenant("cargo-mcp-a-"), uniqueTenant("cargo-mcp-b-")
	seedORG(t, platform, tenant, "party-a", "party-b", "party-c", "line-1")
	seedORG(t, platform, other, "party-z")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE", PartyID: "line-1",
		ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at})
	must("role line-1", err)
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: at.Add(72 * time.Hour)}
	for _, h := range []struct{ party, house string }{{"party-a", "A"}, {"party-b", "B"}, {"party-c", "C"}} {
		contexts.planSea(t, h.party, h.house, comapi.ContainerLCL, fulapi.ContainerLCL)
	}
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(10)})
	must("policy", err)
	_, err = finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE", ExpectedRevision: 1,
		RateType: finapi.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at, ValidUntil: at.AddDate(0, 1, 0),
		Source: "CFETS central parity", OccurredAt: hours(10)})
	must("rate", err)
	// A and C lose money, B does not
	for _, x := range []struct {
		house string
		cost  int64
	}{{"A", 1_200_000}, {"B", 100_000}, {"C", 1_000_000}} {
		job := "JOB-" + x.house
		_, err = finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "open-" + x.house, TenantID: tenant, ActorID: "fin-1", JobID: job, ShipmentID: "SH-" + x.house, OccurredAt: hours(10)})
		must("open "+job, err)
		r, err := finModule.ApplyPricingBasis(ctx, finapi.ApplyPricingBasis{CommandID: "apply-" + x.house, TenantID: tenant, ActorID: "fin-1", JobID: job, ExpectedRevision: 1,
			QuoteID: "Q-" + x.house, OrderID: "O-" + x.house, OccurredAt: hours(11)})
		must("apply "+job, err)
		_, err = finModule.RecordChargeLine(ctx, finapi.RecordChargeLine{CommandID: "line-" + x.house, TenantID: tenant, ActorID: "fin-1", JobID: job, ExpectedRevision: r.NewRevision,
			ChargeCode: "TRUCKING", Cost: &finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: x.cost}}, Reason: "pickup", OccurredAt: hours(12)})
		must("cost of "+job, err)
	}

	// the server: the gateway's tenant "gw-a" is this test's tenant, "gw-b" the other; a role that sees LE, one that
	// sees another legal entity only, one that sees every legal entity
	endpoint := "http://fin-mcp.test:8600/mcp"
	signer := mcptest.NewSigner(endpoint, "g1")
	spy := &spyAuthorizer{}
	read := []string{"fin:ReadProfit"}
	var cfg MCPConfig
	cfg.Audience, cfg.GatewayID, cfg.PublicKeys, cfg.CursorKey, cfg.authorizer = endpoint, "g1", signer.PublicPEM(), bytes.Repeat([]byte("k"), 32), spy
	cfg.Tenants = map[string]fin.MCPTenant{
		"gw-a": {CargoTenant: tenant, Roles: map[string]fin.MCPRole{
			"analyst":  {Permissions: read, LegalEntities: fin.MCPEntities{IDs: []string{"LE"}}},
			"outsider": {Permissions: read, LegalEntities: fin.MCPEntities{IDs: []string{"OTHER-LE"}}},
			"wide":     {Permissions: read, LegalEntities: fin.MCPEntities{All: true}},
		}},
		"gw-b": {CargoTenant: other, Roles: map[string]fin.MCPRole{"analyst": {Permissions: read, LegalEntities: fin.MCPEntities{IDs: []string{"LE"}}}}},
	}
	logs := &lockedBuffer{}
	server, err := platform.MCP(cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(server.KeyIDs) != 1 || server.KeyIDs[0] != signer.KeyID() {
		t.Fatalf("the key ids %v", server.KeyIDs)
	}
	srv := httptest.NewServer(server.Handler)
	defer srv.Close()
	c := &mcpClient{t: t, srv: srv, signer: signer}
	alice := mcptransport.Caller{Subject: "alice", Tenant: "gw-a", Roles: []string{"analyst"}, Client: "idp-host"}
	outsider := mcptransport.Caller{Subject: "olga", Tenant: "gw-a", Roles: []string{"outsider"}}
	wide := mcptransport.Caller{Subject: "walt", Tenant: "gw-a", Roles: []string{"wide"}}
	stranger := mcptransport.Caller{Subject: "alice", Tenant: "gw-b", Roles: []string{"analyst"}}

	// the catalogue needs no assertion, a call does
	list := c.send("tools/list", "", mcptest.Body(1, "tools/list", map[string]any{}, ""))
	if tools, _ := list.rpc["result"].(map[string]any)["tools"].([]any); list.status != 200 || len(tools) != 3 {
		t.Fatalf("tools/list: %d %s", list.status, list.raw)
	}
	if got := c.send("tools/call", "list_loss_jobs", mcptest.Body(2, "tools/call", map[string]any{"name": "list_loss_jobs", "arguments": map[string]any{"legal_entity_id": "LE"}}, "")); got.status != 403 {
		t.Fatalf("a call without an assertion: %d", got.status)
	}

	// list_loss_jobs with FIN's pages: two of the three jobs lose money, two rows a call, then one, then the end
	first := c.call(alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "limit": 1})
	if first.isError || len(first.rows("rows")) != 1 || first.data["next_cursor"] == nil || first.data["as_of"] == nil {
		t.Fatalf("the first loss: %s", first.raw)
	}
	row := first.rows("rows")[0].(map[string]any)
	if row["job_id"] != "JOB-A" || row["profit_basis"] != "SEPARATE" || len(row["profit_version"].(string)) != 64 {
		t.Fatalf("the first row: %v", row)
	}
	profitOf := func(m any) float64 { return m.(map[string]any)["profit"].(map[string]any)["minor"].(float64) }
	if profitOf(row) >= 0 || row["profit"].(map[string]any)["currency"] != "CNY" {
		t.Fatalf("not a loss: %v", row)
	}
	second := c.call(alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "limit": 1, "cursor": first.data["next_cursor"]})
	if second.isError || len(second.rows("rows")) != 1 || second.rows("rows")[0].(map[string]any)["job_id"] != "JOB-C" {
		t.Fatalf("the second loss: %s", second.raw)
	}
	all := c.call(alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE"})
	if len(all.rows("rows")) != 2 || all.data["next_cursor"] != nil || all.data["truncated"] != false || all.data["row_cap_reached"] != false {
		t.Fatalf("all the losses: %s", all.raw)
	}
	// another member's cursor, and a cursor for another legal entity, are refused
	if got := c.call(wide, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "limit": 1, "cursor": first.data["next_cursor"]}); !got.isError || !strings.Contains(got.text, "start again without a cursor") {
		t.Fatalf("another member's cursor: %s", got.raw)
	}

	// get_job_profit: the version of the row, and the parts add up
	profit := c.call(alice, "get_job_profit", map[string]any{"job_id": "JOB-A"})
	if profit.isError || profit.data["profit_version"] != row["profit_version"] || profit.data["legal_entity_id"] != "LE" || profit.data["profit_basis"] != "SEPARATE" {
		t.Fatalf("the profit: %s", profit.raw)
	}
	minor := func(key string) float64 { return profit.data[key].(map[string]any)["minor"].(float64) }
	if minor("revenue")-minor("cost")-minor("pool_cost") != minor("profit") || minor("profit") != profitOf(row) {
		t.Fatalf("the parts do not add up: %v", profit.data)
	}
	// B is not a loss, and its profit reads the same
	if b := c.call(alice, "get_job_profit", map[string]any{"job_id": "JOB-B"}); b.isError || b.data["profit"].(map[string]any)["minor"].(float64) < 0 {
		t.Fatalf("the profit of B: %s", b.raw)
	}

	// list_job_cost_reviews: a request made by another person in FIN is read back
	version := profit.data["profit_version"].(string)
	_, err = finModule.RequestCostReview(ctx, finapi.RequestCostReview{CommandID: "mcp-review-1", TenantID: tenant, ActorID: "carol", JobID: "JOB-A", ProfitVersion: version, Reason: "the trucking cost looks wrong"})
	must("a cost review", err)
	reviews := c.call(alice, "list_job_cost_reviews", map[string]any{"job_id": "JOB-A"})
	if reviews.isError || len(reviews.rows("requests")) != 1 || reviews.data["legal_entity_id"] != "LE" {
		t.Fatalf("the reviews: %s", reviews.raw)
	}
	review := reviews.rows("requests")[0].(map[string]any)
	if review["request_id"] != "CRR-1" || review["state"] != "OPEN" || review["profit_version"] != version || review["resolution"] != nil || review["reason"] != "the trucking cost looks wrong" ||
		profitOf(review) != profitOf(row) {
		t.Fatalf("the review: %v", review)
	}
	if _, has := review["requested_by"]; has {
		t.Fatalf("a person's name is in the result: %v", review)
	}
	if got := c.call(alice, "list_job_cost_reviews", map[string]any{"job_id": "JOB-A", "state": "RESOLVED"}); got.isError || len(got.rows("requests")) != 0 {
		t.Fatalf("resolved reviews: %s", got.raw)
	}
	if got := c.call(alice, "list_job_cost_reviews", map[string]any{"job_id": "JOB-C"}); got.isError || len(got.rows("requests")) != 0 {
		t.Fatalf("reviews of C: %s", got.raw)
	}

	// a tenant does not see another's jobs: the other tenant has the same legal entity and none of the jobs
	if got := c.call(stranger, "list_loss_jobs", map[string]any{"legal_entity_id": "LE"}); got.isError || len(got.rows("rows")) != 0 {
		t.Fatalf("the other tenant's losses: %s", got.raw)
	}
	for _, tool := range []string{"get_job_profit", "list_job_cost_reviews"} {
		if got := c.call(stranger, tool, map[string]any{"job_id": "JOB-A"}); !got.isError || got.text != "no such job or request" {
			t.Fatalf("%s of another tenant's job: %s", tool, got.raw)
		}
	}

	// a legal entity the role does not see is refused, and a job of one is a job that does not exist, told as the one
	// that is not there
	if got := c.call(outsider, "list_loss_jobs", map[string]any{"legal_entity_id": "LE"}); !got.isError || got.text != "you are not allowed to do this in FIN" {
		t.Fatalf("a legal entity not allowed: %s", got.raw)
	}
	for _, tool := range []string{"get_job_profit", "list_job_cost_reviews"} {
		hidden := c.call(outsider, tool, map[string]any{"job_id": "JOB-A"})
		missing := c.call(outsider, tool, map[string]any{"job_id": "JOB-NOWHERE"})
		if !hidden.isError || hidden.text != "no such job or request" || missing.text != hidden.text {
			t.Fatalf("%s of a hidden job: %q, of a missing one: %q", tool, hidden.text, missing.text)
		}
		// the one that may see it reads it, and so does the one that sees every legal entity
		if got := c.call(wide, tool, map[string]any{"job_id": "JOB-A"}); got.isError {
			t.Fatalf("%s as the wide role: %s", tool, got.raw)
		}
	}
	// a role the policy does not name, a tenant it does not name, and a member without roles have nothing
	for _, who := range []mcptransport.Caller{{Subject: "x", Tenant: "gw-a", Roles: []string{"unknown"}}, {Subject: "x", Tenant: "gw-unknown", Roles: []string{"analyst"}},
		{Subject: "x", Tenant: "gw-a", Roles: []string{}}} {
		if got := c.call(who, "get_job_profit", map[string]any{"job_id": "JOB-A"}); !got.isError || got.text != "you are not allowed to do this in FIN" {
			t.Fatalf("%+v: %s", who, got.raw)
		}
	}
	// no tenant or person in the arguments
	for _, field := range []string{"tenant_id", "actor_id"} {
		if got := c.call(alice, "get_job_profit", map[string]any{"job_id": "JOB-A", field: "x"}); !got.isError || !strings.Contains(got.text, "not valid") {
			t.Fatalf("%s in the arguments: %s", field, got.raw)
		}
	}

	// FIN records the member as the actor: every query FIN was asked was about the tenant of the policy and the subject
	// of the assertion, for ReadProfit, and none about the gateway, the client or the service
	asked := spy.asked()
	if len(asked) == 0 {
		t.Fatal("FIN's authorizer was not asked")
	}
	members := map[string]bool{tenant + "|alice|ReadProfit": true, tenant + "|olga|ReadProfit": true, tenant + "|walt|ReadProfit": true, other + "|alice|ReadProfit": true}
	sawAlice := false
	for _, a := range asked {
		if !members[a] {
			t.Errorf("FIN was asked as %q", a)
		}
		sawAlice = sawAlice || a == tenant+"|alice|ReadProfit"
	}
	if !sawAlice {
		t.Errorf("FIN was never asked about alice: %v", asked)
	}
	// the audit: one line per call, with the caller's short hash and no argument
	if n := strings.Count(logs.String(), `msg="fin mcp call"`); n < 20 || strings.Contains(logs.String(), "JOB-A") || strings.Contains(logs.String(), "the trucking cost") {
		t.Errorf("the log: %d call lines, arguments in it: %v", n, strings.Contains(logs.String(), "JOB-A"))
	}
	for _, secret := range []string{"BEGIN", "eyJ"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("%q is in the log", secret)
		}
	}

	// the query the tools added to FIN (V2.101), asked of a FIN that authorizes as cargo-api's does: it answers the
	// job's legal entity to the permission ReadProfit and to no one else, and does not know another tenant's job
	guarded, err := platform.FIN(orgModule, rateModule, fulModule, &DocumentSnapshots{}, phyModule, permissions{})
	if err != nil {
		t.Fatal(err)
	}
	as := func(tenantID, actor string, perms ...string) context.Context {
		return identity.WithPrincipal(ctx, identity.Principal{TenantID: tenantID, ActorID: actor, Permissions: perms})
	}
	entity := func(c context.Context, tenantID, actor, job string) (string, error) {
		return guarded.GetJobLegalEntity(c, finapi.GetJobLegalEntity{TenantID: tenantID, ActorID: actor, JobID: job})
	}
	if le, err := entity(as(tenant, "alice", "fin:ReadProfit"), tenant, "alice", "JOB-A"); err != nil || le != "LE" {
		t.Fatalf("the legal entity of JOB-A: %q %v", le, err)
	}
	for name, tc := range map[string]struct {
		ctx                context.Context
		tenant, actor, job string
		want               error
	}{
		"an unknown job":            {as(tenant, "alice", "fin:ReadProfit"), tenant, "alice", "JOB-NOWHERE", finapi.ErrNotFound},
		"another tenant's job":      {as(other, "alice", "fin:ReadProfit"), other, "alice", "JOB-A", finapi.ErrNotFound},
		"without the permission":    {as(tenant, "alice", "rate:*"), tenant, "alice", "JOB-A", finapi.ErrUnauthorized},
		"without a principal":       {ctx, tenant, "alice", "JOB-A", finapi.ErrUnauthorized},
		"another actor than named":  {as(tenant, "mallory", "fin:ReadProfit"), tenant, "alice", "JOB-A", finapi.ErrUnauthorized},
		"another tenant than named": {as(other, "alice", "fin:ReadProfit"), tenant, "alice", "JOB-A", finapi.ErrUnauthorized},
		"no actor":                  {as(tenant, "alice", "fin:ReadProfit"), tenant, "", "JOB-A", finapi.ErrUnauthenticated},
	} {
		if le, err := entity(tc.ctx, tc.tenant, tc.actor, tc.job); !errors.Is(err, tc.want) || le != "" {
			t.Errorf("%s: %q %v, want %v", name, le, err, tc.want)
		}
	}
}
