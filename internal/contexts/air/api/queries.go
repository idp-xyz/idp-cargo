package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Movement states: OPEN; FINAL once its facts are finalized for DOC (AirDocFacts v1), until a change returns it
// to OPEN; CANCELLED.
const (
	StateOpen      = "OPEN"
	StateFinal     = "FINAL"
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

// Bases of a house's quantities: PLANNED from FUL's plan; RECEIVED (the lots' latest measures once every lot
// completed receipt) and TENDERED (what was tendered in the consolidation, at the lots' latest measures) from
// PHY's evidence as FUL relays it (decision N1-PHY OD-2, option A).
const (
	BasisPlanned  = "PLANNED"
	BasisReceived = "RECEIVED"
	BasisTendered = "TENDERED"
)

// Security states of a house's cargo, from PHY's screening results as FUL relays them.
const (
	SecurityPending = "PENDING"
	SecurityCleared = "CLEARED"
	SecurityFailed  = "FAILED"
)

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
	GoodsDescription  string // the order line's, as FUL relays it from COM; empty for parts FUL planned before it kept one
}

// Screening is a lot's latest screening result as PHY recorded it and FUL relays it: the screening party's
// evidence (an airport security station), its method code, reference and time.
type Screening struct {
	LotID      string
	Outcome    string // PASSED, FAILED, PENDING or NOT_REQUIRED
	Screener   string
	Method     string
	Reference  string
	ScreenedAt time.Time
}

// House is a house air waybill association: the FUL shipment, its HAWB number and the facts frozen from FUL's
// plan version or from PHY's measures FUL relayed (Basis, with FUL's EvidenceVersion and the SecurityStatus),
// with the documented chargeable weight (in grams) and the rule it was computed by. Parts stay as planned.
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
	EvidenceVersion  int    // FUL's evidence version the measures came from; 0 for PLANNED
	SecurityStatus   string // "", SecurityPending, SecurityCleared or SecurityFailed
	Screenings       []Screening
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
	DocFactsVersion        int // the latest AirDocFacts version; 0 before the first finalization. Current while State is FINAL.
}

// AirDocFacts statuses: FINAL while the version is the movement's latest and the movement has not changed since;
// SUPERSEDED once a change returned the movement to OPEN (AirDocFactsSuperseded.v1) or a later version was
// finalized. DOC marks documents built on a superseded version stale. FINAL speaks for AIR only: AIR does not yet
// follow FUL's events, so a house takes FUL's newer facts when it is refreshed (by the API client, T6), and that
// refresh supersedes the version.
const (
	DocFactsFinal      = "FINAL"
	DocFactsSuperseded = "SUPERSEDED"
)

// SecuritySPX is the consignment security status of a master and its houses in AirDocFacts v1: cargo screened by
// the screening party (or not requiring it), secure for passenger, all-cargo and all-mail aircraft.
const SecuritySPX = "SPX"

// MAWBSourceBookingConfirmation says the MAWB number came from the airline's booking confirmation (OD-7).
const MAWBSourceBookingConfirmation = "BOOKING_CONFIRMATION"

// AirDocFacts is AirDocFacts v1 (relationship 11, AIR -> DOC): one immutable finalized version of an air
// movement's facts that DOC builds the MAWB and HAWBs on. It carries the master (airline, MAWB and its source,
// route, flights, totals with the chargeable weight computed on them, security status) and every live house
// (HAWB, the FUL plan and evidence versions its facts came from, PHY's measures with the documented chargeable
// weight, security status with the screening evidence, and its parts with their goods descriptions). Weights are
// in GRM and volumes in CMQ, every house's volume measured. It carries no parties, contacts, charges, declared
// values or handling codes: DOC takes parties from COM and ORG. Content is the version's JSON exactly as AIR
// published and stored it, and SHA256 its SHA-256, unchanged for ever; the fields below are read from it.
type AirDocFacts struct {
	TenantID               string
	MovementID             string
	Version                int
	Status                 string // DocFactsFinal or DocFactsSuperseded
	Content                []byte
	SHA256                 string
	FinalizedBy            string
	FinalizedAt            time.Time
	ConsolidationID        string
	OperatingLegalEntityID string
	AirlinePartyID         string
	AirlinePartyRevision   int64
	AirlineDesignator      string
	AWBPrefix              string
	MAWB                   string
	MAWBSource             string // MAWBSourceBookingConfirmation
	BookingID              string
	CapacityVersion        int
	Origin                 Airport
	Destination            Airport
	LocationEdition        string
	Flights                []DocFlight
	Pieces                 int64
	GrossWeight            kernel.Quantity // GRM
	Volume                 kernel.Quantity // CMQ
	ChargeableWeight       kernel.Quantity // GRM
	ChargeableRule         string
	SecurityStatus         string // SecuritySPX
	Houses                 []DocHouse
}

// DocFlight is a flight of the finalized movement, with the airline's estimates.
type DocFlight struct {
	Flight string
	From   Airport
	To     Airport
	ETD    time.Time
	ETA    time.Time
}

// DocHouse is a house of a finalized version.
type DocHouse struct {
	ShipmentID       string
	HAWB             string
	PlanVersion      int
	EvidenceVersion  int
	Basis            string // BasisReceived or BasisTendered
	Pieces           int64
	GrossWeight      kernel.Quantity // GRM
	Volume           kernel.Quantity // CMQ
	ChargeableWeight kernel.Quantity // GRM
	SecurityStatus   string          // SecuritySPX
	Screenings       []Screening
	Parts            []DocPart
}

// DocPart is a part of a house in a finalized version, with the goods description of its order line.
type DocPart struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	PackageType       string
	GoodsDescription  string
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
	// GetAirDocFacts reads a finalized version of a movement's facts; Version 0 reads the latest.
	GetAirDocFacts struct {
		TenantID   string
		MovementID string
		Version    int
	}
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
	AirDocFactsQueries
}

// AirDocFactsQueries is AIR's AirDocFacts v1 for DOC. A movement never finalized, or a version it does not have,
// yields ErrNotFound.
type AirDocFactsQueries interface {
	GetAirDocFacts(ctx context.Context, q GetAirDocFacts) (AirDocFacts, error)
}
