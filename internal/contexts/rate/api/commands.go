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
//
// V0.5: ChargeCode is a listed, active code of the catalogue (RATE_CHARGE_CODE_UNLISTED otherwise). A line is MANUAL
// (Source empty), RATED or RATED_OVERRIDDEN: a rated line names a RateSnapshot of purpose QUOTE_LINE recorded for the
// quote's legal entity, customer, scope and service level within the record window before the quote, and RATE takes
// its sell (and its cost, when a BUY line of the code priced one) and per-unit basis from it; an overridden line gives
// the Sell entered, in the rated currency, and a Reason. Selling below the rated sell by more than the legal entity's
// threshold needs a price approval (RATE_CARD_DISCOUNT).
type QuoteLine struct {
	ChargeCode     string
	Sell           kernel.Money
	Cost           kernel.Money
	Source         string
	SnapshotID     string
	OverrideReason string
}

// Line sources of a quote line.
const (
	LineManual          = "MANUAL"
	LineRated           = "RATED"
	LineRatedOverridden = "RATED_OVERRIDDEN"
)

// QuoteScope is the lane and cargo a quote is priced for (V0.5): AIR LOOSE or SEA LCL, UN/LOCODEs as RATE resolved them
// in the snapshot, an optional carrier, and the cargo. A quote with rated lines and no scope takes its snapshot's.
type QuoteScope struct {
	Mode          string
	ContainerMode string
	Origin        string
	Destination   string
	Via           string
	Carrier       string
	Cargo         Cargo
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
	Scope           *QuoteScope
	// V0.5 part 3b: Kind is ONE_OFF (empty) or AGREEMENT. An AGREEMENT has no lines: it names the DRAFT version of the
	// customer's CUSTOMER SELL card it prices with (one entry per lane, valid over the quote's validity) and a scope of
	// its mode only; each lane is checked against the valid buy rates at a reference quantity (AGREEMENT_BELOW_FLOOR),
	// converted at the quote's exchange rates. Accepting it publishes the version.
	Kind       string
	Agreement  *AgreementRef
	OccurredAt time.Time
}

// Quote kinds (V0.5 part 3b).
const (
	QuoteOneOff    = "ONE_OFF"
	QuoteAgreement = "AGREEMENT"
)

// AgreementRef is the customer card version an AGREEMENT quote prices with.
type AgreementRef struct {
	RateCardID string
	Version    int
}

// DiscountPolicy may be implemented by a MarginPolicy: how far below the rated sell, in basis points of it, an
// overridden quote line of a legal entity may sell before the quote needs approval (RATE_CARD_DISCOUNT). Without it,
// any sell below the rated one needs approval.
type DiscountPolicy interface {
	RateCardDiscountBP(ctx context.Context, tenantID, legalEntityID string) (int, error)
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
// records it, which is kept too (T1). Accepting an AGREEMENT publishes its customer card version in the same
// transaction (V0.5 part 3b).
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
