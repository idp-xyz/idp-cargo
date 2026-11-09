// Package api is the published language of COM (Commercial Commitment): commands, the obligation
// reservation contract for FUL, the obligation snapshot queries, events and error kinds. It imports only
// the standard library and the kernel.
package api

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// OrderLine is one goods requirement of an order: packs of one package type with their declared gross
// weight and, optionally, volume (C3). Each line is allocated to obligations on its own.
type OrderLine struct {
	Description string
	Packs       int64
	PackageType string // UN/ECE Recommendation 21 code, such as PX for pallets or CT for cartons
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity // zero when not declared
}

// RecordCustomerOrder records a customer's order against a quote RATE has accepted. The customer must be
// the quote's customer, and ORG must confirm it as a customer of the quote's legal entity at OccurredAt.
// TenantID and ActorID must come from a verified principal.
type RecordCustomerOrder struct {
	CommandID       string
	TenantID        string
	ActorID         string
	OrderID         string
	QuoteID         string
	CustomerPartyID string
	Lines           []OrderLine
	OccurredAt      time.Time
}

// AllocateOrderToObligation commits packs of an order line, with their declared gross weight and volume,
// to a new fulfillment obligation. Lines are numbered from 1 in the order they were recorded. The line's
// active obligations never exceed its packs, gross weight or volume; Volume is declared exactly when the
// line declares one, in the same unit.
type AllocateOrderToObligation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	LineNo           int
	ObligationID     string
	Packs            int64
	GrossWeight      kernel.Quantity
	Volume           kernel.Quantity
	OccurredAt       time.Time
}

// RevokeObligation revokes an active obligation that no shipment holds. COM writes a new version with
// status REVOKED, and the obligation's packs become allocatable again (C6).
type RevokeObligation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	ObligationID     string
	OccurredAt       time.Time
}

// AmendObligation replaces an active obligation that no shipment holds by a new obligation on the same
// line: one command revokes the old obligation and allocates NewObligationID (C6).
type AmendObligation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	ObligationID     string
	NewObligationID  string
	Packs            int64
	GrossWeight      kernel.Quantity
	Volume           kernel.Quantity
	OccurredAt       time.Time
}

// CancelCustomerOrder cancels an order for good. Its active obligations are revoked; while a shipment holds
// one of them, the order cannot be cancelled (C6).
type CancelCustomerOrder struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// ReserveObligationForFulfillment records that a FUL shipment holds an obligation version. The version
// must be the obligation's latest and ACTIVE, and the shipment must not hold the obligation already. Other
// shipments may hold it too, each planning a part of it (F1).
type ReserveObligationForFulfillment struct {
	CommandID         string
	TenantID          string
	ActorID           string
	ObligationID      string
	ObligationVersion int64
	ShipmentID        string
	OccurredAt        time.Time
}

// ReleaseObligationReservation clears the reservation a shipment holds on an obligation version, after FUL
// released the obligation or cancelled the shipment.
type ReleaseObligationReservation struct {
	CommandID         string
	TenantID          string
	ActorID           string
	ObligationID      string
	ObligationVersion int64
	ShipmentID        string
	OccurredAt        time.Time
}

// Receipt confirms a committed command. Replaying the same command returns the same receipt.
type Receipt struct {
	CommandID   string
	OrderID     string
	NewRevision int64
}

// Commands is COM's command entry point.
type Commands interface {
	RecordCustomerOrder(ctx context.Context, cmd RecordCustomerOrder) (Receipt, error)
	AllocateOrderToObligation(ctx context.Context, cmd AllocateOrderToObligation) (Receipt, error)
	RevokeObligation(ctx context.Context, cmd RevokeObligation) (Receipt, error)
	AmendObligation(ctx context.Context, cmd AmendObligation) (Receipt, error)
	CancelCustomerOrder(ctx context.Context, cmd CancelCustomerOrder) (Receipt, error)
}

// ObligationReservations is ObligationReservation v1, the contract FUL uses to write to COM (rule T4): FUL
// reserves an obligation version before planning it and releases it before COM may revoke it. COM never
// asks FUL; the reservation is COM's own fact.
type ObligationReservations interface {
	ReserveObligationForFulfillment(ctx context.Context, cmd ReserveObligationForFulfillment) (Receipt, error)
	ReleaseObligationReservation(ctx context.Context, cmd ReleaseObligationReservation) (Receipt, error)
}

// Actions an Authorizer decides on.
const (
	ActionRecordCustomerOrder             = "RecordCustomerOrder"
	ActionAllocateOrderToObligation       = "AllocateOrderToObligation"
	ActionRevokeObligation                = "RevokeObligation"
	ActionAmendObligation                 = "AmendObligation"
	ActionCancelCustomerOrder             = "CancelCustomerOrder"
	ActionReserveObligationForFulfillment = "ReserveObligationForFulfillment"
	ActionReleaseObligationReservation    = "ReleaseObligationReservation"
)

// Authorizer decides whether a verified actor may perform an order action. The identity and policy layer
// supplies it. For the reservation commands, orderID is the order that holds the obligation.
type Authorizer interface {
	AuthorizeOrderAction(ctx context.Context, tenantID, actorID, action, orderID string) error
}

// Error kinds returned by Commands, ObligationReservations and Queries, wrapped around the original cause.
// Transports map these, not the causes, to their status codes.
var (
	ErrUnauthenticated     = errors.New("com: unauthenticated")
	ErrUnauthorized        = errors.New("com: unauthorized")
	ErrNotFound            = errors.New("com: not found")
	ErrConflict            = errors.New("com: conflicts with the stored state")
	ErrIdempotencyConflict = errors.New("com: command id reused with a different payload")
	ErrRejected            = errors.New("com: rejected by business rules")
)
