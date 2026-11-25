package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// fakeFIN is FIN's query side as the tools see it. Each field is a behaviour; a nil one answers the zero value. Calls
// are counted and the principal each call carried is kept.
type fakeFIN struct {
	mu         sync.Mutex
	calls      map[string]int
	principals []identity.Principal
	queries    []any

	jobs     func(ctx context.Context, q api.ListJobProfits) (api.JobProfitPage, error)
	profit   func(ctx context.Context, q api.GetJobProfit) (api.JobProfit, error)
	entity   func(ctx context.Context, q api.GetJobLegalEntity) (string, error)
	requests func(ctx context.Context, q api.ListCostReviewRequests) (api.CostReviewPage, error)
}

func (f *fakeFIN) note(ctx context.Context, name string, q any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
	f.queries = append(f.queries, q)
	p, _ := identity.PrincipalFrom(ctx)
	f.principals = append(f.principals, p)
}

func (f *fakeFIN) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeFIN) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakeFIN) ListJobProfits(ctx context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
	f.note(ctx, "ListJobProfits", q)
	if f.jobs == nil {
		return api.JobProfitPage{}, nil
	}
	return f.jobs(ctx, q)
}

func (f *fakeFIN) GetJobProfit(ctx context.Context, q api.GetJobProfit) (api.JobProfit, error) {
	f.note(ctx, "GetJobProfit", q)
	if f.profit == nil {
		return api.JobProfit{}, nil
	}
	return f.profit(ctx, q)
}

func (f *fakeFIN) GetJobLegalEntity(ctx context.Context, q api.GetJobLegalEntity) (string, error) {
	f.note(ctx, "GetJobLegalEntity", q)
	if f.entity == nil {
		return "LE", nil
	}
	return f.entity(ctx, q)
}

func (f *fakeFIN) ListCostReviewRequests(ctx context.Context, q api.ListCostReviewRequests) (api.CostReviewPage, error) {
	f.note(ctx, "ListCostReviewRequests", q)
	if f.requests == nil {
		return api.CostReviewPage{}, nil
	}
	return f.requests(ctx, q)
}

func cny(minor int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: minor} }

// testPolicy: the gateway tenant "gw" is CargoPai's tenant "cargo-a". Role "analyst" reads legal entity LE, "wide"
// every one, "other" only LE2; "nothing" is named but grants no legal entity the tests use.
func testPolicy() Policy {
	read := []string{PermissionReadProfit}
	return Policy{Tenants: map[string]TenantPolicy{
		"gw": {CargoTenant: "cargo-a", Roles: map[string]RolePolicy{
			"analyst": {Permissions: read, LegalEntities: Entities{IDs: []string{"LE"}}},
			"wide":    {Permissions: read, LegalEntities: Entities{All: true}},
			"other":   {Permissions: read, LegalEntities: Entities{IDs: []string{"LE2"}}},
		}},
		"gw-b": {CargoTenant: "cargo-b", Roles: map[string]RolePolicy{"analyst": {Permissions: read, LegalEntities: Entities{IDs: []string{"LE"}}}}},
	}}
}

// rig serves the three tools through the SDK's stateless handler. A request names its caller in headers, which stand
// for the verifier of the transport (the adapter cannot import it).
type rig struct {
	t      *testing.T
	fin    *fakeFIN
	h      http.Handler
	now    time.Time
	mu     sync.Mutex
	log    bytes.Buffer
	nextID int
}

type rigOption func(*Config)

func newRig(t *testing.T, fin *fakeFIN, opts ...rigOption) *rig {
	t.Helper()
	r := &rig{t: t, fin: fin, now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	cfg := Config{
		Caller: func(ctx context.Context) (Subject, bool) { s, ok := ctx.Value(subjectKey{}).(Subject); return s, ok },
		Policy: testPolicy(), CursorKey: bytes.Repeat([]byte("k"), 32), Limits: Limits{PerMinute: 10_000, Concurrent: 32},
		Now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now },
		Log: slog.New(slog.NewTextHandler(&lockedWriter{r: r}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, o := range opts {
		o(&cfg)
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "fin-test", Version: "1"}, nil)
	if err := Register(s, fin, cfg); err != nil {
		t.Fatal(err)
	}
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	r.h = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if sub := req.Header.Get("X-Test-Subject"); sub != "" {
			var roles []string
			if v := req.Header.Get("X-Test-Roles"); v != "" {
				roles = strings.Split(v, ",")
			}
			req = req.WithContext(context.WithValue(req.Context(), subjectKey{}, Subject{Tenant: req.Header.Get("X-Test-Tenant"), ID: sub, Roles: roles,
				Client: "idp-host", Actor: "idp-host"}))
		}
		inner.ServeHTTP(w, req)
	})
	return r
}

type subjectKey struct{}

type lockedWriter struct{ r *rig }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	return w.r.log.Write(p)
}

func (r *rig) logged() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.log.String()
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

// who is a caller of the tests.
type who struct{ tenant, sub, roles string }

var (
	alice = who{"gw", "alice", "analyst"}
	bob   = who{"gw", "bob", "analyst"}
)

type reply struct {
	status  int
	body    string
	rpc     map[string]any
	isError bool
	text    string
	data    map[string]any // structuredContent
}

func (r *rig) rpc(c *who, method string, params map[string]any, name string) reply {
	r.t.Helper()
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.mu.Unlock()
	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	p := map[string]any{"_meta": meta}
	for k, v := range params {
		p[k] = v
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": p})
	if err != nil {
		r.t.Fatal(err)
	}
	return r.raw(c, method, name, body)
}

func (r *rig) raw(c *who, method, name string, body []byte) reply {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	if c != nil {
		req.Header.Set("X-Test-Tenant", c.tenant)
		req.Header.Set("X-Test-Subject", c.sub)
		req.Header.Set("X-Test-Roles", c.roles)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	out := reply{status: w.Code, body: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.rpc)
	if res, ok := out.rpc["result"].(map[string]any); ok {
		out.isError, _ = res["isError"].(bool)
		if content, _ := res["content"].([]any); len(content) > 0 {
			out.text, _ = content[0].(map[string]any)["text"].(string)
		}
		out.data, _ = res["structuredContent"].(map[string]any)
	}
	return out
}

// call calls a tool as c with the arguments.
func (r *rig) call(c *who, tool string, args map[string]any) reply {
	r.t.Helper()
	return r.rpc(c, "tools/call", map[string]any{"name": tool, "arguments": args}, tool)
}

func newServer() *sdk.Server { return sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, nil) }

func (r *rig) policyForTest() Policy { return testPolicy() }
