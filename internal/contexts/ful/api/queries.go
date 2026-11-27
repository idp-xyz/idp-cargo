package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Shipment statuses.
const (
	StatusDraft     = "DRAFT"
	StatusPlanned   = "PLANNED"
	StatusCancelled = "CANCELLED"
)

// ObligationRef is a COM obligation version a shipment plans, with the order it belongs to.
type ObligationRef struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
}

// ShipmentPlanSnapshot is ShipmentPlanSnapshot v1: the current plan of a shipment, for BOOK and later
// steps. The cargo scope is the sum of the planned parts, which a cancelled shipment no longer has.
// It carries no names (MD-05) and never asserts loading or delivery.
type ShipmentPlanSnapshot struct {
	TenantID         string
	ShipmentID       string
	Revision         int64
	Status           string // StatusDraft, StatusPlanned or StatusCancelled
	PlanVersion      int
	ConfirmedVersion int // the latest confirmed plan version, 0 if none
	CustomerPartyID  string
	LegalEntityID    string
	TransportMode    string
	Origin           kernel.LocationCode
	Destination      kernel.LocationCode
	DepartureWindow  kernel.ValidityInterval
	Packs            int64
	GrossWeight      kernel.Quantity
	Volume           kernel.Quantity // zero when the obligations declare none
	Obligations      []ObligationRef
	PlannedAt        time.Time // when the current plan version was written
}

// PlannedPart is the part of a COM obligation version a shipment plans (F1): its packs, gross weight and
// volume, and what COM committed for the whole obligation.
type PlannedPart struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity // zero when the obligation declares none
	ObligationPacks   int64
	ObligationWeight  kernel.Quantity
	ObligationVolume  kernel.Quantity
}

// ShipmentPlanSnapshotV2 is ShipmentPlanSnapshot v1 with the part of each obligation the shipment plans
// (F1). In v1 the obligations are listed without their parts; the scope is the sum of the parts in both.
type ShipmentPlanSnapshotV2 struct {
	TenantID         string
	ShipmentID       string
	Revision         int64
	Status           string // StatusDraft, StatusPlanned or StatusCancelled
	PlanVersion      int
	ConfirmedVersion int // the latest confirmed plan version, 0 if none
	CustomerPartyID  string
	LegalEntityID    string
	TransportMode    string
	ContainerMode    string // N1-PHY OD-9; empty when not planned
	Origin           kernel.LocationCode
	Destination      kernel.LocationCode
	DepartureWindow  kernel.ValidityInterval
	Packs            int64
	GrossWeight      kernel.Quantity
	Volume           kernel.Quantity // zero when the obligations declare none
	Parts            []PlannedPart   // none once cancelled
	PlannedAt        time.Time       // when the current plan version was written
	// Since FUL V0.3 (F2, option 4): the IATA airport codes of an AIR route, which air waybills and messages
	// use, and the edition of the location reference data the route was resolved with; empty for plan versions
	// written before V0.3. EnteredAt is when FUL recorded the current plan version (T1), zero before V0.3.
	OriginIATA      string
	DestinationIATA string
	LocationEdition string
	EnteredAt       time.Time
}

// GetShipmentPlanSnapshot asks for a shipment's current plan. TenantID must come from a verified
// principal.
type GetShipmentPlanSnapshot struct {
	TenantID   string
	ShipmentID string
}

// StageMain is the main carriage of a shipment's route, the only stage FUL V0.4 executes in the China export
// slice (N1).
const StageMain = "MAIN"

// LoadIntentPart is the part of a COM obligation version a shipment intends to load, with the package type
// COM recorded for the obligation.
type LoadIntentPart struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	PackageType       string // UN/ECE Recommendation 21, such as CT for cartons
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity // zero when the obligation declares none
	GoodsDescription  string          // the order line's goods description; empty for parts planned before FUL kept it
}

