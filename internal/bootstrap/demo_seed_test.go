package bootstrap

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// The seed writes ORG's master data directly, so it writes test tenants only; it refuses another before it touches the
// database.
func TestTheDemoSeedRefusesATenantThatIsNotATestTenant(t *testing.T) {
	for _, tenant := range []string{"acme", "", "prod-eii-test", "eii", "demo"} {
		err := SeedDemoTenant(context.Background(), "postgres://nobody@127.0.0.1:1/none", tenant, time.Now(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "eii- or demo-") || !strings.Contains(err.Error(), "test tenants only") {
			t.Errorf("tenant %q: %v", tenant, err)
		}
	}
}
