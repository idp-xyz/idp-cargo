package mcp_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.idp.xyz/idp-cargo/internal/transport/mcp"
	"go.idp.xyz/idp-cargo/internal/transport/mcp/mcptest"
)

const audience = "http://fin-mcp:8600/mcp"

type probeIn struct {
	N string `json:"n"`
}

// rig is the real handler (the verifier in front of the SDK's stateless Streamable HTTP handler) with one tool that
// says who called and counts how often it ran.
type rig struct {
	t      *testing.T
	signer *mcptest.Signer
	h      http.Handler
	ran    atomic.Int32
	log    *bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, signer: mcptest.NewSigner(audience, "g1"), log: &bytes.Buffer{}}
	v, err := mcp.NewVerifier(r.signer.PublicPEM(), audience, "g1")
	if err != nil {
		t.Fatal(err)
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "probe", Version: "1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "probe", Description: "Says who called.", InputSchema: map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"n"}, "properties": map[string]any{"n": map[string]any{"type": "string", "maxLength": 20}}}},
		func(ctx context.Context, _ *sdk.CallToolRequest, in probeIn) (*sdk.CallToolResult, any, error) {
			r.ran.Add(1)
			c, ok := mcp.CallerFrom(ctx)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "caller=" + c.Subject + "/" + c.Tenant + " known=" + boolText(ok) + " n=" + in.N}}}, nil, nil
		})
	r.h = mcp.NewHandler(s, v, slog.New(slog.NewTextHandler(r.log, nil)))
	return r
}

func boolText(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

var alice = mcp.Caller{Subject: "alice", Tenant: "t1", Roles: []string{"analyst"}, Client: "idp-host"}

type reply struct {
	code int
	body string
	json map[string]any
}

func (r *rig) post(method, name string, body []byte) reply {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header = mcptest.Headers(method, name)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	out := reply{code: w.Code, body: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.json)
	return out
}

func (r *rig) call(c mcp.Caller, n string) reply {
	r.t.Helper()
	return r.post("tools/call", "probe", r.signer.Call(c, 1, "tools/call", map[string]any{"name": "probe", "arguments": map[string]any{"n": n}}))
}

func text(t *testing.T, r reply) string {
	t.Helper()
	res, _ := r.json["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content: %s", r.body)
	}
	return content[0].(map[string]any)["text"].(string)
}

// The SDK's two behaviours the guide could not verify (section 9, item 2, and 3.4): the caller put on the request's
// context by the middleware reaches the tool handler's context, and the body the middleware gives back is the one the
// SDK reads. Both are true in v1.7.0; if either stopped being true the tool would not see its caller or its arguments.
func TestTheCallerOnTheRequestContextReachesTheToolAndTheBodyGivenBackIsRead(t *testing.T) {
	r := newRig(t)
	got := r.call(alice, "hello")
	if got.code != 200 || text(t, got) != "caller=alice/t1 known=yes n=hello" || r.ran.Load() != 1 {
		t.Fatalf("%d %s (ran %d)", got.code, got.body, r.ran.Load())
	}
	if !strings.Contains(got.body, `"resultType":"complete"`) {
		t.Fatalf("not a 2026-07-28 result: %s", got.body)
	}
}

func TestTheCatalogueAndTheDiscoveryNeedNoAssertion(t *testing.T) {
	r := newRig(t)
	list := r.post("tools/list", "", mcptest.Body(1, "tools/list", map[string]any{}, ""))
	if list.code != 200 || !strings.Contains(list.body, `"name":"probe"`) {
		t.Fatalf("tools/list: %d %s", list.code, list.body)
	}
	discover := r.post("server/discover", "", mcptest.Body(2, "server/discover", map[string]any{}, ""))
	if discover.code != 200 || !strings.Contains(discover.body, "supportedVersions") {
		t.Fatalf("server/discover: %d %s", discover.code, discover.body)
	}
	if r.ran.Load() != 0 {
		t.Fatal("a tool ran")
	}
}

