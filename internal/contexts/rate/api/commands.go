// Package api is the published language of RATE (Rate, Quote & Commercial Rules): commands, the
// QuoteSnapshot v1 query, events and error kinds. It imports only the standard library and the kernel.
package api

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// QuoteLine is one priced charge. Cost is RATE-internal: it decides whether a price approval is needed
// and never appears in a QuoteSnapshot.
type QuoteLine struct {
	ChargeCode string
	Sell       kernel.Money
	Cost       kernel.Money
}

// CreateQuote prices a quote for a party that ORG confirms as a customer of the legal entity at
// OccurredAt. TenantID and ActorID must come from a verified principal.
type CreateQuote struct {
	CommandID       string
	TenantID        string
	ActorID         string
	QuoteID         string
	LegalEntityID   string
	CustomerPartyID string
	Currency        kernel.CurrencyCode
	Lines           []QuoteLine
	Validity        kernel.ValidityInterval
	OccurredAt      time.Time
}

// ApproveQuotePrice authorizes a quote whose margin is below the floor. The approver is ActorID and
// must not be the quote's author.
type ApproveQuotePrice struct {
	CommandID        string
	TenantID         string
	ActorID          string
	QuoteID          string
	ExpectedRevision int64
	ReasonRef        string
	OccurredAt       time.Time
}

// AcceptQuote records the customer's acceptance, with evidence, within the quote's validity.
type AcceptQuote struct {
	CommandID        string
	TenantID         string
	ActorID          string
	QuoteID          string
	ExpectedRevision int64
	EvidenceRef      string
	OccurredAt       time.Time
}

// Receipt confirms a committed command. Replaying the same command returns the same receipt.
type Receipt struct {
	CommandID   string
	QuoteID     string
	NewRevision int64
}

// Commands is RATE's command entry point.
type Commands interface {
	CreateQuote(ctx context.Context, cmd CreateQuote) (Receipt, error)
	ApproveQuotePrice(ctx context.Context, cmd ApproveQuotePrice) (Receipt, error)
	AcceptQuote(ctx context.Context, cmd AcceptQuote) (Receipt, error)
}

// Actions an Authorizer decides on.
const (
	ActionCreateQuote       = "CreateQuote"
	ActionApproveQuotePrice = "ApproveQuotePrice"
	ActionAcceptQuote       = "AcceptQuote"
)

// Authorizer decides whether a verified actor may perform a quote action. The identity and policy layer
// supplies it.
type Authorizer interface {
	AuthorizeQuoteAction(ctx context.Context, tenantID, actorID, action, quoteID string) error
}

// MarginPolicy supplies the margin floor, in basis points of the sell total, for a legal entity's
// quotes. A quote below it needs a price approval.
type MarginPolicy interface {
	MarginFloorBP(ctx context.Context, tenantID, legalEntityID string) (int, error)
}

// Error kinds returned by Commands and Queries, wrapped around the original cause. Transports map these,
// not the causes, to their status codes.
var (
	ErrUnauthenticated     = errors.New("rate: unauthenticated")
	ErrUnauthorized        = errors.New("rate: unauthorized")
	ErrNotFound            = errors.New("rate: not found")
	ErrConflict            = errors.New("rate: revision conflict")
	ErrIdempotencyConflict = errors.New("rate: command id reused with a different payload")
	ErrRejected            = errors.New("rate: rejected by business rules")
)
