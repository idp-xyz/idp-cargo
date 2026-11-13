package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ExchangeRateEntry is an entry of a legal entity's rate table.
type ExchangeRateEntry struct {
	EntryNo    int
	RateType   string
	From       kernel.CurrencyCode
	Rate       int64
	Scale      int
	FromDigits int
	ToDigits   int
	ValidFrom  time.Time
	ValidUntil time.Time
	Source     string
	RecordedBy string
	RecordedAt time.Time
	EndedBy    string // set when the period was ended earlier
	EndReason  string
}

// ChargeTaxTreatment is a version of a charge code's tax treatment, in force from EffectiveFrom (YYYY-MM-DD) until the
// code's next version.
type ChargeTaxTreatment struct {
	Version       int
	ChargeCode    string
	Treatment     string
	RateBP        int
	EffectiveFrom string
	SetBy         string
	SetAt         time.Time
}

// InvoiceSeries is a note kind's numbering: its template, first number and the last sequence value per period ("" for
// a template that never resets).
type InvoiceSeries struct {
	Kind        string
	Template    string
	FirstNumber int64
	Last        []NumberPeriod
}

// NumberPeriod is the last sequence value a period took.
type NumberPeriod struct {
	Period string
	Last   int64
}

// FinancePolicy is a legal entity's policy.
type FinancePolicy struct {
	LegalEntityID      string
	Revision           int64
	FunctionalCurrency kernel.CurrencyCode
	BookCode           string
	Rates              []ExchangeRateEntry
	TaxTreatments      []ChargeTaxTreatment
	Series             []InvoiceSeries
	VarianceThresholds []VarianceThreshold
}

// Job kinds and states.
const (
	KindAirExportHouse    = "AIR_EXPORT_HOUSE"
	KindSeaExportLCLHouse = "SEA_EXPORT_LCL_HOUSE"

	JobOpen      = "OPEN"
	JobCancelled = "CANCELLED"
)

// Sources and statuses of sides: a sell side ESTIMATED (WIP) or INVOICED (on a debit note), a cost side version
// ACCRUED or RELEASED (amount zero; StatusReleased).
const (
	SourceQuote  = "QUOTE"
	SourceRated  = "RATED" // from RATE's rated estimate; the charges RATE could not rate are listed by the basis in RATE
	SourceManual = "MANUAL"

	StatusEstimated = "ESTIMATED"
	StatusInvoiced  = "INVOICED"
	StatusAccrued   = "ACCRUED"
)

// FxSnapshot is the conversion a local amount was derived with.
type FxSnapshot struct {
	EntryNo    int
	RateType   string
	Rate       int64
	Scale      int
	FromDigits int
	ToDigits   int
	Source     string
}

// SideVersion is a version of a sell or cost side; Local is in the functional currency. An INVOICED side names the
// debit note line that billed it.
type SideVersion struct {
	Version       int
	PartyID       string
	Amount        kernel.Money
	Local         kernel.Money
	FX            *FxSnapshot // nil when the amount is in the functional currency
	Status        string
	Reason        string
	InvoiceID     string
	InvoiceLineNo int
	ConvertedAt   time.Time // when FX was applied, for an estimate a credit note returned at its earlier conversion; zero otherwise
	RecordedBy    string
	RecordedAt    time.Time
}

// ChargeLine is a charge of a job with its side versions, the latest last. A line with a cost side has its accrual
// status (ACCRUED, PARTLY_MATCHED, MATCHED or RELEASED) and what creditor invoices' live matches consume of it, in its
// currency and in the functional currency (FIN V0.5).
type ChargeLine struct {
	LineNo           int
	ChargeCode       string
	Description      string
	Source           string
	BasisNo          int
	QuoteLineNo      int
	Reason           string
	Sell             []SideVersion
	Cost             []SideVersion
	CostStatus       string
	CostMatched      kernel.Money
	CostMatchedLocal kernel.Money
}

