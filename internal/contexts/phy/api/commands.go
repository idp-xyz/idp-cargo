// Package api is the published language of PHY (Physical Logistics) for consolidations and their houses:
// consolidation and membership commands, ConsolMembership v1, events and error kinds. It imports only the
// standard library and the kernel.
//
// A consolidation (CargoWise: consol) is a transport organisation that several customers' houses share under
// one master: an export air consolidation under one MAWB, or an export ocean LCL consolidation in the
// forwarder's own container or on a co-loader's bill. Its identity is its own, never a MAWB, MBL, booking or
// co-load bill number. A house is a FUL shipment; PHY holds which consolidation it travels in (the
// membership) and commits first, FUL records the attachment after asking PHY (decision N1-PHY, OD-1).
package api

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Transport modes of a consolidation.
const (
	ModeAir = "AIR"
	ModeSea = "SEA"
)

// Consolidation kinds of the China export slice (decision N1-PHY, OD-6).
const (
	KindAirConsol    = "AIR_CONSOL"     // houses under one MAWB
	KindSeaLCLOwn    = "SEA_LCL_OWN"    // the forwarder stuffs its own container at a CFS
	KindSeaLCLCoload = "SEA_LCL_COLOAD" // houses are tendered to a co-loader's CFS and travel under its bill
)

// CreateConsolidation opens a consolidation. Origin and Destination are UN/LOCODEs, an alias carriers use,
// or for AIR an IATA airport code; PHY resolves them against the location reference data and freezes the
// UN/LOCODEs, for AIR the IATA airport codes, and the edition (F2). PlannedDeparture lies after OccurredAt and
// HouseCutOff not after PlannedDeparture. The operating legal entity is the forwarder entity that runs the
// consolidation and pays the carrier; in N1 every house belongs to it (OD-10). TenantID and ActorID must come
// from a verified principal; OccurredAt lies within the record window before PHY records the command (T1).
type CreateConsolidation struct {
	CommandID              string
	TenantID               string
	ActorID                string
	ConsolidationID        string
	Mode                   string // ModeAir or ModeSea
	Kind                   string // KindAirConsol for AIR; KindSeaLCLOwn or KindSeaLCLCoload for SEA
	OperatingLegalEntityID string
	Origin                 kernel.LocationCode
	Destination            kernel.LocationCode
	PlannedDeparture       time.Time
	HouseCutOff            time.Time
	OccurredAt             time.Time
}

// AmendConsolidation writes a new version of an OPEN or CLOSED consolidation with a reason: a flight or
// vessel change moves the planned departure and house cut-off of the whole consolidation and its houses
// (CargoWise practice). The route changes only while no house is attached; zero locations keep it.
type AmendConsolidation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ConsolidationID  string
	ExpectedRevision int64
	Origin           kernel.LocationCode
	Destination      kernel.LocationCode
	PlannedDeparture time.Time
	HouseCutOff      time.Time
	Reason           string
	OccurredAt       time.Time
}

// CloseConsolidation is the house cut-off: it freezes the membership. At least one house must be attached,
// and FUL must re-confirm every house PLANNED at the plan version PHY verified and allocated to this
// consolidation (FUL's attachment); otherwise the house must be refreshed or recorded in FUL first.
type CloseConsolidation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ConsolidationID  string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// ReopenConsolidation reopens a CLOSED consolidation with a reason (OD-5).
type ReopenConsolidation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ConsolidationID  string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// CancelConsolidation cancels an OPEN consolidation without houses, with a reason.
type CancelConsolidation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ConsolidationID  string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// Membership reasons (MembershipReason) for detaching or transferring a house.
const (
	ReasonRollover             = "ROLLOVER"
	ReasonFlightOrVesselChange = "FLIGHT_OR_VESSEL_CHANGE"
	ReasonCarrierChange        = "CARRIER_CHANGE"
	ReasonShutOut              = "SHUT_OUT"
	ReasonSpaceShortage        = "SPACE_SHORTAGE"
	ReasonCustomerRequest      = "CUSTOMER_REQUEST"
	ReasonWrongConsol          = "WRONG_CONSOL"
	ReasonShipmentCancelled    = "SHIPMENT_CANCELLED" // set by EndMembershipForCancelledShipment only
	ReasonOther                = "OTHER"              // needs ReasonText
)

// AttachHouse attaches a FUL shipment, whole, to an OPEN consolidation. PHY reads FUL's LoadIntent v1 first:
// the shipment must be PLANNED, of the consolidation's mode, origin and destination (OD-8) and legal entity
// (OD-10), and its departure window must contain the planned departure; cargo need not be received yet. A
// shipment travels in at most one consolidation on its main carriage. ExpectedMembershipRevision is 0 for a
// shipment never attached before. Afterwards the caller records the attachment in FUL
// (RecordConsolAttachment), as the S1 API client runs the steps (T6).
type AttachHouse struct {
	CommandID                  string
	TenantID                   string
	ActorID                    string
	ConsolidationID            string
	ShipmentID                 string
	ExpectedMembershipRevision int64
	OccurredAt                 time.Time
}

