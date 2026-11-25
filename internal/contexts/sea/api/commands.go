// Package api is the published language of SEA (Sea Freight) V0.1: the ocean movement of a sea LCL consolidation,
// its voyage legs and cut-offs taken from the carrier's or co-loader's booking confirmation, the master bill
// reference the carrier or co-loader states, and the house bill of lading (HBL) of every house. It imports only the
// standard library and the kernel.
//
// One ocean movement per sea consolidation holds the voyage (CargoWise: the consol's routing and master bill); a
// roll to another vessel the carrier confirms under the same booking moves the whole movement and leaves the houses
// as they are, while another booking is another movement. Vessel, voyage and cut-offs come only from BOOK's
// CarrierBooking v1. A house is a FUL shipment attached to the consolidation, as FUL reports it in
// OceanOperationPlan v1 (decision N1-PHY OD-2, option A); SEA draws its HBL number from the operating legal entity's
// series when it associates the house. Issuing and releasing bills of lading stay with DOC; container numbers,
// seals' parties and VGM come with the container visits (sea freight part 1e).
package api

import (
	"context"
	"errors"
	"time"
)

// OpenOceanMovement opens the ocean movement of a sea LCL consolidation from its booking: BOOK must report the
// booking SEA_LCL_CONSOL_EQUIPMENT (the consolidator's own container, movement kind SEA_LCL_OWN) or SEA_LCL_CO_LOAD
// (space of a co-loader, SEA_LCL_COLOAD), in force with confirmed capacity and allocated to the consolidation; the
// movement takes the carrier or co-loader, the operating legal entity, the port route and the voyage legs with
// their cut-offs from it. One open movement per consolidation. TenantID and ActorID must come from a verified
// principal; OccurredAt lies within the record window before SEA records the command (T1).
type OpenOceanMovement struct {
	CommandID       string
	TenantID        string
	ActorID         string
	MovementID      string
	ConsolidationID string
	BookingID       string
	OccurredAt      time.Time
}

// LinkCarrierBooking takes a newer capacity version of the movement's booking (a schedule change, or a roll to
// another vessel the carrier confirmed): the voyage legs and cut-offs are replaced, the old ones kept as
// superseded; houses and the master bill stay.
type LinkCarrierBooking struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// Evidence references a stored document: the draft bill, e-mail or portal page the number was read from. SEA keeps
// the reference and the SHA-256 of the content, not the content.
type Evidence struct {
	SHA256     string // 64 lower-case hex characters
	StorageID  string
	MediaType  string // such as application/pdf
	CapturedAt time.Time
}

// RecordMasterBill records the master bill reference of an open movement as its carrier or co-loader states it,
// with the evidence: a CARRIER_MBL or CARRIER_SEA_WAYBILL of the shipping line on a SEA_LCL_OWN movement, the
// COLOAD_BILL of the co-loader on a SEA_LCL_COLOAD one. The issuer is the movement's carrier or co-loader. A
// further number (or kind) supersedes the current one, which stays readable; a number is current on one movement
// of the tenant per issuer. It records a reference: the bill itself is the carrier's, and DOC's once received.
type RecordMasterBill struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	Number           string // capital letters and digits, with single hyphens or slashes inside; 35 characters at most
	Kind             string // MasterBillCarrierMBL, MasterBillCarrierSeaWaybill or MasterBillCoLoad
	Evidence         Evidence
	OccurredAt       time.Time
}

// RegisterHblSeries gives a legal entity its HBL numbering, once (numbers from idp-platform-go, MD-28): a template of
// capital letters and digits around one sequence {seq:N} (N from 4 to 10), such as SHAL{seq:7}, whose numbers are HBL
// numbers (sixteen characters at most), and the first number. HBL templates do not reset yet: SEA does not know the
// issuing office's time zone, so a year or month in the number could follow UTC.
type RegisterHblSeries struct {
	CommandID     string
	TenantID      string
	ActorID       string
	LegalEntityID string
	Template      string
	FirstNumber   int64
	OccurredAt    time.Time
}

// AssociateHouseBill associates a house with the movement. FUL's OceanOperationPlan v1 must report the shipment
// PLANNED by SEA and not as FCL, attached to the movement's consolidation, on its port route (OD-8) and of its
// operating legal entity (OD-10). The house keeps the HBL number it had under the legal entity; otherwise HBL is the
// number given, or empty to draw the next one from the entity's series (a number of the series given by hand to
// another shipment before is passed over). Its packs, gross weight and volume are
// frozen: PHY's measures when FUL relays them (basis RECEIVED or TENDERED), else FUL's plan (PLANNED); with its
// parts and their goods descriptions, and the sealed containers of the consolidation holding its packs as FUL
// relays them from PHY (the per-container breakdown, with PHY's evidence ids).
type AssociateHouseBill struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	ShipmentID       string
	HBL              string
	OccurredAt       time.Time
}

// RefreshHouseBillFacts takes a newer FUL plan of a house (a shortload, a changed part), or measures or sealed
// containers FUL newly relays from PHY, into a new entry with the same HBL; the same plan version, measures and
// containers again are refused as nothing to refresh.
type RefreshHouseBillFacts struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	ShipmentID       string
	OccurredAt       time.Time
}

// EndHouseBillAssociation ends a house's association once FUL reports the shipment no longer attached to the
// consolidation (detached or transferred) or cancelled. The HBL number stays the shipment's and moves with it.
type EndHouseBillAssociation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	ShipmentID       string
	OccurredAt       time.Time
}