// AppliedBasis is a quote applied to the job for an order (QUOTE_ACCEPTED: as quoted, or re-evaluated on the
// shipment's cargo at PlanVersion, OD-7 A), or RATE's rated estimate of the shipment at a plan version
// (SHIPMENT_ESTIMATE).
type AppliedBasis struct {
	BasisNo       int
	BasisID       string
	Purpose       string
	QuoteID       string
	QuoteRevision int64
	OrderID       string
	PlanVersion   int
	AppliedBy     string
	AppliedAt     time.Time
}

// FreightJob is a job with its bases and charge lines.
type FreightJob struct {
	TenantID           string
	JobID              string
	Revision           int64
	LegalEntityID      string
	ShipmentID         string
	Kind               string
	FunctionalCurrency kernel.CurrencyCode
	BookCode           string
	CustomerPartyID    string
	PlanVersion        int
	State              string
	OpenedAt           time.Time
	CancelReason       string
	CancelledAt        time.Time
	Bases              []AppliedBasis
	Lines              []ChargeLine
	Notes              []NoteSummary
}

// NoteSummary is a debit or credit note of a job.
type NoteSummary struct {
	InvoiceID     string
	Kind          string
	Number        string
	DebtorPartyID string
	State         string
	Total         kernel.Money
}

// ChargeCodeProfit is the estimated revenue, cost and profit of a charge code.
type ChargeCodeProfit struct {
	ChargeCode string
	Revenue    kernel.Money
	Cost       kernel.Money
	Profit     kernel.Money
}

// JobPoolShare is the job's share of a consol cost pool line, under the line's latest apportionment: the basis
// quantity it was weighted by and the share in the line's currency and in the functional currency.
type JobPoolShare struct {
	PoolID      string
	LineNo      int
	LineVersion int
	ChargeCode  string
	Method      string
	Stage       string
	Quantity    int64
	Unit        string
	Amount      kernel.Money
	Local       kernel.Money
}

// JobProfit is a job's economics in the functional currency: revenue from the latest sell sides, billed ones at their
// debit note's local amount (Invoiced is that part); cost from the cost sides, what creditor invoices matched at their
// ACCOUNTING rates (CostMatched) plus what is still accrued at the estimates' rates (CostAccrued; FIN V0.5); and
// PoolCost from the job's shares of consol cost pools (FIN V0.4). Profit is Revenue - Cost - PoolCost.
type JobProfit struct {
	JobID       string
	Revision    int64
	ByCode      []ChargeCodeProfit
	Revenue     kernel.Money
	Invoiced    kernel.Money
	Cost        kernel.Money
	CostMatched kernel.Money
	CostAccrued kernel.Money
	PoolCost    kernel.Money
	Profit      kernel.Money
	PoolShares  []JobPoolShare
}

// CreditTerms is a version of a debtor's terms.
type CreditTerms struct {
	Version      int
	Limit        kernel.Money
	PaymentDays  int
	PaymentBasis string
	SetBy        string
	SetAt        time.Time
}

// CreditHold is a hold, released or in force.
type CreditHold struct {
	HoldNo        int
	Code          string
	Reason        string
	PlacedBy      string
	PlacedAt      time.Time
	ReleasedBy    string
	ReleasedAt    time.Time
	ReleaseReason string
}

// CreditAccount is a debtor's account with a legal entity. Exposure is the debtor's sell on the legal entity's open
// jobs: Unbilled (estimates) and Billed (on debit notes not reversed; every issued debit note stays open until
// settlement is recorded, a later version). In ListCreditExposure a debtor with exposure and no account has Status
// NO_ACCOUNT and no terms.
type CreditAccount struct {
	LegalEntityID string
	DebtorPartyID string
	Revision      int64
	Status        string // CreditOK or CreditOnHold
	Terms         []CreditTerms
	Holds         []CreditHold
	Exposure      kernel.Money
	Unbilled      kernel.Money
	Billed        kernel.Money // what is open of the debtor's debit notes on open jobs, in the functional currency
	OverLimit     bool
	// OnAccount is what the debtor's receipts have unallocated, per currency. It does not lower the exposure.
	OnAccount []kernel.Money
}

// BillingProfileVersion is a version of a debtor's billing profile.
type BillingProfileVersion struct {
	Version           int
	BuyerKind         string
	Title             string
	TaxpayerNumber    string
	PartyRevision     int64
	RegisteredAddress string
	RegisteredPhone   string
	BankName          string
	BankAccount       string
	EvidenceRef       string
	SetBy             string
	SetAt             time.Time
}

