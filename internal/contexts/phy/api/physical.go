package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PHY V0.2: the physical facts of a consolidation's houses. A cargo lot (CargoWise: the receipt of a house at
// the warehouse) holds what was received, measured and screened of a house at one facility, and the
// differences against FUL's plan; a load execution tenders the houses of an AIR_CONSOL to the airline terminal
// or of a SEA_LCL_COLOAD to the co-loader's CFS, with the carrier's evidence afterwards. Every fact carries its
// evidence; corrections are reversal lines and new versions. Stuffing a container and building up a ULD come
// with load units. TenantID and ActorID must come from a verified principal; OccurredAt lies within the record
// window before PHY records the command (T1).

// Facility is where cargo is received or tendered: the UN/LOCODE of the place and the facility's code there
// (the forwarder's warehouse, a CFS, the airline terminal).
type Facility struct {
	Location kernel.LocationCode
	Code     string
}

// Evidence is the external evidence of a physical fact: its source (a scan device, a CFS tally, the airline
// terminal, the airport security station), the source's reference, when it occurred, and the SHA-256 of the
// raw payload where there is one.
type Evidence struct {
	Source     string
	Reference  string
	SHA256     string
	OccurredAt time.Time
}

// Pack conditions, measurement methods, screening outcomes and discrepancy resolutions.
const (
	ConditionGood    = "GOOD"
	ConditionDamaged = "DAMAGED"
	ConditionWet     = "WET"
	ConditionOpened  = "OPENED"

	MethodScale          = "SCALE"
	MethodDimensioner    = "DIMENSIONER"
	MethodCFSTally       = "CFS_TALLY"
	MethodTerminalWeight = "TERMINAL_WEIGHT"

	ScreeningPassed      = "PASSED"
	ScreeningFailed      = "FAILED"
	ScreeningPending     = "PENDING"
	ScreeningNotRequired = "NOT_REQUIRED"

	ResolvedByReplan   = "FUL_REPLANNED" // FUL's plan now equals what was received (SHORT or OVER)
	ResolvedByAccepted = "ACCEPTED"      // with a reason
	ResolvedByClaim    = "CLAIM_OPENED"  // with the claim's reference
)

// ReceiveCargo opens a cargo lot for a shipment FUL reports PLANNED, with its first receipt line
// (ExpectedRevision 0), or adds a receipt line to the lot while it is receiving. One lot per shipment and
// facility is in force; cargo may arrive before its house is attached.
type ReceiveCargo struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LotID            string
	ShipmentID       string
	Facility         Facility
	ExpectedRevision int64
	Packs            int64
	PackageType      string // UN/ECE Recommendation 21
	Condition        string
	Evidence         Evidence
	OccurredAt       time.Time
}

// PackageGroup is a count of packages of one type and size, in millimetres.
type PackageGroup struct {
	Count       int64
	PackageType string
	LengthMM    int64
	WidthMM     int64
	HeightMM    int64
}

// RecordCargoMeasurement adds a measurement version of the whole lot, in the plan's weight unit and, where the
// plan declares a volume, its volume unit.
type RecordCargoMeasurement struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LotID            string
	ExpectedRevision int64
	GrossWeight      kernel.Quantity
	Volume           kernel.Quantity // zero when not measured
	PackageGroups    []PackageGroup
	Method           string
	Evidence         Evidence
	OccurredAt       time.Time
}

// RecordScreeningResult records the screening result of an AIR lot, as external evidence of the screening
// party (the airport security station; CN-7). A FAILED result holds every tender with live packs of the lot.
type RecordScreeningResult struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LotID            string
	ExpectedRevision int64
	Outcome          string
	Screener         string // the screening party's reference
	Method           string // a method code, such as XRAY or ETD
	Reference        string // the screening certificate or record number
	Evidence         Evidence
	OccurredAt       time.Time
}