// FinalizeOceanDocFacts publishes the next OceanDocFacts v1 of an OPEN movement and makes the movement FINAL.
// FUL's plan of every live house is read before the transaction. The facts state the on-board date when a loading
// is recorded: the earliest, as a container's LOADED_ON_BOARD event or the carrier's LOADED_ON_BOARD fact. A later
// link, refresh, end, new seal or new VGM assertion, the carrier's rejection, or a loading that changes that date,
// returns the movement to OPEN and leaves the published version as it was; finalizing again publishes the next
// version. Any other container or carrier fact, and the carrier's acceptance, do not.
type FinalizeOceanDocFacts struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// RecordOceanCarrierFact records a carrier report: loaded on board, departed, rolled or discharged. It names one
// live container of the movement, or the current master bill, or a live house's HBL. The same source reference
// again returns the fact already recorded. The movement's state and revision do not change, including when it is
// FINAL, with one exception: a LOADED_ON_BOARD report that changes the on-board date the published facts state
// (they state none, or a later one) returns a FINAL movement to OPEN, and the receipt names its next revision.
// HappenedAt is when the carrier says it happened; OccurredAt is when it is recorded. SHA256 is the carrier message.
type RecordOceanCarrierFact struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	Kind             string
	ContainerNumber  string
	BillNumber       string
	Vessel           string
	Voyage           string
	Source           string
	Reference        string
	SHA256           string
	HappenedAt       time.Time
	OccurredAt       time.Time
}

// CancelOceanMovement cancels an open movement without houses and without containers (every container visit
// released first), with a reason; its current master bill reference is voided.
type CancelOceanMovement struct {
	CommandID        string
	TenantID         string
	ActorID          string
	MovementID       string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// Receipt confirms a committed command. Replaying the same command returns the same receipt. House commands also
// name the house entry and its HBL, RecordMasterBill the master bill entry; RegisterHblSeries names the legal entity
// in MovementID's place. Container visit commands name the visit (VisitID, with its revision) instead of a
// movement, RecordContainerSeal the seal's number in the visit's history, AssertVgm or RecordVgmResponse the
// assertion's version, and RecordContainerEvent the event's number. RecordOceanCarrierFact names the fact; the
// movement's revision moves only when the fact returns a FINAL movement to OPEN.
type Receipt struct {
	CommandID    string
	MovementID   string
	NewRevision  int64
	EntryNo      int
	HBL          string
	BillNo       int
	VisitID      string
	SealNo       int
	AssertionNo  int
	EventNo      int
	ReleaseNo    int
	FactsVersion int
	FactNo       int
	// ChangeID names the change set a settings command is about (in MovementID's place, which is then empty);
	// SettingsVersion is the version of SEA's settings that it made (an approval), 0 for any other command; Notice is
	// what a settings proposal says besides its receipt, the same on a replay.
	ChangeID        string
	SettingsVersion int
	Notice          string
}

// Commands is SEA's command entry point.
type Commands interface {
	VisitCommands
	OpenOceanMovement(ctx context.Context, cmd OpenOceanMovement) (Receipt, error)
	LinkCarrierBooking(ctx context.Context, cmd LinkCarrierBooking) (Receipt, error)
	RecordMasterBill(ctx context.Context, cmd RecordMasterBill) (Receipt, error)
	RegisterHblSeries(ctx context.Context, cmd RegisterHblSeries) (Receipt, error)
	AssociateHouseBill(ctx context.Context, cmd AssociateHouseBill) (Receipt, error)
	RefreshHouseBillFacts(ctx context.Context, cmd RefreshHouseBillFacts) (Receipt, error)
	EndHouseBillAssociation(ctx context.Context, cmd EndHouseBillAssociation) (Receipt, error)
	FinalizeOceanDocFacts(ctx context.Context, cmd FinalizeOceanDocFacts) (Receipt, error)
	RecordOceanCarrierFact(ctx context.Context, cmd RecordOceanCarrierFact) (Receipt, error)
	CancelOceanMovement(ctx context.Context, cmd CancelOceanMovement) (Receipt, error)
}

// Actions an Authorizer decides on: the command names.
const (
	ActionOpenOceanMovement       = "OpenOceanMovement"
	ActionLinkCarrierBooking      = "LinkCarrierBooking"
	ActionRecordMasterBill        = "RecordMasterBill"
	ActionRegisterHblSeries       = "RegisterHblSeries" // subject: the legal entity
	ActionAssociateHouseBill      = "AssociateHouseBill"
	ActionRefreshHouseBillFacts   = "RefreshHouseBillFacts"
	ActionEndHouseBillAssociation = "EndHouseBillAssociation"
	ActionFinalizeOceanDocFacts   = "FinalizeOceanDocFacts"
	ActionRecordOceanCarrierFact  = "RecordOceanCarrierFact"
	ActionCancelOceanMovement     = "CancelOceanMovement"
)

// Authorizer decides whether a verified actor may perform an action on a movement (or, for a series, a legal
// entity).
type Authorizer interface {
	AuthorizeSeaAction(ctx context.Context, tenantID, actorID, action, subjectID string) error
}

// Error kinds returned by Commands and Queries, wrapped around the original cause.
var (
	ErrUnauthenticated     = errors.New("sea: unauthenticated")
	ErrUnauthorized        = errors.New("sea: unauthorized")
	ErrNotFound            = errors.New("sea: not found")
	ErrConflict            = errors.New("sea: conflicts with the stored state")
	ErrIdempotencyConflict = errors.New("sea: command id reused with a different payload")
	ErrRejected            = errors.New("sea: rejected by business rules")
)
