package bootstrap

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/transport/mcp/mcptest"
)

const mcpAudience = "http://fin-mcp:8600/mcp"

// writeMCPConfig writes a good configuration and the two key files in a directory of its own and returns the path of
// the configuration; edit changes its text before it is written.
func writeMCPConfig(t *testing.T, edit func(text string) string) string {
	t.Helper()
	dir := t.TempDir()
	signer := mcptest.NewSigner(mcpAudience, "g1")
	if err := os.WriteFile(filepath.Join(dir, "gateway.pub.pem"), signer.PublicPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cursor.key"), []byte(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text := `{"audience":"` + mcpAudience + `","gateway_id":"g1","public_key_file":"gateway.pub.pem","cursor_key_file":"cursor.key",
		"tenants":{"eii-test":{"cargo_tenant":"eii-test","roles":{"finance-analyst":{"permissions":["fin:ReadProfit"],"legal_entities":["LE"]}}}},
		"limits":{"calls_per_minute":30,"concurrent":2}}`
	if edit != nil {
		text = edit(text)
	}
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The server does not start with a configuration that is not valid: nothing is guessed, and nothing turns the
// verification of the caller off.
func TestTheMCPConfigurationIsReadStrictly(t *testing.T) {
	cfg, err := LoadMCPConfig(writeMCPConfig(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	role := cfg.Tenants["eii-test"].Roles["finance-analyst"]
	if cfg.Audience != mcpAudience || cfg.GatewayID != "g1" || len(cfg.CursorKey) != 32 || len(cfg.PublicKeys) == 0 || cfg.Limits.CallsPerMinute != 30 || cfg.Limits.Concurrent != 2 ||
		cfg.Tenants["eii-test"].CargoTenant != "eii-test" || len(role.LegalEntities.IDs) != 1 || role.LegalEntities.IDs[0] != "LE" {
		t.Fatalf("%+v", cfg)
	}
	replace := func(from, to string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, from) {
				t.Fatalf("the configuration has no %q", from)
			}
			return strings.Replace(s, from, to, 1)
		}
	}
	for name, edit := range map[string]func(string) string{
		"an unknown key":                    replace(`"limits"`, `"verify":false,"limits"`),
		"a switch that turns the check off": replace(`"gateway_id":"g1"`, `"gateway_id":"g1","skip_verification":true`),
		"data after the object":             func(s string) string { return s + `{}` },
		"not JSON":                          func(string) string { return `audience: x` },
		"no audience":                       replace(`"audience":"`+mcpAudience+`"`, `"audience":""`),
		"an audience not ending in /mcp":    replace(mcpAudience, "http://fin-mcp:8600/"),
		"an audience that is not a URL":     replace(mcpAudience, "fin-mcp"),
		"an audience with a query":          replace(mcpAudience, mcpAudience+"?x=1"),
		"an audience of another scheme":     replace(mcpAudience, "ftp://fin-mcp/mcp"),
		"no gateway id":                     replace(`"gateway_id":"g1"`, `"gateway_id":""`),
		"a gateway id with a blank":         replace(`"gateway_id":"g1"`, `"gateway_id":"g 1"`),
		"no public key file":                replace(`"public_key_file":"gateway.pub.pem"`, `"public_key_file":""`),
		"a public key file that is absent":  replace("gateway.pub.pem", "absent.pem"),
		"a cursor key file that is absent":  replace("cursor.key", "absent.key"),
		"no tenant":                         replace(`"tenants":{"eii-test":{"cargo_tenant":"eii-test","roles":{"finance-analyst":{"permissions":["fin:ReadProfit"],"legal_entities":["LE"]}}}}`, `"tenants":{}`),
		"a write permission":                replace("fin:ReadProfit", "fin:RequestCostReview"),
		"every permission":                  replace("fin:ReadProfit", "*"),
		"legal entities none":               replace(`"legal_entities":["LE"]`, `"legal_entities":[]`),
		"legal entities of a number":        replace(`"legal_entities":["LE"]`, `"legal_entities":5`),
		"a rate over the largest":           replace(`"calls_per_minute":30`, `"calls_per_minute":6001`),
		"a negative rate":                   replace(`"calls_per_minute":30`, `"calls_per_minute":-1`),
		"too many at once":                  replace(`"concurrent":2`, `"concurrent":65`),
	} {
		if _, err := LoadMCPConfig(writeMCPConfig(t, edit)); err == nil {
			t.Errorf("%s: the configuration was accepted", name)
		}
	}
	if _, err := LoadMCPConfig(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("an absent file was accepted")
	}
}

// The files the configuration names: the public key must be P-256 PEM and the cursor key 32 bytes or more of base64.
func TestTheMCPKeyFilesAreChecked(t *testing.T) {
	for name, tc := range map[string]struct{ file, content string }{
		"no public key":                   {"gateway.pub.pem", ""},
		"a public key that is not PEM":    {"gateway.pub.pem", "not a key"},
		"a cursor key of 31 bytes":        {"cursor.key", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 31)))},
		"a cursor key that is not base64": {"cursor.key", "%%%"},
		"an empty cursor key":             {"cursor.key", ""},
	} {
		path := writeMCPConfig(t, nil)
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), tc.file), []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadMCPConfig(path); err == nil {
			t.Errorf("%s: the configuration was accepted", name)
		}
	}
	// an absolute path in the configuration is used as it is
	path := writeMCPConfig(t, nil)
	abs := filepath.Join(filepath.Dir(path), "cursor.key")
	text, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(text), `"cursor.key"`, `"`+abs+`"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMCPConfig(path); err != nil {
		t.Fatal(err)
	}
}
