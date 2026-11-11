// Package api is the published language of FIN (Freight Job Economics) V0.2, N1 step 8: each legal entity's finance
// policy (functional currency, book, exchange rates, tax treatments of charge codes, number series of notes), the
// freight job of a house shipment with its estimated charges (from the accepted quote, RATE's PricingBasis v1, or
// entered by hand), the debit notes billing them on the issued house document and the credit notes reversing those,
// and its profit; debtors' billing profiles; debtors' credit accounts with manual holds, which DOC asks before issuing
// a house document (CreditStatus v1, C7). It imports only the standard library and the kernel.
//
// FIN asks before its own transaction (T3): FUL's CostDriver v1 for the shipment, RATE's PricingBasis v1 for the
// quote, DOC's TransportDocumentSnapshot v1 for a debit note, ORG's legal entity, party roles and party identity. It
// never writes to them. Settlement, consol cost pools, creditor invoices, fapiao, postings and recognition come later.
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
// the functional currency in [ValidFrom, ValidUntil), a period of at most a year; FromDigits and ToDigits are the two
// currencies' ISO 4217 minor digits, which the kernel checks. Entries of one type and currency never overlap
// (FIN_RATE_OVERLAP); an entry's rate never changes. OD-5 B: the ACCOUNTING rate is a monthly rate set by the
// accountant.
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

// EndExchangeRate ends an entry's period earlier, once, with a reason, to correct a wrong entry: the new end lies
// within the entry's period, and no charge may have been converted at the entry at or after it (FIN_RATE_IN_USE). A
// corrected entry is then recorded from the new end.
type EndExchangeRate struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	ExpectedRevision int64
	EntryNo          int
	ValidUntil       time.Time
	Reason           string
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
// customer and a cost estimate when it has one, converted at FIN's ESTIMATE rate (FIN_RATE_MISSING). A quote is applied
// to a job once, and a quote and order to one job of the tenant (FIN_BASIS_APPLIED): another shipment of the order, or
// another order of the quote in the same shipment, takes manual lines.
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
// line lacks (never a cost on a quote's discount line). A side keeps its debtor or creditor unless Value.PartyID names
// another. Amounts are bounded by 10^15 minor units.
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

// Tax treatments of a charge code in a legal entity (the tax adviser confirms each code): international freight
// forwarding is exempt from VAT and takes ordinary fapiao only, domestic services are taxable at a rate, disbursements
// pass through. A charged amount is what the debtor pays; the fapiao splits out the tax.
const (
	TaxExemptIntlFreight = "EXEMPT_INTL_FREIGHT_FORWARDING"
	TaxTaxable           = "TAXABLE"
	TaxPassThrough       = "PASS_THROUGH_DISBURSEMENT"
)

// SetChargeTaxTreatment records the next version of a charge code's tax treatment in a legal entity's policy, in
// force from EffectiveFrom (YYYY-MM-DD), later than the code's latest version; RateBP is a TAXABLE code's VAT rate in
// basis points (1 to 3000). It is refused when a debit note of the legal entity bills the code on or after that date
// (FIN_TAX_TREATMENT_IN_USE).
type SetChargeTaxTreatment struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	ExpectedRevision int64
	ChargeCode       string
	Treatment        string
	RateBP           int
	EffectiveFrom    string
	OccurredAt       time.Time
}

// Note kinds and states: a debit note is ISSUED until credit notes reverse all its lines.
const (
	KindDebitNote  = "DEBIT_NOTE"
	KindCreditNote = "CREDIT_NOTE"

	NoteIssued    = "ISSUED"
	NoteCancelled = "CANCELLED_BY_CREDIT_NOTE"
)

// RegisterInvoiceSeries fixes a legal entity's number series of a note kind, once (MD-28): numbers are Prefix (one to
// ten capital letters, digits or hyphens) and the sequence zero-padded to Width digits (4 to 10, twenty characters in
// all), from FirstNumber, drawn in order at issue without gaps. The debit and credit note prefixes never start one
// another.
type RegisterInvoiceSeries struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	ExpectedRevision int64
	Kind             string
	Prefix           string
	Width            int
	FirstNumber      int64
	OccurredAt       time.Time
}

// Buyer kinds of a billing profile.
const (
	BuyerCNEnterprise = "CN_ENTERPRISE"
	BuyerForeign      = "FOREIGN"
)

// SetBillingProfile records the next version of a debtor's billing profile (MD-21): FIN reads the debtor's identity
// from ORG and freezes the invoice title (the registered name; a foreign buyer's English name when ORG has one) and,
// for a Chinese enterprise, the taxpayer number (its unified social credit code in ORG, required). The registered
// address, phone, bank and account are the enterprise's own, optional, never a person's contact; EvidenceRef names
// what they were checked against. ExpectedVersion is the latest version read, 0 for a new profile.
type SetBillingProfile struct {
	CommandID         string
	TenantID          string
	ActorID           string
	DebtorPartyID     string
	ExpectedVersion   int
	BuyerKind         string
	RegisteredAddress string
	RegisteredPhone   string
	BankName          string
	BankAccount       string
	EvidenceRef       string
	OccurredAt        time.Time
}