// BillingProfile is a debtor's billing profile with its versions, the latest last.
type BillingProfile struct {
	DebtorPartyID string
	Versions      []BillingProfileVersion
}

// DocumentRef is the issued house document a debit note bills on and when that version was issued.
type DocumentRef struct {
	DocumentID   string
	Version      int
	Number       string
	MasterNumber string
	IssuedAt     time.Time
}

// TaxTotal is a note's total under one tax treatment and rate, in the note's and the functional currency.
type TaxTotal struct {
	Treatment string
	RateBP    int
	Amount    kernel.Money
	Local     kernel.Money
}

// DebtorInvoiceLine is a line of a note: the job's line and charge code, the amount in the side's currency, in the
// note's currency and in the functional currency, the conversion and the tax treatment. A debit note line names the
// sell side version it billed, a credit note line the debit note line it reverses (amounts negated).
type DebtorInvoiceLine struct {
	LineNo        int
	JobLineNo     int
	SideVersion   int
	CreditsLineNo int
	ChargeCode    string
	Amount        kernel.Money
	Invoiced      kernel.Money
	Local         kernel.Money
	FX            *FxSnapshot
	TaxVersion    int
	Treatment     string
	RateBP        int
}

// DebtorInvoice is a debit note or a credit note. Dates are YYYY-MM-DD. A debit note has its due date, payment terms
// (TermsVersion 0 without a credit account), house document, the credit notes reversing its lines and Open, the total
// not reversed; a credit note names the debit note it reverses, the reason and whether it wrote the charges off.
type DebtorInvoice struct {
	InvoiceID        string
	Kind             string
	Number           string
	JobID            string
	LegalEntityID    string
	ShipmentID       string
	DebtorPartyID    string
	DebtorRevision   int64
	BillingProfile   BillingProfileVersion
	Currency         kernel.CurrencyCode
	InvoiceDate      string
	DueDate          string
	PaymentDays      int
	PaymentBasis     string
	TermsVersion     int
	Document         DocumentRef
	CreditsInvoiceID string
	Reason           string
	WriteOff         bool
	State            string
	Total            kernel.Money
	LocalTotal       kernel.Money
	Settled          kernel.Money // a debit note: what receipts settle of it
	Open             kernel.Money // a debit note: its total less its credit notes and what is settled
	TaxTotals        []TaxTotal
	Lines            []DebtorInvoiceLine
	CreditNotes      []string
	Settlements      []InvoiceSettlement // GetDebtorInvoice: the allocations that settle a debit note
	IssuedBy         string
	IssuedAt         time.Time
}

// InvoiceSettlement is an allocation of a receipt that settles part of a debit note.
type InvoiceSettlement struct {
	ReceiptID    string
	AllocationNo int
	Amount       kernel.Money
	Date         string
}

// Debtor receipt states.
const (
	ReceiptRecorded  = "RECORDED"
	ReceiptCancelled = "CANCELLED"
)

// DebtorReceiptAllocation is an allocation of a receipt to a debit note; a reversed one names who reversed it and why.
type DebtorReceiptAllocation struct {
	AllocationNo  int
	InvoiceID     string
	Amount        kernel.Money
	Date          string
	AllocatedBy   string
	AllocatedAt   time.Time
	ReversedBy    string
	ReverseReason string
	ReversedOn    string
	ReversedAt    time.Time
}

// DebtorReceipt is money received from a debtor: the debtor is credited with Amount and BankCharges, of which
// Unallocated is on account.
type DebtorReceipt struct {
	TenantID       string
	ReceiptID      string
	Revision       int64
	LegalEntityID  string
	DebtorPartyID  string
	DebtorRevision int64
	Amount         kernel.Money
	BankCharges    kernel.Money
	Unallocated    kernel.Money
	ReceivedOn     string
	EvidenceSource string
	EvidenceRef    string
	State          string
	RecordedBy     string
	RecordedAt     time.Time
	CancelReason   string
	CancelledBy    string
	CancelledOn    string
	CancelledAt    time.Time
	Allocations    []DebtorReceiptAllocation
}

