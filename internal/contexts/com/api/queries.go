package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// CustomerRef identifies the ORG party an order is for, at the revision ORG confirmed when the order was
// recorded.
type CustomerRef struct {
	PartyID       string
	PartyRevision int64
}

// ObligationSnapshot is the immutable obligation snapshot of FulfillmentObligationAllocated v1: what COM
// committed to fulfil, for FUL to plan against. It carries no names, which FUL does not consume (MD-05).
type ObligationSnapshot struct {
	TenantID          string
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	OrderLineNo       int
	Description       string
	Quantity          kernel.Quantity
	LegalEntityID     string
	Customer          CustomerRef
	QuoteID           string
	QuoteRevision     int64
	AllocatedAt       time.Time
}

// GetObligationSnapshot asks for one version of an obligation. TenantID must come from a verified
// principal.
type GetObligationSnapshot struct {
	TenantID     string
	ObligationID string
	Version      int64
}

// Queries is COM's read side for other contexts. An unknown obligation or version yields ErrNotFound.
type Queries interface {
	GetObligationSnapshot(ctx context.Context, q GetObligationSnapshot) (ObligationSnapshot, error)
}