// CompleteReceipt ends receiving against FUL's current LoadIntent v1: short, over and damaged packs and a
// gross weight or volume beyond tolerance (5 % and 10 %) are recorded as discrepancies, never as a change of the
// plan (C4). The lot needs a measurement.
type CompleteReceipt struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LotID            string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// ResolveCargoDiscrepancy resolves an open discrepancy of a lot: FUL_REPLANNED once FUL's plan equals the packs
// received (FUL changed the parts, F1), ACCEPTED with a reason, or CLAIM_OPENED with the claim's reference.
type ResolveCargoDiscrepancy struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LotID            string
	ExpectedRevision int64
	DiscrepancyNo    int
	Resolution       string
	Reference        string
	Reason           string
	OccurredAt       time.Time
}

// CloseCargoLot closes a received lot without open discrepancies once every received pack is loaded, or the
// packs not loaded are accounted for by FUL: its plan of the shipment equals what was loaded, or the shipment
// is cancelled and nothing of it is loaded.
type CloseCargoLot struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LotID            string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// Load execution kinds: a TENDER of loose cargo or of a co-load's houses, or the STUFFING of a planned container
// (PHY V0.3), whose closing evidence is the container's seal (source EvidenceSeal, the seal number as reference).
const (
	ExecutionTender   = "TENDER"
	ExecutionStuffing = "STUFFING"

	EvidenceSeal = "SEAL"
)

// Load execution states.
const (
	ExecutionOpen    = "OPEN"
	ExecutionLoading = "LOADING"
	ExecutionOnHold  = "ON_HOLD"
	ExecutionClosed  = "CLOSED"
)

// Reversal reasons, hold reasons and carrier facts.
const (
	ReversalUnstuffed            = "UNSTUFFED"
	ReversalOffloadedByForwarder = "OFFLOADED_BY_FORWARDER"
	ReversalMisloaded            = "MISLOADED"

	HoldScreeningFailed = "SECURITY_SCREENING_FAILED" // placed by PHY on a FAILED screening result
	HoldDGQuery         = "DG_QUERY"
	HoldCustoms         = "CUSTOMS_HOLD"
	HoldOverweight      = "OVERWEIGHT"
	HoldDamage          = "DAMAGE"

	CarrierAccepted      = "ACCEPTED"
	CarrierManifested    = "MANIFESTED"
	CarrierDeparted      = "DEPARTED"
	CarrierOffloaded     = "OFFLOADED"
	CarrierArrived       = "ARRIVED"
	CarrierLoadedOnBoard = "LOADED_ON_BOARD"

	LoadConflict  = "LOAD_CONFLICT" // more pieces than tendered
	LoadOffloaded = "OFFLOADED"     // the carrier offloaded pieces
)

// OpenLoadExecution opens a load execution at a facility of the consolidation's origin:
//   - a TENDER of an AIR_CONSOL or SEA_LCL_COLOAD consolidation, OPEN or CLOSED, at the airline terminal or the
//     co-loader's CFS; it names no load unit;
//   - the STUFFING of a planned container (LoadUnitNo) of a CLOSED SEA_LCL_OWN consolidation, at the forwarder's
//     CFS (PHY V0.3). A unit takes one stuffing; its cargo is checked against the unit's limits as it is stuffed: a
//     line taking the container, its tare and cargo, over the default maximum gross, or its cargo over the capacity,
//     is recorded and holds the execution (OVERWEIGHT), which a release with its evidence (such as the plate)
//     lifts. Weights are in GRM, KGM or TNE and volumes in CMQ, DMQ, LTR or MTQ.
type OpenLoadExecution struct {
	CommandID       string
	TenantID        string
	ActorID         string
	ExecutionID     string
	ConsolidationID string
	Kind            string // ExecutionTender or ExecutionStuffing
	Facility        Facility
	LoadUnitNo      int // the planned container a STUFFING fills
	OccurredAt      time.Time
}

// StartLoading starts loading an OPEN execution.
type StartLoading struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// RecordCargoLoaded records packs of a lot tendered under the house's live membership in the execution's
// consolidation. The lot's live loaded packs never exceed its received packs, nor the house's its planned packs
// (CONSERVATION_VIOLATION). GrossWeight and Volume are measured for the line, or zero to apportion them from the
// lot's latest measurement. Packs of a lot whose screening FAILED are recorded and hold the execution.
type RecordCargoLoaded struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	LotID            string
	Packs            int64
	GrossWeight      kernel.Quantity
	Volume           kernel.Quantity
	Evidence         Evidence
	OccurredAt       time.Time
}

