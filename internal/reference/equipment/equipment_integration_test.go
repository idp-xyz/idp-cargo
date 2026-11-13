//go:build integration

package equipment

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/reference/api"
)

func directory(t *testing.T) (Directory, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_POSTGRES_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_POSTGRES_DSN to a PostgreSQL 16 application role")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db, err := bentopg.NewDB(pool)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return d, pool
}

// Backlog PB8, against edition EQUIPMENT-2026-11-S1 (migration ref/000004).
func TestResolveContainerTypesOfS1(t *testing.T) {
	d, pool := directory(t)
	ctx := context.Background()
	for code, want := range map[string]api.ContainerType{
		"22G1": {Code: "22G1", TEUTenths: 10, MaxGrossKg: 30480, TareKg: 2250, CapacityCM3: 33200000, Edition: "EQUIPMENT-2026-11-S1"},
		"42G1": {Code: "42G1", TEUTenths: 20, MaxGrossKg: 30480, TareKg: 3750, CapacityCM3: 67700000, Edition: "EQUIPMENT-2026-11-S1"},
		"45G1": {Code: "45G1", TEUTenths: 20, MaxGrossKg: 30480, TareKg: 3900, CapacityCM3: 76300000, Edition: "EQUIPMENT-2026-11-S1"},
	} {
		got, err := d.ResolveContainerType(ctx, code)
		if err != nil || got.Description == "" {
			t.Fatalf("%s: %+v, %v", code, got, err)
		}
		got.Description = ""
		if got != want {
			t.Errorf("%s = %+v, want %+v", code, got, want)
		}
	}
	for _, code := range []string{"22R1", "40HC", "22g1", "", "22G1 "} {
		if got, err := d.ResolveContainerType(ctx, code); !errors.Is(err, api.ErrUnknownEquipment) {
			t.Errorf("%q = %+v, %v; want ErrUnknownEquipment", code, got, err)
		}
	}
	// The application reads the reference data and never writes it.
	_, err := pool.Exec(ctx, `UPDATE cargo_ref.container_size_type SET tare_kg = 1 WHERE code = '22G1'`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != "42501" && pgErr.Code != "23001") {
		t.Fatalf("rewriting an edition: %v", err)
	}
}
