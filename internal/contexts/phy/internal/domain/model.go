// Package domain is the model of PHY (Physical Logistics) for consolidations and their houses (PHY V0.1, the
// first step of the N1 forwarder daily loop): a consolidation shared by houses of several customers, and each
// house's membership ledger on its main carriage. A consolidation never merges its houses' commercial or legal
// responsibilities (UL-05): each house stays a FUL shipment of one customer and one legal entity, attached
// whole. Membership is a plan-layer fact; it never asserts receipt or loading (DDD-010). PHY V0.2 adds the
// physical facts: cargo lots (receipt, measurement, screening, discrepancies) and tender load executions
// (lines, reversals, holds, carrier evidence), and reconciles a consolidation on them. Load units, stuffing and
// build-up come with SEA. It depends on the standard library and the kernel only.
package domain

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

type TenantID string
type ConsolidationID string
type ShipmentID string

// Mode is a consolidation's transport mode; it equals its houses' mode (C11).
type Mode string

const (
	ModeAir Mode = "AIR"
	ModeSea Mode = "SEA"
)

func (m Mode) Valid() bool { return m == ModeAir || m == ModeSea }

// Kind is a consolidation kind of the China export slice (decision N1-PHY, OD-6).
type Kind string

const (
	KindAirConsol    Kind = "AIR_CONSOL"
	KindSeaLCLOwn    Kind = "SEA_LCL_OWN"
	KindSeaLCLCoload Kind = "SEA_LCL_COLOAD"
)

// fits reports whether the kind is one of the mode's.
func (k Kind) fits(m Mode) bool {
	switch k {
	case KindAirConsol:
		return m == ModeAir
	case KindSeaLCLOwn, KindSeaLCLCoload:
		return m == ModeSea
	}
	return false
}

// State is about membership only; booked, departed and arrived are carrier facts recorded elsewhere (UL-17).
type State string

const (
	StateOpen       State = "OPEN"
	StateClosed     State = "CLOSED"
	StateReconciled State = "RECONCILED"
	StateCancelled  State = "CANCELLED"
)

// StageMain is the main carriage, the only membership stage in N1.
const StageMain = "MAIN"

var (
	ErrInvalidConsolidation = errors.New("phy: invalid consolidation")
	ErrInvalidRoute         = errors.New("phy: the route needs two different resolved locations, with IATA airport codes for AIR only")
	ErrInvalidSchedule      = errors.New("phy: the planned departure must lie after the business time and the house cut-off not after it")
	ErrInvalidTransition    = errors.New("phy: invalid state transition")
	ErrInvalidChange        = errors.New("phy: invalid change")
	ErrConflict             = errors.New("phy: concurrent revision conflict")
	ErrNotOpen              = errors.New("phy: the consolidation is not open")
	ErrHousesAttached       = errors.New("phy: houses are attached to the consolidation")
	ErrNoHouses             = errors.New("phy: no house is attached to the consolidation")
	ErrOverEnvelope         = errors.New("phy: PHY_ENVELOPE_EXCEEDED: the houses' planned totals exceed the planned load units' envelope")
	ErrInvalidIntent        = errors.New("phy: invalid load intent")
	ErrIntentNotPlanned     = errors.New("phy: FUL does not report the shipment PLANNED")
	ErrModeMismatch         = errors.New("phy: the shipment's transport mode differs from the consolidation's")
	ErrFCLHouse             = errors.New("phy: PHY_FCL_HOUSE: an LCL consolidation takes no FCL shipment (decision N1-PHY OD-9)")
	// ContainerFCL is the container mode of a shipment FUL plans as a full container (N1-PHY OD-9).
	ContainerFCL            = "FCL"
	ErrRouteMismatch        = errors.New("phy: the shipment's origin or destination differs from the consolidation's")
	ErrWindowMismatch       = errors.New("phy: the consolidation's planned departure lies outside the shipment's departure window")
	ErrOtherLegalEntity     = errors.New("phy: the shipment belongs to another legal entity than the one operating the consolidation")
	ErrInvalidMembership    = errors.New("phy: invalid membership")
	ErrAlreadyAttached      = errors.New("phy: the shipment is attached to a consolidation already")
	ErrNotAttached          = errors.New("phy: the shipment is not attached to the consolidation")
	ErrSameConsolidation    = errors.New("phy: a house is transferred to another consolidation")
	ErrInvalidReason        = errors.New("phy: invalid membership reason")
	ErrNothingToRefresh     = errors.New("phy: FUL reports the plan version PHY verified")
	ErrShipmentNotCancelled = errors.New("phy: FUL does not report the shipment CANCELLED")
	ErrMemberStale          = errors.New("phy: FUL no longer reports the house PLANNED at the plan version PHY verified; refresh it")
	ErrMemberNotAllocated   = errors.New("phy: FUL has not recorded the house's attachment to the consolidation")
)

// Route is a consolidation's origin and destination as UN/LOCODEs resolved against the location reference
// data, for AIR with their IATA airport codes, and the edition used (F2). An AIR route is airport to airport.
type Route struct {
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
	OriginIATA      string
	DestinationIATA string
	LocationEdition string
}

func (r Route) valid(m Mode) bool {
	locations := r.Origin.Scheme == kernel.UNLOCODE && r.Origin.Valid() && r.Destination.Scheme == kernel.UNLOCODE &&
		r.Destination.Valid() && r.Origin != r.Destination
	iata := func(code string) bool { return (kernel.LocationCode{Scheme: kernel.IATAAirport, Code: code}).Valid() }
	airports := r.OriginIATA == "" && r.DestinationIATA == ""
	if m == ModeAir {
		airports = iata(r.OriginIATA) && iata(r.DestinationIATA) && r.OriginIATA != r.DestinationIATA
	}
	return locations && airports && edition(r.LocationEdition)
}