// The guide's negative tests (7.3), against the real handler, and the issuer: each is a 403 whose body says nothing of
// why, and the tool does not run.
func TestRefusedCallsAnswer403AndRunNothing(t *testing.T) {
	r := newRig(t)
	args := map[string]any{"name": "probe", "arguments": map[string]any{"n": "a"}}
	good := func() []byte { return r.signer.Call(alice, 1, "tools/call", args) }
	// a body whose _meta holds an assertion made by sign over params (which may differ from the params sent)
	with := func(assertion string, params map[string]any) []byte {
		return mcptest.Body(1, "tools/call", params, assertion)
	}
	forge := func(mutate func(h, c map[string]any)) string {
		h, c := r.signer.Header(), r.signer.Claims(alice, "tools/call", mcptest.Params(args))
		mutate(h, c)
		return r.signer.Raw(h, c)
	}
	other := mcptest.NewSigner(audience, "g1") // a key the server does not hold, under the server's own kid
	otherKid := other.Header()
	otherKid["kid"] = r.signer.KeyID()
	hs256 := func() string {
		h, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "idp-caller+jwt", "kid": r.signer.KeyID()})
		c, _ := json.Marshal(r.signer.Claims(alice, "tools/call", mcptest.Params(args)))
		signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
		mac := hmac.New(sha256.New, r.signer.PublicPEM()) // the public key's PEM as the secret
		mac.Write([]byte(signing))
		return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	rawHeader := func(h map[string]any, sig string) string {
		hb, _ := json.Marshal(h)
		cb, _ := json.Marshal(r.signer.Claims(alice, "tools/call", mcptest.Params(args)))
		return base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb) + "." + sig
	}
	replayed := good()
	if first := r.post("tools/call", "probe", replayed); first.code != 200 {
		t.Fatalf("the first presentation: %d %s", first.code, first.body)
	}
	// one byte of the arguments changed
	altered := map[string]any{"name": "probe", "arguments": map[string]any{"n": "b"}}
	tests := map[string][]byte{
		"only the plain caller, as admin": mcptest.Body(1, "tools/call", map[string]any{"name": "probe", "arguments": map[string]any{"n": "a"},
			"_meta": map[string]any{"io.idp.telos/caller": map[string]any{"subject": "admin", "tenant": "t1", "roles": []string{"admin"}}}}, ""),
		"no assertion at all":    mcptest.Body(1, "tools/call", args, ""),
		"an audience of another": with(forge(func(_, c map[string]any) { c["aud"] = "http://web:8580/mcp" }), args),
		"an expired assertion": with(forge(func(_, c map[string]any) {
			c["iat"] = time.Now().Add(-4 * time.Minute).Unix()
			c["exp"] = time.Now().Add(-3 * time.Minute).Unix()
		}), args),
		"the same assertion twice":   replayed,
		"arguments changed a byte":   with(r.signer.Assertion(alice, "tools/call", mcptest.Params(args)), altered),
		"another method's":           with(r.signer.Assertion(alice, "resources/read", mcptest.Params(args)), args),
		"alg none":                   with(rawHeader(map[string]any{"alg": "none", "typ": "idp-caller+jwt", "kid": r.signer.KeyID()}, ""), args),
		"alg HS256 keyed by the PEM": with(hs256(), args),
		"alg ES384":                  with(rawHeader(map[string]any{"alg": "ES384", "typ": "idp-caller+jwt", "kid": r.signer.KeyID()}, base64.RawURLEncoding.EncodeToString(make([]byte, 96))), args),
		"an unknown kid":             with(forge(func(h, _ map[string]any) { h["kid"] = "no-such-key" }), args),
		"a key the server does not hold under its kid": with(other.Raw(otherKid, other.Claims(alice, "tools/call", mcptest.Params(args))), args),
		"another issuer":                    with(forge(func(_, c map[string]any) { c["iss"] = "idp-mcp-gateway:g2" }), args),
		"no issuer":                         with(forge(func(_, c map[string]any) { delete(c, "iss") }), args),
		"an issuer that is not a gateway's": with(forge(func(_, c map[string]any) { c["iss"] = "g1" }), args),
	}
	for name, body := range tests {
		before := r.ran.Load()
		got := r.post("tools/call", "probe", body)
		if got.code != http.StatusForbidden || r.ran.Load() != before {
			t.Errorf("%s: %d %s (the tool ran: %v)", name, got.code, got.body, r.ran.Load() != before)
			continue
		}
		msg, _ := got.json["error"].(map[string]any)
		if msg["message"] != mcp.ErrAssertion.Error() {
			t.Errorf("%s: the body says more than it should: %s", name, got.body)
		}
	}
	// every other method needs an assertion too, not only tools/call
	for _, method := range []string{"initialize", "ping", "resources/read", "prompts/list", "notifications/initialized"} {
		if got := r.post(method, "", mcptest.Body(1, method, map[string]any{}, "")); got.code != http.StatusForbidden {
			t.Errorf("%s without an assertion: %d %s", method, got.code, got.body)
		}
	}
	if r.ran.Load() != 1 {
		t.Fatalf("the tool ran %d times; only the first presentation of the good call may run it", r.ran.Load())
	}
	// the reasons are in the log, with no assertion in it
	logged := r.log.String()
	for _, want := range []string{"issued by", "presented before", "another request", "no assertion"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log does not say %q", want)
		}
	}
	if strings.Contains(logged, "eyJ") {
		t.Fatal("an assertion is in the log")
	}
}

