// Package mcp is the inbound transport of a Cargo MCP server behind the IDP TELOS gateway (idp-mcp): the HTTP handler
// of the MCP Streamable HTTP transport in its stateless form, and the verifier of the caller assertion the gateway
// signs.
// The server knows a person only from that assertion: a compact JWS (ES256) that names the member, the gateway's tenant
// and roles, this server's endpoint as the audience and a digest of the request. The verifier is a middleware in front
// of the SDK's handler, so that it digests the raw JSON-RPC body the gateway sent, not what the SDK made of it. A
// request whose assertion is missing or wrong is answered 403 and runs nothing; the reason goes to the log only.
package mcp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"
)

// MetaCallerAssertion is where the gateway puts its signature over the caller (idp-mcp ADR-0001 D44).
const MetaCallerAssertion = "io.idp.telos/caller-assertion"

const (
	assertionType = "idp-caller+jwt"
	assertionSkew = 30 * time.Second // the gateway's clock and this one may differ by this much
	assertionMaxT = 5 * time.Minute  // an assertion valid for longer is not one the gateway makes
	maxAssertion  = 8192
	issuerPrefix  = "idp-mcp-gateway:"
)

// Caller is the member an assertion names, taken from the assertion only. Client (the OAuth client) and Actor (the
// delegating party of a token exchange) are for the audit, never for authorization.
type Caller struct {
	Subject string
	Tenant  string
	Roles   []string
	Client  string
	Actor   string
}

type callerKey struct{}

// WithCaller returns a context carrying the verified caller.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom answers the verified caller on ctx. Without one the call is refused: there is no default caller.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// Verifier checks the gateway's assertions: the signature against the public keys it holds, the issuer
// ("idp-mcp-gateway:<gateway id>"), the audience, the times, the digest of the request and that the id was not seen.
type Verifier struct {
	keys     map[string]*ecdsa.PublicKey // by RFC 7638 thumbprint, the assertion's kid
	audience string
	issuer   string
	now      func() time.Time

	mu   sync.Mutex
	seen map[string]int64 // jti -> when it may be forgotten (unix seconds)
}

// NewVerifier reads one or more PEM public keys (PKIX, P-256; several while the gateway's key is rotated). audience is
// this server's endpoint exactly as it is registered in idp-mcp, gatewayID the gateway this server accepts.
func NewVerifier(pemBytes []byte, audience, gatewayID string) (*Verifier, error) {
	if audience == "" || gatewayID == "" {
		return nil, errors.New("mcp: the verifier needs the endpoint this server is registered under and the gateway's id")
	}
	v := &Verifier{keys: map[string]*ecdsa.PublicKey{}, audience: audience, issuer: issuerPrefix + gatewayID, now: time.Now, seen: map[string]int64{}}
	for rest := pemBytes; ; {
		// nothing but blanks may stand outside the PEM blocks: pem.Decode skips what comes before a block, and a truncated
		// block reads as none, so each is looked for here; no message prints what the file holds
		at := bytes.Index(rest, []byte("-----BEGIN"))
		if at < 0 {
			if len(bytes.TrimSpace(rest)) != 0 {
				return nil, errors.New("mcp: the public key file holds something besides PEM blocks, or a block that is cut short")
			}
			break
		}
		if len(bytes.TrimSpace(rest[:at])) != 0 {
			return nil, errors.New("mcp: the public key file holds something outside its PEM blocks")
		}
		var block *pem.Block
		if block, rest = pem.Decode(rest[at:]); block == nil {
			return nil, errors.New("mcp: the public key file holds a PEM block that does not read")
		}
		if block.Type != "PUBLIC KEY" {
			return nil, errors.New("mcp: a PEM block of the public key file is not a PUBLIC KEY")
		}
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, errors.New("mcp: a PEM block of the public key file is not a PKIX public key")
		}
		pub, ok := k.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() {
			return nil, errors.New("mcp: the public key is not an EC key on P-256")
		}
		kid, err := keyID(pub)
		if err != nil {
			return nil, err
		}
		v.keys[kid] = pub
	}
	if len(v.keys) == 0 {
		return nil, errors.New("mcp: no public key found: without the gateway's key no caller can be verified")
	}
	return v, nil
}

// KeyIDs are the thumbprints of the keys this verifier holds, sorted, for the operator's log.
func (v *Verifier) KeyIDs() []string {
	out := make([]string, 0, len(v.keys))
	for k := range v.keys {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// keyID is the RFC 7638 thumbprint of a P-256 key, as the gateway computes it (idp-mcp's KeyID).
func keyID(pub *ecdsa.PublicKey) (string, error) {
	raw, err := pub.Bytes() // 0x04 || X || Y
	if err != nil || len(raw) != 65 {
		return "", errors.New("mcp: not an uncompressed P-256 point")
	}
	enc := base64.RawURLEncoding.EncodeToString
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + enc(raw[1:33]) + `","y":"` + enc(raw[33:]) + `"}`))
	return enc(sum[:]), nil
}

