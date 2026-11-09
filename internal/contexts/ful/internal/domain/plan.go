package domain

import (
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

type TransportMode string

// Transport modes of UN/ECE Recommendation 19 that S1 plans: one shipment, one mode.
const (
	ModeAir  TransportMode = "AIR"
	ModeSea  TransportMode = "SEA"
	ModeRoad TransportMode = "ROAD"
	ModeRail TransportMode = "RAIL"
)

func (m TransportMode) Valid() bool {
	switch m {
	case ModeAir, ModeSea, ModeRoad, ModeRail:
		return true
	}
	return false
}

// Route is the transport mode, the origin and destination UN/LOCODEs and the planned departure window.
// A draft may leave parts of it empty; a confirmed plan has all of them.
type Route struct {
	Mode        TransportMode
	Origin      kernel.LocationCode
	Destination kernel.LocationCode
	Departure   kernel.ValidityInterval
}

func (r Route) valid() bool {
	noWindow := r.Departure.From.IsZero() && r.Departure.Until.IsZero()
	return (r.Mode == "" || r.Mode.Valid()) && unlocode(r.Origin) && unlocode(r.Destination) &&
		(r.Origin.IsZero() || r.Origin != r.Destination) && (noWindow || r.Departure.Valid() && !r.Departure.Until.IsZero())
}

func unlocode(l kernel.LocationCode) bool {
	return l.IsZero() || l.Valid() && l.Scheme == kernel.UNLOCODE
}

// Complete reports whether the route names the mode, both locations and the departure window.
func (r Route) Complete() bool {
	return r.valid() && r.Mode != "" && !r.Origin.IsZero() && !r.Destination.IsZero() && !r.Departure.From.IsZero()
}

// Same reports whether two routes are equal, comparing instants rather than their representation.
func (r Route) Same(o Route) bool {
	return r.Mode == o.Mode && r.Origin == o.Origin && r.Destination == o.Destination &&
		r.Departure.From.Equal(o.Departure.From) && r.Departure.Until.Equal(o.Departure.Until)
}

// CargoScope is what a plan version moves: the sums of the packs, gross weights and volumes of the
// obligations it plans. The shipment does not override them in S1 (C4).
type CargoScope struct {
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity // zero when the obligations declare none
}

// scopeOf sums the measures of obligations, starting in the first one's units.
func scopeOf(obs []ObligationRef) (CargoScope, error) {
	if len(obs) == 0 {
		return CargoScope{}, nil
	}
	scope := CargoScope{GrossWeight: kernel.Quantity{Unit: obs[0].GrossWeight.Unit}, Volume: kernel.Quantity{Unit: obs[0].Volume.Unit}}
	for _, o := range obs {
		var err error
		if scope, err = scope.add(o, 1); err != nil {
			return CargoScope{}, err
		}
	}
	return scope, nil
}

// add returns s plus or minus an obligation's measure. Gross weights must share a unit, and volumes too;
// an obligation with a volume and one without do not mix in one shipment.
func (s CargoScope) add(o ObligationRef, sign int64) (CargoScope, error) {
	weight, err := s.GrossWeight.Add(kernel.Quantity{Unit: o.GrossWeight.Unit, Amount: sign * o.GrossWeight.Amount})
	if err != nil {
		return CargoScope{}, err
	}
	volume, err := s.Volume.Add(kernel.Quantity{Unit: o.Volume.Unit, Amount: sign * o.Volume.Amount})
	if err != nil {
		return CargoScope{}, err
	}
	return CargoScope{Packs: s.Packs + sign*o.Packs, GrossWeight: weight, Volume: volume}, nil
}

type PlanChange string

const (
	PlanCreated            PlanChange = "CREATED"
	PlanObligationAdded    PlanChange = "OBLIGATION_ADDED"
	PlanAmended            PlanChange = "AMENDED"
	PlanObligationReleased PlanChange = "OBLIGATION_RELEASED"
)

func (c PlanChange) valid() bool {
	switch c {
	case PlanCreated, PlanObligationAdded, PlanAmended, PlanObligationReleased:
		return true
	}
	return false
}

// PlanVersion is one immutable version of the shipment plan: the route, the cargo scope of the
// obligations planned at that version, what changed and, for an amendment, why. Every change adds a
// version, so the original plan stays visible (FUL-Rollover).
type PlanVersion struct {
	No     int
	Change PlanChange
	Route  Route
	Scope  CargoScope
	Reason string
	By     string
	At     time.Time
}

// Confirmation records who confirmed which plan version. A shipment is PLANNED while its latest plan
// version is confirmed.
type Confirmation struct {
	PlanNo int
	By     string
	At     time.Time
}
