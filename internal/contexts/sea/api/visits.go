package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// SEA V0.2 (sea freight part 1e-i): container visits. A container visit is one use of one container for one ocean
// movement: the container assigned to a planned load unit of the consolidation, the seals applied to it, and the
// record that it is stuffed and sealed, the verified gross mass submitted for it, and the carrier's or the terminal's
// facts about it. OceanDocFacts come later.

// AssignContainer assigns a container to a planned load unit of an OPEN movement of the consolidator's own
// container (SEA_LCL_OWN): its ISO 6346 number (owner code, category, serial and check digit, validated), its
// size-type and, when known, its own tare and maximum gross from the CSC plate or the carrier. A container has one
// live visit in the tenant, and a load unit of a movement one live container. LoadUnitNo is the number PHY gave
// the planned unit; SEA does not ask PHY for it, and RecordStuffingDone checks it against what FUL relays. The
// assignment is dated not before the movement was opened; the visit then has its own history.
type AssignContainer struct {
	CommandID       string
	TenantID        string
	ActorID         string
	VisitID         string
	MovementID      string
	LoadUnitNo      int
	ContainerNumber string // such as CSQU3054383; trimmed and written in capital letters
	SizeType        string // ISO 6346 size-type, such as 22G1
	TareKg          int64  // 0 when the plate is not known yet
	MaxGrossKg      int64
	PlateSource     string // PlateCSC or PlateCarrier, with the plate data
	OccurredAt      time.Time
}

// RecordContainerSeal records a seal applied to the container: its number, the party that applied it (the
// carrier, the shipper or customs), when, and the source it was read from. A further seal replaces the current
// one, which stays as history (a customs inspection reseals).
type RecordContainerSeal struct {
	CommandID        string
	TenantID         string
	ActorID          string
	VisitID          string
	ExpectedRevision int64
	Number           string // capital letters and digits with single spaces, hyphens or slashes inside, 35 at most
	Party            string // SealCarrier, SealShipper or SealCustoms
	AppliedAt        time.Time
	Source           string
	OccurredAt       time.Time
}

