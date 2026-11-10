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

// CommercialTerms are the Incoterms rule, its edition (2010 or 2020) and named place, and the service level,
// a code of RATE's service-level list (C8).
type CommercialTerms struct {
	IncotermsRule    string
	IncotermsVersion string
	NamedPlace       string
	ServiceLevel     string
}

// RecordCustomerOrder records an order against a quote RATE has accepted. The ordering party,
// CustomerPartyID, must be the quote's recipient, and ORG must confirm it in the role the quote went to for
// the quote's legal entity at OccurredAt (R7). The order freezes the quote's commercial terms; Terms may
// complete what the quote left open, never change what it agreed (C8). The order policy of the quote's
// legal entity bounds how many orders the quote backs and how long after its validity the order may come
// (C1, C2). OccurredAt is the business time; it may be up to the record window earlier than the time COM
// records the command, and not later (T1). TenantID and ActorID must come from a verified principal.
type RecordCustomerOrder struct {
	CommandID       string
	TenantID        string
	ActorID         string
	OrderID         string
	QuoteID         string
	CustomerPartyID string
	Lines           []OrderLine
	Terms           CommercialTerms
	OccurredAt      time.Time
	// Transport and Parties are optional (C9, C11): the requested transport mode and movement type, and the
	// shipper, consignee and notify party, carried from ORG as for SetOrderParty.
	Transport Transport
	Parties   []OrderParty
}

// AmendOrderLine corrects what the customer declared for a line (C10): the line's next version, with a
// reason, without a new quote. A difference measured at receipt is not a correction; FUL records it. The
// line may not fall below what its active obligations commit, and keeps its package type and units while it
// has any; revoke or amend the obligations first (C6).
type AmendOrderLine struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	LineNo           int
	Description      string
	Packs            int64
	PackageType      string
	GrossWeight      kernel.Quantity
	Volume           kernel.Quantity // zero when not declared
	Reason           string
	OccurredAt       time.Time
}

// AddOrderLine adds a line to a recorded order, numbered after its lines, such as goods the customer added
// (C10).
type AddOrderLine struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	Line             OrderLine
	Reason           string
	OccurredAt       time.Time
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
	AmendOrderLine(ctx context.Context, cmd AmendOrderLine) (Receipt, error)
	AddOrderLine(ctx context.Context, cmd AddOrderLine) (Receipt, error)
	CloseCustomerOrder(ctx context.Context, cmd CloseCustomerOrder) (Receipt, error)
}

// CloseCustomerOrder records that the work on an order is finished. Only an order whose lines are all
// allocated can be closed. A closed order no longer changes: lines, obligations, parties and transport stay,
// it cannot be cancelled, and shipments may no longer reserve its obligations, though those that hold them
// may release them. Its contacts are then kept, restricted, until the retention period ends (P1).
type CloseCustomerOrder struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// SetOrderPolicy records the next version of the order policy of a legal entity, or of the tenant default
// when LegalEntityID is empty (C1, C2): how many orders one accepted quote may back (1 to 999) and for how
// many days after the quote's validity an order may still be recorded (0 to 30). ExpectedVersion is the
// version the caller read, 0 when none was recorded. The version takes effect when COM records it.
type SetOrderPolicy struct {
	CommandID         string
	TenantID          string
	ActorID           string
	LegalEntityID     string
	ExpectedVersion   int64
	MaxOrdersPerQuote int
	GraceDays         int
}

// PolicyReceipt confirms a recorded policy version. Replaying the same command returns the same receipt.
type PolicyReceipt struct {
	CommandID     string
	LegalEntityID string // empty for the tenant default
	Version       int64
}

// GetOrderPolicy asks for the order policy an order of the legal entity follows.
type GetOrderPolicy struct {
	TenantID      string
	LegalEntityID string
}

// OrderPolicy is the policy in force: Scope is LEGAL_ENTITY, TENANT (the tenant default) or BUILT_IN (none
// recorded: one order per quote and no grace); Version is 0 for BUILT_IN.
type OrderPolicy struct {
	Scope             string
	LegalEntityID     string
	Version           int64
	MaxOrdersPerQuote int
	GraceDays         int
}

// OrderPolicies is COM's entry point for order policies.
type OrderPolicies interface {
	SetOrderPolicy(ctx context.Context, cmd SetOrderPolicy) (PolicyReceipt, error)
	GetOrderPolicy(ctx context.Context, q GetOrderPolicy) (OrderPolicy, error)
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
	ActionAmendOrderLine                  = "AmendOrderLine"
	ActionAddOrderLine                    = "AddOrderLine"
	ActionSetOrderPolicy                  = "SetOrderPolicy"
	ActionCloseCustomerOrder              = "CloseCustomerOrder"
)

// Authorizer decides whether a verified actor may perform an order action. The identity and policy layer
// supplies it. For the reservation commands, orderID is the order that holds the obligation; for
// SetOrderPolicy and ListOrdersDueForContactErasure it is empty.
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