// LoadIntent is LoadIntent v1 (relationship 7, FUL -> PHY): what a shipment intends to load on its main
// carriage, read by PHY before it attaches the shipment to a consolidation and again before it closes one. It
// is an intent, never a loading fact, and carries no names (MD-05). ConsolidationID is the consolidation FUL
// recorded the shipment attached to on the stage, with the PHY membership entry; empty when none. PHY compares
// PlanVersion, not Revision, with what it verified: recording the attachment moves the revision, not the plan.
type LoadIntent struct {
	TenantID          string
	ShipmentID        string
	Revision          int64
	Status            string // StatusDraft, StatusPlanned or StatusCancelled
	PlanVersion       int
	ConfirmedVersion  int
	CustomerPartyID   string
	LegalEntityID     string
	TransportMode     string
	ContainerMode     string // N1-PHY OD-9: LCL or FCL on SEA, LOOSE on AIR; empty when not planned
	Origin            kernel.LocationCode
	Destination       kernel.LocationCode
	OriginIATA        string
	DestinationIATA   string
	LocationEdition   string
	DepartureWindow   kernel.ValidityInterval
	Packs             int64
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
	Parts             []LoadIntentPart // none once cancelled
	Stage             string           // StageMain
	ConsolidationID   string
	MembershipEntryNo int
}

// AirOperationPlan is AirOperationPlan v1 (relationship 9, FUL -> AIR): a shipment's plan as AIR needs it to
// associate the house with the master waybill of its consolidation: the plan with its parts and package types,
// and the consolidation FUL recorded the shipment attached to, with the PHY membership entry and revision
// (decision N1-PHY OD-2, option A: membership and measures reach AIR through FUL). Packs, GrossWeight and Volume
// are planned. Once FUL recorded PHY's evidence (EvidenceVersion > 0) it relays the measures: MeasuredBasis
// TENDERED with what was tendered in the attached consolidation at the lots' latest measures, else RECEIVED with
// the lots' latest measures once every lot completed receipt, else empty; with the security state of the cargo
// and the execution status. It carries no names.
type AirOperationPlan struct {
	TenantID        string
	ShipmentID      string
	Revision        int64
	Status          string
	PlanVersion     int
	CustomerPartyID string
	LegalEntityID   string
	TransportMode   string
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
	OriginIATA      string
	DestinationIATA string
	LocationEdition string
	DepartureWindow kernel.ValidityInterval
	// AgreedWindow (V2.110) is the window the customer agreed to: DepartureWindow widened to the departures the customer
	// accepted (RecordCustomerDateAnswer). AIR and SEA hold the house to it.
	AgreedWindow       kernel.ValidityInterval
	Packs              int64
	GrossWeight        kernel.Quantity
	Volume             kernel.Quantity
	Parts              []LoadIntentPart // none once cancelled
	Stage              string
	ConsolidationID    string // empty when FUL records no attachment on the stage
	MembershipEntryNo  int
	MembershipRevision int64
	// FUL V0.4, from PHY's verified evidence
	EvidenceVersion     int
	ExecutionStatus     string // NOT_STARTED, PARTIALLY_DEPARTED, DEPARTED, PARTIALLY_ARRIVED, ARRIVED
	MeasuredBasis       string // "", MeasuredReceived or MeasuredTendered
	MeasuredPacks       int64
	MeasuredGrossWeight kernel.Quantity
	MeasuredVolume      kernel.Quantity
	SecurityStatus      string // "", SecurityPending, SecurityCleared or SecurityFailed
	Screenings          []SecurityScreening
}

// OceanOperationPlan is OceanOperationPlan v1 (relationship 10, FUL -> SEA): a SEA shipment's plan as SEA needs it to
// associate the house with the bill of its consolidation (decision N1-PHY OD-2, option A: membership, measures and
// stuffing reach SEA through FUL): the plan with its container mode and its parts, their package types and goods
// descriptions, and the consolidation FUL recorded the shipment attached to, with the PHY membership entry and
// revision. Packs, GrossWeight and Volume are planned. Once FUL recorded PHY's evidence (EvidenceVersion > 0) it
// relays the measures (MeasuredBasis as in AirOperationPlan v1), the execution status, and the sealed containers of
// the attached consolidation with the shipment's packs in them (Stuffings, PHY V0.3; evidence versions recorded
// before V2.60 carry none until FUL records the evidence again). It carries no names. Like AirOperationPlan v1 it
// answers any shipment, with its TransportMode, so that SEA can end the association of a house planned elsewhere.
type OceanOperationPlan struct {
	TenantID        string
	ShipmentID      string
	Revision        int64
	Status          string
	PlanVersion     int
	CustomerPartyID string
	LegalEntityID   string
	TransportMode   string // ModeSea
	ContainerMode   string // N1-PHY OD-9: LCL or FCL; empty when not planned
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
	LocationEdition string
	DepartureWindow kernel.ValidityInterval
	// AgreedWindow (V2.110) is the window the customer agreed to: DepartureWindow widened to the departures the customer
	// accepted (RecordCustomerDateAnswer). AIR and SEA hold the house to it.
	AgreedWindow       kernel.ValidityInterval
	Packs              int64
	GrossWeight        kernel.Quantity
	Volume             kernel.Quantity
	Parts              []LoadIntentPart // none once cancelled
	Stage              string
	ConsolidationID    string // empty when FUL records no attachment on the stage
	MembershipEntryNo  int
	MembershipRevision int64
	// From PHY's verified evidence
	EvidenceVersion     int
	ExecutionStatus     string
	MeasuredBasis       string // "", MeasuredReceived or MeasuredTendered
	MeasuredPacks       int64
	MeasuredGrossWeight kernel.Quantity
	MeasuredVolume      kernel.Quantity
	Stuffings           []ContainerStuffing
}

