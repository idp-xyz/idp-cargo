package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }

// cargo-api refuses to start without a token key of 32 bytes or more or a database, and mints development tokens
// only for what its verifier accepts.
func TestRun(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	for name, tc := range map[string]struct {
		args []string
		vars map[string]string
		want string
	}{
		"no key":          {nil, map[string]string{"IDP_CARGO_POSTGRES_DSN": "postgres://x"}, "IDP_CARGO_API_TOKEN_KEY"},
		"a short key":     {nil, map[string]string{"IDP_CARGO_API_TOKEN_KEY": base64.StdEncoding.EncodeToString([]byte("short"))}, "32 bytes"},
		"a key not b64":   {nil, map[string]string{"IDP_CARGO_API_TOKEN_KEY": "%%%"}, "base64"},
		"no database":     {nil, map[string]string{"IDP_CARGO_API_TOKEN_KEY": key}, "IDP_CARGO_POSTGRES_DSN"},
		"an unknown flag": {[]string{"-port", "1"}, map[string]string{"IDP_CARGO_API_TOKEN_KEY": key}, "flag provided but not defined"},
		"an argument":     {[]string{"serve"}, map[string]string{"IDP_CARGO_API_TOKEN_KEY": key}, "unexpected arguments"},
		"a token without a tenant": {[]string{"-mint-token", "-actor", "alice"}, map[string]string{"IDP_CARGO_API_TOKEN_KEY": key},
			"would not verify"},
		"a token living over a day": {[]string{"-mint-token", "-tenant", "t1", "-actor", "alice", "-ttl", "25h"},
			map[string]string{"IDP_CARGO_API_TOKEN_KEY": key}, "would not verify"},
		"a malformed permission": {[]string{"-mint-token", "-tenant", "t1", "-actor", "alice", "-permissions", "rate:read"},
			map[string]string{"IDP_CARGO_API_TOKEN_KEY": key}, "would not verify"},
	} {
		err := run(context.Background(), tc.args, env(tc.vars), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-mint-token", "-tenant", "t1", "-actor", "alice", "-permissions", "rate:Read,rate:MaintainChargeCodes"},
		env(map[string]string{"IDP_CARGO_API_TOKEN_KEY": key}), &out); err != nil || strings.Count(strings.TrimSpace(out.String()), ".") != 2 {
		t.Fatalf("a token: %q %v", out.String(), err)
	}
}
