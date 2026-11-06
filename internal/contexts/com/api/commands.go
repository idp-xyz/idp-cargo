// Package api is the published language of COM (Commercial Commitment): commands, the obligation
// snapshot query, events and error kinds. It imports only the standard library and the kernel.
package api

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// OrderLine is one goods or service requirement of an order; each line is allocated to obligations on
// its own.
type OrderLine struct {
	Description string
	Quantity    kernel.Quantity
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

// AllocateOrderToObligation commits part or all of an order line's remaining quantity to a new
// fulfillment obligation. Lines are numbered from 1 in the order they were recorded.
type AllocateOrderToObligation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	LineNo           int
	ObligationID     string
	Quantity         kernel.Quantity
	OccurredAt       time.Time
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
}

// Actions an Authorizer decides on.
const (
	ActionRecordCustomerOrder       = "RecordCustomerOrder"
	ActionAllocateOrderToObligation = "AllocateOrderToObligation"
)

// Authorizer decides whether a verified actor may perform an order action. The identity and policy layer
// supplies it.
type Authorizer interface {
	AuthorizeOrderAction(ctx context.Context, tenantID, actorID, action, orderID string) error
}

// Error kinds returned by Commands and Queries, wrapped around the original cause. Transports map these,
// not the causes, to their status codes.
var (
	ErrUnauthenticated     = errors.New("com: unauthenticated")
	ErrUnauthorized        = errors.New("com: unauthorized")
	ErrNotFound            = errors.New("com: not found")
	ErrConflict            = errors.New("com: conflicts with the stored state")
	ErrIdempotencyConflict = errors.New("com: command id reused with a different payload")
	ErrRejected            = errors.New("com: rejected by business rules")
)