// IssueDebitNote bills sell sides of a job to one debtor (CargoWise INV): the job's lines in LineNos, whose latest sell
// side is not billed, not zero and the debtor's. FIN asks DOC for the shipment's issued house document
// (FIN_NO_ISSUED_DOCUMENT without one) and ORG for the debtor's role and identity; the debtor's billing profile must
// name it as ORG does (FIN_NO_BILLING_PROFILE, FIN_BILLING_PROFILE_STALE). Currency is the functional currency, which
// converts other lines at the INVOICE rate, or the currency of every line, converted into the functional currency at
// the ACCOUNTING rate. Every charge code needs a tax treatment in force at InvoiceDate (YYYY-MM-DD, the date of the
// issue; FIN_TAX_TREATMENT_MISSING). The note freezes the profile version, the credit account's payment terms with the
// due date (due on the invoice date without an account) and the document, and takes the next number of the debit note
// series (FIN_NO_SERIES); its billed sides become INVOICED.
type IssueDebitNote struct {
	CommandID        string
	TenantID         string
	ActorID          string
	JobID            string
	ExpectedRevision int64
	InvoiceID        string
	DebtorPartyID    string
	LineNos          []int
	Currency         kernel.CurrencyCode
	InvoiceDate      string
	OccurredAt       time.Time
}

// IssueCreditNote reverses whole lines of an issued debit note of the job with a reason (CargoWise CRD): LineNos are
// the debit note's lines, each reversed once, at the debit note's amounts, conversion and tax, negated. The note takes
// the next number of the credit note series; the reversed sides return to ESTIMATED, to be revised or billed again,
// and a debit note with every line reversed becomes CANCELLED_BY_CREDIT_NOTE.
type IssueCreditNote struct {
	CommandID        string
	TenantID         string
	ActorID          string
	JobID            string
	ExpectedRevision int64
	InvoiceID        string
	DebitNoteID      string
	LineNos          []int
	Reason           string
	InvoiceDate      string
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
// (policy), the job (job and note commands), the debtor (billing profile) or <legal entity>/<debtor> (credit account);
// Version is the rate entry recorded or ended, the tax treatment set, the series registered, the pricing basis
// applied, the charge line recorded, the side version written, the billing profile version, the terms version set or
// the hold placed or released (0 for opening a policy or job, cancelling a job and issuing a note); Number is the
// number of a note issued. For a billing profile NewRevision is its version.
type Receipt struct {
	CommandID   string
	SubjectID   string
	NewRevision int64
	Version     int
	Number      string
}

// Commands is FIN's command entry point.
type Commands interface {
	OpenFinancePolicy(ctx context.Context, cmd OpenFinancePolicy) (Receipt, error)
	RecordExchangeRate(ctx context.Context, cmd RecordExchangeRate) (Receipt, error)
	EndExchangeRate(ctx context.Context, cmd EndExchangeRate) (Receipt, error)
	OpenFreightJob(ctx context.Context, cmd OpenFreightJob) (Receipt, error)
	ApplyPricingBasis(ctx context.Context, cmd ApplyPricingBasis) (Receipt, error)
	RecordChargeLine(ctx context.Context, cmd RecordChargeLine) (Receipt, error)
	ReviseChargeSide(ctx context.Context, cmd ReviseChargeSide) (Receipt, error)
	CancelFreightJob(ctx context.Context, cmd CancelFreightJob) (Receipt, error)
	SetCreditTerms(ctx context.Context, cmd SetCreditTerms) (Receipt, error)
	PlaceCreditHold(ctx context.Context, cmd PlaceCreditHold) (Receipt, error)
	ReleaseCreditHold(ctx context.Context, cmd ReleaseCreditHold) (Receipt, error)
	SetChargeTaxTreatment(ctx context.Context, cmd SetChargeTaxTreatment) (Receipt, error)
	RegisterInvoiceSeries(ctx context.Context, cmd RegisterInvoiceSeries) (Receipt, error)
	SetBillingProfile(ctx context.Context, cmd SetBillingProfile) (Receipt, error)
	IssueDebitNote(ctx context.Context, cmd IssueDebitNote) (Receipt, error)
	IssueCreditNote(ctx context.Context, cmd IssueCreditNote) (Receipt, error)
}

// Actions an Authorizer decides on: the command names.
const (
	ActionOpenFinancePolicy  = "OpenFinancePolicy"
	ActionRecordExchangeRate = "RecordExchangeRate"
	ActionEndExchangeRate    = "EndExchangeRate"
	ActionOpenFreightJob     = "OpenFreightJob"
	ActionApplyPricingBasis  = "ApplyPricingBasis"
	ActionRecordChargeLine   = "RecordChargeLine"
	ActionReviseChargeSide   = "ReviseChargeSide"
	ActionCancelFreightJob   = "CancelFreightJob"
	ActionSetCreditTerms     = "SetCreditTerms"
	ActionPlaceCreditHold    = "PlaceCreditHold"
	ActionReleaseCreditHold  = "ReleaseCreditHold"

	ActionSetChargeTaxTreatment = "SetChargeTaxTreatment"
	ActionRegisterInvoiceSeries = "RegisterInvoiceSeries"
	ActionSetBillingProfile     = "SetBillingProfile"
	ActionIssueDebitNote        = "IssueDebitNote"
	ActionIssueCreditNote       = "IssueCreditNote"
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