// AcceptCarrierLoadingConflict releases the conflict between SEA's records and a carrier's loading report at a reseal
// (V2.89): the pending item GATED_IN_BEFORE_RESEAL that is raised only because the carrier reported a loading (naming
// the container, or naming none while it is the movement's only live container) dated before PHY sealed the container
// again, while the container's own events show neither a loading nor a stay at the terminal then. The operator, having
// checked, states a Reason (at most 500 characters, required), which is kept with who and when; the item then reads as
// a warning. The release covers that report against that seal of PHY's and nothing else. It is accepted while the
// movement is OPEN, on a container that has such an item; an item raised by the container's own loading or by its
// gate-in with no return cannot be released this way. The receipt names the release (ReleaseNo). The published facts
// do not change.
type AcceptCarrierLoadingConflict struct {
	CommandID        string
	TenantID         string
	ActorID          string
	VisitID          string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// RecordStuffingDone records that an assigned container is stuffed and sealed. A seal must be recorded, and the
// movement's live houses must carry, as FUL relays it from PHY (OD-2), one sealed stuffing of the visit's load
// unit; refresh the houses first when FUL has newer evidence. The visit keeps PHY's stuffing and becomes STUFFED:
// its container is then named in the houses' per-container breakdown. It is dated not before PHY sealed.
//
// PHY keeps the seal it closed the stuffing with as it was keyed, and the size-type of the unit it planned; neither
// is corrected there once the container is sealed. When that seal is none of the visit's seals (compared by
// letters and digits alone, whatever the case and the separators) or that size-type is not the visit's, the command
// is refused unless it states a DifferenceReason, which the visit keeps. A container assigned to the wrong unit is
// released and assigned again instead. Without a difference a reason is refused.
type RecordStuffingDone struct {
	CommandID        string
	TenantID         string
	ActorID          string
	VisitID          string
	ExpectedRevision int64
	DifferenceReason string // only when PHY's seal or size-type differs from the container's (500 characters)
	OccurredAt       time.Time
}

// ReleaseContainer gives an assigned container back unused, with a reason; its number and its load unit are free
// for another visit. A stuffed container is not released.
type ReleaseContainer struct {
	CommandID        string
	TenantID         string
	ActorID          string
	VisitID          string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// AssertVgm states the verified gross mass of a stuffed container (SOLAS): the mass, method 1 (the packed container
// weighed) or method 2 (cargo, packing and tare), the responsible party — the shipper named on the master bill,
// which for the consolidator's own container is the forwarder — and the authorised signatory's name. The mass is a
// positive weight and, where the plate's tare is known, not below it. It is not the cargo's net weight and not a sum
// of declared weights. The business time is the submission. After the current voyage's VGM cut-off it needs a late
// reason; without a cut-off, or while still before it, a reason is refused. A further assertion while one is already
// submitted supersedes it. The name is personal data: it is stored apart, a visit never carries it, and a read states
// a restricted purpose (ReadVgmSignatory).
type AssertVgm struct {
	CommandID                string
	TenantID                 string
	ActorID                  string
	VisitID                  string
	ExpectedRevision         int64
	Mass                     kernel.Quantity // grams, kilograms or tonnes
	Method                   string          // VgmMethod1 or VgmMethod2
	ResponsiblePartyID       string
	ResponsiblePartyRevision int64
	DeterminedAt             time.Time
	LateReason               string
	Signatory                string // the authorised person's name, 100 characters at most
	OccurredAt               time.Time
}

// RecordContainerEvent records a carrier or terminal fact about the container: the empty container released or picked
// up, its arrival at the CFS, its gate-in, its return from the terminal, its loading on board, or a roll to another
// vessel. It never moves a house. A container is gated in only once its verified gross mass is submitted, and loaded
// only once it is gated in. GATE_OUT_TERMINAL is the full container taken back out of the terminal to the forwarder's
// warehouse: it is accepted only while the container is gated in, returns the visit to VGM_SUBMITTED and is refused on
// a loaded container (handled outside the system).
// Stuffing is recorded by RecordStuffingDone. The source reference is recorded once: the same message again returns
// the same event and leaves the revision, and the same reference with other content is refused. Location is the
// UN/LOCODE where the fact happened. HappenedAt is when it happened; OccurredAt is when it is recorded. A loading
// that changes the on-board date the movement's published facts state returns a FINAL movement to OPEN.
type RecordContainerEvent struct {
	CommandID        string
	TenantID         string
	ActorID          string
	VisitID          string
	ExpectedRevision int64
	Kind             string
	Location         kernel.LocationCode
	HappenedAt       time.Time
	Source           string
	Reference        string
	OccurredAt       time.Time
}

// RecordVgmResponse records the carrier's answer to the latest assertion, once: accepted or rejected, with the
// source the answer was read from, not before the assertion was submitted. The visit stays VGM_SUBMITTED. Accepting
// it leaves a FINAL movement FINAL; rejecting it returns that movement to OPEN.
type RecordVgmResponse struct {
	CommandID        string
	TenantID         string
	ActorID          string
	VisitID          string
	ExpectedRevision int64
	Response         string // VgmAccepted or VgmRejected
	Source           string
	OccurredAt       time.Time
}

// VisitCommands is SEA's command entry point for container visits. Their receipts name the visit (VisitID) and,
// for a seal, its number in the visit's history (SealNo), for a verified gross mass the assertion's version
// (AssertionNo).
type VisitCommands interface {
	AssignContainer(ctx context.Context, cmd AssignContainer) (Receipt, error)
	RecordContainerSeal(ctx context.Context, cmd RecordContainerSeal) (Receipt, error)
	RecordStuffingDone(ctx context.Context, cmd RecordStuffingDone) (Receipt, error)
	ReleaseContainer(ctx context.Context, cmd ReleaseContainer) (Receipt, error)
	AssertVgm(ctx context.Context, cmd AssertVgm) (Receipt, error)
	RecordVgmResponse(ctx context.Context, cmd RecordVgmResponse) (Receipt, error)
	RecordContainerEvent(ctx context.Context, cmd RecordContainerEvent) (Receipt, error)
	AcceptCarrierLoadingConflict(ctx context.Context, cmd AcceptCarrierLoadingConflict) (Receipt, error)
}

// Actions of container visits; the subject is the visit.
const (
	ActionAssignContainer              = "AssignContainer"
	ActionRecordContainerSeal          = "RecordContainerSeal"
	ActionRecordStuffingDone           = "RecordStuffingDone"
	ActionReleaseContainer             = "ReleaseContainer"
	ActionAssertVgm                    = "AssertVgm"
	ActionRecordVgmResponse            = "RecordVgmResponse"
	ActionRecordContainerEvent         = "RecordContainerEvent"
	ActionAcceptCarrierLoadingConflict = "AcceptCarrierLoadingConflict"
	ActionReadVgmSignatory             = "ReadVgmSignatory"
	ActionReadVgmSignatoryLog          = "ReadVgmSignatoryAccess"
)

// Visit states, seal parties and plate sources.
const (
	VisitAssigned = "ASSIGNED"
	VisitStuffed  = "STUFFED"
	VisitVGM      = "VGM_SUBMITTED"
	VisitGatedIn  = "GATED_IN"
	VisitLoaded   = "LOADED"
	VisitReleased = "RELEASED"

	EventEmptyReleased = "EMPTY_RELEASED"
	EventEmptyPickedUp = "EMPTY_PICKED_UP"
	EventArrivedAtCFS  = "ARRIVED_AT_CFS"
	EventGateIn        = "GATE_IN_TERMINAL"
	EventGateOut       = "GATE_OUT_TERMINAL"
	EventLoaded        = "LOADED_ON_BOARD"
	EventRolled        = "ROLLED"

	VgmMethod1 = "METHOD_1" // the packed container was weighed
	VgmMethod2 = "METHOD_2" // cargo, packing and tare were summed

	VgmAccepted = "ACCEPTED"
	VgmRejected = "REJECTED"

	SealCarrier = "CARRIER"
	SealShipper = "SHIPPER"
	SealCustoms = "CUSTOMS"

	PlateCSC     = "CSC_PLATE"
	PlateCarrier = "CARRIER"
)

// ContainerSeal is a seal of a container visit.
type ContainerSeal struct {
	SealNo     int
	Number     string
	Party      string
	AppliedAt  time.Time
	Source     string
	RecordedBy string
	RecordedAt time.Time
}

// CarrierConflictRelease is an operator's release of a conflict with a carrier's loading report: the report (its fact
// number on the movement), PHY's seal and when it was sealed, the reason, who and when.
type CarrierConflictRelease struct {
	ReleaseNo   int
	FactNo      int
	PHYSeal     string
	PHYSealedAt time.Time
	Reason      string
	RecordedBy  string
	RecordedAt  time.Time
}

// ContainerVisit is a container visit with its seals, latest first.
type ContainerVisit struct {
	TenantID        string
	VisitID         string
	Revision        int64
	MovementID      string
	ConsolidationID string
	LoadUnitNo      int
	ContainerNumber string
	SizeType        string
	TareKg          int64 // 0 when not recorded
	MaxGrossKg      int64
	PlateSource     string
	Status          string
	SealNumber      string // the current seal; empty before one is recorded
	SealParty       string
	Seals           []ContainerSeal
	Assertions      []VgmAssertion           // latest first; none carries the signatory's name
	Events          []ContainerEvent         // latest first
	Releases        []CarrierConflictRelease // latest first
	ExecutionID     string                   // PHY's stuffing of the unit, once STUFFED
	// DifferenceReason is why the container was taken as stuffed although PHY's seal or size-type differed.
	DifferenceReason string
	StuffedAt        time.Time
	ReleaseReason    string
	ReleasedAt       time.Time
	AssignedBy       string
	AssignedAt       time.Time
}

// Queries of container visits.
type (
	GetContainerVisit struct{ TenantID, VisitID string }
	// ListContainerVisits lists a movement's visits in the order they were assigned, released ones included.
	ListContainerVisits struct{ TenantID, MovementID string }
	// FindLiveContainerVisit asks which live visit holds a container number in the tenant.
	FindLiveContainerVisit struct{ TenantID, ContainerNumber string }
)

// VisitQueries is SEA's read side for container visits. An unknown visit, or a number without a live visit,
// yields ErrNotFound.
type VisitQueries interface {
	GetContainerVisit(ctx context.Context, q GetContainerVisit) (ContainerVisit, error)
	ListContainerVisits(ctx context.Context, q ListContainerVisits) ([]ContainerVisit, error)
	FindLiveContainerVisit(ctx context.Context, q FindLiveContainerVisit) (ContainerVisit, error)
}

// Published event types of container visits; their subject is the visit, and they name its movement.
// ContainerEventRecorded carries the kind of the event as its reason: ContainerEventStuffed in V0.2.
const (
	ContainerAssigned      = "ContainerAssigned.v1"
	SealRecorded           = "SealRecorded.v1"
	ContainerEventRecorded = "ContainerEventRecorded.v1"
	ContainerReleased      = "ContainerReleased.v1"
	// CarrierLoadingConflictReleased names the release (release_no) of a conflict with a carrier's loading report.
	CarrierLoadingConflictReleased = "CarrierLoadingConflictReleased.v1"
	VgmAsserted                    = "VgmAsserted.v1"
	VgmResponseRecorded            = "VgmResponseRecorded.v1"

	ContainerEventStuffed = "STUFFED"
)

// ContainerEvent is a carrier or terminal fact about a container visit. Latest first on the visit.
type ContainerEvent struct {
	EventNo    int
	Kind       string
	Location   kernel.LocationCode
	HappenedAt time.Time
	Source     string
	Reference  string
	RecordedBy string
	RecordedAt time.Time
}

// VgmAssertion is one version of a container's verified gross mass. It has no signatory: that name is read, and
// logged, through ReadVgmSignatory.
type VgmAssertion struct {
	Version                  int
	MassGrams                int64
	Method                   string
	ResponsiblePartyID       string
	ResponsiblePartyRevision int64
	DeterminedAt             time.Time
	SubmittedAt              time.Time
	LateReason               string
	RecordedBy               string
	RecordedAt               time.Time
	Response                 string
	ResponseSource           string
	RespondedAt              time.Time
	ResponseBy               string
}

// ReadVgmSignatory reads the authorised person's name for one assertion. Purpose is a customs audit, a legal claim
// or the person's own request (kernel.PurposeCustomsAudit, PurposeLegalClaim, PurposeDataSubjectRequest). The read
// is logged with the actor and the purpose, and the log is kept three years.
type ReadVgmSignatory struct {
	TenantID string
	ActorID  string
	VisitID  string
	Version  int
	Purpose  string
}

// VgmSignatory is the name. It is not part of a container visit.
type VgmSignatory struct{ Name string }

// ReadVgmSignatoryAccess lists the logged reads of one assertion, newest first. An entry has no name.
type ReadVgmSignatoryAccess struct {
	TenantID string
	ActorID  string
	VisitID  string
	Version  int
}

// VgmSignatoryAccess is one logged read.
type VgmSignatoryAccess struct {
	AccessID   string
	ActorID    string
	Purpose    string
	RecordedAt time.Time
}

// VgmQueries reads a signatory's name, which is logged, and the log of those reads.
type VgmQueries interface {
	ReadVgmSignatory(ctx context.Context, q ReadVgmSignatory) (VgmSignatory, error)
	ReadVgmSignatoryAccess(ctx context.Context, q ReadVgmSignatoryAccess) ([]VgmSignatoryAccess, error)
}
