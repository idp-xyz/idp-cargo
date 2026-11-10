package application

import (
	"context"
	"errors"
	"fmt"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var ErrLocationRefused = errors.New("ful application: the location reference data does not resolve the location for the transport mode")

// ResolvedLocation is a route location resolved against the location reference data (decision F2, option 4):
// its UN/LOCODE, for AIR its IATA airport code, and the edition used.
type ResolvedLocation struct {
	Code    kernel.LocationCode
	IATA    string
	Edition string
}

// Locations resolves a UN/LOCODE, an alias carriers use, or for AIR an IATA airport code, for a transport mode.
// It is called outside FUL's transactions.
type Locations interface {
	Resolve(ctx context.Context, mode ful.TransportMode, l kernel.LocationCode) (ResolvedLocation, error)
}

// resolveRoute resolves the locations a route names, so that the plan version freezes the UN/LOCODEs, the
// IATA airport codes of an AIR route and the reference data edition. Locations need the transport mode.
func resolveRoute(ctx context.Context, locations Locations, r ful.Route) (ful.Route, error) {
	r.OriginIATA, r.DestinationIATA, r.LocationEdition = "", "", ""
	if r.Origin.IsZero() && r.Destination.IsZero() {
		return r, nil
	}
	if locations == nil {
		return ful.Route{}, fmt.Errorf("%w: FUL has no location reference data", ErrMissingDependency)
	}
	if !r.Mode.Valid() {
		return ful.Route{}, fmt.Errorf("%w: a transport mode is needed to resolve the route's locations", ful.ErrInvalidPlan)
	}
	editions := map[string]bool{}
	for _, end := range []struct {
		code *kernel.LocationCode
		iata *string
	}{{&r.Origin, &r.OriginIATA}, {&r.Destination, &r.DestinationIATA}} {
		if end.code.IsZero() {
			continue
		}
		resolved, err := locations.Resolve(ctx, r.Mode, *end.code)
		if err != nil {
			return ful.Route{}, err
		}
		*end.code = resolved.Code
		if r.Mode == ful.ModeAir {
			*end.iata = resolved.IATA
		}
		editions[resolved.Edition] = true
		r.LocationEdition = resolved.Edition
	}
	if len(editions) != 1 {
		return ful.Route{}, fmt.Errorf("%w: the reference data edition changed while the route was resolved; try again", ful.ErrConflict)
	}
	return r, nil
}

// RequestedModes asks COM which transport mode the order of an obligation version requests (C11), whatever the
// version's status or reservations; empty when the order requests none. It is called outside FUL's
// transactions.
type RequestedModes interface {
	RequestedMode(ctx context.Context, tenant ful.TenantID, id ful.ObligationID, version int64) (ful.TransportMode, error)
}

// requestedModes reads the obligations a shipment plans at the expected revision and asks COM what their
// orders request, so that an amended route's mode can be checked (C11). It checks modes only: whether COM
// still holds the obligations for the shipment is checked when the plan is confirmed, as before.
func requestedModes(ctx context.Context, d Deps, modes RequestedModes, id ful.ShipmentID, expected int64, s Scope) ([]ful.ObligationRef, error) {
	if modes == nil {
		return nil, ErrMissingDependency
	}
	var planned []ful.ObligationRef
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		shipment, revision, err := d.Shipments.Read(txCtx, s.TenantID, id)
		if err != nil {
			return err
		}
		if revision != expected {
			return ful.ErrConflict
		}
		planned = shipment.Planned()
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i, ob := range planned {
		if planned[i].RequestedMode, err = modes.RequestedMode(ctx, s.TenantID, ob.ObligationID, ob.ObligationVersion); err != nil {
			return nil, err
		}
	}
	return planned, nil
}
