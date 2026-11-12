// Package reference is RATE's anticorruption layer for the location reference data (decision F2, option 4).
package reference

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

// Locations resolves rate entry and evaluation locations. Call it outside RATE's transactions.
type Locations struct{ Ref refapi.Locations }

var _ app.Locations = Locations{}

// Resolve resolves a UN/LOCODE, or an alias carriers use, for the mode and answers the UN/LOCODE and the edition.
// The reference data's answers are kept as text only.
func (l Locations) Resolve(ctx context.Context, mode rate.Mode, code string) (string, string, error) {
	loc, err := l.Ref.ResolveLocation(ctx, refapi.ResolveLocation{Scheme: refapi.SchemeUNLOCODE, Code: code, Mode: string(mode)})
	if errors.Is(err, refapi.ErrUnknownLocation) || errors.Is(err, refapi.ErrAmbiguousLocation) || errors.Is(err, refapi.ErrNotForMode) ||
		errors.Is(err, refapi.ErrInvalidQuery) {
		return "", "", fmt.Errorf("%w: %s for %s: %v", app.ErrLocationRefused, code, mode, err.Error())
	}
	if err != nil {
		return "", "", err
	}
	return loc.UNLocode, loc.Edition, nil
}
