package main

import (
	"fmt"
	"strings"
	"testing"
)

const mod = "go.idp.xyz/idp-cargo"

func TestClassify(t *testing.T) {
	for path, want := range map[string]class{
		"cmd/cargo-worker":                        {layer: layerEntry},
		"internal/bootstrap":                      {layer: layerBootstrap},
		"internal/kernel":                         {layer: layerKernel},
		"internal/foundation/messaging":           {layer: layerFoundation},
		"internal/transport/http":                 {layer: layerInbound},
		"internal/reference/api":                  {layer: layerReference, part: partAPI},
		"internal/contexts/org":                   {layer: layerContext, context: "org", part: partModule},
		"internal/contexts/org/api":               {layer: layerContext, context: "org", part: partAPI},
		"internal/contexts/org/internal/domain":   {layer: layerContext, context: "org", part: partDomain},
		"internal/contexts/org/internal/domain/x": {layer: layerContext, context: "org", part: partDomain},
		"internal/contexts/org/internal/adapters": {layer: layerContext, context: "org", part: partOther},
		"internal/contexts/org/helpers":           {layer: layerContext, context: "org", part: partStray},
		"internal/common":                         {layer: layerUnknown},
		"tools/dependency-check":                  {layer: layerTool},
	} {
		if got := classify(mod, mod+"/"+path); got != want {
			t.Errorf("classify(%s) = %+v, want %+v", path, got, want)
		}
	}
}

func TestImportRules(t *testing.T) {
	allow := map[[2]string]bool{{"com", "org"}: true}
	for _, tc := range []struct {
		from, imp, rule string
	}{
		{"internal/contexts/org/api", "time", ""},
		{"internal/contexts/org/api", mod + "/internal/kernel", ""},
		{"internal/contexts/org/api", "go.idp.xyz/idp-bento-go/eventing", "D2"},
		{"internal/contexts/org/api", mod + "/internal/contexts/org/internal/domain", "D2"},
		{"internal/contexts/org/internal/domain", mod + "/internal/kernel", ""},
		{"internal/contexts/org/internal/domain", mod + "/internal/contexts/org/internal/application", "domain"},
		{"internal/contexts/org/internal/domain", "github.com/jackc/pgx/v5", "domain"},
		{"internal/contexts/org/internal/adapters/postgres", "github.com/jackc/pgx/v5", ""},
		{"internal/contexts/org/internal/adapters/postgres", mod + "/internal/foundation/bento", ""},
		{"internal/contexts/com/internal/adapters/org", mod + "/internal/contexts/org/api", ""},
		{"internal/contexts/rate/internal/application", mod + "/internal/contexts/org/api", "D1"},
		{"internal/contexts/com/internal/application", mod + "/internal/contexts/org/internal/domain", "D1"},
		{"internal/contexts/com", mod + "/internal/contexts/org", "D1"},
		{"internal/contexts/org", mod + "/internal/bootstrap", "D4"},
		{"internal/contexts/org/internal/adapters/http", mod + "/internal/transport/http", "D4"},
		{"internal/foundation/bento", mod + "/internal/kernel", ""},
		{"internal/foundation/bento", mod + "/internal/contexts/org/api", "D5"},
		{"internal/foundation/bento", mod + "/internal/foundation/messaging", "D5"},
		{"internal/kernel", "time", ""},
		{"internal/kernel", "github.com/shopspring/decimal", "kernel"},
		{"internal/kernel", mod + "/internal/foundation/bento", "kernel"},
		{"internal/transport/http", mod + "/internal/contexts/org/api", ""},
		{"internal/transport/http", mod + "/internal/contexts/org", "D6"},
		{"internal/transport/http", mod + "/internal/contexts/org/internal/domain", "D6"},
		{"cmd/cargo-worker", mod + "/internal/bootstrap", ""},
		{"cmd/cargo-worker", mod + "/internal/foundation/messaging", "entry"},
		{"cmd/cargo-worker", "github.com/jackc/pgx/v5", ""},
		{"internal/bootstrap", mod + "/internal/contexts/org/internal/domain", ""},
	} {
		f, bad := judge(mod, classify(mod, mod+"/"+tc.from), tc.imp, allow)
		if got := map[bool]string{true: f.rule}[bad]; got != tc.rule {
			t.Errorf("%s -> %s: rule %q (%s), want %q", tc.from, tc.imp, got, f.detail, tc.rule)
		}
	}
}

func TestPackagesOutsideTheLayersAreReported(t *testing.T) {
	found := checkImports(mod, []listedPackage{
		{ImportPath: mod + "/internal/common"},
		{ImportPath: mod + "/internal/contexts/org/helpers"},
		{ImportPath: mod + "/internal/contexts/org/api", Imports: []string{"time"}, XTestImports: []string{mod + "/internal/contexts/org/api"}},
	}, nil)
	if got := fmt.Sprint(found); !strings.Contains(got, "layer internal/common") || !strings.Contains(got, "layout internal/contexts/org/helpers") || len(found) != 2 {
		t.Fatalf("findings = %v", found)
	}
}

func TestGeneratedAllowlistCoversPresentContextsOnly(t *testing.T) {
	var doc contextMapDoc
	for _, r := range [][2]string{{"ORG", "COM"}, {"RATE", "COM"}, {"FUL", "PHY"}, {"PHY", "FUL"}, {"ORG", "FIN"}} {
		doc.Relationships = append(doc.Relationships, struct {
			From string `json:"frm"`
			To   string `json:"to"`
		}{r[0], r[1]})
	}
	lines := generate(doc, []string{"org", "com", "ful", "phy"})
	if got := strings.Join(lines, ";"); got != "com org;ful phy;phy ful" {
		t.Fatalf("allowlist = %q", got)
	}
	allow, err := parseAllowlist("# header\n" + strings.Join(lines, "\n") + "\n")
	if err != nil || !allow[[2]string{"com", "org"}] || allow[[2]string{"org", "com"}] {
		t.Fatalf("parsed allowlist = %v, %v", allow, err)
	}
	if _, err = parseAllowlist("com org rate\n"); err == nil {
		t.Fatal("a malformed line must be rejected")
	}
}

func TestCodeownersMustNameEveryContextAndTheKernel(t *testing.T) {
	required := []string{"internal/contexts/org/", "internal/kernel/"}
	for name, tc := range map[string]struct {
		text string
		want int
	}{
		"both named":           {"*  @idpxyz\n/internal/contexts/org/ @idpxyz\ninternal/kernel/** @idpxyz\n", 0},
		"catch-all only":       {"* @idpxyz\n", 2},
		"kernel without owner": {"/internal/contexts/org/ @idpxyz\n/internal/kernel/\n", 1},
		"missing file":         {"", 2},
	} {
		if got := checkCodeowners(tc.text, required); len(got) != tc.want {
			t.Errorf("%s: %d findings %v, want %d", name, len(got), got, tc.want)
		}
	}
}