// zero reports whether no location is given.
func (r Route) zero() bool { return r.Origin.IsZero() && r.Destination.IsZero() }

// sameLocations compares the locations, not the edition they were resolved with.
func (r Route) sameLocations(o Route) bool {
	return r.Origin == o.Origin && r.Destination == o.Destination && r.OriginIATA == o.OriginIATA && r.DestinationIATA == o.DestinationIATA
}

func edition(e string) bool {
	if e == "" || len(e) > 64 {
		return false
	}
	for i := 0; i < len(e); i++ {
		c := e[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && (i == 0 || (c != '.' && c != '_' && c != '-')) {
			return false
		}
	}
	return true
}

// CargoScope is packs, gross weight and volume as kernel quantities; volume is zero when the shipment
// declares none.
type CargoScope struct {
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity
}

func (s CargoScope) valid() bool {
	return s.Packs > 0 && s.GrossWeight.Unit.Valid() && s.GrossWeight.Amount > 0 &&
		(s.Volume == kernel.Quantity{} || s.Volume.Unit.Valid() && s.Volume.Amount > 0)
}

// Part is a part of a COM obligation version a house plans, as FUL reported it (F1).
type Part struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	PackageType       string
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
}

func (p Part) valid(scope CargoScope) bool {
	return identifier(p.ObligationID) && p.ObligationVersion > 0 && identifier(p.OrderID) && p.Packs > 0 && packageType(p.PackageType) &&
		p.GrossWeight.Unit == scope.GrossWeight.Unit && p.GrossWeight.Amount > 0 && p.Volume.Unit == scope.Volume.Unit &&
		(p.Volume.Unit == "") == (p.Volume.Amount == 0) && p.Volume.Amount >= 0
}

func packageType(code string) bool {
	if len(code) != 2 {
		return false
	}
	for i := range 2 {
		if (code[i] < 'A' || code[i] > 'Z') && (code[i] < '0' || code[i] > '9') {
			return false
		}
	}
	return true
}

// IntentStatus is a FUL shipment's plan status as LoadIntent v1 reports it.
type IntentStatus string

const (
	IntentDraft     IntentStatus = "DRAFT"
	IntentPlanned   IntentStatus = "PLANNED"
	IntentCancelled IntentStatus = "CANCELLED"
)

// LoadIntent is FUL's LoadIntent v1 for a shipment (relationship 7), read before PHY's transaction (T3): its
// revision and plan version, status, legal entity, mode, route, departure window, scope and parts, and the
// consolidation FUL recorded it attached to on the main carriage.
type LoadIntent struct {
	ShipmentID      ShipmentID
	Revision        int64
	Status          IntentStatus
	PlanVersion     int
	LegalEntityID   string
	Mode            Mode
	ContainerMode   string // as FUL plans it (N1-PHY OD-9): LCL or FCL on SEA, LOOSE on AIR; empty when not planned
	Route           Route
	DepartureWindow kernel.ValidityInterval
	Scope           CargoScope
	Parts           []Part
	ConsolidationID ConsolidationID
	EntryNo         int
}

// complete checks what a membership entry freezes: a scope that is the sum of its parts.
func (i LoadIntent) complete() bool {
	if !identifier(string(i.ShipmentID)) || i.Revision < 1 || i.PlanVersion < 1 || !i.Scope.valid() || len(i.Parts) == 0 {
		return false
	}
	sum := CargoScope{GrossWeight: kernel.Quantity{Unit: i.Scope.GrossWeight.Unit}, Volume: kernel.Quantity{Unit: i.Scope.Volume.Unit}}
	for _, p := range i.Parts {
		if !p.valid(i.Scope) {
			return false
		}
		sum.Packs += p.Packs
		sum.GrossWeight.Amount += p.GrossWeight.Amount
		sum.Volume.Amount += p.Volume.Amount
	}
	return sum == i.Scope
}

// Event types, as package api publishes them.
const (
	eventConsolidationCreated    = "ConsolidationCreated.v1"
	eventConsolidationAmended    = "ConsolidationAmended.v1"
	eventConsolidationClosed     = "ConsolidationClosed.v1"
	eventConsolidationReopened   = "ConsolidationReopened.v1"
	eventConsolidationCancelled  = "ConsolidationCancelled.v1"
	eventConsolidationReconciled = "ConsolidationReconciled.v1"
	eventLoadUnitPlanned         = "LoadUnitPlanned.v1"
	eventLoadUnitRemoved         = "LoadUnitRemoved.v1"
	eventHouseAttached           = "HouseAttached.v1"
	eventHouseDetached           = "HouseDetached.v1"
	eventHouseTransferred        = "HouseTransferred.v1"
	eventHouseIntentRefreshed    = "HouseIntentRefreshed.v1"
)

// Event carries identifiers only; the Outbox adapter maps it to a Bento envelope. SubjectID is the
// consolidation for consolidation events, the shipment for house events, the lot for cargo lot events and the
// execution for load execution events; ItemNo numbers the line, measurement, screening result, hold, evidence
// or discrepancy an event is about.
type Event struct {
	Type                string
	At                  time.Time
	TenantID            TenantID
	SubjectID           string
	ConsolidationID     ConsolidationID
	Version             int
	ShipmentID          ShipmentID
	EntryNo             int
	FromConsolidationID ConsolidationID
	Reason              string
	LotID               CargoLotID
	ExecutionID         LoadExecutionID
	ItemNo              int
}

const (
	maxText = 500 // the longest reason kept
	maxID   = 128 // the longest identifier cargo_phy stores
)

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxText }

func identifier(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxID }

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
