// Package api is the published language of FUL (Freight Fulfillment): shipment plan commands,
// ShipmentPlanSnapshot v1, events and error kinds. It imports only the standard library and the kernel.
package api

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// CreateShipmentFromObligation starts a shipment from a part of one COM obligation version. COM must
// report the version ACTIVE, the obligation's latest, and reserved for ShipmentID, which the caller
// reserves through COM's ObligationReservation v1 first. ShipmentID is the shipment's own identity, never
// an order id. TenantID and ActorID must come from a verified principal.
//
// The part (F1) is Packs, GrossWeight and Volume, in the obligation's units, with Volume exactly when the
// obligation declares one. Zero Packs plans all that other shipments leave of the obligation. The live parts
// of an obligation never exceed it; the part with its last packs takes all its weight and volume left.
type CreateShipmentFromObligation struct {
	CommandID         string
	TenantID          string
	ActorID           string
	ShipmentID        string
	ObligationID      string
	ObligationVersion int64
	Packs             int64
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
	OccurredAt        time.Time
}

// AddObligationToShipment plans a part of one more obligation version of the shipment's customer and legal
// entity, with the same COM checks and part rules, while the plan is a draft.
type AddObligationToShipment struct {
	CommandID         string
	TenantID          string
	ActorID           string
	ShipmentID        string
	ExpectedRevision  int64
	ObligationID      string
	ObligationVersion int64
	Packs             int64
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
	OccurredAt        time.Time
}

// ChangeShipmentPart replans the part of an obligation version a shipment plans, with a reason (F1): a
// carrier's shortload is replanned by reducing the part here and planning the rest in another shipment;
// COM's commitment is unchanged. Packs must be positive; the part rules are those of
// CreateShipmentFromObligation. A confirmed plan that changes must be confirmed again.
type ChangeShipmentPart struct {
	CommandID         string
	TenantID          string
	ActorID           string
	ShipmentID        string
	ExpectedRevision  int64
	ObligationID      string
	ObligationVersion int64
	Packs             int64
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
	Reason            string
	OccurredAt        time.Time
}

// AmendShipmentPlan writes a new plan version with a changed route and a reason. Locations are UN/LOCODEs;
// a draft may leave parts of the route empty. A confirmed plan that changes must be confirmed again.
type AmendShipmentPlan struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ShipmentID       string
	ExpectedRevision int64
	TransportMode    string // ModeAir, ModeSea, ModeRoad or ModeRail
	Origin           kernel.LocationCode
	Destination      kernel.LocationCode
	DepartureWindow  kernel.ValidityInterval
	Reason           string
	OccurredAt       time.Time
}

// ConfirmShipmentPlan confirms the latest plan version. The route must be complete, and COM must
// re-confirm every planned obligation ACTIVE, at its planned version and reserved for the shipment.
// Booking may still be pending.
type ConfirmShipmentPlan struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ShipmentID       string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// ReleaseObligationFromShipment stops planning an obligation version; the caller then releases COM's
// reservation. The last obligation leaves only by cancelling the shipment.
type ReleaseObligationFromShipment struct {
	CommandID         string
	TenantID          string
	ActorID           string
	ShipmentID        string
	ExpectedRevision  int64
	ObligationID      string
	ObligationVersion int64
	OccurredAt        time.Time
}

// CancelShipment cancels a shipment for good and releases every obligation it plans; the caller then
// releases COM's reservations. The plan history stays.
type CancelShipment struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ShipmentID       string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// RecordConsolAttachment records, in the shipment, that PHY attached it to a consolidation (FUL V0.4,
// relationship 8). FUL asks PHY's ConsolMembership v1 for the shipment's live membership, which must be in
// ConsolidationID and of the shipment's mode and route; FUL never decides the attachment (decision N1-PHY,
// OD-1). A current attachment to another consolidation, or to an earlier membership entry of the same one,
// ends with it. The shipment must not be cancelled.
type RecordConsolAttachment struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ShipmentID       string
	ExpectedRevision int64
	ConsolidationID  string
	OccurredAt       time.Time
}

// RecordConsolDetachment records that the shipment left a consolidation in PHY: ConsolMembership v1 must
// hold no live entry of the shipment in ConsolidationID. A cancelled shipment's attachment ends with
// CancelShipment instead.
type RecordConsolDetachment struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ShipmentID       string
	ExpectedRevision int64
	ConsolidationID  string
	OccurredAt       time.Time
}

// ConsolAttachmentCommands records PHY's consol membership in a shipment (FUL V0.4).
type ConsolAttachmentCommands interface {
	RecordConsolAttachment(ctx context.Context, cmd RecordConsolAttachment) (Receipt, error)
	RecordConsolDetachment(ctx context.Context, cmd RecordConsolDetachment) (Receipt, error)
}

// Transport modes.
const (
	ModeAir  = "AIR"
	ModeSea  = "SEA"
	ModeRoad = "ROAD"
	ModeRail = "RAIL"
)

// Receipt confirms a committed command. Replaying the same command returns the same receipt.
type Receipt struct {
	CommandID   string
	ShipmentID  string
	NewRevision int64
}

// Commands is FUL's command entry point.
type Commands interface {
	CreateShipmentFromObligation(ctx context.Context, cmd CreateShipmentFromObligation) (Receipt, error)
	AddObligationToShipment(ctx context.Context, cmd AddObligationToShipment) (Receipt, error)
	AmendShipmentPlan(ctx context.Context, cmd AmendShipmentPlan) (Receipt, error)
	ConfirmShipmentPlan(ctx context.Context, cmd ConfirmShipmentPlan) (Receipt, error)
	ReleaseObligationFromShipment(ctx context.Context, cmd ReleaseObligationFromShipment) (Receipt, error)
	ChangeShipmentPart(ctx context.Context, cmd ChangeShipmentPart) (Receipt, error)
	CancelShipment(ctx context.Context, cmd CancelShipment) (Receipt, error)
}

// Actions an Authorizer decides on.
const (
	ActionCreateShipmentFromObligation  = "CreateShipmentFromObligation"
	ActionAddObligationToShipment       = "AddObligationToShipment"
	ActionAmendShipmentPlan             = "AmendShipmentPlan"
	ActionConfirmShipmentPlan           = "ConfirmShipmentPlan"
	ActionReleaseObligationFromShipment = "ReleaseObligationFromShipment"
	ActionChangeShipmentPart            = "ChangeShipmentPart"
	ActionCancelShipment                = "CancelShipment"
	ActionRecordConsolAttachment        = "RecordConsolAttachment"
	ActionRecordConsolDetachment        = "RecordConsolDetachment"
)

// Authorizer decides whether a verified actor may perform a shipment action. The identity and policy
// layer supplies it.
type Authorizer interface {
	AuthorizeShipmentAction(ctx context.Context, tenantID, actorID, action, shipmentID string) error
}

// Error kinds returned by Commands and Queries, wrapped around the original cause. Transports map these,
// not the causes, to their status codes.
var (
	ErrUnauthenticated     = errors.New("ful: unauthenticated")
	ErrUnauthorized        = errors.New("ful: unauthorized")
	ErrNotFound            = errors.New("ful: not found")
	ErrConflict            = errors.New("ful: conflicts with the stored state")
	ErrIdempotencyConflict = errors.New("ful: command id reused with a different payload")
	ErrRejected            = errors.New("ful: rejected by business rules")
)
