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
	// ExecutionRework is a sealed STUFFING opened for rework (OpenStuffingForRework): the same execution, its seal
	// broken and kept in its reworks, unloaded by whole houses (UnloadHouseForRework) until it is sealed again with a
	// new seal (CloseLoadExecution: CLOSED) or, with no live packs, given up (CancelLoadExecution: CANCELLED). The unit
	// is IN_USE while its stuffing is in REWORK.
	ExecutionRework = "REWORK"
	// ExecutionCancelled is a stuffing given up with no live packs (CancelLoadExecution), terminal and kept as history.
	ExecutionCancelled = "CANCELLED"
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
//     CFS (PHY V0.3). A unit has at most one stuffing that is not cancelled (CancelLoadExecution gives up an
//     empty one);
//     its cargo is checked against the unit's limits as it is stuffed: a
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

// CancelLoadExecution gives up a stuffing that holds no live packs: a STUFFING that is OPEN, LOADING or ON_HOLD with
// nothing loaded, or with every loaded pack unloaded again. The reason is required (500 characters) and the business
// time is not before the execution's last change. The execution becomes CANCELLED: terminal, it keeps its lines,
// reversals and holds as history and takes no further command, and it records who gave it up, when and why
// (LoadExecution). A hold still placed ends with it. The container is PLANNED again: it can take a new stuffing (a new
// execution id) or be removed with RemovePlannedLoadUnit. A stuffing with live packs, a sealed one and a tender are
// refused (ErrRejected); a tender is closed with its receipt, a sealed container stays sealed. The event is
// LoadExecutionCancelled.v1, which carries the load unit; its reason, free text, stays out of it. Authorised as the
// other load execution commands are, on the execution's consolidation (ActionCancelLoadExecution).
type CancelLoadExecution struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// OpenStuffingForRework opens a sealed container for rework, before the consolidation departs (V2.85, the first step of
// "a house leaves a sealed container"). The execution is a STUFFING in state CLOSED of a CLOSED consolidation (not
// RECONCILED or CANCELLED), no carrier evidence has been recorded on it (the cargo has then left the forwarder's hands)
// and no loading discrepancy is open on it. The reason is required (500 characters) and the business time is not before
// the execution's last change. The execution becomes REWORK, the same execution and never a new one on the unit: its
// seal is broken and kept as history in the execution's Reworks with who asked for the rework (the actor of this
// command), when and why; the execution carries no close facts while it is in REWORK. In REWORK nothing is accepted but
// UnloadHouseForRework, CloseLoadExecution with a new seal, and CancelLoadExecution once no packs are left: no load,
// start, hold, carrier evidence or discrepancy, and no RecordCargoUnloaded. The consolidation is not reopened,
// reconciled or cancelled meanwhile. The event is LoadExecutionReworkOpened.v1, which carries the load unit as its
// item; the reason, free text, stays out of it. The receipt's ItemNo is the number of the rework. Authorised as the
// other load execution commands are, on the execution's consolidation (ActionOpenStuffingForRework). Nothing reads the
// rework yet but PHY's own views: FUL, SEA, DOC and FIN do not know the state, which the README says of each.
type OpenStuffingForRework struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// UnloadHouseForRework unloads one whole house from a container in REWORK, in one command: every live line of the house
// in this execution is reversed completely, each as a reversal with the reason UNSTUFFED and the evidence of the
// warehouse's confirmation (Evidence), dated OccurredAt, the business date of the unloading. The house must have live
// packs in this execution and in no other execution of the consolidation: a house loaded in more than one container is
// partial unloading, which is refused. A lot of the house that is CLOSED is opened again first (back to RECEIVED, and
// the lot records who, when and the execution and rework that caused it; no other command reopens a lot), which refuses
// a lot while the shipment has another lot in force at its facility. The events are CargoLotReopened.v1 for each lot
// opened again and CargoUnloaded.v1 for each reversal. The house stays attached to the consolidation with nothing
// loaded: it leaves in a later step. Until then the consolidation cannot reconcile (the house's loaded packs are fewer
// than its plan) and FIN reads the house at the stage PLANNED, with no unit entry. The receipt's ItemNo is the number
// of lines reversed.
type UnloadHouseForRework struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ExecutionID      string
	ExpectedRevision int64
	ShipmentID       string
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
	CancelLoadExecution(ctx context.Context, cmd CancelLoadExecution) (Receipt, error)
	OpenStuffingForRework(ctx context.Context, cmd OpenStuffingForRework) (Receipt, error)
	UnloadHouseForRework(ctx context.Context, cmd UnloadHouseForRework) (Receipt, error)
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

