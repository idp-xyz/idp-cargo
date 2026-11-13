// Package api is the published language of FIN (Freight Job Economics) V0.2, N1 step 8: each legal entity's finance
// policy (functional currency, book, exchange rates, tax treatments of charge codes, number series of notes), the
// freight job of a house shipment with its estimated charges (from the accepted quote, RATE's PricingBasis v1, or
// entered by hand), the debit notes billing them on the issued house document and the credit notes reversing those,
// and its profit; debtors' billing profiles; debtors' credit accounts with manual holds, which DOC asks before issuing
// a house document (CreditStatus v1, C7). It imports only the standard library and the kernel.
//
// FIN asks before its own transaction (T3): FUL's CostDriver v1 for the shipment, RATE's PricingBasis v1 for the
// quote, DOC's TransportDocumentSnapshot v1 for a debit note, ORG's legal entity, party roles and party identity. It
// never writes to them. V0.3 adds settlement: debtor receipts recorded from bank or ERP evidence and allocated to
// debit notes, and debtor statements. V0.4 adds consol cost pools; V0.5 creditor invoices matched to accruals, cost
// variance approval, the release of accruals and the closing of pools; V0.6 creditor payments. Fapiao, postings and
// recognition come later.
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

// RateFreightJob asks RATE to rate the job's shipment (PricingBasis v1 SHIPMENT_ESTIMATE, RATE V0.5) at the
// service level given, with an optional via and carrier: FUL's CostDriver v1 gives the route and cargo as planned, and
// RATE prices both sides (the customer's card, then the tariff; the given carrier's cards, else the lowest total). ORG
// confirms the customer as a debtor and each supplier as a creditor. Every rated line becomes a RATED charge line with a
// sell side for the customer and a cost side for the supplier, converted at FIN's ESTIMATE rate; a charge RATE could
// not rate is left to a person (manual lines). A job is priced by its accepted quotes or by one rated estimate
// (FIN_BASIS_APPLIED); a shipment RATE cannot rate is FIN_NOT_PRICEABLE. Receipt.Version is the number of lines added;
// the charges RATE could not rate are listed by RATE's basis (GetPricingBasis with the job's basis id). A command retried
// after FUL's plan changed is an idempotency conflict: ask again with a new command id.
type RateFreightJob struct {
	CommandID        string
	TenantID         string
	ActorID          string
	JobID            string
	ExpectedRevision int64
	ServiceLevel     string
	Via              string
	Carrier          string
	OccurredAt       time.Time
}

// RateConsolCostPool asks RATE to rate the pool's consolidation on the BUY side (PricingBasis v1
// CONSOL_COST_ESTIMATE) for the lane and service level given and its carrier (the carrier's cards; without one, the
// lowest total): PHY's PhysicalCostBasis v1 gives the mode and the cargo of the live houses. Every rated cost becomes a
// RATED pool cost line, ACCRUED for its supplier at the ESTIMATE rate and apportioned by Method; a pool takes one rated
// estimate (FIN_BASIS_APPLIED). As for jobs, RATE's basis lists the costs it could not rate, and a command retried after
// PHY's consolidation changed is an idempotency conflict.
type RateConsolCostPool struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PoolID           string
	ExpectedRevision int64
	Carrier          string
	Origin           string
	Destination      string
	Via              string
	ServiceLevel     string
	Method           string
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

// RegisterInvoiceSeries fixes a legal entity's numbering of a note kind, once (numbers from idp-platform-go, MD-28): a
// template such as DN{seq:6}, DN{yy}{seq:5} or CN{yyyy}{mm}-{seq:4} — a literal prefix of one to ten capital letters,
// digits or hyphens, then literals, the date tokens {yyyy}, {yy}, {mm}, {q} and one {seq:N} with N from 4 to 10 —
// whose numbers are 5 to 20 capital letters, digits and hyphens. The sequence resets by the finest date token (year,
// quarter, month, or never) and a note takes the period of its invoice date; numbers are drawn at issue without gaps
// within a period. FirstNumber starts the sequence: of a template that never resets, or, for one that resets, of the
// period of FirstPeriodDate (YYYY-MM-DD; needed when FirstNumber is above 1, as FIN keeps no time zone of the legal
// entity to place the registration's instant). The debit and credit note prefixes never start one another.
type RegisterInvoiceSeries struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	ExpectedRevision int64
	Kind             string
	Template         string
	FirstNumber      int64
	FirstPeriodDate  string
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
// address, phone, bank and account are the enterprise's own, optional, never a person's contact (a sole trader, code
// 92..., takes neither phone nor account); EvidenceRef is the reference of the document they were checked against
// (letters, digits, space and . _ : / # -, up to 200; no names). ExpectedVersion is the latest version read, 0 for a new
// profile.
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
// (FIN_NO_ISSUED_DOCUMENT without one, or when its valid version was issued after OccurredAt: a note backdated before an
// amendment's issue is refused; DOC issues HAWBs only so far, so a SEA job cannot be billed yet) and ORG for the debtor's role and identity; the debtor's billing profile must
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
// the debit note's lines, each reversed once, at the debit note's amounts, conversion and tax, negated. A credit note
// reduces what the debtor owes, and the lines it leaves on the debit note total more than zero (a discount line goes
// with, or after, the lines it discounts). The note takes the next number of the credit note series. The reversed sides
// return to ESTIMATED at their amount, converted at the ESTIMATE rate in force (FIN_RATE_MISSING without one), to be
// revised or billed again; with WriteOff they return at zero, a waived charge. A debit note with every line reversed
// becomes CANCELLED_BY_CREDIT_NOTE.
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
	WriteOff         bool
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

