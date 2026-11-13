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

// ContainerMode is how the shipment's cargo travels in its mode (decision N1-PHY OD-9, option A: requested on the COM
// order and planned in FUL): LCL or FCL on SEA, LOOSE on AIR; none on ROAD or RAIL yet. PHY's LCL consolidations
// refuse FCL houses.
type ContainerMode string

const (
	ContainerLCL   ContainerMode = "LCL"
	ContainerFCL   ContainerMode = "FCL"
	ContainerLoose ContainerMode = "LOOSE"
)

// For reports whether the container mode belongs to the transport mode (an empty one belongs to any).
func (c ContainerMode) For(m TransportMode) bool {
	switch c {
	case "":
		return true
	case ContainerLCL, ContainerFCL:
		return m == ModeSea
	case ContainerLoose:
		return m == ModeAir
	}
	return false
}

// Route is the transport mode, the origin and destination UN/LOCODEs and the planned departure window.
// A draft may leave parts of it empty; a confirmed plan has all of them. Since FUL V0.3 (decision F2, option
// 4) every location is resolved against the location reference data: the route freezes the UN/LOCODEs, for AIR
// the IATA airport codes, and the reference data edition used. Routes stored before V0.3 have neither.
type Route struct {
	Mode TransportMode
	// ContainerMode (N1-PHY OD-9) is left out of a command's request hash when unset, so commands recorded before it hash
	// as they did.
	ContainerMode   ContainerMode `json:",omitzero"`
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
	Departure       kernel.ValidityInterval
	OriginIATA      string
	DestinationIATA string
	LocationEdition string
}

func (r Route) valid() bool {
	noWindow := r.Departure.From.IsZero() && r.Departure.Until.IsZero()
	return (r.Mode == "" || r.Mode.Valid()) && (r.ContainerMode == "" || r.Mode != "" && r.ContainerMode.For(r.Mode)) &&
		unlocode(r.Origin) && unlocode(r.Destination) &&
		(r.Origin.IsZero() || r.Origin != r.Destination) && (noWindow || r.Departure.Valid() && !r.Departure.Until.IsZero()) &&
		iata(r.OriginIATA, r.Mode, r.Origin) && iata(r.DestinationIATA, r.Mode, r.Destination) && len(r.LocationEdition) <= 64
}

// iata checks an IATA airport code: three letters, only on an AIR route, only with a location.
func iata(code string, mode TransportMode, l kernel.LocationCode) bool {
	if code == "" {
		return true
	}
	return mode == ModeAir && !l.IsZero() && (kernel.LocationCode{Scheme: kernel.IATAAirport, Code: code}).Valid()
}

// Resolved reports whether every location of the route was resolved against the reference data (F2): an
// edition when it names a location, and on an AIR route the IATA airport code of each location.
func (r Route) Resolved() bool {
	if r.Origin.IsZero() && r.Destination.IsZero() {
		return true
	}
	if r.LocationEdition == "" {
		return false
	}
	if r.Mode == ModeAir {
		return (r.Origin.IsZero() || r.OriginIATA != "") && (r.Destination.IsZero() || r.DestinationIATA != "")
	}
	return true
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
	return r.Mode == o.Mode && r.ContainerMode == o.ContainerMode && r.Origin == o.Origin && r.Destination == o.Destination &&
		r.Departure.From.Equal(o.Departure.From) && r.Departure.Until.Equal(o.Departure.Until) &&
		r.OriginIATA == o.OriginIATA && r.DestinationIATA == o.DestinationIATA && r.LocationEdition == o.LocationEdition
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
	No        int
	Change    PlanChange
	Route     Route
	Scope     CargoScope
	Reason    string
	By        string
	At        time.Time
	EnteredAt time.Time // when FUL recorded it (T1); zero before FUL V0.3
}

// Confirmation records who confirmed which plan version. A shipment is PLANNED while its latest plan
// version is confirmed.
type Confirmation struct {
	PlanNo    int
	By        string
	At        time.Time
	EnteredAt time.Time // when FUL recorded it (T1); zero before FUL V0.3
}