// RecordCargoUnloaded reverses packs of a load line, while loading or on hold; the line stays.
type RecordCargoUnloaded struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	LineNo           int
	Packs            int64
	Reason           string
	Evidence         Evidence
	OccurredAt       time.Time
}

// PlaceLoadExecutionOnHold holds an execution: DG_QUERY, CUSTOMS_HOLD, OVERWEIGHT or DAMAGE, with evidence.
type PlaceLoadExecutionOnHold struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	Reason           string
	Text             string
	Evidence         Evidence
	OccurredAt       time.Time
}

// ReleaseLoadExecutionHold releases a hold with evidence; a screening hold once the lot's latest screening is
// PASSED or NOT_REQUIRED, or its packs are unloaded. Releasing the last hold resumes loading.
type ReleaseLoadExecutionHold struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	HoldNo           int
	Evidence         Evidence
	OccurredAt       time.Time
}

// CloseLoadExecution closes a loading execution with the tender receipt of the airline terminal or the
// co-loader's CFS.
type CloseLoadExecution struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	Evidence         Evidence
	OccurredAt       time.Time
}

// RecordCarrierLoadEvidence records the carrier's status of a closed tender's pieces (OD-4: entered by hand in
// N1), of the master or of one house (ShipmentID), on a flight or voyage. A source reference recorded already
// with the same content is accepted again without change; with other content it is rejected. More pieces than
// tendered are recorded and raise a LOAD_CONFLICT discrepancy; offloaded pieces an OFFLOADED discrepancy.
type RecordCarrierLoadEvidence struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	Fact             string
	ShipmentID       string // empty for the master
	Pieces           int64
	GrossWeight      kernel.Quantity // zero when the carrier gave none
	Transport        string          // flight or voyage, such as MU5101
	Evidence         Evidence
	OccurredAt       time.Time
}

// ResolveLoadingDiscrepancy resolves an open loading discrepancy: ACCEPTED with a reason or CLAIM_OPENED with
// the claim's reference.
type ResolveLoadingDiscrepancy struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	DiscrepancyNo    int
	Resolution       string
	Reference        string
	Reason           string
	OccurredAt       time.Time
}

// ReconcileConsolidation reconciles a CLOSED consolidation on its physical facts: FUL reports every live house
// PLANNED at the plan version PHY froze and allocated to the consolidation, and its live loaded packs equal the
// plan (a difference went to another shipment through FUL and the house was refreshed, F1); no load execution
// is open and no discrepancy of the houses' lots or of the executions is open. A reconciled consolidation never
// changes again.
type ReconcileConsolidation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ConsolidationID  string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// PhysicalCommands is PHY's command entry point for cargo lots and load executions.
type PhysicalCommands interface {
	ReceiveCargo(ctx context.Context, cmd ReceiveCargo) (Receipt, error)
	RecordCargoMeasurement(ctx context.Context, cmd RecordCargoMeasurement) (Receipt, error)
	RecordScreeningResult(ctx context.Context, cmd RecordScreeningResult) (Receipt, error)
	CompleteReceipt(ctx context.Context, cmd CompleteReceipt) (Receipt, error)
	ResolveCargoDiscrepancy(ctx context.Context, cmd ResolveCargoDiscrepancy) (Receipt, error)
	CloseCargoLot(ctx context.Context, cmd CloseCargoLot) (Receipt, error)
	OpenLoadExecution(ctx context.Context, cmd OpenLoadExecution) (Receipt, error)
	StartLoading(ctx context.Context, cmd StartLoading) (Receipt, error)
	RecordCargoLoaded(ctx context.Context, cmd RecordCargoLoaded) (Receipt, error)
	RecordCargoUnloaded(ctx context.Context, cmd RecordCargoUnloaded) (Receipt, error)
	PlaceLoadExecutionOnHold(ctx context.Context, cmd PlaceLoadExecutionOnHold) (Receipt, error)
	ReleaseLoadExecutionHold(ctx context.Context, cmd ReleaseLoadExecutionHold) (Receipt, error)
	CloseLoadExecution(ctx context.Context, cmd CloseLoadExecution) (Receipt, error)
	RecordCarrierLoadEvidence(ctx context.Context, cmd RecordCarrierLoadEvidence) (Receipt, error)
	ResolveLoadingDiscrepancy(ctx context.Context, cmd ResolveLoadingDiscrepancy) (Receipt, error)
}

