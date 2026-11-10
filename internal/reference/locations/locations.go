// Package locations resolves location codes against the current edition of Cargo's location reference data
// in cargo_ref (decision F2, option 4; MD-26). It is read-only: editions are loaded by migrations or the
// reference importer, never by contexts.
package locations

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/reference/api"
)

var (
	unlocodeForm = regexp.MustCompile(`^[A-Z]{2}[A-Z2-9]{3}$`)
	iataForm     = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Directory reads cargo_ref on a transaction owner of its own, outside any context's transaction.
type Directory struct{ DB *bentopg.DB }

var _ api.Locations = Directory{}

// New returns a Directory on db.
func New(db *bentopg.DB) (Directory, error) {
	if db == nil {
		return Directory{}, errors.New("reference: locations need a database")
	}
	return Directory{DB: db}, nil
}

// ResolveLocation resolves a UN/LOCODE, an alias carriers use for the mode, or, for AIR, an IATA airport code.
// The location must serve the mode: AIR needs the airport function and one IATA airport code, SEA the port
// function; ROAD and RAIL take any known location. A UN/LOCODE that does not serve the mode resolves through
// an alias of the mode if the edition has one.
func (d Directory) ResolveLocation(ctx context.Context, q api.ResolveLocation) (api.Location, error) {
	switch q.Mode {
	case api.ModeAir, api.ModeSea, api.ModeRoad, api.ModeRail:
	default:
		return api.Location{}, fmt.Errorf("%w: mode %q", api.ErrInvalidQuery, q.Mode)
	}
	switch {
	case q.Scheme == api.SchemeUNLOCODE && unlocodeForm.MatchString(q.Code):
	case q.Scheme == api.SchemeIATA && iataForm.MatchString(q.Code):
		if q.Mode != api.ModeAir {
			return api.Location{}, fmt.Errorf("%w: IATA airport codes name locations for AIR only", api.ErrInvalidQuery)
		}
	default:
		return api.Location{}, fmt.Errorf("%w: %s code %q", api.ErrInvalidQuery, q.Scheme, q.Code)
	}
	var out api.Location
	err := d.DB.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		r, err := d.DB.ReadExecutor(txCtx)
		if err != nil {
			return err
		}
		var edition string
		if err = r.QueryRow(txCtx, `SELECT edition FROM cargo_ref.location_edition WHERE is_current`).Scan(&edition); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no current edition of the location reference data", api.ErrUnknownLocation)
		} else if err != nil {
			return err
		}
		if q.Scheme == api.SchemeIATA {
			var codes []string
			rows, err := r.Query(txCtx, `SELECT unlocode FROM cargo_ref.airport_code WHERE edition=$1 AND iata=$2`, edition, q.Code)
			if err != nil {
				return err
			}
			codes, err = pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			switch len(codes) {
			case 0:
				return fmt.Errorf("%w: IATA airport %s in edition %s; give the UN/LOCODE", api.ErrUnknownLocation, q.Code, edition)
			case 1:
				out, err = read(txCtx, r, edition, codes[0])
				out.Via = api.ViaIATA
				return err
			default:
				return fmt.Errorf("%w: IATA %s names %v; give the UN/LOCODE", api.ErrAmbiguousLocation, q.Code, codes)
			}
		}
		loc, err := read(txCtx, r, edition, q.Code)
		known := err == nil
		if err != nil && !errors.Is(err, api.ErrUnknownLocation) {
			return err
		}
		if known && serves(loc, q.Mode) {
			out, out.Via = loc, api.ViaUNLOCODE
			return nil
		}
		var target string
		err = r.QueryRow(txCtx, `SELECT unlocode FROM cargo_ref.location_alias WHERE edition=$1 AND mode=$2 AND alias=$3`,
			edition, q.Mode, q.Code).Scan(&target)
		switch {
		case err == nil:
			aliased, err := read(txCtx, r, edition, target)
			if err != nil {
				return err
			}
			if !serves(aliased, q.Mode) {
				return fmt.Errorf("%w: alias %s names %s, which does not serve %s", api.ErrNotForMode, q.Code, target, q.Mode)
			}
			out, out.Via = aliased, api.ViaAlias
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		case known:
			return fmt.Errorf("%w: %s (%s, functions %s) for %s", api.ErrNotForMode, loc.UNLocode, loc.Name, loc.Functions, q.Mode)
		}
		return fmt.Errorf("%w: %s in edition %s", api.ErrUnknownLocation, q.Code, edition)
	})
	return out, err
}

func read(ctx context.Context, r bentopg.Querier, edition, code string) (api.Location, error) {
	loc := api.Location{Edition: edition}
	var iata *string
	err := r.QueryRow(ctx, `SELECT l.unlocode,l.name,l.country_code,l.functions,a.iata FROM cargo_ref.location l
        LEFT JOIN cargo_ref.airport_code a ON a.edition=l.edition AND a.unlocode=l.unlocode
        WHERE l.edition=$1 AND l.unlocode=$2`, edition, code).Scan(&loc.UNLocode, &loc.Name, &loc.CountryCode, &loc.Functions, &iata)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Location{}, fmt.Errorf("%w: %s in edition %s", api.ErrUnknownLocation, code, edition)
	}
	if err != nil {
		return api.Location{}, err
	}
	if iata != nil {
		loc.IATAAirport = *iata
	}
	return loc, nil
}

// serves reports whether a location serves a transport mode (F2): AIR needs the airport function and one IATA
// airport code, SEA the port function.
func serves(l api.Location, mode string) bool {
	switch mode {
	case api.ModeAir:
		return len(l.Functions) == 8 && l.Functions[3] == '4' && l.IATAAirport != ""
	case api.ModeSea:
		return len(l.Functions) == 8 && l.Functions[0] == '1'
	}
	return true
}
