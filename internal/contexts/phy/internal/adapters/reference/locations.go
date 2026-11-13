// Package reference is PHY's anticorruption layer for Cargo's location reference data (decision F2, option
// 4) and its equipment reference data (PB8). The reference data's error kinds are kept as text only.
package reference

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

// Locations resolves consolidation locations through internal/reference/api. Call it outside PHY's
// transactions.
type Locations struct{ Ref refapi.Locations }

var _ app.Locations = Locations{}

func (l Locations) Resolve(ctx context.Context, mode phy.Mode, code kernel.LocationCode) (app.ResolvedLocation, error) {
	scheme := refapi.SchemeUNLOCODE
	if code.Scheme == kernel.IATAAirport {
		scheme = refapi.SchemeIATA
	}
	loc, err := l.Ref.ResolveLocation(ctx, refapi.ResolveLocation{Scheme: scheme, Code: code.Code, Mode: string(mode)})
	if errors.Is(err, refapi.ErrUnknownLocation) || errors.Is(err, refapi.ErrAmbiguousLocation) || errors.Is(err, refapi.ErrNotForMode) ||
		errors.Is(err, refapi.ErrInvalidQuery) {
		return app.ResolvedLocation{}, fmt.Errorf("%w: %s %s for %s: %v", app.ErrLocationRefused, code.Scheme, code.Code, mode, err.Error())
	}
	if err != nil {
		return app.ResolvedLocation{}, err
	}
	return app.ResolvedLocation{Code: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: loc.UNLocode}, IATA: loc.IATAAirport, Edition: loc.Edition}, nil
}
