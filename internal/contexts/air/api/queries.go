package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Movement states. FINAL, with AirDocFacts v1 for DOC, comes once PHY's measures and screening reach AIR.
const (
	StateOpen      = "OPEN"
	StateCancelled = "CANCELLED"
)

// Entry starts and ends of a house association.
const (
	EntryAssociated        = "ASSOCIATED"
	EntryRefreshed         = "REFRESHED"
	EntryDetached          = "DETACHED"
	EntryTransferred       = "TRANSFERRED"
	EntryShipmentCancelled = "SHIPMENT_CANCELLED"
)

// Bases of a house's quantities: PLANNED from FUL's plan until PHY's measures are relayed (MEASURED, later).
const BasisPlanned = "PLANNED"

// Airport is an airport as AIR froze it: its UN/LOCODE and IATA code.
type Airport struct {
	Code kernel.LocationCode
	IATA string
}

// FlightSegment is a flight of the movement as the airline confirmed it, from the booking's capacity version;
// ETD and ETA are the airline's estimates.
type FlightSegment struct {
	Segment         int
	Flight          string
	From            Airport
	To              Airport
	ETD             time.Time
	ETA             time.Time
	BookingID       string
	CapacityVersion int
	Current         bool
}

// HousePart is a part of a COM obligation the house carries, with its order.
type HousePart struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	PackageType       string
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
}

// House is a house air waybill association: the FUL shipment, its HAWB number and the facts frozen from FUL's
// plan version, with the documented chargeable weight (in grams) and the rule it was computed by.
type House struct {
	EntryNo          int
	ShipmentID       string
	HAWB             string
	FULRevision      int64
	PlanVersion      int
	Pieces           int64
	GrossWeight      kernel.Quantity
	Volume           kernel.Quantity
	ChargeableWeight kernel.Quantity // GRM
	ChargeableRule   string
	Basis            string
	Parts            []HousePart
	Start            string
	AssociatedAt     time.Time
	End              string // empty while live
	EndedAt          time.Time
	WindowDrift      bool // the house's departure window does not contain the first current flight's ETD
}

// AirMovement is an air movement with its MAWB, flights, live houses and master totals. Master chargeable weight
// is computed on the master totals, not as a sum of the houses'.
type AirMovement struct {
	TenantID               string
	MovementID             string
	Revision               int64
	State                  string
	ConsolidationID        string
	OperatingLegalEntityID string
	AirlinePartyID         string
	AirlinePartyRevision   int64
	AirlineDesignator      string
	AWBPrefix              string
	Origin                 Airport
	Destination            Airport
	LocationEdition        string
	MAWB                   string // PPP-SSSSSSSS
	MAWBStatus             string // ASSIGNED or VOIDED
	BookingID              string
	CapacityVersion        int
	Flights                []FlightSegment // current first, then superseded
	Houses                 []House         // live
	Pieces                 int64
	GrossWeight            kernel.Quantity
	Volume                 kernel.Quantity
	ChargeableWeight       kernel.Quantity
}

// HawbSeries is a legal entity's HAWB series.
type HawbSeries struct {
	LegalEntityID string
	Prefix        string
	Width         int
	Next          int64
	Revision      int64
}

// Queries.
type (
	GetAirMovement        struct{ TenantID, MovementID string }
	FindAirMovementByMAWB struct{ TenantID, MAWB string }
	FindHouseWaybill      struct{ TenantID, LegalEntityID, HAWB string }
	GetHawbSeries         struct{ TenantID, LegalEntityID string }
)

// HouseWaybillRef is where a HAWB number belongs: its shipment for ever and, while live, the movement.
type HouseWaybillRef struct {
	LegalEntityID string
	HAWB          string
	ShipmentID    string
	MovementID    string // empty when no association is live
	EntryNo       int
}

// Queries is AIR's read side. Unknown movements, MAWBs, HAWBs and series yield ErrNotFound.
type Queries interface {
	GetAirMovement(ctx context.Context, q GetAirMovement) (AirMovement, error)
	FindAirMovementByMAWB(ctx context.Context, q FindAirMovementByMAWB) (AirMovement, error)
	FindHouseWaybill(ctx context.Context, q FindHouseWaybill) (HouseWaybillRef, error)
	GetHawbSeries(ctx context.Context, q GetHawbSeries) (HawbSeries, error)
}
