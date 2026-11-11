// Package api is the published language of FIN (Freight Job Economics) V0.1, the first part of N1 step 8: each
// legal entity's finance policy (functional currency, book, exchange rates), the freight job of a house shipment
// with its estimated charges (from the accepted quote, RATE's PricingBasis v1, or entered by hand) and estimated
// profit, and debtors' credit accounts with manual holds, which DOC asks before issuing a house document
// (CreditStatus v1, C7). It imports only the standard library and the kernel.
//
// FIN asks before its own transaction (T3): FUL's CostDriver v1 for the shipment, RATE's PricingBasis v1 for the
// quote, ORG's legal entity and party roles. It never writes to them. Debit notes, credit notes and billing
// profiles follow in V0.2; consol cost pools, creditor invoices, fapiao, postings and recognition later.
package api

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// OpenFinancePolicy records a legal entity's functional currency and book code, once (UL-08: jobs freeze them). The
// legal entity must be VERIFIED in ORG.
type OpenFinancePolicy struct {
	CommandID          string
	TenantID           string
	ActorID            string
	LegalEntityID      string
	FunctionalCurrency kernel.CurrencyCode
	BookCode           string
	OccurredAt         time.Time
}

// Rate types: ESTIMATE converts estimates; INVOICE and ACCOUNTING come into use with invoices and postings.
const (
	RateEstimate   = "ESTIMATE"
	RateInvoice    = "INVOICE"
	RateAccounting = "ACCOUNTING"
)

// RecordExchangeRate adds an exchange rate to a legal entity's policy: one unit of From is Rate × 10^−Scale units of
// the functional currency in [ValidFrom, ValidUntil) (no end when ValidUntil is zero), FromDigits and ToDigits the
// ISO 4217 minor digits. Entries of one type and currency never overlap (FIN_RATE_OVERLAP); an entry never
// changes. OD-5 B: the ACCOUNTING rate is a monthly rate set by the accountant.
type RecordExchangeRate struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	ExpectedRevision int64
	RateType         string
	From             kernel.CurrencyCode
	Rate             int64
	Scale            int
	FromDigits       int
	ToDigits         int
	ValidFrom        time.Time
	ValidUntil       time.Time
	Source           string
	OccurredAt       time.Time
}

// OpenFreightJob opens the job of a house shipment under the shipment's legal entity (OD-1 A: one job per shipment
// and legal entity that is not cancelled). FIN reads FUL's CostDriver v1 (the shipment DRAFT or PLANNED, by AIR or
// SEA) and the legal entity's policy; the job freezes the legal entity, functional currency, book and customer.
type OpenFreightJob struct {
	CommandID  string
	TenantID   string
	ActorID    string
	JobID      string
	ShipmentID string
	OccurredAt time.Time
}

// ApplyPricingBasis applies an accepted quote to the job for one of the shipment's orders: FIN reads RATE's
// PricingBasis v1 and FUL's CostDriver v1 first; the quote must back the order in the shipment, for the job's legal
// entity and customer (FIN_NOT_PRICEABLE). Each quote line becomes a charge line with a sell estimate for the
// customer and a cost estimate when it has one, converted at FIN's ESTIMATE rate (FIN_RATE_MISSING). A quote and
// order are applied once across jobs (FIN_BASIS_APPLIED): another shipment of the order takes manual lines.
type ApplyPricingBasis struct {
	CommandID        string
	TenantID         string
	ActorID          string
	JobID            string
	ExpectedRevision int64
	QuoteID          string
	OrderID          string
	OccurredAt       time.Time
}

// ChargeSide is a side entered by hand: the debtor of a sell side (an ORG party in the CUSTOMER or OVERSEAS_AGENT
// role for the job's legal entity) or the creditor of a cost side (CARRIER, CO_LOADER or OVERSEAS_AGENT; empty
// while not known), and the amount, not negative.
type ChargeSide struct {
	PartyID string
	Amount  kernel.Money
}

// RecordChargeLine adds a charge line entered by hand, with a sell side, a cost side or both, and a reason.
type RecordChargeLine struct {
	CommandID        string
	TenantID         string
	ActorID          string
	JobID            string
	ExpectedRevision int64
	ChargeCode       string
	Description      string
	Sell             *ChargeSide
	Cost             *ChargeSide
	Reason           string
	OccurredAt       time.Time
}

// Sides of a charge line.
const (
	SideSell = "SELL"
	SideCost = "COST"
)

