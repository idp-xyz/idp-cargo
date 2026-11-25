package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The pinned single vector of idp-sandbox (internal/mcp/assertion_test.go, whose constants vectorKey, vectorAssertion,
// vectorAudience, vectorDigest and vectorParams are copied here unchanged, with meta from its mcp_test.go): a request,
// its digest, and an assertion that idp-mcp's gateway made for it with its real signer (CallerSigner, at
// 2026-11-17T00:00:00Z) for the endpoint http://sandbox:8570/mcp, with the public half of the key that signed it. The
// same request and digest are pinned in idp-mcp's TestRequestDigestIsTheOneUpstreamsCompute. Here it checks the digest
// this server computes, that the digest does not move when the request is written another way, that this verifier (set
// to the sandbox's endpoint and to the gateway id the assertion names, which the test reads from it) accepts what the
// real signer made, and that a verifier for another endpoint refuses it. The platform's vectors (callervectors_test.go)
// judge the refusals.
const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

const (
	vectorKey = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEohuazOuwtXVv4bbe+34tuxPRZYT0
MhfvC5orxYuBtlzQDkcbdyY8ArPLyQmVNAZRh6jp2RnXFF/+CLtd4X/dUA==
-----END PUBLIC KEY-----
`
	vectorAssertion = "eyJhbGciOiJFUzI1NiIsImtpZCI6ImdTUDRRczdHbUJKbTdnbmtZSEhvZDRzNWN2YmRUalZfYWhxcUR2Q0F1M1EiLCJ0eXAiOiJpZHAtY2FsbGVyK2p3dCJ9.eyJhY3RvciI6ImlkcC1ob3N0IiwiYXVkIjoiaHR0cDovL3NhbmRib3g6ODU3MC9tY3AiLCJjbGllbnQiOiJpZHAtaG9zdCIsImV4cCI6MTc5NDg3MzY2MCwiaWF0IjoxNzk0ODczNjAwLCJpc3MiOiJpZHAtbWNwLWdhdGV3YXk6ZzEiLCJqdGkiOiJDWWNhYlJPczRBNjZBMFN3IiwicmVxIjoic2hhMjU2Ojg4NTFiNDQ1MTliNmMwNDgwODhiZmQ2NjA5MDU4MWI0MzNiNzQ4OTI1NzUwMWJiMmE5MWUxN2UwZTg0YTViMzMiLCJyb2xlcyI6WyJvcGVyYXRvciJdLCJzdWIiOiJzdmMtb3BlcmF0b3IiLCJ0ZW5hbnRfaWQiOiJ0MSJ9.oSghV4NExUaodJA0St5d2ItxL69AsLxugvttrFzFlL0sbNB2VGC-B2FQwTphO15IsJN7fUZi-GrhyTLuDT2X5w"
	vectorAudience  = "http://sandbox:8570/mcp"
	vectorDigest    = "sha256:8851b44519b6c048088bfd66090581b433b7489257501bb2a91e17e0e84a5b33"
	vectorParams    = `{"name":"sandbox_run","arguments":{"workspaceId":"0123456789abcdef0123456789abcdef","language":"python","code":"print('<a> & é')","timeoutSeconds":5.0},` + meta + `}`
)

var vectorTime = time.Date(2026, 11, 17, 0, 0, 20, 0, time.UTC)

func vectorParamsOf(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var p map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestThePinnedVectorsDigestIsTheGatewaysAndDoesNotMoveWithTheSpelling(t *testing.T) {
	if d, err := requestDigest("tools/call", vectorParamsOf(t, vectorParams)); err != nil || d != vectorDigest {
		t.Fatalf("digest %s %v, the gateway computes %s", d, err, vectorDigest)
	}
	// the digest is of the JSON value, not of its bytes: other spacing, key order, escapes and _meta do not change it
	respelled := `{ "arguments": {"timeoutSeconds":5.0, "code":"print('<a> & é')", "language":"python","workspaceId":"0123456789abcdef0123456789abcdef"}, "_meta":{"x":1}, "name":"sandbox_run"}`
	if d, _ := requestDigest("tools/call", vectorParamsOf(t, respelled)); d != vectorDigest {
		t.Fatalf("the same request spelled differently has digest %s", d)
	}
	// a changed number is another request: 5.0 is not 5
	if d, _ := requestDigest("tools/call", vectorParamsOf(t, `{"name":"sandbox_run","arguments":{"workspaceId":"0123456789abcdef0123456789abcdef","language":"python","code":"print('<a> & é')","timeoutSeconds":5}}`)); d == vectorDigest {
		t.Fatal("5 and 5.0 have the same digest: numbers are digested as written")
	}
}

func TestThePinnedVectorsAssertionIsAcceptedOnceAndOnlyForItsEndpoint(t *testing.T) {
	// the gateway id is the one the real signer put in the assertion
	claims := claimsOf(t, vectorAssertion)
	iss, _ := claims["iss"].(string)
	const prefix = "idp-mcp-gateway:"
	if len(iss) <= len(prefix) || iss[:len(prefix)] != prefix {
		t.Fatalf("the pinned assertion's issuer is not a gateway's: %q", iss)
	}
	v, err := NewVerifier([]byte(vectorKey), vectorAudience, iss[len(prefix):])
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return vectorTime }
	c, err := v.Verify(vectorAssertion, "tools/call", vectorParamsOf(t, vectorParams))
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "svc-operator" || c.Tenant != "t1" || len(c.Roles) != 1 || c.Roles[0] != "operator" || c.Client != "idp-host" || c.Actor != "idp-host" {
		t.Fatalf("%+v", c)
	}
	if _, err := v.Verify(vectorAssertion, "tools/call", vectorParamsOf(t, vectorParams)); err == nil {
		t.Fatal("the same assertion was accepted twice")
	}
	other, _ := NewVerifier([]byte(vectorKey), "http://fin:8600/mcp", iss[len(prefix):])
	other.now = func() time.Time { return vectorTime }
	if _, err := other.Verify(vectorAssertion, "tools/call", vectorParamsOf(t, vectorParams)); err == nil {
		t.Fatal("an assertion made for the sandbox was accepted for another endpoint")
	}
}

// The middleware reads a call before the SDK does, so what it takes for one call has to be the one call the SDK takes.
func TestSplitRequestTakesOneCallAndNothingElse(t *testing.T) {
	good := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"x","_meta":{"a":1}}}`
	got, err := splitRequest([]byte(good))
	if err != nil || got.method != "tools/call" || string(got.id) != "7" || len(got.params) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for name, body := range map[string]string{
		"data after the call":             good + ` {}`,
		"two calls":                       good + good,
		"a repeated id":                   `{"id":1,"id":2,"method":"ping"}`,
		"a repeated method":               `{"method":"tools/list","method":"tools/call"}`,
		"a repeated params":               `{"method":"ping","params":{},"params":{"a":1}}`,
		"a repeated key in params":        `{"method":"tools/call","params":{"name":"x","arguments":{"n":"EVIL"},"arguments":{"n":"a"}}}`,
		"a repeated key in the arguments": `{"method":"tools/call","params":{"name":"x","arguments":{"n":"EVIL","n":"a"}}}`,
		"a repeated key in an array":      `{"method":"tools/call","params":{"name":"x","arguments":{"l":[{"a":1,"a":2}]}}}`,
		"a repeated _meta":                `{"method":"tools/call","params":{"_meta":{"a":1},"_meta":{"b":2}}}`,
		"a body that nests too deep":      `{"method":"ping","params":{"x":` + strings.Repeat(`{"a":`, 70) + `1` + strings.Repeat(`}`, 70) + `}}`,
		"another case":                    `{"Method":"tools/call","method":"tools/list"}`,
		"only another case":               `{"METHOD":"tools/list"}`,
		"a batch":                         `[` + good + `]`,
		"a method of a number":            `{"method":5}`,
		"an empty method":                 `{"method":""}`,
		"params of a list":                `{"method":"ping","params":[1]}`,
		"a half object":                   `{"method":"ping"`,
	} {
		if _, err := splitRequest([]byte(body)); err == nil {
			t.Errorf("%s: taken for a call", name)
		}
	}
	if got, err := splitRequest([]byte(`{"method":"ping"}`)); err != nil || string(got.id) != "null" || got.params != nil {
		t.Errorf("a call without an id or params: %+v %v", got, err)
	}
}