// LotReopen records that a closed lot was opened again by the unloading of its house from a container in rework
// (UnloadHouseForRework): the stuffing's execution and the number of its rework, the first reversal of the lot's packs
// in that unloading, the closing it undid (who closed the lot and when) and who reopened it and when (ReopenedAt is the
// business time of the unloading). A lot is reopened by nothing else; CargoLotReopened.v1 is its event.
type LotReopen struct {
	ReopenNo    int
	ExecutionID string
	ReworkNo    int
	ReversalNo  int
	ClosedBy    string
	ClosedAt    time.Time
	ReopenedBy  string
	ReopenedAt  time.Time
	EnteredAt   time.Time
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
	Reopens             []LotReopen // each time the lot was closed and opened again, in order
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

// Rework is one rework of a sealed stuffing, and with the broken seals of all of them the history of the seals the
// execution carried: who asked for it, when (AskedAt is the business time) and why, the seal that was broken (the whole
// close evidence, with who sealed the container and when), and how it ended: End is empty while the rework is open,
// SEALED when the container was sealed again (EndedBy, EndedAt, NewSealReference) or GIVEN_UP when the stuffing was
// given up with no packs left (EndedBy, EndedAt). FIN will require that whoever confirms an apportionment held back for
// a rework is not AskedBy.
type Rework struct {
	ReworkNo         int
	Reason           string
	AskedBy          string
	AskedAt          time.Time
	AskedEnteredAt   time.Time
	BrokenSeal       Evidence
	BrokenSealedBy   string
	BrokenSealedAt   time.Time
	End              string // empty, ReworkSealed or ReworkGivenUp
	EndedBy          string
	EndedAt          time.Time
	NewSealReference string
}

// How a rework ended.
const (
	ReworkSealed  = "SEALED"
	ReworkGivenUp = "GIVEN_UP"
)

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
	// CancelledBy, CancelledAt and CancelReason say who gave up a CANCELLED stuffing, when and why; empty otherwise.
	CancelledBy  string
	CancelledAt  time.Time
	CancelReason string
	// Reworks are the reworks of a stuffing, in order; a stuffing in REWORK has its last one open and no close facts.
	Reworks         []Rework
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

// TenderEvidence is what PhysicalLoadEvidence v1 tells of a load execution with lines of the shipment: the shipment's
// lines (with their reversals applied); its live packs with their gross weight and volume, lines measured for
// themselves as measured and apportioned lines at their lot's latest measurement (so a terminal weight taken after the
// tender counts; weights and volumes in different units are converted to grams and cubic centimetres before they are
// added, since V2.84; since V2.82 a lot loaded in more than one execution is measured over all of them, so the figures
// of a lot's executions add up to its measure, see below); the pieces of the shipment the carrier's evidence shows
// departed, arrived and offloaded (its own reports, or the master's when they cover the tender); the tender receipt,
// the carrier's evidence of the master and of the shipment, and their open discrepancies. A container's figure for a
// lot is its part of the lot's measure as the lot is loaded now: it can change after the container is sealed, when
// another container loads or unloads packs of the lot, when packs of the lot are measured for themselves elsewhere, or
// when the lot is measured again (a lot of 6 packs at 301 kg with 5 packs in a sealed container reads 250 kg there, and
// 251 kg once a second container takes the sixth pack). The digest of the packing list changes with it. The digest is
// there for a reader that keeps it (none does yet: the step that holds an apportionment back while a container is
// reworked will). FIN's close comparison today notices a moved figure because the house's recorded quantity differs
// from the one PHY's basis gives now, and asks for a new apportionment.
type TenderEvidence struct {
	ExecutionID       string
	Revision          int64
	ConsolidationID   string
	Kind              string
	State             string
	Facility          Facility
	LoadUnitNo        int    // the container of a STUFFING (PHY V0.3), whose seal is the closing evidence
	LoadUnitType      string // its ISO 6346 size-type (PHY V0.3)
	LoadedPacks       int64
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
	Departed          int64
	Arrived           int64
	Offloaded         int64
	Lines             []LoadLine
	ClosedAt          time.Time
	CloseEvidence     Evidence
	Reworks           []Rework // of a stuffing, with the broken seals; a stuffing in REWORK has no close evidence
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

// HouseCostBasis is one live house of PhysicalCostBasis v1: the scope PHY froze when it attached the house (Packs,
// GrossWeight, Volume) and what its tenders actually loaded (lines measured for themselves as measured, apportioned
// lines at their lot's latest measurement; weights and volumes in mixed units are converted to grams and cubic
// centimetres and added, since V2.84, and a volume not known for a line makes the house's volume read zero). A lot
// loaded in more than one load execution is measured over all of them (since V2.82): its apportioned packs share what
// the lot's measure leaves after the lines measured for themselves, so a house whose packs are all loaded and whose own
// measurements do not exceed its lots' reads the lots' measure. A house nothing was loaded of has zero LoadedPacks and
// a zero LastLoadedAt.
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
	// Units is what of the house is loaded in each load unit, in unit order: one entry for every unit whose stuffing
	// holds live packs of the house. A house loaded only by tenders has none.
	Units []HouseUnitLoad
}

// HouseUnitLoad is what of a house is loaded in one load unit: the live packs of its lines in the unit's stuffing, with
// their weight and volume measured as the house's totals are. Where the units of measure allow the house's totals to be
// added, the entries of a house loaded only by stuffings sum to its LoadedPacks, LoadedGrossWeight and LoadedVolume.
// Weights and volumes in different units are converted to grams (GRM) and cubic centimetres (CMQ) and added, in an
// entry and in the total, since V2.84: before it the total read zero where the entries' units differed and an entry
// added amounts in different units under one of them (weights) or read zero (volumes). A volume not known for a line
// (no volume measured, or an apportioned line of a lot measured without one) still makes the entry's volume read zero;
// a zero volume cannot tell a lot with no volume from that. A quantity in a unit PHY cannot convert makes the sum a
// quantity in that unit, which a reader that converts refuses. The entries of a house add up to the measure of its lot
// when all the lot's packs are loaded, however they are split over the stuffings (since V2.82): the lot's measure less
// what its lines measured for themselves took is shared over its apportioned packs, and divided over the stuffings by
// the largest remainder, equal remainders going to the lower unit number. Lines measured for themselves are believed:
// when they exceed the lot's measure nothing is left for the other packs and the entries add up to more than the lot. A
// lot part loaded takes a share of its measure rounded down.
type HouseUnitLoad struct {
	UnitNo            int
	ExecutionID       string
	LoadedPacks       int64
	LoadedGrossWeight kernel.Quantity
	LoadedVolume      kernel.Quantity
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
	PackingLists           []UnitPackingList // one for every load unit that has a stuffing, in unit order
	// FormerHouses are the houses that left the consolidation, in shipment order (V2.92). GivenUpStuffings are the
	// stuffings of the consolidation that were given up after a rework, in unit order and, for one unit, by stuffing.
	// Neither is part of any digest, and neither changes Revision.
	FormerHouses     []FormerHouse
	GivenUpStuffings []GivenUpStuffing
}

// How a house left a consolidation (FormerHouse.End).
const (
	FormerDetached    = "DETACHED"
	FormerTransferred = "TRANSFERRED"
	FormerCancelled   = "SHIPMENT_CANCELLED"
)

// Where a former house's rework came from (FormerHouse.ReworkFrom).
const (
	ReworkFromEntry    = "ENTRY"    // the membership entry that ended names it (the house left a CLOSED consolidation)
	ReworkFromReversal = "REVERSAL" // the entry names none; the reversal lines of the house say which rework unloaded it
	ReworkFromNone     = "NONE"     // no rework unloaded the house
)

// FormerHouse is a house whose latest membership entry in the consolidation ended by a detach, a transfer or the
// shipment's cancellation: one per house, listed in shipment order, whatever the house's earlier entries were. The
// latest entry decides: a house that left and was attached to the consolidation again is live and not listed, and one
// that left, came back and then had its shipment cancelled is listed as cancelled, with that entry's reason, person and
// times. An entry ended by a refresh is never the latest (the house stays with a new entry).
//
// End is DETACHED, TRANSFERRED or SHIPMENT_CANCELLED; for a transfer TransferredTo is the consolidation the house went
// to. Reason and ReasonText are the operator's (SHIPMENT_CANCELLED for a cancellation, PHY's own). EndedBy is who ended
// the entry, EndedAt the business time and EndedEnteredAt when PHY recorded it.
//
// The rework is the latest rework of a sealed container of the consolidation in which the house was unloaded, as the
// stuffing's Reworks give it (ReworkExecutionID, ReworkNo, ReworkUnitNo and ReworkAskedBy; the stuffing may be sealed
// again or given up). ReworkFrom says where PHY found it: ENTRY when the entry that ended names it (the database
// records it only for an entry ended in a CLOSED consolidation), REVERSAL when the entry names none but the house's
// reversal lines in the consolidation's stuffings carry a rework number (the house left an OPEN consolidation after its
// container was emptied and the consolidation reopened, or its shipment was cancelled), and NONE when no rework
// unloaded the house (shut out before stuffing, or never loaded). A reversal-derived rework is the latest that unloaded
// the house since the entry began (only reversals PHY recorded at or after the start of the house's latest entry count,
// so the reversals of an earlier stay do not), even if the house was loaded again in another stuffing afterwards.
// ReworkUnitNo and ReworkAskedBy are empty when the stuffing is not found among the consolidation's.
type FormerHouse struct {
	ShipmentID        string
	End               string
	TransferredTo     string
	Reason            string
	ReasonText        string
	EndedBy           string
	EndedAt           time.Time
	EndedEnteredAt    time.Time
	ReworkFrom        string
	ReworkExecutionID string
	ReworkNo          int
	ReworkUnitNo      int
	ReworkAskedBy     string
}

// GivenUpStuffing is a stuffing of the consolidation in state CANCELLED (given up with no packs left) that has at least
// one rework: its unit, its execution and its reworks as UnitPackingList.Reworks gives them, the last of which ended
// GIVEN_UP and the earlier ones SEALED. A cancelled stuffing has no packing list; this is where its reworks are read.
type GivenUpStuffing struct {
	UnitNo      int
	ExecutionID string
	Reworks     []Rework
}

// UnitPackingList is what one load unit holds as this basis reads it, with a digest of it.
// A container's figure for a lot is its part of the lot's measure as the lot is loaded now: it can change after the
// container is sealed, when another container loads or unloads packs of the lot, when packs of the lot are measured
// for themselves elsewhere, or when the lot is measured again (a lot of 6 packs at 301 kg with 5 packs in a sealed
// container reads 250 kg there, and 251 kg once a second container takes the sixth pack). The digest of the packing
// list changes with it. The digest is there for a reader that keeps it (none does yet: the step that holds an
// apportionment back while a container is reworked will). FIN's close comparison today notices a moved figure because
// the house's recorded quantity differs from the one PHY's basis gives now, and asks for a new apportionment.
//
// Two bases answer the same Digest for a unit exactly when they answer the same stuffing, state and lines for it: a
// reader that kept a digest can tell that anything it would apportion on has changed, whatever changed it (a load, an
// unload, the seal, a lot measured again, more packs received of a lot, a load or unload in another
// container). Digest is the SHA-256, in hexadecimal, of this text: the lines "unit
// <UnitNo>", "execution <ExecutionID>" and "state <ExecutionState>", the strings as Go quoted strings, and then for
// each line in shipment order the line "line <ShipmentID> <LoadedPacks> <weight unit> <weight amount> <volume unit>
// <volume amount>" (strings quoted, numbers in decimal); every line ends with a line feed. A unit with a stuffing and
// no live packs has no lines and still has a digest.
type UnitPackingList struct {
	UnitNo         int
	ExecutionID    string
	ExecutionState string
	Lines          []UnitPackingLine // the houses with live packs in the unit, in shipment order
	Digest         string            // SHA-256, hexadecimal
	// Reworks are the unit's stuffing's reworks, in order: who asked for each, when and why, the seal it broke and how
	// it ended. FIN reads who asked from here. They are not part of the Digest, whose text is unchanged: the digest
	// moves with the state, REWORK while the container is open for rework, and with the lines. It is not a witness of a
	// rework: a rework with nothing unloaded and a reseal leaves the state and the lines as they were, so the digest is
	// the same, and a reader that must know of a rework reads Reworks.
	Reworks []Rework
	// Limits is the unit's cargo by the allotted figures against the unit's default limits. It is not part of the
	// Digest.
	Limits UnitLimitsCheck
}

// UnitLimitsCheck is a unit's cargo, as its packing list reads it, against the default limits of its container type,
// worked out on every read by the rule that places the OVERWEIGHT hold. The hold and this field come from one function,
// so they agree with each other; they do not always equal the sum of the entries published for the unit's houses: a
// house with a line or a lot measure without a volume publishes no volume where the check counts the volume that is
// known, the check converts each line to grams and cubic centimetres and rounds it up where the published figure of a
// house whose lines share one unit is added as it is (a house with lines in different units is converted line by line
// since V2.84, as the check does). A container can be over without a hold: a sealed container takes none, and a person
// may have released the hold. This field shows the state at the time of reading; nothing in the repository reads it
// yet.
type UnitLimitsCheck struct {
	Checked      bool   // false when a measure is in a unit that cannot be converted: Unchecked names it, and the rest is not known
	Unchecked    string // the unit code that was not converted
	CargoGrams   int64
	CargoCubicCm int64
	OverMaxGross bool // the container's tare and the cargo are over its default maximum gross
	OverCapacity bool // the cargo is over its default capacity
}

// UnitPackingLine is what of one house is loaded in a unit: the same figures as the house's HouseUnitLoad for it.
type UnitPackingLine struct {
	ShipmentID        string
	LoadedPacks       int64
	LoadedGrossWeight kernel.Quantity
	LoadedVolume      kernel.Quantity
}

// PhysicalCostBasisQueries is PhysicalCostBasis v1, which FIN reads.
type PhysicalCostBasisQueries interface {
	GetPhysicalCostBasis(ctx context.Context, q GetPhysicalCostBasis) (PhysicalCostBasis, error)
}