// ReceiptLine is one immutable receipt of packs into a lot.
type ReceiptLine struct {
	LineNo      int
	Packs       int64
	PackageType string
	Condition   string
	Evidence    Evidence
	ReceivedAt  time.Time // business time
	EnteredAt   time.Time // when PHY recorded it (T1)
}

// Measurement is one immutable measurement version of a lot.
type Measurement struct {
	Version       int
	GrossWeight   kernel.Quantity
	Volume        kernel.Quantity
	PackageGroups []PackageGroup
	Method        string
	Evidence      Evidence
	MeasuredAt    time.Time
	EnteredAt     time.Time
}

// Screening is an immutable screening result.
type Screening struct {
	ResultNo   int
	Outcome    string
	Screener   string
	Method     string
	Reference  string
	Evidence   Evidence
	RecordedAt time.Time
	EnteredAt  time.Time
}

// Discrepancy is a difference against the plan found at receipt: SHORT, OVER, DAMAGED (packs, unit PCS),
// WEIGHT_VARIANCE or VOLUME_VARIANCE (in the quantity's unit); Resolution is empty while open.
type Discrepancy struct {
	DiscrepancyNo int
	Kind          string
	Expected      int64
	Observed      int64
	Unit          string
	RaisedAt      time.Time
	Resolution    string
	Reference     string
	Reason        string
	ResolvedAt    time.Time
}

// CargoLot is a house's cargo at a facility: what was received, measured and screened, the plan it was
// compared with, its discrepancies, and its live loaded packs across every load execution.
type CargoLot struct {
	TenantID            string
	LotID               string
	Revision            int64
	ShipmentID          string
	Facility            Facility
	Mode                string
	State               string // RECEIVING, RECEIVED, CLOSED
	ExpectedFULRevision int64
	ExpectedPlanVersion int
	ExpectedPacks       int64
	ExpectedGrossWeight kernel.Quantity
	ExpectedVolume      kernel.Quantity
	ReceivedPacks       int64
	LoadedPacks         int64
	Screening           string // the latest outcome; empty when none was recorded
	Lines               []ReceiptLine
	Measurements        []Measurement
	Screenings          []Screening
	Discrepancies       []Discrepancy
	CompletedAt         time.Time
	ClosedAt            time.Time
}

// LoadLine is one immutable line of a lot's packs tendered under a house's membership entry; LivePacks,
// LiveGrossWeight and LiveVolume are what its reversals leave.
type LoadLine struct {
	LineNo          int
	ShipmentID      string
	LotID           string
	EntryNo         int
	Packs           int64
	GrossWeight     kernel.Quantity
	Volume          kernel.Quantity
	Basis           string // MEASURED or APPORTIONED
	MeasurementNo   int    // the lot's measurement version apportioned from
	LivePacks       int64
	LiveGrossWeight kernel.Quantity
	LiveVolume      kernel.Quantity
	Evidence        Evidence
	LoadedAt        time.Time
	EnteredAt       time.Time
}

// Reversal reverses packs of a load line.
type Reversal struct {
	ReversalNo  int
	LineNo      int
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity
	Reason      string
	Evidence    Evidence
	ReversedAt  time.Time
	EnteredAt   time.Time
}