// DetachHouse takes a house out of an OPEN consolidation with a reason; the history stays. Afterwards the
// caller records the detachment in FUL (RecordConsolDetachment).
type DetachHouse struct {
	CommandID                  string
	TenantID                   string
	ActorID                    string
	ConsolidationID            string
	ShipmentID                 string
	ExpectedMembershipRevision int64
	Reason                     string
	ReasonText                 string
	OccurredAt                 time.Time
}

// TransferHouse moves a house from one OPEN consolidation to another of the same mode in one step (a
// rollover, a flight or vessel change), with the attach checks on the target. COM is not touched and FUL's
// parts stay as they are (F1); the caller then records the new attachment in FUL.
type TransferHouse struct {
	CommandID                  string
	TenantID                   string
	ActorID                    string
	ConsolidationID            string // the consolidation the house leaves
	ToConsolidationID          string
	ShipmentID                 string
	ExpectedMembershipRevision int64
	Reason                     string
	ReasonText                 string
	OccurredAt                 time.Time
}

// RefreshHouseIntent takes a changed FUL plan into the membership (a changed part, an amendment confirmed
// again): PHY reads LoadIntent v1 again, re-checks the attach rules and writes a new entry. The consolidation
// must be OPEN.
type RefreshHouseIntent struct {
	CommandID                  string
	TenantID                   string
	ActorID                    string
	ConsolidationID            string
	ShipmentID                 string
	ExpectedMembershipRevision int64
	OccurredAt                 time.Time
}

// EndMembershipForCancelledShipment ends the membership of a shipment FUL reports CANCELLED, in an OPEN or
// CLOSED consolidation.
type EndMembershipForCancelledShipment struct {
	CommandID                  string
	TenantID                   string
	ActorID                    string
	ConsolidationID            string
	ShipmentID                 string
	ExpectedMembershipRevision int64
	OccurredAt                 time.Time
}

// Receipt confirms a committed command. Replaying the same command returns the same receipt. Consolidation
// commands return the consolidation's new revision; membership commands return the shipment's new membership
// revision, the consolidation the house is in (or left) and the membership entry they wrote or ended.
type Receipt struct {
	CommandID       string
	ConsolidationID string
	ShipmentID      string // membership commands
	NewRevision     int64
	EntryNo         int // membership commands
}

// Commands is PHY's command entry point for consolidations and their houses.
type Commands interface {
	CreateConsolidation(ctx context.Context, cmd CreateConsolidation) (Receipt, error)
	AmendConsolidation(ctx context.Context, cmd AmendConsolidation) (Receipt, error)
	CloseConsolidation(ctx context.Context, cmd CloseConsolidation) (Receipt, error)
	ReopenConsolidation(ctx context.Context, cmd ReopenConsolidation) (Receipt, error)
	CancelConsolidation(ctx context.Context, cmd CancelConsolidation) (Receipt, error)
	AttachHouse(ctx context.Context, cmd AttachHouse) (Receipt, error)
	DetachHouse(ctx context.Context, cmd DetachHouse) (Receipt, error)
	TransferHouse(ctx context.Context, cmd TransferHouse) (Receipt, error)
	RefreshHouseIntent(ctx context.Context, cmd RefreshHouseIntent) (Receipt, error)
	EndMembershipForCancelledShipment(ctx context.Context, cmd EndMembershipForCancelledShipment) (Receipt, error)
}

// Actions an Authorizer decides on.
const (
	ActionCreateConsolidation               = "CreateConsolidation"
	ActionAmendConsolidation                = "AmendConsolidation"
	ActionCloseConsolidation                = "CloseConsolidation"
	ActionReopenConsolidation               = "ReopenConsolidation"
	ActionCancelConsolidation               = "CancelConsolidation"
	ActionAttachHouse                       = "AttachHouse"
	ActionDetachHouse                       = "DetachHouse"
	ActionTransferHouse                     = "TransferHouse" // asked for both consolidations
	ActionRefreshHouseIntent                = "RefreshHouseIntent"
	ActionEndMembershipForCancelledShipment = "EndMembershipForCancelledShipment"
)

// Authorizer decides whether a verified actor may perform an action on a consolidation. The identity and
// policy layer supplies it.
type Authorizer interface {
	AuthorizeConsolidationAction(ctx context.Context, tenantID, actorID, action, consolidationID string) error
}

// Error kinds returned by Commands and Queries, wrapped around the original cause. Transports map these,
// not the causes, to their status codes.
var (
	ErrUnauthenticated     = errors.New("phy: unauthenticated")
	ErrUnauthorized        = errors.New("phy: unauthorized")
	ErrNotFound            = errors.New("phy: not found")
	ErrConflict            = errors.New("phy: conflicts with the stored state")
	ErrIdempotencyConflict = errors.New("phy: command id reused with a different payload")
	ErrRejected            = errors.New("phy: rejected by business rules")
)
