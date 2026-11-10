//go:build integration

package locations

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

func directory(t *testing.T) (Directory, *bentopg.DB) {
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
	return d, db
}

// Decision F2, option 4, against the S1 subset of the UNECE release of 2026-11-01 (migration ref/000002).
func TestResolveLocationsOfTheS1Subset(t *testing.T) {
	d, _ := directory(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		q              api.ResolveLocation
		unlocode, iata string
		via            string
	}{
		"an IATA airport code for AIR":               {api.ResolveLocation{Scheme: api.SchemeIATA, Code: "PVG", Mode: api.ModeAir}, "CNPVG", "PVG", api.ViaIATA},
		"an airport UN/LOCODE for AIR":               {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "USJFK", Mode: api.ModeAir}, "USJFK", "JFK", api.ViaUNLOCODE},
		"Hongqiao, which CNSHA now names, for AIR":   {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "CNSHA", Mode: api.ModeAir}, "CNSHA", "SHA", api.ViaUNLOCODE},
		"CNSHA for SEA, through the carriers' alias": {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "CNSHA", Mode: api.ModeSea}, "CNSHG", "", api.ViaAlias},
		"Ningbo port through its alias":              {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "CNNGB", Mode: api.ModeSea}, "CNNBG", "", api.ViaAlias},
		"a port for SEA":                             {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "DEHAM", Mode: api.ModeSea}, "DEHAM", "", api.ViaUNLOCODE},
		"a single-airport city for AIR":              {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "USLAX", Mode: api.ModeAir}, "USLAX", "LAX", api.ViaUNLOCODE},
		"any known location for ROAD":                {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "CNSGH", Mode: api.ModeRoad}, "CNSGH", "", api.ViaUNLOCODE},
	} {
		loc, err := d.ResolveLocation(ctx, tc.q)
		if err != nil || loc.UNLocode != tc.unlocode || (tc.iata != "" && loc.IATAAirport != tc.iata) || loc.Via != tc.via || loc.Edition != "UNLOCODE-2026-11-01-S1" {
			t.Errorf("%s: %+v %v", name, loc, err)
		}
	}
	for name, tc := range map[string]struct {
		q    api.ResolveLocation
		want error
	}{
		"New York, a city of several airports, for AIR": {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "USNYC", Mode: api.ModeAir}, api.ErrNotForMode},
		"Tokyo, a city of several airports, for AIR":    {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "JPTYO", Mode: api.ModeAir}, api.ErrNotForMode},
		"Chicago, whose IATA column reads MDW, for AIR": {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "USCHI", Mode: api.ModeAir}, api.ErrNotForMode},
		"Shanghai city for AIR":                         {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "CNSGH", Mode: api.ModeAir}, api.ErrNotForMode},
		"an IATA city code":                             {api.ResolveLocation{Scheme: api.SchemeIATA, Code: "NYC", Mode: api.ModeAir}, api.ErrUnknownLocation},
		"a port for AIR":                                {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "CNSHG", Mode: api.ModeAir}, api.ErrNotForMode},
		"an airport for SEA":                            {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "CNPVG", Mode: api.ModeSea}, api.ErrNotForMode},
		"an IATA code for SEA":                          {api.ResolveLocation{Scheme: api.SchemeIATA, Code: "PVG", Mode: api.ModeSea}, api.ErrInvalidQuery},
		"a code the subset does not hold":               {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "FRPAR", Mode: api.ModeAir}, api.ErrUnknownLocation},
		"a malformed code":                              {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "CN-SH", Mode: api.ModeSea}, api.ErrInvalidQuery},
		"an unknown mode":                               {api.ResolveLocation{Scheme: api.SchemeUNLOCODE, Code: "DEHAM", Mode: "BARGE"}, api.ErrInvalidQuery},
	} {
		if _, err := d.ResolveLocation(ctx, tc.q); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

// The application role reads reference data and cannot change it.
func TestReferenceDataIsReadOnlyForTheApplication(t *testing.T) {
	_, db := directory(t)
	ctx := context.Background()
	for _, sql := range []string{
		`INSERT INTO cargo_ref.airport_code (edition, iata, unlocode, source) VALUES ('UNLOCODE-2026-11-01-S1','PEK','CNSGH','x')`,
		`UPDATE cargo_ref.location SET name='Elsewhere' WHERE unlocode='CNSHG'`,
		`UPDATE cargo_ref.location_edition SET is_current=false`,
	} {
		err := db.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
			e, err := db.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			_, err = e.Exec(txCtx, sql)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: err = %v, want insufficient privilege", sql, err)
		}
	}
}
