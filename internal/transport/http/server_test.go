package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	bentoapp "go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// tokens: "good" is alice in t1, anything else is refused.
type tokens struct{}

func (tokens) Verify(_ context.Context, token string) (identity.Principal, error) {
	if token != "good" {
		return identity.Principal{}, identity.ErrInvalidToken
	}
	return identity.Principal{TenantID: "t1", ActorID: "alice", Permissions: []string{"demo:*"}}, nil
}

var (
	errUnauthenticated = errors.New("demo: unauthenticated")
	errUnauthorized    = errors.New("demo: unauthorized")
	errNotFound        = errors.New("demo: not found")
	errConflict        = errors.New("demo: revision conflict")
	errIdempotency     = errors.New("demo: command id reused")
	errRejected        = errors.New("demo: rejected by business rules")
	errTooLate         = errors.New("demo: DEMO_TOO_LATE: the thing is closed")
	demoKinds          = httpapi.Errors{Unauthenticated: errUnauthenticated, Unauthorized: errUnauthorized, NotFound: errNotFound,
		Conflict: errConflict, IdempotencyConflict: errIdempotency, Rejected: errRejected}
)

// demo is a context with routes that exercise the helpers.
type demo struct{}

func (demo) RegisterHTTP(mux *nethttp.ServeMux) {
	mux.HandleFunc("GET /v1/whoami", func(w nethttp.ResponseWriter, r *nethttp.Request) {
		p, _ := identity.PrincipalFrom(r.Context())
		httpapi.WriteJSON(w, nethttp.StatusOK, map[string]string{"tenant": p.TenantID, "actor": p.ActorID, "correlation": httpapi.CorrelationID(r.Context())})
	})
	mux.HandleFunc("POST /v1/things/{id}/change", func(w nethttp.ResponseWriter, r *nethttp.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if !httpapi.Decode(w, r, &body) {
			return
		}
		rev, ok := httpapi.ExpectedRevision(w, r)
		if !ok {
			return
		}
		id, ok := httpapi.CommandID(w, r)
		if !ok {
			return
		}
		httpapi.WriteReceipt(w, nethttp.StatusOK, httpapi.Receipt{CommandID: id, SubjectID: r.PathValue("id"), NewRevision: rev + 1}, "")
	})
	mux.HandleFunc("GET /v1/fail/{kind}", func(w nethttp.ResponseWriter, r *nethttp.Request) {
		errs := map[string]error{"unauthenticated": errUnauthenticated, "unauthorized": errUnauthorized, "not-found": errNotFound,
			"conflict": errConflict, "idempotency": errIdempotency, "rejected": fmt.Errorf("%w: %w", errRejected, fmt.Errorf("%w: thing T-1", errTooLate)),
			"rejected-plain": fmt.Errorf("%w: demo: a KG_GROSS unit is not allowed here", errRejected), "uncertain": bentoapp.ErrCommitUncertain,
			// a code quoted from what the caller sent is not the error's code
			"rejected-spoofed":  fmt.Errorf("%w: %w", errRejected, fmt.Errorf("demo: invalid group %q", "x: DEMO_TOO_LATE: y")),
			"conflict-database": fmt.Errorf("%w: %w", errConflict, &pgconn.PgError{Code: "23514", Message: "cargo_demo: row of tenant t1 refused"}),
			"internal":          errors.New("pq: relation does not exist")}
		httpapi.Fail(w, r, demoKinds, errs[r.PathValue("kind")])
	})
	mux.HandleFunc("GET /v1/panic", func(nethttp.ResponseWriter, *nethttp.Request) { panic("boom") })
	mux.HandleFunc("GET /v1/panic-late", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[1,2,`))
		w.(nethttp.Flusher).Flush()
		panic("boom after writing")
	})
}

func handler(t *testing.T, logs io.Writer, ready error) nethttp.Handler {
	t.Helper()
	h, err := NewHandler(Config{Verifier: tokens{}, Logger: slog.New(slog.NewJSONHandler(logs, nil)), Ready: func(context.Context) error { return ready }},
		demo{})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type call struct {
	method, path, body string
	header             map[string]string
}

func do(h nethttp.Handler, c call) *httptest.ResponseRecorder {
	r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	if c.body == "" {
		r = httptest.NewRequest(c.method, c.path, nil)
	}
	for k, v := range c.header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func problem(t *testing.T, w *httptest.ResponseRecorder) httpapi.Problem {
	t.Helper()
	var p httpapi.Problem
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type %q: %s", ct, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

var auth = map[string]string{"Authorization": "Bearer good"}

func with(h map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// Requests under /v1 carry a verified bearer token; health needs none; every request has a correlation id; errors are
// problems; panics are 500 problems; and the access log names the principal but no token.
func TestHandler(t *testing.T) {
	var logs bytes.Buffer
	h := handler(t, &logs, nil)
	if w := do(h, call{method: "GET", path: "/healthz"}); w.Code != 200 || w.Header().Get("X-Correlation-ID") == "" {
		t.Fatalf("healthz: %d %v", w.Code, w.Header())
	}
	if w := do(h, call{method: "GET", path: "/readyz"}); w.Code != 200 {
		t.Fatalf("readyz: %d", w.Code)
	}
	if w := do(handler(t, io.Discard, errors.New("db down")), call{method: "GET", path: "/readyz"}); w.Code != 503 || problem(t, w).Code != "UNAVAILABLE" {
		t.Fatalf("not ready: %d %s", w.Code, w.Body)
	}
	for name, header := range map[string]map[string]string{"no token": nil, "another scheme": {"Authorization": "Basic good"},
		"a bad token": {"Authorization": "Bearer bad"}} {
		w := do(h, call{method: "GET", path: "/v1/whoami", header: header})
		if p := problem(t, w); w.Code != 401 || p.Code != "UNAUTHENTICATED" || w.Header().Get("WWW-Authenticate") == "" || p.CorrelationID == "" {
			t.Fatalf("%s: %d %+v", name, w.Code, p)
		}
	}
	w := do(h, call{method: "GET", path: "/v1/whoami", header: with(auth, "X-Correlation-ID", "client-corr-0001")})
	var who map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &who)
	if w.Code != 200 || who["tenant"] != "t1" || who["actor"] != "alice" || who["correlation"] != "client-corr-0001" ||
		w.Header().Get("X-Correlation-ID") != "client-corr-0001" {
		t.Fatalf("whoami: %d %v %v", w.Code, who, w.Header())
	}
	if w = do(h, call{method: "GET", path: "/v1/whoami", header: with(auth, "X-Correlation-ID", "bad id with blanks")}); w.Header().Get("X-Correlation-ID") ==
		"bad id with blanks" || len(w.Header().Get("X-Correlation-ID")) != 32 {
		t.Fatalf("a malformed correlation id is replaced: %v", w.Header())
	}
	if w = do(h, call{method: "GET", path: "/v1/nothing", header: auth}); w.Code != 404 || problem(t, w).Code != "NOT_FOUND" {
		t.Fatalf("no route: %d %s", w.Code, w.Body)
	}
	if w = do(h, call{method: "DELETE", path: "/v1/whoami", header: auth}); w.Code != 405 || problem(t, w).Code != "METHOD_NOT_ALLOWED" ||
		!strings.Contains(w.Header().Get("Allow"), "GET") {
		t.Fatalf("another method: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if w = do(h, call{method: "GET", path: "/elsewhere"}); w.Code != 404 || problem(t, w).Code != "NOT_FOUND" {
		t.Fatalf("outside /v1: %d %s", w.Code, w.Body)
	}
	if w = do(h, call{method: "GET", path: "/v1/panic", header: auth}); w.Code != 500 || problem(t, w).Code != "INTERNAL" {
		t.Fatalf("panic: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(logs.String(), `"actor_id":"alice"`) || !strings.Contains(logs.String(), `"panic":"boom"`) || strings.Contains(logs.String(), "good") {
		t.Fatalf("logs: %s", logs.String())
	}
	if _, err := NewHandler(Config{}); err == nil {
		t.Fatal("a handler without its verifier, logger and readiness check")
	}
}

// Error kinds map to status codes and business codes; commands take Idempotency-Key and If-Match; bodies are JSON
// objects with known fields.
func TestProblemsAndCommands(t *testing.T) {
	h := handler(t, io.Discard, nil)
	for kind, want := range map[string]struct {
		status int
		code   string
	}{
		"unauthenticated": {401, "UNAUTHENTICATED"}, "unauthorized": {403, "FORBIDDEN"}, "not-found": {404, "NOT_FOUND"},
		"conflict": {409, "VERSION_CONFLICT"}, "idempotency": {409, "IDEMPOTENCY_MISMATCH"}, "rejected": {422, "DEMO_TOO_LATE"},
		"rejected-plain": {422, "BUSINESS_RULE_VIOLATION"}, "uncertain": {503, "COMMIT_UNCERTAIN"}, "internal": {500, "INTERNAL"},
		"rejected-spoofed": {422, "BUSINESS_RULE_VIOLATION"}, "conflict-database": {409, "VERSION_CONFLICT"},
	} {
		w := do(h, call{method: "GET", path: "/v1/fail/" + kind, header: auth})
		p := problem(t, w)
		if w.Code != want.status || p.Code != want.code || p.Status != want.status || p.Instance != "/v1/fail/"+kind {
			t.Errorf("%s: %d %+v", kind, w.Code, p)
		}
		if kind == "internal" && strings.Contains(w.Body.String(), "does not exist") {
			t.Errorf("an internal error described: %s", w.Body)
		}
		if kind == "conflict-database" && strings.Contains(w.Body.String(), "cargo_demo") {
			t.Errorf("the database's message described: %s", w.Body)
		}
		if kind == "uncertain" && w.Header().Get("Retry-After") == "" {
			t.Errorf("no Retry-After")
		}
	}
	change := func(body string, header map[string]string) *httptest.ResponseRecorder {
		return do(h, call{method: "POST", path: "/v1/things/T-1/change", body: body, header: header})
	}
	full := with(auth, "Content-Type", "application/json", "Idempotency-Key", "cmd-1", "If-Match", `"3"`)
	w := change(`{"name":"x"}`, full)
	var rc httpapi.Receipt
	_ = json.Unmarshal(w.Body.Bytes(), &rc)
	if w.Code != 200 || rc != (httpapi.Receipt{CommandID: "cmd-1", SubjectID: "T-1", NewRevision: 4}) || w.Header().Get("ETag") != `"4"` {
		t.Fatalf("a change: %d %s %v", w.Code, w.Body, w.Header())
	}
	for name, tc := range map[string]struct {
		body   string
		header map[string]string
		status int
		code   string
	}{
		"no content type":      {`{"name":"x"}`, with(full, "Content-Type", ""), 415, "UNSUPPORTED_MEDIA_TYPE"},
		"text":                 {`{"name":"x"}`, with(full, "Content-Type", "text/plain"), 415, "UNSUPPORTED_MEDIA_TYPE"},
		"an unknown field":     {`{"name":"x","admin":true}`, full, 400, "MALFORMED_REQUEST"},
		"a key in capitals":    {`{"NAME":"x"}`, full, 400, "MALFORMED_REQUEST"},
		"a key twice":          {`{"name":"x","name":"y"}`, full, 400, "MALFORMED_REQUEST"},
		"a nested key twice":   {`{"name":"x","extra":{"a":1,"a":2}}`, full, 400, "MALFORMED_REQUEST"},
		"an array":             {`[{"name":"x"}]`, full, 400, "MALFORMED_REQUEST"},
		"a wrong type":         {`{"name":7}`, full, 400, "MALFORMED_REQUEST"},
		"cut short":            {`{"name":"x"`, full, 400, "MALFORMED_REQUEST"},
		"too large after it":   {`{"name":"x"}` + strings.Repeat(" ", httpapi.MaxBodyBytes), full, 413, "BODY_TOO_LARGE"},
		"If-Match +1":          {`{"name":"x"}`, with(full, "If-Match", `"+1"`), 400, "MALFORMED_REQUEST"},
		"If-Match 01":          {`{"name":"x"}`, with(full, "If-Match", `"01"`), 400, "MALFORMED_REQUEST"},
		"a weak If-Match":      {`{"name":"x"}`, with(full, "If-Match", `W/"1"`), 400, "MALFORMED_REQUEST"},
		"two objects":          {`{"name":"x"}{"name":"y"}`, full, 400, "MALFORMED_REQUEST"},
		"not JSON":             {`name=x`, full, 400, "MALFORMED_REQUEST"},
		"too large":            {`{"name":"` + strings.Repeat("x", httpapi.MaxBodyBytes) + `"}`, full, 413, "BODY_TOO_LARGE"},
		"no If-Match":          {`{"name":"x"}`, with(full, "If-Match", ""), 428, "PRECONDITION_REQUIRED"},
		"an unquoted If-Match": {`{"name":"x"}`, with(full, "If-Match", "3"), 400, "MALFORMED_REQUEST"},
		"a negative If-Match":  {`{"name":"x"}`, with(full, "If-Match", `"-1"`), 400, "MALFORMED_REQUEST"},
		"If-Match *":           {`{"name":"x"}`, with(full, "If-Match", "*"), 400, "MALFORMED_REQUEST"},
		"no Idempotency-Key":   {`{"name":"x"}`, with(full, "Idempotency-Key", ""), 400, "IDEMPOTENCY_KEY_REQUIRED"},
		"a key with blanks":    {`{"name":"x"}`, with(full, "Idempotency-Key", "a b"), 400, "MALFORMED_REQUEST"},
		"a key too long":       {`{"name":"x"}`, with(full, "Idempotency-Key", strings.Repeat("k", 129)), 400, "MALFORMED_REQUEST"},
	} {
		w := change(tc.body, tc.header)
		if p := problem(t, w); w.Code != tc.status || p.Code != tc.code {
			t.Errorf("%s: %d %+v", name, w.Code, p)
		}
	}
}

// A handler that panics after it began its response aborts the connection: the client sees a broken response, not a
// short one that looks complete; the access log still has the request.
func TestPanicAfterWriting(t *testing.T) {
	var logs bytes.Buffer
	srv := httptest.NewServer(handler(t, &syncWriter{w: &logs}, nil))
	defer srv.Close()
	req, _ := nethttp.NewRequest("GET", srv.URL+"/v1/panic-late", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := srv.Client().Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("a response cut by a panic read as complete")
	}
	srv.Close()
	if out := logs.String(); !strings.Contains(out, `"panic":"boom after writing"`) || !strings.Contains(out, `"path":"/v1/panic-late","status":0`) {
		t.Fatalf("logs: %s", out)
	}
}

// syncWriter serializes the server goroutines' log writes.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}