// Hold is a hold placed on an execution, and its release.
type Hold struct {
	HoldNo          int
	Reason          string
	LotID           string // the lot that failed screening
	Text            string
	Evidence        Evidence
	PlacedAt        time.Time
	Released        bool
	ReleaseEvidence Evidence
	ReleasedAt      time.Time
}

// CarrierEvidence is the carrier's status of tendered pieces, of the master or of one house.
type CarrierEvidence struct {
	EvidenceNo  int
	Fact        string
	ShipmentID  string // empty for the master
	Pieces      int64
	GrossWeight kernel.Quantity
	Transport   string
	Evidence    Evidence
	RecordedAt  time.Time
	EnteredAt   time.Time
}

// LoadingDiscrepancy is a difference the carrier's evidence shows against the tender; Resolution is empty
// while open.
type LoadingDiscrepancy struct {
	DiscrepancyNo int
	Kind          string // LoadConflict or LoadOffloaded
	EvidenceNo    int
	ShipmentID    string // empty for the master
	Expected      int64  // pieces tendered
	Observed      int64
	RaisedAt      time.Time
	Resolution    string
	Reference     string
	Reason        string
	ResolvedAt    time.Time
}

// LoadExecution is a tender of a consolidation's houses with its lines, reversals, holds, the carrier's
// evidence and the discrepancies it raised. TenderedPacks are its live packs.
type LoadExecution struct {
	TenantID        string
	ExecutionID     string
	Revision        int64
	ConsolidationID string
	Kind            string
	Mode            string
	Facility        Facility
	LoadUnitNo      int // the container a STUFFING fills (PHY V0.3)
	State           string
	TenderedPacks   int64
	OpenedAt        time.Time
	StartedAt       time.Time
	ClosedAt        time.Time
	CloseEvidence   Evidence
	Lines           []LoadLine
	Reversals       []Reversal
	Holds           []Hold
	CarrierEvidence []CarrierEvidence
	Discrepancies   []LoadingDiscrepancy
}

// GetCargoLot asks for a cargo lot.
type GetCargoLot struct {
	TenantID string
	LotID    string
}

// GetLoadExecution asks for a load execution.
type GetLoadExecution struct {
	TenantID    string
	ExecutionID string
}

// ListOpenDiscrepancies asks for open discrepancies of cargo lots and load executions, as a work list: of one
// consolidation (its executions and the lots of its live houses), of one shipment, or of the tenant. Limit is 1
// to 200; zero means 50.
type ListOpenDiscrepancies struct {
	TenantID        string
	ConsolidationID string
	ShipmentID      string
	Limit           int
}

// OpenDiscrepancy is a row of ListOpenDiscrepancies: of a cargo lot (LotID) or of a load execution
// (ExecutionID, with its consolidation).
type OpenDiscrepancy struct {
	LotID           string
	ExecutionID     string
	ConsolidationID string
	ShipmentID      string // empty for a master's loading discrepancy
	DiscrepancyNo   int
	Kind            string
	Expected        int64
	Observed        int64
	Unit            string
	RaisedAt        time.Time
}

// GetPhysicalLoadEvidence asks for a shipment's physical facts (PhysicalLoadEvidence v1).
type GetPhysicalLoadEvidence struct {
	TenantID   string
	ShipmentID string
}

// LotEvidence is what PhysicalLoadEvidence v1 tells of one of the shipment's lots: received packs, whether
// receipt was completed, the latest measurement version and screening result, loaded packs and open
// discrepancies.
type LotEvidence struct {
	LotID             string
	Revision          int64
	Facility          Facility
	State             string
	Completed         bool
	ReceivedPacks     int64
	LoadedPacks       int64
	Measurement       Measurement // zero Version when none
	Screening         Screening   // zero ResultNo when none
	OpenDiscrepancies []Discrepancy
}