// Statement entry kinds.
const (
	EntryDebitNote        = "DEBIT_NOTE"
	EntryCreditNote       = "CREDIT_NOTE"
	EntryReceipt          = "RECEIPT"
	EntryReceiptCancelled = "RECEIPT_CANCELLED"
)

// StatementEntry is a line of a statement: a debit note adds to what the debtor owes; a credit note and a receipt (its
// amount and bank charges) take off; a cancelled receipt adds back. Number is a note's number or a receipt's evidence;
// Balance is the balance after the entry.
type StatementEntry struct {
	Date           string
	Kind           string
	ID             string
	Number         string
	DocumentNumber string
	Amount         kernel.Money
	Balance        kernel.Money
}

// StatementItem is a debit note open at the end of a statement.
type StatementItem struct {
	InvoiceID      string
	Number         string
	DocumentNumber string
	InvoiceDate    string
	DueDate        string
	Total          kernel.Money
	Credited       kernel.Money
	Settled        kernel.Money
	Open           kernel.Money
	DaysOverdue    int
}

// StatementReceipt is a receipt with money on account at the end of a statement.
type StatementReceipt struct {
	ReceiptID      string
	ReceivedOn     string
	EvidenceSource string
	EvidenceRef    string
	Credited       kernel.Money
	Unallocated    kernel.Money
}

// StatementAgeing sums the open items by days past due at the end of a statement.
type StatementAgeing struct {
	NotDue     kernel.Money
	Days1To30  kernel.Money
	Days31To60 kernel.Money
	Days61To90 kernel.Money
	Over90     kernel.Money
}

// CurrencyStatement is a debtor's statement in one currency. Closing is Opening and the entries, and also the open
// items less the money on account.
type CurrencyStatement struct {
	Currency  kernel.CurrencyCode
	Opening   kernel.Money
	Entries   []StatementEntry
	Closing   kernel.Money
	OpenItems []StatementItem
	OnAccount []StatementReceipt
	Ageing    StatementAgeing
}

// DebtorStatement is a debtor's account with a legal entity over a period, per currency.
type DebtorStatement struct {
	LegalEntityID string
	DebtorPartyID string
	From          string
	To            string
	Currencies    []CurrencyStatement
}

// Queries.
type (
	GetFinancePolicy    struct{ TenantID, LegalEntityID string }
	GetFreightJob       struct{ TenantID, JobID string }
	GetJobProfit        struct{ TenantID, JobID string }
	ListJobsForShipment struct{ TenantID, ShipmentID string }
	GetCreditAccount    struct{ TenantID, LegalEntityID, DebtorPartyID string }
	// ListCreditExposure lists a legal entity's debtors with an account or with exposure, over-limit and accountless
	// ones first, for the daily review OD-8 A asks for.
	ListCreditExposure struct{ TenantID, LegalEntityID string }
	GetBillingProfile  struct{ TenantID, DebtorPartyID string }
	GetDebtorInvoice   struct{ TenantID, InvoiceID string }
	// ListOpenDebtorItems lists a debtor's open debit notes with a legal entity (issued, with an amount neither reversed
	// nor settled), the oldest due first.
	ListOpenDebtorItems struct{ TenantID, LegalEntityID, DebtorPartyID string }
	GetDebtorReceipt    struct{ TenantID, ReceiptID string }
	// ListDebtorReceipts lists a debtor's receipts with a legal entity, in the order received.
	ListDebtorReceipts struct{ TenantID, LegalEntityID, DebtorPartyID string }
	// GetDebtorStatement states a debtor's account with a legal entity from From to To (civil dates, YYYY-MM-DD, at
	// most 366 days apart), per currency and by business dates: notes count from their invoice date, receipts from the
	// date received until the date cancelled, allocations from their date until the date reversed. It gives the opening
	// balance, the entries of the period, the closing balance and, as of To, the open items with their days overdue,
	// the money on account and the ageing.
	GetDebtorStatement struct{ TenantID, LegalEntityID, DebtorPartyID, From, To string }
	// ListDebitNotesOnChangedDocuments lists a legal entity's open debit notes whose house document changed after
	// billing, for a person to decide (a credit note, a corrected bill): FIN asks DOC when asked, as it consumes no DOC
	// event yet.
	ListDebitNotesOnChangedDocuments struct{ TenantID, LegalEntityID string }
)