// Evidence sources of a debtor receipt and the reasons to cancel one.
const (
	EvidenceBank = "BANK"
	EvidenceERP  = "ERP"

	ReceiptEnteredInError = "ENTERED_IN_ERROR"
	ReceiptReturned       = "PAYMENT_RETURNED"
)

// RecordDebtorReceipt records money received from a debtor of a legal entity, from bank or ERP evidence (OD-2 B: FIN
// keeps the settlement status of debit notes; the cash book, bank reconciliation and exchange differences stay in the
// ERP). Amount is what the bank account was credited with; BankCharges, in the same currency and not above the amount,
// is what banks deducted on the way and the legal entity bears: the debtor is credited with both. ReceivedOn is the
// civil date received (YYYY-MM-DD), not after the day of the command and not more than 366 days before it.
// EvidenceRef is a reference (a bank statement line, an ERP voucher), never free text; a legal entity records a piece
// of evidence once (FIN_EVIDENCE_USED) until that receipt is cancelled. The debtor must hold the CUSTOMER or
// OVERSEAS_AGENT role for the legal entity in ORG. A payment in another currency than the notes it pays is entered at
// the amount it settles in the notes' currency.
type RecordDebtorReceipt struct {
	CommandID      string
	TenantID       string
	ActorID        string
	ReceiptID      string
	LegalEntityID  string
	DebtorPartyID  string
	Amount         kernel.Money
	BankCharges    kernel.Money
	ReceivedOn     string
	EvidenceSource string
	EvidenceRef    string
	OccurredAt     time.Time
}

// ReceiptAllocation is an amount of a receipt for a debit note.
type ReceiptAllocation struct {
	InvoiceID string
	Amount    kernel.Money
}

// AllocateDebtorReceipt settles debit notes from a receipt: each an ISSUED debit note of the receipt's legal entity,
// debtor and currency (FIN_NOT_ALLOCATABLE), named once, with a positive amount within what the receipt has
// unallocated and the note has open (FIN_OVER_ALLOCATED). Date is the civil date of the command, not before the date
// received or a note's invoice date. What stays unallocated is on account.
type AllocateDebtorReceipt struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ReceiptID        string
	ExpectedRevision int64
	Allocations      []ReceiptAllocation
	Date             string
	OccurredAt       time.Time
}

// ReverseDebtorReceiptAllocation reverses one allocation of a receipt with a reason, once: its debit note is open
// again for the amount, which is back on account. Date is the civil date of the command, on or after the allocation's.
type ReverseDebtorReceiptAllocation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ReceiptID        string
	ExpectedRevision int64
	AllocationNo     int
	Reason           string
	Date             string
	OccurredAt       time.Time
}

// CancelDebtorReceipt cancels a receipt that settles nothing (its allocations reversed first), entered in error or
// returned by the bank. Date is the civil date of the command.
type CancelDebtorReceipt struct {
	CommandID        string
	TenantID         string
	ActorID          string
	ReceiptID        string
	ExpectedRevision int64
	Reason           string // ReceiptEnteredInError or ReceiptReturned
	Date             string
	OccurredAt       time.Time
}

// Apportionment methods of a consol cost pool (OD-6 A: CHARGEABLE is the default of both modes) and the stages an
// apportionment is based on: the houses' loaded facts (ACTUAL), or the attach plan scope while loading evidence is
// not complete (PLANNED, to be re-apportioned once it is).
const (
	MethodChargeable = "CHARGEABLE"
	MethodVolume     = "VOLUME"
	MethodPieces     = "PIECES"
	MethodManual     = "MANUAL"
	// MethodRetained keeps lines' costs on the consolidation (FIN V0.5), with a reason, once PHY reports it cancelled
	// or without a house left: the consolidation's own cost, such as dead freight, as CargoWise leaves a consol charge
	// on the consol when it is not apportioned.
	MethodRetained = "RETAINED"

	BasisActual  = "ACTUAL"
	BasisPlanned = "PLANNED"
)

