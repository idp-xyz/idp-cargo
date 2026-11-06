//go:build integration

package bootstrap

import (
	"context"
	"os"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// allowAll authorizes every ORG and RATE action.
type allowAll struct{}

func (allowAll) AuthorizeRoleApproval(context.Context, string, string, string, string) error {
	return nil
}

func (allowAll) AuthorizeQuoteAction(context.Context, string, string, string, string) error {
	return nil
}

type marginFloor int

func (f marginFloor) MarginFloorBP(context.Context, string, string) (int, error) { return int(f), nil }

func usd(minor int64) kernel.Money { return kernel.Money{Currency: "USD", Minor: minor} }

func uniqueTenant(prefix string) string {
	return prefix + time.Now().UTC().Format("20060102T150405.000000000")
}

// openPlatform connects as the PostgreSQL 16 application role. CI must never skip these tests.
func openPlatform(t *testing.T) *Platform {
	t.Helper()
	dsn := os.Getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_POSTGRES_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_POSTGRES_DSN to a PostgreSQL 16 application role")
	}
	platform, err := Open(context.Background(), Config{PostgresDSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	return platform
}

// seedORG writes active parties and the verified legal entity "LE" as fixture rows, on an owner of its own.
func seedORG(t *testing.T, p *Platform, tenant string, parties ...string) {
	t.Helper()
	fixture, err := p.newDB()
	if err != nil {
		t.Fatal(err)
	}
	err = (bento.TenantTransactor{DB: fixture, Tenant: tenant}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
		e, err := fixture.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		for _, party := range parties {
			if _, err = e.Exec(txCtx, `INSERT INTO cargo_org.business_party(tenant_id,party_id,legal_name,registration_country,status)
                VALUES($1,$2,'Fixture Corp','US','ACTIVE')`, tenant, party); err != nil {
				return err
			}
		}
		_, err = e.Exec(txCtx, `INSERT INTO cargo_org.legal_entity(tenant_id,legal_entity_id,registered_name,jurisdiction,registration_evidence_ref,status)
            VALUES($1,'LE','Fixture Corp','US','registry-verified','VERIFIED')`, tenant)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
