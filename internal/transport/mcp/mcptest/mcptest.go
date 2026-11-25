// Package mcptest signs the caller assertions that a test, or the local check of tools/demo/fin-mcp.sh, sends to an MCP
// server of Cargo, the way idp-mcp's gateway would: with a key pair of its own, so that no real key is ever needed. No
// Cargo server imports it.
package mcptest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strconv"
	"time"

	"go.idp.xyz/idp-cargo/internal/transport/mcp"
)

// Signer makes assertions for Audience as the gateway Issuer would.
type Signer struct {
	Key      *ecdsa.PrivateKey
	Issuer   string
	Audience string
	Now      func() time.Time
	n        int
}

// NewSigner makes a signer with a new P-256 key for the endpoint audience and the gateway id.
func NewSigner(audience, gatewayID string) *Signer {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return &Signer{Key: key, Issuer: "idp-mcp-gateway:" + gatewayID, Audience: audience, Now: time.Now}
}

// PublicPEM is the PKIX PEM of the public key, as an operator gives it to the server.
func (s *Signer) PublicPEM() []byte {
	der, err := x509.MarshalPKIXPublicKey(&s.Key.PublicKey)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// KeyID is the RFC 7638 thumbprint of the public key.
func (s *Signer) KeyID() string {
	raw, err := s.Key.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + enc(raw[1:33]) + `","y":"` + enc(raw[33:]) + `"}`))
	return enc(sum[:])
}

var b64 = base64.RawURLEncoding

// Raw signs any header and claims with the key (ES256 over the two), for a test that forges one part.
func (s *Signer) Raw(header, claims map[string]any) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signing := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	sum := sha256.Sum256([]byte(signing))
	r, ss, err := ecdsa.Sign(rand.Reader, s.Key, sum[:])
	if err != nil {
		panic(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig)
}

// Claims are the claims of a good assertion of c for the request: the gateway's own, valid for a minute from now.
func (s *Signer) Claims(c mcp.Caller, method string, params map[string]json.RawMessage) map[string]any {
	digest, err := mcp.Digest(map[string]any{"method": method, "params": withoutMeta(params)})
	if err != nil {
		panic(err)
	}
	s.n++
	now := s.Now()
	claims := map[string]any{"iss": s.Issuer, "aud": s.Audience, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		"jti": "jti-" + strconv.FormatInt(now.UnixNano(), 36) + "-" + strconv.Itoa(s.n), "sub": c.Subject, "tenant_id": c.Tenant, "roles": c.Roles, "req": digest}
	if c.Client != "" {
		claims["client"] = c.Client
	}
	if c.Actor != "" {
		claims["actor"] = c.Actor
	}
	return claims
}

// Header is the header of a good assertion.
func (s *Signer) Header() map[string]any {
	return map[string]any{"alg": "ES256", "typ": "idp-caller+jwt", "kid": s.KeyID()}
}

// Assertion signs a good assertion of c for the request.
func (s *Signer) Assertion(c mcp.Caller, method string, params map[string]json.RawMessage) string {
	return s.Raw(s.Header(), s.Claims(c, method, params))
}

func withoutMeta(params map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range params {
		if k != "_meta" {
			out[k] = v
		}
	}
	return out
}

// Params turns the params of a call into the form the signer digests.
func Params(params map[string]any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range params {
		raw, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		out[k] = raw
	}
	return out
}

// Body is the JSON-RPC body of a call the gateway would send: the params, and in _meta the protocol version, the
// capabilities and, when assertion is not empty, the assertion.
func Body(id int, method string, params map[string]any, assertion string) []byte {
	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	if assertion != "" {
		meta[mcp.MetaCallerAssertion] = assertion
	}
	p := map[string]any{"_meta": meta}
	for k, v := range params {
		p[k] = v
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": p})
	if err != nil {
		panic(err)
	}
	return body
}

// Call is the body of a signed call of c.
func (s *Signer) Call(c mcp.Caller, id int, method string, params map[string]any) []byte {
	return Body(id, method, params, s.Assertion(c, method, Params(params)))
}

// Headers are the headers of a request of the gateway's protocol for the method (and tool name).
func Headers(method, name string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
	h.Set("Mcp-Protocol-Version", "2026-07-28")
	h.Set("Mcp-Method", method)
	if name != "" {
		h.Set("Mcp-Name", name)
	}
	return h
}
