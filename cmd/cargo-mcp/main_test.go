package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }

func config(t *testing.T, key []byte, cursor string) string {
	t.Helper()
	dir := t.TempDir()
	if key == nil {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		key = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	}
	for name, content := range map[string][]byte{"gateway.pub.pem": key, "cursor.key": []byte(cursor)} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	text := `{"audience":"http://fin-mcp:8600/mcp","gateway_id":"g1","public_key_file":"gateway.pub.pem","cursor_key_file":"cursor.key",
		"tenants":{"t":{"cargo_tenant":"t","roles":{"r":{"permissions":["fin:ReadProfit"],"legal_entities":["LE"]}}}}}`
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// cargo-mcp refuses to start without a configuration, a valid public key of the gateway and a cursor key of 32 bytes or
// more, and without a database; it has no flag that turns the verification of the caller off.
func TestRun(t *testing.T) {
	good := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))
	dsn := map[string]string{"IDP_CARGO_POSTGRES_DSN": "postgres://x"}
	for name, tc := range map[string]struct {
		args []string
		vars map[string]string
		want string
	}{
		"no configuration":             {nil, dsn, "-config is required"},
		"an unknown flag":              {[]string{"-config", "x", "-no-verify"}, dsn, "flag provided but not defined"},
		"an argument":                  {[]string{"-config", "x", "serve"}, dsn, "unexpected arguments"},
		"an absent configuration":      {[]string{"-config", filepath.Join(t.TempDir(), "absent.json")}, dsn, "no such file"},
		"a public key that is not PEM": {[]string{"-config", config(t, []byte("nope"), good)}, dsn, "PEM blocks"},
		"a short cursor key":           {[]string{"-config", config(t, nil, base64.StdEncoding.EncodeToString([]byte("short")))}, dsn, "32 bytes"},
		"no database":                  {[]string{"-config", config(t, nil, good)}, nil, "IDP_CARGO_POSTGRES_DSN"},
	} {
		err := run(context.Background(), tc.args, env(tc.vars))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