// ContainerStuffing is a sealed container of the attached consolidation with the shipment's live packs in it, as
// PHY verified them (lines measured for themselves as measured, apportioned lines at their lot's latest
// measurement): PHY's stuffing (load execution) and planned unit with its ISO 6346 size-type, PHY's load lines of
// the shipment with live packs in it (with ExecutionID, the PHY evidence ids), and the seal PHY recorded as the
// stuffing's closing evidence: its number, the SHA-256 of its document when given, and when it was applied. The
// container's number and plate data are SEA's.
type ContainerStuffing struct {
	ExecutionID string
	LoadUnitNo  int
	TypeCode    string
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity
	LineNos     []int
	Seal        string
	SealSHA256  string
	SealedAt    time.Time
}

// OceanOperationPlanQueries is FUL's OceanOperationPlan v1 for SEA. An unknown shipment yields ErrNotFound.
type OceanOperationPlanQueries interface {
	GetOceanOperationPlan(ctx context.Context, q GetShipmentPlanSnapshot) (OceanOperationPlan, error)
}

// OceanEvidenceCurrency is OceanEvidenceCurrency v1 (relationship 10, FUL -> SEA): whether the load evidence FUL
// recorded of a shipment is PHY's current one. NewVersionDue is true when RecordVerifiedLoadEvidence would record a
// new evidence version now: PHY's evidence differs from FUL's latest recorded one by the comparison that command
// makes (the digest, the execution status and the packs final for planning), read for the consolidation FUL records
// the shipment attached to. It is false when FUL is current, and for a cancelled shipment, which that command refuses
// and records nothing for (Status says CANCELLED). A shipment with no evidence recorded yet has a version due, even
// where PHY's evidence is empty: the command records an empty first version. Revision and EvidenceVersion are what
// FUL held when it answered. The answer carries no names and is read without a lock.
type OceanEvidenceCurrency struct {
	TenantID        string
	ShipmentID      string
	Revision        int64
	Status          string
	EvidenceVersion int
	NewVersionDue   bool
}

// OceanEvidenceQueries is FUL's OceanEvidenceCurrency v1 for SEA. An unknown shipment yields ErrNotFound.
type OceanEvidenceQueries interface {
	GetOceanEvidenceCurrency(ctx context.Context, q GetShipmentPlanSnapshot) (OceanEvidenceCurrency, error)
}

// OceanOperationQueries is what SEA reads of FUL: the plan of a shipment and whether FUL's evidence of it is current.
type OceanOperationQueries interface {
	OceanOperationPlanQueries
	OceanEvidenceQueries
}

// SecurityScreening is a lot's latest screening result as PHY recorded it: external evidence of the screening
// party (the airport security station), its method code, reference and time.
type SecurityScreening struct {
	LotID      string
	Outcome    string
	Screener   string
	Method     string
	Reference  string
	ScreenedAt time.Time
}

// Measure bases, security states and execution statuses of AirOperationPlan v1.
const (
	MeasuredReceived = "RECEIVED"
	MeasuredTendered = "TENDERED"

	SecurityPending = "PENDING"
	SecurityCleared = "CLEARED"
	SecurityFailed  = "FAILED"

	ExecutionNotStarted        = "NOT_STARTED"
	ExecutionPartiallyDeparted = "PARTIALLY_DEPARTED"
	ExecutionDeparted          = "DEPARTED"
	ExecutionPartiallyArrived  = "PARTIALLY_ARRIVED"
	ExecutionArrived           = "ARRIVED"
)

