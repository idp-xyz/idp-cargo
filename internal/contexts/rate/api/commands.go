// Package api is the published language of RATE (Rate, Quote & Commercial Rules): commands, the
// QuoteSnapshot v1 query, events and error kinds. It imports only the standard library and the kernel.
package api

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// QuoteLine is one priced charge (R8, R9). Sell and Cost each carry their currency; a line in a currency
// other than the quote's needs an ExchangeRate. A zero sell amount is a charge included elsewhere; a
// negative one is a discount, which has no cost and makes the quote need a price approval. Cost is
// RATE-internal: it decides whether a price approval is needed and never appears in a QuoteSnapshot.
type QuoteLine struct {
	ChargeCode string
	Sell       kernel.Money
	Cost       kernel.Money
}

// ExchangeRate converts amounts in From into the quote currency (R8): one unit of From is
// Rate × 10^−Scale units of the quote currency. FromDigits and QuoteDigits are the ISO 4217 minor digits
// of the two currencies. Amounts convert line by line, halves rounded away from zero. The rate prices the
// quote; invoicing and accounting rates belong to FIN.
type ExchangeRate struct {
	From        kernel.CurrencyCode
	Rate        int64
	Scale       int
	FromDigits  int
	QuoteDigits int
	Source      string
	AsOf        time.Time
}

// CommercialTerms are the optional Incoterms rule, its edition (2010 or 2020) and named place, and the
// service level a quote is priced for (C8); the order takes them over.
type CommercialTerms struct {
	IncotermsRule    string
	IncotermsVersion string
	NamedPlace       string
	ServiceLevel     string
}

// Roles a quote may go to (R7); ORG must confirm the party in the role for the legal entity.
const (
	RecipientCustomer      = "CUSTOMER"
	RecipientOverseasAgent = "OVERSEAS_AGENT"
	RecipientShipper       = "SHIPPER"
	RecipientConsignee     = "CONSIGNEE"
)

// CreateQuote prices a quote for a party that ORG confirms in RecipientRole (CUSTOMER when empty) for the
// legal entity at OccurredAt. Currency is the currency the quote totals in. TenantID and ActorID must come
// from a verified principal. OccurredAt is the business time; it may be up to the record window earlier
// than the time RATE records the command, and not later (T1).
type CreateQuote struct {
	CommandID       string
	TenantID        string
	ActorID         string
	QuoteID         string
	LegalEntityID   string
	CustomerPartyID string
	RecipientRole   string
	Currency        kernel.CurrencyCode
	Lines           []QuoteLine
	ExchangeRates   []ExchangeRate
	Terms           CommercialTerms
	Validity        kernel.ValidityInterval
	OccurredAt      time.Time
}

// ApproveQuotePrice authorizes a quote that needs a price approval: its margin is below the floor or it
// has a discount line. The approver is ActorID; the quote's author may approve it only with a role the
// Authorizer allows ActionApproveOwnQuotePrice, and the approval is then flagged as a self-approval (R3).
type ApproveQuotePrice struct {
	CommandID        string
	TenantID         string
	ActorID          string
	QuoteID          string
	ExpectedRevision int64
	ReasonRef        string
	OccurredAt       time.Time
}

// Kinds of acceptance evidence (R5).
const (
	EvidenceSignedDocument = "SIGNED_DOCUMENT"
	EvidenceEmail          = "EMAIL"
	EvidenceCustomerPortal = "CUSTOMER_PORTAL"
)

// AcceptQuote records the customer's acceptance at OccurredAt, within the quote's validity, with the kind
// and reference of its evidence. OccurredAt may be up to the record window earlier than the time RATE
// records it, which is kept too (T1).
type AcceptQuote struct {
	CommandID        string
	TenantID         string
	ActorID          string
	QuoteID          string
	ExpectedRevision int64
	EvidenceKind     string
	EvidenceRef      string
	OccurredAt       time.Time
}

// ExpireQuote ends a quote that was not accepted, once its validity has passed (R4). A scheduled job calls
// it with its own principal.
type ExpireQuote struct {
	CommandID        string
	TenantID         string
	ActorID          string
	QuoteID          string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// Receipt confirms a committed command. Replaying the same command returns the same receipt. SubjectID is what the
// command changed: the quote (QuoteID too), the charge code, the rate card or the snapshot; NewRevision is the quote's
// or card's revision, the code's version, or 1 for a snapshot.
type Receipt struct {
	CommandID   string
	QuoteID     string
	SubjectID   string
	NewRevision int64
}

// Commands is RATE's command entry point.
type Commands interface {
	CreateQuote(ctx context.Context, cmd CreateQuote) (Receipt, error)
	ApproveQuotePrice(ctx context.Context, cmd ApproveQuotePrice) (Receipt, error)
	AcceptQuote(ctx context.Context, cmd AcceptQuote) (Receipt, error)
	ExpireQuote(ctx context.Context, cmd ExpireQuote) (Receipt, error)
}

// Actions an Authorizer decides on.
const (
	ActionCreateQuote          = "CreateQuote"
	ActionApproveQuotePrice    = "ApproveQuotePrice"
	ActionApproveOwnQuotePrice = "ApproveOwnQuotePrice" // allow it only to roles designated to approve their own quotes (R3)
	ActionAcceptQuote          = "AcceptQuote"
	ActionExpireQuote          = "ExpireQuote"
)

// Authorizer decides whether a verified actor may perform a RATE action: a quote action on the quote, or a catalogue,
// card or rating action on the code, card or snapshot passed as quoteID. The identity and policy layer supplies it.
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
