package mcp

// The platform's conformance vectors of the caller assertion (idp-mcp ADR-0001 D44): the file idp-mcp generates with
// its real signer, published as idp-contracts/vectors/caller-assertion/v1.json and kept byte-identical in idp-mcp,
// idp-sandbox, idp-web and here (testdata/caller-assertion-v1.json, a copy that must not be changed). Every case runs
// through this server's real middleware, with the keys and the audience of the file, the time of the step and the
// gateway id g1, and must come out as the file says: the caller of the file for an acceptance, a 403 for the stated
// reason otherwise. A fresh verifier serves each case; its steps share it, which is how a replay is told.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const callerVectorsFormat = "idp.telos.caller-assertion-vectors/v1"

type callerVectors struct {
	Format   string `json:"format"`
	Audience string `json:"audience"`
	Keys     []struct {
		Name string `json:"name"`
		PEM  string `json:"pem"`
	} `json:"keys"`
	Cases []struct {
		Name  string `json:"name"`
		Steps []struct {
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
			Now    string                     `json:"now"`
			Expect struct {
				Outcome string `json:"outcome"`
				Caller  *struct {
					Subject string   `json:"subject"`
					Tenant  string   `json:"tenant"`
					Roles   []string `json:"roles"`
					Client  string   `json:"client"`
					Actor   string   `json:"actor"`
				} `json:"caller"`
				Reason string `json:"reason"`
			} `json:"expect"`
		} `json:"steps"`
	} `json:"cases"`
}

// What this verifier says for each reason of the vocabulary. A refusal must match its own reason and no other, so the
// reasons stay told apart: a verifier that refused an expired assertion as a bad signature would not pass.
var callerVectorReasons = map[string][]string{
	"no_assertion":     {"no assertion"},
	"malformed":        {"not a compact JWS", "not base64url", "claims are not an object"},
	"header":           {"header alg"},
	"unknown_key":      {"does not hold"},
	"bad_signature":    {"signature"},
	"audience":         {", not for "},
	"expired":          {"expired"},
	"issued_in_future": {"future"},
	"lifetime":         {"too long"},
	"missing_claim":    {"no subject or id"},
	"request_mismatch": {"another request"},
	"replay":           {"presented before"},
}

func reasonsOf(text string) []string {
	var out []string
	for reason, words := range callerVectorReasons {
		for _, w := range words {
			if strings.Contains(text, w) {
				out = append(out, reason)
				break
			}
		}
	}
	return out
}

// reasons records the reason of each refusal the middleware logs.
type reasons struct {
	mu   sync.Mutex
	last string
}

func (r *reasons) Enabled(context.Context, slog.Level) bool { return true }
func (r *reasons) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *reasons) WithGroup(string) slog.Handler            { return r }
func (r *reasons) Handle(_ context.Context, rec slog.Record) error {
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "reason" {
			r.mu.Lock()
			r.last = a.Value.String()
			r.mu.Unlock()
		}
		return true
	})
	return nil
}

// claimsOf reads the claims of an assertion without checking anything.
func claimsOf(t *testing.T, assertion string) map[string]any {
	t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

func assertionOf(params map[string]json.RawMessage) string {
	var meta map[string]json.RawMessage
	var a string
	_ = json.Unmarshal(params["_meta"], &meta)
	_ = json.Unmarshal(meta[MetaCallerAssertion], &a)
	return a
}

func TestEveryCaseOfTheCallerAssertionVectorsComesOutAsTheFileSays(t *testing.T) {
	raw, err := os.ReadFile("testdata/caller-assertion-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f callerVectors
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Format != callerVectorsFormat || len(f.Keys) == 0 || len(f.Cases) == 0 || f.Audience == "" {
		t.Fatalf("not a file of format %s", callerVectorsFormat)
	}
	var keys []byte
	for _, k := range f.Keys {
		keys = append(keys, k.PEM...)
	}
	accepted, refused := 0, 0
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			v, err := NewVerifier(keys, f.Audience, "g1")
			if err != nil {
				t.Fatal(err)
			}
			var now time.Time
			v.now = func() time.Time { return now }
			var log reasons
			var got Caller
			ran := false
			h := v.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, ran = CallerFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			}), slog.New(&log))
			for i, s := range c.Steps {
				where := c.Name + " step " + string(rune('1'+i))
				if now, err = time.Parse(time.RFC3339, s.Now); err != nil {
					t.Fatalf("%s: %v", where, err)
				}
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": s.Method, "params": s.Params})
				req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
				got, ran, log.last = Caller{}, false, ""
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				switch s.Expect.Outcome {
				case "accept":
					want := s.Expect.Caller
					if iss, _ := claimsOf(t, assertionOf(s.Params))["iss"].(string); iss != "idp-mcp-gateway:g1" {
						t.Fatalf("%s: an accepted case is issued by %q, not by the gateway g1", where, iss)
					}
					if w.Code != 200 || !ran || got.Subject != want.Subject || got.Tenant != want.Tenant || got.Client != want.Client || got.Actor != want.Actor ||
						strings.Join(got.Roles, ",") != strings.Join(want.Roles, ",") || got.Roles == nil {
						t.Fatalf("%s: want %+v accepted, got %d %+v (ran %v)", where, *want, w.Code, got, ran)
					}
					accepted++
				case "refuse":
					if w.Code != http.StatusForbidden || ran {
						t.Fatalf("%s: want a 403 for %q, got %d (the handler ran: %v)", where, s.Expect.Reason, w.Code, ran)
					}
					if rs := reasonsOf(log.last); len(rs) != 1 || rs[0] != s.Expect.Reason {
						t.Fatalf("%s: want the reason %q, the log says %q (reasons %v)", where, s.Expect.Reason, log.last, rs)
					}
					if strings.Contains(w.Body.String(), log.last) {
						t.Fatalf("%s: the reason reached the caller: %s", where, w.Body.String())
					}
					refused++
				default:
					t.Fatalf("%s: outcome %q", where, s.Expect.Outcome)
				}
			}
		})
	}
	t.Logf("%d cases, %d accepted steps, %d refused steps", len(f.Cases), accepted, refused)
	if accepted == 0 || refused == 0 {
		t.Fatal("the vectors have to hold acceptances and refusals")
	}
}

// The vectors say what to do with the issuer only for their own cases: every accepted one is issued by g1. A verifier
// of another gateway id refuses all of them, and does so after the other rules (so that the reasons above hold).
func TestAnotherGatewayIdRefusesEveryAcceptedVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/caller-assertion-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f callerVectors
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	var keys []byte
	for _, k := range f.Keys {
		keys = append(keys, k.PEM...)
	}
	n := 0
	for _, c := range f.Cases {
		for _, s := range c.Steps {
			if s.Expect.Outcome != "accept" {
				continue
			}
			v, _ := NewVerifier(keys, f.Audience, "g2")
			now, _ := time.Parse(time.RFC3339, s.Now)
			v.now = func() time.Time { return now }
			if _, err := v.Verify(assertionOf(s.Params), s.Method, s.Params); err == nil || !strings.Contains(err.Error(), "issued by") {
				t.Fatalf("%s: an assertion of gateway g1 was not refused by a verifier of g2: %v", c.Name, err)
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("no accepted case")
	}
}