// Canonical is the canonical JSON of idp-mcp's release.Canonical, copied: keys sorted, no insignificant whitespace, no
// HTML escaping, numbers as written. It is not RFC 8785.
func Canonical(v any) ([]byte, error) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		var err error
		if raw, err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Digest is "sha256:<hex>" of the canonical JSON of v.
func Digest(v any) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// requestDigest is what the gateway signed about the request: the method and the params without _meta.
func requestDigest(method string, params map[string]json.RawMessage) (string, error) {
	p := make(map[string]json.RawMessage, len(params))
	for k, v := range params {
		if k != "_meta" {
			p[k] = v
		}
	}
	return Digest(map[string]any{"method": method, "params": p})
}

type assertionClaims struct {
	Issuer   string   `json:"iss"`
	Audience any      `json:"aud"`
	Issued   int64    `json:"iat"`
	Expires  int64    `json:"exp"`
	ID       string   `json:"jti"`
	Subject  string   `json:"sub"`
	Tenant   string   `json:"tenant_id"`
	Roles    []string `json:"roles"`
	Client   string   `json:"client"`
	Actor    string   `json:"actor"`
	Request  string   `json:"req"`
}

func (c assertionClaims) forAudience(aud string) bool {
	switch a := c.Audience.(type) {
	case string:
		return a == aud
	case []any:
		for _, x := range a {
			if s, _ := x.(string); s == aud {
				return true
			}
		}
	}
	return false
}

// Verify returns the caller an assertion names, if the gateway signed it for this server and for this request, it has
// not expired, and it was not presented before. The error says why not, for the log: it is never given to the caller.
func (v *Verifier) Verify(assertion, method string, params map[string]json.RawMessage) (Caller, error) {
	if assertion == "" {
		return Caller{}, errors.New("no assertion")
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 || len(assertion) > maxAssertion {
		return Caller{}, errors.New("not a compact JWS")
	}
	dec := base64.RawURLEncoding.DecodeString
	hraw, err1 := dec(parts[0])
	craw, err2 := dec(parts[1])
	sig, err3 := dec(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return Caller{}, errors.New("not base64url")
	}
	var h struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hraw, &h) != nil || h.Alg != "ES256" || h.Typ != assertionType {
		return Caller{}, fmt.Errorf("header alg %q typ %q", h.Alg, h.Typ)
	}
	key := v.keys[h.Kid]
	if key == nil {
		return Caller{}, fmt.Errorf("signed with a key this server does not hold (kid %q)", h.Kid)
	}
	if len(sig) != 64 {
		return Caller{}, errors.New("signature length")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return Caller{}, errors.New("signature does not verify")
	}
	var c assertionClaims
	if json.Unmarshal(craw, &c) != nil {
		return Caller{}, errors.New("claims are not an object")
	}
	now := v.now()
	switch {
	case !c.forAudience(v.audience):
		return Caller{}, fmt.Errorf("made for %v, not for %s", c.Audience, v.audience)
	case c.Expires == 0 || now.After(time.Unix(c.Expires, 0).Add(assertionSkew)):
		return Caller{}, errors.New("expired")
	case c.Issued == 0 || time.Unix(c.Issued, 0).After(now.Add(assertionSkew)):
		return Caller{}, errors.New("issued in the future")
	case c.Expires-c.Issued > int64(assertionMaxT/time.Second) || c.Expires < c.Issued:
		return Caller{}, errors.New("valid for too long")
	case c.Subject == "" || c.ID == "":
		return Caller{}, errors.New("no subject or id")
	}
	want, err := requestDigest(method, params)
	if err != nil || c.Request != want {
		return Caller{}, errors.New("made for another request")
	}
	// The issuer is judged after the rest, so that a refusal keeps the reason of the rule that fails first in the
	// platform's vectors; the id is recorded only when every check has passed.
	if c.Issuer != v.issuer {
		return Caller{}, fmt.Errorf("issued by %q, not by %q", c.Issuer, v.issuer)
	}
	if !v.first(c.ID, c.Expires+int64(assertionSkew/time.Second), now.Unix()) {
		return Caller{}, errors.New("presented before")
	}
	roles := c.Roles
	if roles == nil {
		roles = []string{}
	}
	return Caller{Subject: c.Subject, Tenant: c.Tenant, Roles: roles, Client: c.Client, Actor: c.Actor}, nil
}

// first records id until forget and reports whether it had not been seen. The table holds at most the assertions of the
// last minute and a half; it is cleared of expired ids whenever it has grown.
func (v *Verifier) first(id string, forget, now int64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, dup := v.seen[id]; dup {
		return false
	}
	if len(v.seen) >= 4096 {
		for k, t := range v.seen {
			if t < now {
				delete(v.seen, k)
			}
		}
	}
	v.seen[id] = forget
	return true
}