// A request is read once by the middleware and once by the SDK, so it must read the same: a body that is not one call
// is refused before it can be taken for tools/list by the one and tools/call by the other.
func TestABodyThatCouldBeReadTwoWaysIsRefused(t *testing.T) {
	r := newRig(t)
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	call := string(r.signer.Call(alice, 1, "tools/call", map[string]any{"name": "probe", "arguments": map[string]any{"n": "a"}}))
	for name, body := range map[string]string{
		"a batch":                  "[" + list + "," + call + "]",
		"a repeated method":        `{"jsonrpc":"2.0","id":1,"method":"tools/list","method":"tools/call","params":{"name":"probe","arguments":{"n":"x"}}}`,
		"a method in another case": `{"jsonrpc":"2.0","id":1,"method":"tools/list","Method":"tools/call","params":{"name":"probe","arguments":{"n":"x"}}}`,
		"params in another case":   `{"jsonrpc":"2.0","id":1,"method":"tools/list","PARAMS":{"name":"probe"}}`,
		"two calls":                list + " " + list,
		"not an object":            `"tools/list"`,
		"no method":                `{"jsonrpc":"2.0","id":1,"params":{}}`,
		"an empty body":            ``,
	} {
		got := r.post("tools/list", "", []byte(body))
		if got.code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, got.code, got.body)
		}
	}
	if r.ran.Load() != 0 {
		t.Fatal("a tool ran")
	}
	// only POST is served, so a GET or a DELETE does not reach the SDK's session handling
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		req := httptest.NewRequest(m, "/mcp", nil)
		w := httptest.NewRecorder()
		r.h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
			t.Errorf("%s: %d", m, w.Code)
		}
	}
	// a body over the limit
	big := httptest.NewRequest(http.MethodPost, "/mcp", io.LimitReader(zeros{}, 2<<20))
	big.Header = mcptest.Headers("tools/list", "")
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body of 2 MiB: %d", w.Code)
	}
}

// The digest covers one value of each key. A body that holds a key of the params twice, the one the gateway signed
// after the one that is not signed, is refused: the SDK might take the other.
func TestARepeatedKeyCannotCarryArgumentsTheAssertionDoesNotCover(t *testing.T) {
	r := newRig(t)
	signed := map[string]any{"name": "probe", "arguments": map[string]any{"n": "signed"}}
	body := string(r.signer.Call(alice, 1, "tools/call", signed))
	for name, smuggled := range map[string]string{
		"first": strings.Replace(body, `"arguments":{"n":"signed"}`, `"arguments":{"n":"EVIL"},"arguments":{"n":"signed"}`, 1),
		"last":  strings.Replace(body, `"arguments":{"n":"signed"}`, `"arguments":{"n":"signed"},"arguments":{"n":"EVIL"}`, 1),
		"inner": strings.Replace(body, `"arguments":{"n":"signed"}`, `"arguments":{"n":"EVIL","n":"signed"}`, 1),
	} {
		if smuggled == body {
			t.Fatalf("%s: the body was not changed", name)
		}
		if got := r.post("tools/call", "probe", []byte(smuggled)); got.code != http.StatusBadRequest || r.ran.Load() != 0 {
			t.Errorf("%s: %d %s (the tool ran %d times)", name, got.code, got.body, r.ran.Load())
		}
	}
	if got := r.post("tools/call", "probe", []byte(body)); got.code != 200 || !strings.Contains(text(t, got), "n=signed") {
		t.Fatalf("the honest body: %d %s", got.code, got.body)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// The server does not start without a valid P-256 public key, may hold several, and tells its kids.
func TestTheVerifierNeedsAValidP256PublicKey(t *testing.T) {
	good := mcptest.NewSigner(audience, "g1")
	second := mcptest.NewSigner(audience, "g1")
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der384, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	for name, tc := range map[string]struct {
		pem               []byte
		audience, gateway string
	}{
		"no key":                     {nil, audience, "g1"},
		"not PEM":                    {[]byte("not a key"), audience, "g1"},
		"a private key":              {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}), audience, "g1"},
		"a key on P-384":             {pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der384}), audience, "g1"},
		"no audience":                {good.PublicPEM(), "", "g1"},
		"no gateway id":              {good.PublicPEM(), audience, ""},
		"a good one, then a bad one": {append(good.PublicPEM(), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der384})...), audience, "g1"},
	} {
		if _, err := mcp.NewVerifier(tc.pem, tc.audience, tc.gateway); err == nil {
			t.Errorf("%s: the verifier was made", name)
		}
	}
	v, err := mcp.NewVerifier(append(good.PublicPEM(), second.PublicPEM()...), audience, "g1")
	if err != nil {
		t.Fatal(err)
	}
	ids := v.KeyIDs()
	if len(ids) != 2 || !(ids[0] == good.KeyID() || ids[1] == good.KeyID()) || !(ids[0] == second.KeyID() || ids[1] == second.KeyID()) || ids[0] >= ids[1] {
		t.Fatalf("kids %v, want %s and %s sorted", ids, good.KeyID(), second.KeyID())
	}
}