// TenderEvidence is what PhysicalLoadEvidence v1 tells of a load execution with lines of the shipment: the
// shipment's lines (with their reversals applied); its live packs with their gross weight and volume, lines
// measured for themselves as measured and apportioned lines at their lot's latest measurement (so a terminal
// weight taken after the tender counts; volumes in different units are not added); the pieces of the shipment
// the carrier's evidence shows departed, arrived and offloaded (its own reports, or the master's when they
// cover the tender); the tender receipt, the carrier's evidence of the master and of the shipment, and their
// open discrepancies.
type TenderEvidence struct {
	ExecutionID       string
	Revision          int64
	ConsolidationID   string
	Kind              string
	State             string
	Facility          Facility
	LoadUnitNo        int // the container of a STUFFING (PHY V0.3), whose seal is the closing evidence
	LoadedPacks       int64
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
	Departed          int64
	Arrived           int64
	Offloaded         int64
	Lines             []LoadLine
	ClosedAt          time.Time
	CloseEvidence     Evidence
	CarrierEvidence   []CarrierEvidence
	OpenDiscrepancies []LoadingDiscrepancy
}

// PhysicalLoadEvidence is PhysicalLoadEvidence v1 (relationship 8, PHY -> FUL): a shipment's verified physical
// facts. FUL updates its fulfilment projection only from these and re-plans parts (F1) after a short receipt,
// shut-out or offload; it never writes them back. A shipment PHY holds nothing of yields empty lists.
type PhysicalLoadEvidence struct {
	TenantID      string
	ShipmentID    string
	ReceivedPacks int64
	LoadedPacks   int64
	Lots          []LotEvidence
	Tenders       []TenderEvidence
}

// PhysicalLoadEvidenceQueries is PhysicalLoadEvidence v1, which FUL reads (relationship 8).
type PhysicalLoadEvidenceQueries interface {
	GetPhysicalLoadEvidence(ctx context.Context, q GetPhysicalLoadEvidence) (PhysicalLoadEvidence, error)
}

// PhysicalQueries is PHY's read side for cargo lots and load executions. An unknown lot or execution yields
// ErrNotFound.
type PhysicalQueries interface {
	PhysicalLoadEvidenceQueries
	GetCargoLot(ctx context.Context, q GetCargoLot) (CargoLot, error)
	GetLoadExecution(ctx context.Context, q GetLoadExecution) (LoadExecution, error)
	ListOpenDiscrepancies(ctx context.Context, q ListOpenDiscrepancies) ([]OpenDiscrepancy, error)
}

// GetPhysicalCostBasis asks for a consolidation's cost basis (PhysicalCostBasis v1).
type GetPhysicalCostBasis struct {
	TenantID        string
	ConsolidationID string
}

// HouseCostBasis is one live house of PhysicalCostBasis v1: the scope PHY froze when it attached the house
// (Packs, GrossWeight, Volume) and what its tenders actually loaded (lines measured for themselves as
// measured, apportioned lines at their lot's latest measurement; packs, weight or volume in mixed units are
// not added and read zero). A house nothing was loaded of has zero LoadedPacks and a zero LastLoadedAt.
type HouseCostBasis struct {
	ShipmentID         string
	MembershipRevision int64
	Packs              int64
	GrossWeight        kernel.Quantity
	Volume             kernel.Quantity // zero when the shipment declares none
	LoadedPacks        int64
	LoadedGrossWeight  kernel.Quantity
	LoadedVolume       kernel.Quantity
	LastLoadedAt       time.Time // business time of the latest live load line
}

// PhysicalCostBasis is PhysicalCostBasis v1 (PHY -> FIN): the physical facts FIN apportions a consolidation's
// pooled costs over. Houses are the live houses in shipment order; LoadUnits, added in PHY V0.3, the load units
// planned and not removed, in unit order. FIN apportions from these and never writes them back.
type PhysicalCostBasis struct {
	TenantID               string
	ConsolidationID        string
	Revision               int64
	Mode                   string
	Kind                   string
	OperatingLegalEntityID string
	State                  string
	Houses                 []HouseCostBasis
	LoadUnits              []PlannedLoadUnit
}

// PhysicalCostBasisQueries is PhysicalCostBasis v1, which FIN reads.
type PhysicalCostBasisQueries interface {
	GetPhysicalCostBasis(ctx context.Context, q GetPhysicalCostBasis) (PhysicalCostBasis, error)
}