// Changes of a billed house document.
const (
	DocumentCancelled = "DOCUMENT_CANCELLED" // the shipment has no valid house document now
	DocumentReplaced  = "DOCUMENT_REPLACED"  // another house document holds the shipment now
	DocumentAmended   = "DOCUMENT_AMENDED"   // a later version of the document was issued
)

// DocumentChange is an open debit note whose house document changed: the document billed and the valid one now (nil
// when there is none).
type DocumentChange struct {
	InvoiceID     string
	Number        string
	JobID         string
	ShipmentID    string
	DebtorPartyID string
	Change        string
	Billed        DocumentRef
	Current       *DocumentRef
}

// PoolCostVersion is one immutable version of a pool cost line: the creditor (empty while not known), the amount in
// its currency and in the functional currency at the ESTIMATE rate, and, from the second version on, the reason.
type PoolCostVersion struct {
	Version          int
	CreditorPartyID  string
	CreditorRevision int64
	Amount           kernel.Money
	Local            kernel.Money
	Status           string // ACCRUED, or RELEASED (amount zero)
	Reason           string
	RecordedAt       time.Time
	EnteredAt        time.Time
}

// PoolCostLine is one cost of the whole consolidation: a charge code, the supplier's reference (the MAWB, the
// co-loader or CFS booking) and its versions; Method is its default apportionment method. Status is its accrual status;
// Matched what creditor invoices' live matches consume of it; Cost its current cost, what is matched plus what is still
// accrued (once matched final or released, what is matched), in its currency and in the functional currency.
type PoolCostLine struct {
	LineNo       int
	ChargeCode   string
	Description  string
	SupplierRef  string
	Method       string
	Source       string // MANUAL or RATED
	BasisID      string // RATED: RATE's CONSOL_COST_ESTIMATE basis and its line
	BasisLineNo  int
	RecordedAt   time.Time
	EnteredAt    time.Time
	Versions     []PoolCostVersion
	Status       string
	Matched      kernel.Money
	MatchedLocal kernel.Money
	Cost         kernel.Money
	CostLocal    kernel.Money
}

// PoolShare is one house job's share of a pool cost line under an apportionment: the basis quantity it was weighted
// by (chargeable grams, cubic centimetres, pieces or milli freight tonnes) and the share in the line's currency and
// in the functional currency, each conserved exactly.
type PoolShare struct {
	ShareNo    int
	ShipmentID string
	JobID      string
	Quantity   int64
	Unit       string
	Amount     kernel.Money
	Local      kernel.Money
}

// ApportionedLine is one line's shares under an apportionment, at the line version it divided, and the line's cost it
// divided (Amount, Local: the version's figures before FIN V0.5).
type ApportionedLine struct {
	LineNo      int
	LineVersion int
	Method      string
	Reason      string // the MANUAL reason
	Amount      kernel.Money
	Local       kernel.Money
	Shares      []PoolShare
}

// PoolApportionment is one immutable division of pool cost lines over the house jobs: the PhysicalCostBasis v1
// revision and stage it used, and its lines. A line's current shares are those of the latest apportionment covering it.
type PoolApportionment struct {
	ApportionmentNo int
	BasisRevision   int64
	Stage           string // ACTUAL or PLANNED
	By              string
	At              time.Time
	EnteredAt       time.Time
	Lines           []ApportionedLine
}

// ConsolCostPool is a consolidation's cost pool under one legal entity (FIN V0.4, FB3).
type ConsolCostPool struct {
	TenantID           string
	PoolID             string
	Revision           int64
	ConsolidationID    string
	LegalEntityID      string
	Mode               string // AIR or SEA_LCL
	FunctionalCurrency kernel.CurrencyCode
	BookCode           string
	State              string
	OpenedBy           string
	OpenedAt           time.Time
	Lines              []PoolCostLine
	Apportionments     []PoolApportionment
	Closures           []PoolClosure
}

