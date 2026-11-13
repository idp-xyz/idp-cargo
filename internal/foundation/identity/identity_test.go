package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	key = []byte("0123456789abcdef0123456789abcdef")
	now = time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
)

func verifier() HS256Verifier {
	return HS256Verifier{Key: key, Issuer: "idp-cargo-dev", Audience: "idp-cargo-api", Leeway: time.Minute, Now: func() time.Time { return now }}
}

func claims() Claims {
	return Claims{Issuer: "idp-cargo-dev", Audience: "idp-cargo-api", Subject: "alice", TenantID: "t1", Permissions: []string{"rate:Read", "fin:*"},
		IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
}

func sign(t *testing.T, c Claims) string {
	t.Helper()
	token, err := SignHS256(key, c)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// A token signed with the key, from the issuer for the audience, valid now, names the tenant, actor and permissions;
// anything else is refused.
func TestHS256Verifier(t *testing.T) {
	p, err := verifier().Verify(context.Background(), sign(t, claims()))
	if err != nil || p.TenantID != "t1" || p.ActorID != "alice" || len(p.Permissions) != 2 || !p.ExpiresAt.Equal(now.Add(time.Hour).Truncate(time.Second)) {
		t.Fatalf("%+v %v", p, err)
	}
	for name, change := range map[string]func(*Claims){
		"another issuer":         func(c *Claims) { c.Issuer = "someone" },
		"another audience":       func(c *Claims) { c.Audience = "other-api" },
		"expired":                func(c *Claims) { c.ExpiresAt = now.Add(-2 * time.Minute).Unix() },
		"no expiry":              func(c *Claims) { c.ExpiresAt = 0 },
		"issued in the future":   func(c *Claims) { c.IssuedAt = now.Add(2 * time.Minute).Unix() },
		"not yet valid":          func(c *Claims) { c.NotBefore = now.Add(2 * time.Minute).Unix() },
		"living over a day":      func(c *Claims) { c.ExpiresAt = now.Add(25 * time.Hour).Unix() },
		"no tenant":              func(c *Claims) { c.TenantID = "" },
		"a tenant with a blank":  func(c *Claims) { c.TenantID = "t 1" },
		"no subject":             func(c *Claims) { c.Subject = "" },
		"a malformed permission": func(c *Claims) { c.Permissions = []string{"rate:read"} },
	} {
		c := claims()
		change(&c)
		if _, err := verifier().Verify(context.Background(), sign(t, c)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// within the leeway
	c := claims()
	c.ExpiresAt = now.Add(-30 * time.Second).Unix()
	if _, err := verifier().Verify(context.Background(), sign(t, c)); err != nil {
		t.Errorf("expired within the leeway: %v", err)
	}
	good := sign(t, claims())
	parts := strings.Split(good, ".")
	enc := base64.RawURLEncoding.EncodeToString
	other, _ := SignHS256([]byte("another key of thirty-two bytes!"), claims())
	for name, token := range map[string]string{
		"another key":         other,
		"alg none":            enc([]byte(`{"alg":"none"}`)) + "." + parts[1] + ".",
		"alg HS512":           enc([]byte(`{"alg":"HS512","typ":"JWT"}`)) + "." + parts[1] + "." + parts[2],
		"a critical header":   enc([]byte(`{"alg":"HS256","crit":["x"]}`)) + "." + parts[1] + "." + parts[2],
		"claims changed":      parts[0] + "." + enc([]byte(`{"iss":"idp-cargo-dev","aud":"idp-cargo-api","sub":"root","tenant_id":"t1"}`)) + "." + parts[2],
		"two parts":           parts[0] + "." + parts[1],
		"garbage":             "not a token",
		"a signature of junk": parts[0] + "." + parts[1] + ".%%%",
	} {
		if _, err := verifier().Verify(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	short := verifier()
	short.Key = []byte("short")
	if _, err := short.Verify(context.Background(), good); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a short key: %v", err)
	}
	if _, err := SignHS256([]byte("short"), claims()); err == nil {
		t.Error("signed with a short key")
	}
}

// Authorize lets the principal act as its own tenant and actor with a permission of the context; Require checks a
// query's permission.
func TestAuthorize(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{TenantID: "t1", ActorID: "alice", Permissions: []string{"rate:Read", "fin:*"}})
	for _, tc := range []struct {
		bc, tenant, actor, action string
		ok                        bool
	}{
		{"rate", "t1", "alice", "Read", true},
		{"fin", "t1", "alice", "IssueDebitNote", true},
		{"rate", "t1", "alice", "MaintainChargeCodes", false},
		{"rate", "t2", "alice", "Read", false},
		{"rate", "t1", "bob", "Read", false},
		{"org", "t1", "alice", "Read", false},
	} {
		if err := Authorize(ctx, tc.bc, tc.tenant, tc.actor, tc.action); (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrDenied)) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	if err := Authorize(context.Background(), "rate", "t1", "alice", "Read"); !errors.Is(err, ErrDenied) {
		t.Errorf("no principal: %v", err)
	}
	all := WithPrincipal(context.Background(), Principal{TenantID: "t1", ActorID: "alice", Permissions: []string{"*"}})
	if err := Authorize(all, "doc", "t1", "alice", "IssueTransportDocument"); err != nil {
		t.Errorf("every permission: %v", err)
	}
	if p, err := Require(ctx, "rate", ActionRead); err != nil || p.ActorID != "alice" {
		t.Errorf("a read: %+v %v", p, err)
	}
	if _, err := Require(ctx, "org", ActionRead); !errors.Is(err, ErrDenied) {
		t.Errorf("a read without permission: %v", err)
	}
}
