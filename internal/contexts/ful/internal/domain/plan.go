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

// CargoScope is a quantity of cargo: packs, gross weight and volume. A part is the scope of one obligation
// a shipment plans (F1); a plan version's scope is the sum of the parts it plans. The shipment does not
// override what COM committed (C4): a part only divides it.
type CargoScope struct {
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity // zero when the obligations declare none
}

// empty reports whether the scope holds nothing, in no unit.
func (s CargoScope) empty() bool { return s == CargoScope{} }

// scopeOf sums parts, in the first one's units.
func scopeOf(parts []CargoScope) (CargoScope, error) {
	var scope CargoScope
	for _, p := range parts {
		var err error
		if scope, err = scope.add(p, 1); err != nil {
			return CargoScope{}, err
		}
	}
	return scope, nil
}

// add returns s plus or minus another scope; adding an empty scope changes nothing, and an empty s takes
// o's units. Gross weights must share a unit, and volumes too: cargo with a volume and cargo without do not
// mix in one sum.
func (s CargoScope) add(o CargoScope, sign int64) (CargoScope, error) {
	if o.empty() {
		return s, nil
	}
	if s.empty() {
		s.GrossWeight.Unit, s.Volume.Unit = o.GrossWeight.Unit, o.Volume.Unit
	}
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
	PlanPartChanged        PlanChange = "PART_CHANGED" // F1: the planned part of an obligation changed, with a reason
)

func (c PlanChange) valid() bool {
	switch c {
	case PlanCreated, PlanObligationAdded, PlanAmended, PlanObligationReleased, PlanPartChanged:
		return true
	}
	return false
}

// needsReason reports whether a plan version of this change must say why.
func (c PlanChange) needsReason() bool { return c == PlanAmended || c == PlanPartChanged }

// PlanVersion is one immutable version of the shipment plan: the route, the cargo scope of the parts
// planned at that version, what changed and, for an amendment or a changed part, why. Every change adds a
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
