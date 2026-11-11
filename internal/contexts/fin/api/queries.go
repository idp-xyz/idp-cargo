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

// InvoiceSeries is a number series of a note kind.
type InvoiceSeries struct {
	Kind        string
	Prefix      string
	Width       int
	FirstNumber int64
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
}

// Job kinds and states.
const (
	KindAirExportHouse    = "AIR_EXPORT_HOUSE"
	KindSeaExportLCLHouse = "SEA_EXPORT_LCL_HOUSE"

	JobOpen      = "OPEN"
	JobCancelled = "CANCELLED"
)

// Sources and statuses of sides: a sell side ESTIMATED (WIP) or INVOICED (on a debit note), a cost side ACCRUED.
const (
	SourceQuote  = "QUOTE"
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
	RecordedBy    string
	RecordedAt    time.Time
}

// ChargeLine is a charge of a job with its side versions, the latest last.
type ChargeLine struct {
	LineNo      int
	ChargeCode  string
	Description string
	Source      string
	BasisNo     int
	QuoteLineNo int
	Reason      string
	Sell        []SideVersion
	Cost        []SideVersion
}

// AppliedBasis is a quote applied to the job for an order.
type AppliedBasis struct {
	BasisNo       int
	BasisID       string
	QuoteID       string
	QuoteRevision int64
	OrderID       string
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

// JobProfit is a job's economics in the functional currency: revenue from the latest sell sides, billed ones at their
// debit note's local amount (Invoiced is that part), cost from the cost estimates (not yet matched).
type JobProfit struct {
	JobID    string
	Revision int64
	ByCode   []ChargeCodeProfit
	Revenue  kernel.Money
	Invoiced kernel.Money
	Cost     kernel.Money
	Profit   kernel.Money
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
	Billed        kernel.Money
	OverLimit     bool
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

// DocumentRef is the issued house document a debit note bills on.
type DocumentRef struct {
	DocumentID   string
	Version      int
	Number       string
	MasterNumber string
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
// not reversed; a credit note names the debit note it reverses and the reason.
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
	State            string
	Total            kernel.Money
	LocalTotal       kernel.Money
	Open             kernel.Money
	TaxTotals        []TaxTotal
	Lines            []DebtorInvoiceLine
	CreditNotes      []string
	IssuedBy         string
	IssuedAt         time.Time
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
	// ListOpenDebtorItems lists a debtor's open debit notes with a legal entity (not every line reversed), the oldest
	// due first.
	ListOpenDebtorItems struct{ TenantID, LegalEntityID, DebtorPartyID string }
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
}