// AirOperationPlanQueries is FUL's AirOperationPlan v1 for AIR. An unknown shipment yields ErrNotFound.
type AirOperationPlanQueries interface {
	GetAirOperationPlan(ctx context.Context, q GetShipmentPlanSnapshot) (AirOperationPlan, error)
}

// LoadIntentQueries is FUL's LoadIntent v1 for PHY (FUL V0.4). An unknown shipment yields ErrNotFound.
type LoadIntentQueries interface {
	GetLoadIntent(ctx context.Context, q GetShipmentPlanSnapshot) (LoadIntent, error)
}

// Queries is FUL's read side for other contexts. An unknown shipment yields ErrNotFound.
type Queries interface {
	GetShipmentPlanSnapshot(ctx context.Context, q GetShipmentPlanSnapshot) (ShipmentPlanSnapshot, error)
	GetShipmentPlanSnapshotV2(ctx context.Context, q GetShipmentPlanSnapshot) (ShipmentPlanSnapshotV2, error)
}

// GetShipmentExecution asks for a shipment's execution next to its plan (V2.110).
type GetShipmentExecution struct {
	TenantID   string
	ShipmentID string
}

// CapacityLeg is a confirmed leg of a capacity link: the carrier's flight or vessel and voyage, the loading and
// discharge places, and the carrier's estimated departure and arrival (ETA zero when the carrier gave none).
type CapacityLeg struct {
	Loading   kernel.LocationCode
	Discharge kernel.LocationCode
	Flight    string
	Vessel    string
	Voyage    string
	ETD       time.Time
	ETA       time.Time
}

// CapacityLink is one link of the main carriage to a booking's capacity, with the customer's answers to it.
// OutsideWindow says whether its first departure lies outside the plan's departure window as it is now.
type CapacityLink struct {
	LinkNo           int
	PlanVersion      int // the plan version it was made under
	BookingID        string
	BookingRevision  int64
	ConsolidationID  string
	CapacityVersion  int
	Line             int
	CarrierReference string
	Legs             []CapacityLeg
	OutsideWindow    bool
	Answers          []DateAnswer
	LinkedBy         string
	LinkedAt         time.Time
}

// DateAnswer is the customer's answer to a link's departure outside the window, with the window it answered; it
// counts while the plan's window is that one (Counts).
type DateAnswer struct {
	AnswerNo    int
	Answer      string
	EvidenceRef string
	Window      kernel.ValidityInterval
	Counts      bool
	AnsweredBy  string
	AnsweredAt  time.Time
}

// ShipmentExecution is a shipment's execution next to its plan: the plan's departure window and the window the
// customer agreed to, the current consol attachment, the capacity links, whether the current link awaits the
// customer's answer (or was refused), and what PHY's latest verified evidence shows.
type ShipmentExecution struct {
	TenantID         string
	ShipmentID       string
	Revision         int64
	Status           string
	PlanVersion      int
	TransportMode    string
	DepartureWindow  kernel.ValidityInterval
	AgreedWindow     kernel.ValidityInterval
	ConsolidationID  string // empty when FUL records no attachment on the main carriage
	Links            []CapacityLink
	CurrentLink      int  // 0 when the shipment has none
	AwaitingCustomer bool // the current link departs outside the agreed window
	Refused          bool // and the customer's answer that counts refused it
	PlannedPacks     int64
	ExecutionStatus  string
	EvidenceVersion  int
	DepartedPacks    int64
	ArrivedPacks     int64
	OffloadedPacks   int64
}

// ListDateChangesAwaitingCustomer asks for the shipments whose current capacity link departs outside the window the
// customer agreed to, for a work list: of one consolidation they are attached to, or of the tenant when
// ConsolidationID is empty; at most 200, by shipment id.
type ListDateChangesAwaitingCustomer struct {
	TenantID        string
	ConsolidationID string
}

// DateChangeAwaitingCustomer is a row of ListDateChangesAwaitingCustomer.
type DateChangeAwaitingCustomer struct {
	ShipmentID      string
	Revision        int64
	ConsolidationID string
	LinkNo          int
	BookingID       string
	Departure       time.Time
	AgreedWindow    kernel.ValidityInterval
	Refused         bool
}

// ExecutionQueries are FUL's reads of a shipment's execution (V2.110).
type ExecutionQueries interface {
	GetShipmentExecution(ctx context.Context, q GetShipmentExecution) (ShipmentExecution, error)
	ListDateChangesAwaitingCustomer(ctx context.Context, q ListDateChangesAwaitingCustomer) ([]DateChangeAwaitingCustomer, error)
}
