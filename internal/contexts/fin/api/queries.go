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
}

// FinancePolicy is a legal entity's policy.
type FinancePolicy struct {
	LegalEntityID      string
	Revision           int64
	FunctionalCurrency kernel.CurrencyCode
	BookCode           string
	Rates              []ExchangeRateEntry
}

// Job kinds and states.
const (
	KindAirExportHouse    = "AIR_EXPORT_HOUSE"
	KindSeaExportLCLHouse = "SEA_EXPORT_LCL_HOUSE"

	JobOpen      = "OPEN"
	JobCancelled = "CANCELLED"
)

// Sources and statuses of sides: a sell side ESTIMATED (WIP), a cost side ACCRUED.
const (
	SourceQuote  = "QUOTE"
	SourceManual = "MANUAL"

	StatusEstimated = "ESTIMATED"
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

// SideVersion is a version of a sell or cost side; Local is in the functional currency.
type SideVersion struct {
	Version    int
	PartyID    string
	Amount     kernel.Money
	Local      kernel.Money
	FX         *FxSnapshot // nil when the amount is in the functional currency
	Status     string
	Reason     string
	RecordedBy string
	RecordedAt time.Time
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
}

// ChargeCodeProfit is the estimated revenue, cost and profit of a charge code.
type ChargeCodeProfit struct {
	ChargeCode string
	Revenue    kernel.Money
	Cost       kernel.Money
	Profit     kernel.Money
}

// JobProfit is a job's estimated economics in the functional currency: estimates only in V0.1 (sell sides not yet
// invoiced, cost sides not yet matched).
type JobProfit struct {
	JobID    string
	Revision int64
	ByCode   []ChargeCodeProfit
	Revenue  kernel.Money
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

// CreditAccount is a debtor's account with a legal entity; Exposure is the estimated sell of the debtor's open jobs
// of the legal entity (V0.1 has no invoices).
type CreditAccount struct {
	LegalEntityID string
	DebtorPartyID string
	Revision      int64
	Status        string // CreditOK or CreditOnHold
	Terms         []CreditTerms
	Holds         []CreditHold
	Exposure      kernel.Money
	OverLimit     bool
}

// Queries.
type (
	GetFinancePolicy    struct{ TenantID, LegalEntityID string }
	GetFreightJob       struct{ TenantID, JobID string }
	GetJobProfit        struct{ TenantID, JobID string }
	ListJobsForShipment struct{ TenantID, ShipmentID string }
	GetCreditAccount    struct{ TenantID, LegalEntityID, DebtorPartyID string }
	// ListCreditExposure lists a legal entity's credit accounts with their exposure, the over-limit ones first, for
	// the daily review OD-8 A asks for.
	ListCreditExposure struct{ TenantID, LegalEntityID string }
)

// Queries is FIN's read side. Unknown policies, jobs and accounts yield ErrNotFound.
type Queries interface {
	GetFinancePolicy(ctx context.Context, q GetFinancePolicy) (FinancePolicy, error)
	GetFreightJob(ctx context.Context, q GetFreightJob) (FreightJob, error)
	GetJobProfit(ctx context.Context, q GetJobProfit) (JobProfit, error)
	ListJobsForShipment(ctx context.Context, q ListJobsForShipment) ([]FreightJob, error)
	GetCreditAccount(ctx context.Context, q GetCreditAccount) (CreditAccount, error)
	ListCreditExposure(ctx context.Context, q ListCreditExposure) ([]CreditAccount, error)
}