// OpenConsolCostPool opens the consol cost pool of a consolidation (FIN V0.4, FB3): FIN reads PHY's
// PhysicalCostBasis v1; the consolidation must be an air consolidation or an LCL co-load and not cancelled, and its
// operating legal entity, VERIFIED in ORG, becomes the pool's (FIN_CROSS_LEGAL_ENTITY otherwise; cross-entity pools
// come with intercompany charges). One pool per consolidation and legal entity (UL-08); the pool freezes the
// functional currency and book. Matching creditor invoices and closing the pool come with FB4.
type OpenConsolCostPool struct {
	CommandID       string
	TenantID        string
	ActorID         string
	PoolID          string
	ConsolidationID string
	OccurredAt      time.Time
}

// RecordConsolCost adds a cost line of the whole consolidation to the pool, entered by hand with a reason: a charge
// code, the supplier's reference (the MAWB, the co-loader or CFS booking; optional), the line's default apportionment
// method, and the amount, converted at the ESTIMATE rate (FIN_RATE_MISSING). Cost.PartyID names the creditor (CARRIER,
// CO_LOADER or OVERSEAS_AGENT for the pool's legal entity), or stays empty while not known.
type RecordConsolCost struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PoolID           string
	ExpectedRevision int64
	ChargeCode       string
	Description      string
	SupplierRef      string
	Method           string
	Cost             ChargeSide
	Reason           string
	OccurredAt       time.Time
}

// ReviseConsolCost writes the next version of a pool cost line with a reason; the line keeps its creditor unless
// Value.PartyID names another. ExpectedVersion is the line's latest version read (FIN_COST_VERSION).
type ReviseConsolCost struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PoolID           string
	ExpectedRevision int64
	LineNo           int
	ExpectedVersion  int
	Value            ChargeSide
	Reason           string
	OccurredAt       time.Time
}

// ManualShare is a hand-given share of one house shipment, in the line's currency.
type ManualShare struct {
	ShipmentID string
	Amount     int64
}

// ApportionConsolCosts divides pool cost lines' latest versions over the house jobs (LineNos; every line when
// empty). FIN reads PHY's PhysicalCostBasis v1 first: the basis is the houses' loaded facts when every house has
// loading evidence (ACTUAL), the attach plan scope otherwise (PLANNED). Every live house needs an OPEN job of the
// pool's legal entity (FIN_POOL_JOB_MISSING). Method overrides each line's default: CHARGEABLE weighs by the air
// chargeable weight (6000 cm³ per kg, rounded up to 500 g) or the LCL W/M freight tonnes; VOLUME by volume; PIECES
// by packs (FIN_POOL_UNIT, FIN_POOL_NO_BASIS); MANUAL divides one line by Manual shares, naming every house once and
// summing to the line's amount (FIN_MANUAL_SHARES), with a reason. Shares conserve the line's amount and its local
// amount exactly (largest remainder, ties to the first shipment). A new apportionment supersedes earlier ones line by
// line. Since FIN V0.5 each line's current cost is divided (what creditor invoices matched plus what is still
// accrued); RETAINED keeps the lines' costs on the consolidation with a reason, and only once PHY reports it cancelled
// or without houses.
type ApportionConsolCosts struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PoolID           string
	ExpectedRevision int64
	LineNos          []int
	Method           string
	Manual           []ManualShare
	Reason           string
	OccurredAt       time.Time
}

