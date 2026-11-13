// Package identity carries the verified caller of an inbound request. A Verifier turns a bearer token into a
// Principal; the transport puts the Principal on the request context, and authorization and the contexts' inbound
// adapters read it from there. Tenant and actor come from the Principal, never from a request body.
//
// HS256Verifier is a development and test verifier: a token signed with a shared key. The production verifier for
// the IDP identity platform (its issuer, keys and claims) plugs in through the same interface once those are known.
//
// Authorize and Require decide a context's actions from the principal's permissions, standing in for the IDP policy
// service until it is connected: a permission is "<context>:<Action>", "<context>:*" or "*", and the principal must
// be the tenant and actor a command names. (Foundation packages import no other, so the decision lives beside the
// principal it reads.)
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ErrInvalidToken is any token a Verifier refuses; the cause stays out of responses.
var ErrInvalidToken = errors.New("identity: invalid token")

// Principal is a verified caller: the tenant it acts in, the actor (subject) and the permissions it was granted,
// each "<context>:<Action>", "<context>:*" or "*".
type Principal struct {
	TenantID    string
	ActorID     string
	Permissions []string
	ExpiresAt   time.Time
}

// Verifier verifies a bearer token.
type Verifier interface {
	Verify(ctx context.Context, token string) (Principal, error)
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom answers the principal on ctx, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// identifiers: tenant and actor ids as the contexts take them (no blanks or control characters, 1 to 128 bytes).
var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)

// permission: "*", "<context>:*" or "<context>:<Action>".
var permission = regexp.MustCompile(`^(\*|[a-z]{2,8}:(\*|[A-Z][A-Za-z]{1,63}))$`)

// Claims is the payload of a token HS256Verifier accepts.
type Claims struct {
	Issuer      string   `json:"iss"`
	Audience    string   `json:"aud"`
	Subject     string   `json:"sub"`
	TenantID    string   `json:"tenant_id"`
	Permissions []string `json:"permissions"`
	IssuedAt    int64    `json:"iat"`
	NotBefore   int64    `json:"nbf,omitempty"`
	ExpiresAt   int64    `json:"exp"`
}

// HS256Verifier verifies compact JWS tokens signed HMAC-SHA256 with Key (at least 32 bytes), from Issuer for
// Audience, valid now (with Leeway for clock skew) and for at most MaxLifetime from their issue.
type HS256Verifier struct {
	Key         []byte
	Issuer      string
	Audience    string
	Leeway      time.Duration
	MaxLifetime time.Duration
	Now         func() time.Time
}

const minKeyBytes = 32

var b64 = base64.RawURLEncoding

// Verify checks the token's header, signature, issuer, audience, times, tenant, subject and permissions.
func (v HS256Verifier) Verify(_ context.Context, token string) (Principal, error) {
	fail := func(format string, args ...any) (Principal, error) {
		return Principal{}, fmt.Errorf("%w: %s", ErrInvalidToken, fmt.Sprintf(format, args...))
	}
	if len(v.Key) < minKeyBytes || v.Issuer == "" || v.Audience == "" {
		return fail("the verifier needs a key of %d bytes or more, an issuer and an audience", minKeyBytes)
	}
	if len(token) > 8192 {
		return fail("too long")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fail("not a compact JWS")
	}
	var header struct {
		Alg  string `json:"alg"`
		Typ  string `json:"typ"`
		Crit any    `json:"crit"`
	}
	if err := decodePart(parts[0], &header); err != nil {
		return fail("header: %v", err)
	}
	if header.Alg != "HS256" || (header.Typ != "" && header.Typ != "JWT") || header.Crit != nil {
		return fail("algorithm %q, type %q", header.Alg, header.Typ)
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return fail("signature encoding")
	}
	mac := hmac.New(sha256.New, v.Key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return fail("signature")
	}
	var c Claims
	if err = decodePart(parts[1], &c); err != nil {
		return fail("claims: %v", err)
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	t, leeway := now(), v.Leeway
	maxLifetime := v.MaxLifetime
	if maxLifetime == 0 {
		maxLifetime = 24 * time.Hour
	}
	exp, iat, nbf := time.Unix(c.ExpiresAt, 0), time.Unix(c.IssuedAt, 0), time.Unix(c.NotBefore, 0)
	switch {
	case c.Issuer != v.Issuer || c.Audience != v.Audience:
		return fail("issuer or audience")
	case c.ExpiresAt == 0 || c.IssuedAt == 0 || !t.Before(exp.Add(leeway)) || t.Add(leeway).Before(iat) || (c.NotBefore != 0 && t.Add(leeway).Before(nbf)):
		return fail("not valid now")
	case exp.Sub(iat) > maxLifetime || !exp.After(iat):
		return fail("lifetime")
	case !identifier.MatchString(c.TenantID) || !identifier.MatchString(c.Subject):
		return fail("tenant or subject")
	}
	for _, p := range c.Permissions {
		if !permission.MatchString(p) {
			return fail("permission %q", p)
		}
	}
	return Principal{TenantID: c.TenantID, ActorID: c.Subject, Permissions: slices.Clone(c.Permissions), ExpiresAt: exp.UTC()}, nil
}

func decodePart(part string, v any) error {
	raw, err := b64.DecodeString(part)
	if err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	if err = d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

// SignHS256 signs claims for HS256Verifier: for development and tests, never for production identities.
func SignHS256(key []byte, c Claims) (string, error) {
	if len(key) < minKeyBytes {
		return "", fmt.Errorf("identity: a signing key has %d bytes or more", minKeyBytes)
	}
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signing))
	return signing + "." + b64.EncodeToString(mac.Sum(nil)), nil
}

// ErrDenied is every refusal; the contexts' authorizer ports wrap it as unauthorized.
var ErrDenied = errors.New("identity: denied")

// ActionRead is the action of a context's queries: "<context>:Read".
const ActionRead = "Read"

// Allowed reports whether p holds the permission for action in the bounded context bc.
func Allowed(p Principal, bc, action string) bool {
	return slices.Contains(p.Permissions, "*") || slices.Contains(p.Permissions, bc+":*") ||
		slices.Contains(p.Permissions, bc+":"+action)
}

// Authorize lets the principal on ctx perform action in the bounded context bc as tenant and actor: the principal
// must be that tenant and actor and hold the permission.
func Authorize(ctx context.Context, bc, tenantID, actorID, action string) error {
	p, ok := PrincipalFrom(ctx)
	switch {
	case !ok:
		return fmt.Errorf("%w: no verified principal", ErrDenied)
	case p.TenantID != tenantID || p.ActorID != actorID:
		return fmt.Errorf("%w: the principal acts for another tenant or actor", ErrDenied)
	case !Allowed(p, bc, action):
		return fmt.Errorf("%w: %s:%s", ErrDenied, bc, action)
	}
	return nil
}

// Require is Authorize for the principal's own tenant and actor, as queries ask it.
func Require(ctx context.Context, bc, action string) (Principal, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return Principal{}, fmt.Errorf("%w: no verified principal", ErrDenied)
	}
	if !Allowed(p, bc, action) {
		return Principal{}, fmt.Errorf("%w: %s:%s", ErrDenied, bc, action)
	}
	return p, nil
}