// ReviseChargeSide writes the next version of a line's side with a reason; ExpectedSideVersion 0 adds the side the
// line lacks. A sell side keeps its debtor unless Side.PartyID names another.
type ReviseChargeSide struct {
	CommandID           string
	TenantID            string
	ActorID             string
	JobID               string
	ExpectedRevision    int64
	LineNo              int
	Side                string
	ExpectedSideVersion int
	Value               ChargeSide
	Reason              string
	OccurredAt          time.Time
}

// CancelFreightJob cancels an open job with a reason, such as a cancelled shipment; its applied quotes become free
// for another job.
type CancelFreightJob struct {
	CommandID        string
	TenantID         string
	ActorID          string
	JobID            string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// Payment term bases and credit hold reasons.
const (
	TermsFromInvoiceDate = "INVOICE_DATE"
	TermsFromMonthEnd    = "END_OF_MONTH"

	HoldManual    = "MANUAL"
	HoldOverLimit = "OVER_LIMIT"
	HoldOverdue   = "OVERDUE"
)

// SetCreditTerms opens or changes a debtor's credit account with a legal entity: the limit in the functional
// currency and the payment terms. The debtor must hold the CUSTOMER or OVERSEAS_AGENT role for the legal entity in
// ORG. ExpectedRevision is 0 for a new account.
type SetCreditTerms struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	DebtorPartyID    string
	ExpectedRevision int64
	Limit            kernel.Money
	PaymentDays      int
	PaymentBasis     string
	OccurredAt       time.Time
}

// PlaceCreditHold holds a debtor's credit with a reason (OD-8 A: by a credit controller): DOC then refuses to issue
// the debtor's house documents (C7).
type PlaceCreditHold struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	DebtorPartyID    string
	ExpectedRevision int64
	Code             string
	Reason           string
	OccurredAt       time.Time
}

// ReleaseCreditHold releases the hold with a reason.
type ReleaseCreditHold struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	DebtorPartyID    string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// Receipt confirms a committed command; replaying the command returns it again. SubjectID is the legal entity
// (policy), the job, or <legal entity>/<debtor> (credit account); Version the rate entry, charge line or side
// version, terms version or hold the command wrote.
type Receipt struct {
	CommandID   string
	SubjectID   string
	NewRevision int64
	Version     int
}

// Commands is FIN's command entry point.
type Commands interface {
	OpenFinancePolicy(ctx context.Context, cmd OpenFinancePolicy) (Receipt, error)
	RecordExchangeRate(ctx context.Context, cmd RecordExchangeRate) (Receipt, error)
	OpenFreightJob(ctx context.Context, cmd OpenFreightJob) (Receipt, error)
	ApplyPricingBasis(ctx context.Context, cmd ApplyPricingBasis) (Receipt, error)
	RecordChargeLine(ctx context.Context, cmd RecordChargeLine) (Receipt, error)
	ReviseChargeSide(ctx context.Context, cmd ReviseChargeSide) (Receipt, error)
	CancelFreightJob(ctx context.Context, cmd CancelFreightJob) (Receipt, error)
	SetCreditTerms(ctx context.Context, cmd SetCreditTerms) (Receipt, error)
	PlaceCreditHold(ctx context.Context, cmd PlaceCreditHold) (Receipt, error)
	ReleaseCreditHold(ctx context.Context, cmd ReleaseCreditHold) (Receipt, error)
}

// Actions an Authorizer decides on: the command names.
const (
	ActionOpenFinancePolicy  = "OpenFinancePolicy"
	ActionRecordExchangeRate = "RecordExchangeRate"
	ActionOpenFreightJob     = "OpenFreightJob"
	ActionApplyPricingBasis  = "ApplyPricingBasis"
	ActionRecordChargeLine   = "RecordChargeLine"
	ActionReviseChargeSide   = "ReviseChargeSide"
	ActionCancelFreightJob   = "CancelFreightJob"
	ActionSetCreditTerms     = "SetCreditTerms"
	ActionPlaceCreditHold    = "PlaceCreditHold"
	ActionReleaseCreditHold  = "ReleaseCreditHold"
)

// Authorizer decides whether a verified actor may perform an action on a subject.
type Authorizer interface {
	AuthorizeFinAction(ctx context.Context, tenantID, actorID, action, subjectID string) error
}

// Error kinds returned by Commands and Queries, wrapped around the original cause.
var (
	ErrUnauthenticated     = errors.New("fin: unauthenticated")
	ErrUnauthorized        = errors.New("fin: unauthorized")
	ErrNotFound            = errors.New("fin: not found")
	ErrConflict            = errors.New("fin: conflicts with the stored state")
	ErrIdempotencyConflict = errors.New("fin: command id reused with a different payload")
	ErrRejected            = errors.New("fin: rejected by business rules")
)