// Receipt confirms a committed command; replaying the command returns it again. SubjectID is the legal entity
// (policy), the job (job and note commands), the debtor (billing profile), <legal entity>/<debtor> (credit account),
// the debtor receipt, the pool (pool commands), the creditor invoice or the creditor payment; Version is the rate entry recorded or ended,
// the tax treatment set, the series registered, the variance threshold set, the pricing basis applied, the charge line
// recorded, the side version written (a release's too), the billing profile version, the terms version set, the hold
// placed or released, the last allocation written or the one reversed, the pool cost line recorded, the cost version
// written, the apportionment made, the closure made or reopened, the matches of an invoice, or its dispute opened or
// resolved, the last allocation of a creditor payment written or the one reversed (0 for opening a policy, job or pool,
// cancelling a job, issuing a note, recording or cancelling a debtor receipt, a creditor invoice or a creditor payment,
// and approving a variance); Number is the number of a note issued. For a billing
// profile NewRevision is its version.
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
	RateFreightJob(ctx context.Context, cmd RateFreightJob) (Receipt, error)
	RateConsolCostPool(ctx context.Context, cmd RateConsolCostPool) (Receipt, error)
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
	RecordDebtorReceipt(ctx context.Context, cmd RecordDebtorReceipt) (Receipt, error)
	AllocateDebtorReceipt(ctx context.Context, cmd AllocateDebtorReceipt) (Receipt, error)
	ReverseDebtorReceiptAllocation(ctx context.Context, cmd ReverseDebtorReceiptAllocation) (Receipt, error)
	CancelDebtorReceipt(ctx context.Context, cmd CancelDebtorReceipt) (Receipt, error)
	OpenConsolCostPool(ctx context.Context, cmd OpenConsolCostPool) (Receipt, error)
	RecordConsolCost(ctx context.Context, cmd RecordConsolCost) (Receipt, error)
	ReviseConsolCost(ctx context.Context, cmd ReviseConsolCost) (Receipt, error)
	ApportionConsolCosts(ctx context.Context, cmd ApportionConsolCosts) (Receipt, error)
	SetCostVarianceThreshold(ctx context.Context, cmd SetCostVarianceThreshold) (Receipt, error)
	RecordCreditorInvoice(ctx context.Context, cmd RecordCreditorInvoice) (Receipt, error)
	MatchCreditorInvoice(ctx context.Context, cmd MatchCreditorInvoice) (Receipt, error)
	ApproveCostVariance(ctx context.Context, cmd ApproveCostVariance) (Receipt, error)
	DisputeCreditorInvoice(ctx context.Context, cmd DisputeCreditorInvoice) (Receipt, error)
	ResolveCreditorInvoiceDispute(ctx context.Context, cmd ResolveCreditorInvoiceDispute) (Receipt, error)
	CancelCreditorInvoice(ctx context.Context, cmd CancelCreditorInvoice) (Receipt, error)
	ReleaseJobAccrual(ctx context.Context, cmd ReleaseJobAccrual) (Receipt, error)
	ReleasePoolAccrual(ctx context.Context, cmd ReleasePoolAccrual) (Receipt, error)
	CloseConsolCostPool(ctx context.Context, cmd CloseConsolCostPool) (Receipt, error)
	ReopenConsolCostPool(ctx context.Context, cmd ReopenConsolCostPool) (Receipt, error)
	RecordCreditorPayment(ctx context.Context, cmd RecordCreditorPayment) (Receipt, error)
	AllocateCreditorPayment(ctx context.Context, cmd AllocateCreditorPayment) (Receipt, error)
	ReverseCreditorPaymentAllocation(ctx context.Context, cmd ReverseCreditorPaymentAllocation) (Receipt, error)
	CancelCreditorPayment(ctx context.Context, cmd CancelCreditorPayment) (Receipt, error)
}

// Actions an Authorizer decides on: the command names.
const (
	ActionOpenFinancePolicy  = "OpenFinancePolicy"
	ActionRecordExchangeRate = "RecordExchangeRate"
	ActionEndExchangeRate    = "EndExchangeRate"
	ActionOpenFreightJob     = "OpenFreightJob"
	ActionApplyPricingBasis  = "ApplyPricingBasis"
	ActionRateFreightJob     = "RateFreightJob"
	ActionRateConsolCostPool = "RateConsolCostPool"
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

	ActionRecordDebtorReceipt            = "RecordDebtorReceipt"
	ActionAllocateDebtorReceipt          = "AllocateDebtorReceipt"
	ActionReverseDebtorReceiptAllocation = "ReverseDebtorReceiptAllocation"
	ActionCancelDebtorReceipt            = "CancelDebtorReceipt"

	ActionOpenConsolCostPool   = "OpenConsolCostPool"
	ActionRecordConsolCost     = "RecordConsolCost"
	ActionReviseConsolCost     = "ReviseConsolCost"
	ActionApportionConsolCosts = "ApportionConsolCosts"

	ActionSetCostVarianceThreshold      = "SetCostVarianceThreshold"
	ActionRecordCreditorInvoice         = "RecordCreditorInvoice"
	ActionMatchCreditorInvoice          = "MatchCreditorInvoice"
	ActionApproveCostVariance           = "ApproveCostVariance"
	ActionApproveOwnCostVariance        = "ApproveOwnCostVariance" // asked besides ApproveCostVariance when the approver matched the invoice
	ActionDisputeCreditorInvoice        = "DisputeCreditorInvoice"
	ActionResolveCreditorInvoiceDispute = "ResolveCreditorInvoiceDispute"
	ActionCancelCreditorInvoice         = "CancelCreditorInvoice"
	ActionReleaseJobAccrual             = "ReleaseJobAccrual"
	ActionReleasePoolAccrual            = "ReleasePoolAccrual"
	ActionCloseConsolCostPool           = "CloseConsolCostPool"
	ActionReopenConsolCostPool          = "ReopenConsolCostPool"

	ActionRecordCreditorPayment            = "RecordCreditorPayment"
	ActionAllocateCreditorPayment          = "AllocateCreditorPayment"
	ActionReverseCreditorPaymentAllocation = "ReverseCreditorPaymentAllocation"
	ActionCancelCreditorPayment            = "CancelCreditorPayment"
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