// HouseProfit is one house job's economics within a consolidation, in the functional currency: the job's own revenue,
// invoiced part and cost, its share of the pool, and Profit = Revenue - Cost - PoolCost.
type HouseProfit struct {
	ShipmentID string
	JobID      string
	Revenue    kernel.Money
	Invoiced   kernel.Money
	Cost       kernel.Money
	PoolCost   kernel.Money
	Profit     kernel.Money
}

// ConsolProfit is a consolidation's economics under its pool's legal entity, in the functional currency: the pooled
// cost, its lines' current costs (Matched is what creditor invoices matched of it), the part the latest apportionments
// cover, and the houses of those apportionments with their job economics. Houses is empty while nothing is
// apportioned.
type ConsolProfit struct {
	ConsolidationID string
	PoolID          string
	LegalEntityID   string
	State           string
	Pooled          kernel.Money
	Matched         kernel.Money
	Apportioned     kernel.Money
	Retained        kernel.Money // kept on the consolidation (RETAINED): the consolidation's own cost
	Unapportioned   kernel.Money
	Houses          []HouseProfit
}

// GetConsolCostPool asks for a pool; GetConsolProfit for a consolidation's economics through its pool (ErrNotFound
// while the consolidation has no pool).
type (
	GetConsolCostPool struct{ TenantID, PoolID string }
	GetConsolProfit   struct{ TenantID, ConsolidationID string }
)

// Queries is FIN's read side. Unknown policies, jobs, accounts, profiles and notes yield ErrNotFound.
type Queries interface {
	GetFinancePolicy(ctx context.Context, q GetFinancePolicy) (FinancePolicy, error)
	GetFreightJob(ctx context.Context, q GetFreightJob) (FreightJob, error)
	GetJobProfit(ctx context.Context, q GetJobProfit) (JobProfit, error)
	ListJobsForShipment(ctx context.Context, q ListJobsForShipment) ([]FreightJob, error)
	GetCreditAccount(ctx context.Context, q GetCreditAccount) (CreditAccount, error)
	ListCreditExposure(ctx context.Context, q ListCreditExposure) ([]CreditAccount, error)
	GetBillingProfile(ctx context.Context, q GetBillingProfile) (BillingProfile, error)
	GetDebtorInvoice(ctx context.Context, q GetDebtorInvoice) (DebtorInvoice, error)
	ListOpenDebtorItems(ctx context.Context, q ListOpenDebtorItems) ([]DebtorInvoice, error)
	ListDebitNotesOnChangedDocuments(ctx context.Context, q ListDebitNotesOnChangedDocuments) ([]DocumentChange, error)
	GetDebtorReceipt(ctx context.Context, q GetDebtorReceipt) (DebtorReceipt, error)
	ListDebtorReceipts(ctx context.Context, q ListDebtorReceipts) ([]DebtorReceipt, error)
	GetDebtorStatement(ctx context.Context, q GetDebtorStatement) (DebtorStatement, error)
	GetConsolCostPool(ctx context.Context, q GetConsolCostPool) (ConsolCostPool, error)
	GetConsolProfit(ctx context.Context, q GetConsolProfit) (ConsolProfit, error)
	GetCreditorInvoice(ctx context.Context, q GetCreditorInvoice) (CreditorInvoice, error)
	ListCreditorInvoices(ctx context.Context, q ListCreditorInvoices) ([]CreditorInvoice, error)
	ListOpenAccruals(ctx context.Context, q ListOpenAccruals) ([]OpenAccrual, error)
	GetCreditorPayment(ctx context.Context, q GetCreditorPayment) (CreditorPayment, error)
	ListCreditorPayments(ctx context.Context, q ListCreditorPayments) ([]CreditorPayment, error)
	GetCreditorAccount(ctx context.Context, q GetCreditorAccount) (CreditorAccount, error)
	GetFapiaoRecord(ctx context.Context, q GetFapiaoRecord) (FapiaoRecord, error)
	ListFapiaoRecords(ctx context.Context, q ListFapiaoRecords) ([]FapiaoRecord, error)
}
