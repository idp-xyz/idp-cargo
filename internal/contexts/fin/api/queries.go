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
	// InForceThreshold is the version in force now (FIN's clock), 0 when none; ProposedThreshold the version
	// that waits for approval, 0 when none (both since V2.100). The versions above hold who set each, when, and
	// how it was decided.
	InForceThreshold  int
	ProposedThreshold int
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
	// Pending is true for a former share pending finance's confirmation (FIN V2.97): it is in FormerShares, and under
	// F6 INCLUDED_MARKED also in the job's PoolCost and Profit.
	Pending bool
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
	// Since FIN V2.93: the re-routing losses the job bears under the pool lines' latest apportionments (in PoolCost
	// and Profit; ReroutingLoss is their sum), and the former shares of pools of consolidations the job's house left,
	// pending finance's confirmation: they are not in PoolCost or Profit, and are listed here (FormerSharesPending is
	// their sum). The query reads PHY's basis of each pool the job has a share in, and fails when PHY does not answer.
	ReroutingLosses     []JobReroutingLoss
	ReroutingLoss       kernel.Money
	FormerShares        []JobPoolShare
	FormerSharesPending kernel.Money
	// Since FIN V2.97, the basis the profit is told on, from the legal entity's setting F6: SEPARATE (a former share
	// pending is in neither PoolCost nor Profit) or INCLUDED_MARKED (it is in both, and PendingInProfit is the part of
	// them that is pending; zero under SEPARATE). Figures under the two bases are not to be compared. SettingsVersion
	// is the version of the settings the answer was read under.
	ProfitBasis     string
	PendingInProfit kernel.Money
	SettingsVersion int
	// ProfitVersion is an opaque text, the SHA-256 in hex of what the profit is computed from (the job's revision, the
	// revision of each pool the job has a share in, the settings version) and the figures: equal figures from the same
	// sources give the same version, and any change gives another. Clients compare it for equality only, and send it
	// with a cost review request.
	ProfitVersion string
}

// JobReroutingLoss is a re-routing loss a job bears on a pool's cost line, with its voucher and the house's departure.
type JobReroutingLoss struct {
	PoolID     string
	LineNo     int
	Amount     kernel.Money
	Local      kernel.Money
	Voucher    string
	DepartedAt time.Time
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
	GetFinancePolicy struct{ TenantID, LegalEntityID string }
	GetFreightJob    struct{ TenantID, JobID string }
	// GetJobProfit reads a job's economics. An ActorID is checked for ReadProfit; an in-process caller that names none
	// reads as the system, as before V2.99 (the HTTP adapter always names the token's actor).
	GetJobProfit        struct{ TenantID, ActorID, JobID string }
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
// by (chargeable or gross grams, cubic centimetres, pieces, milli freight tonnes or shipments) and the share in the
// line's currency and in the functional currency, each conserved exactly. NotUsed is true for a house the apportionment
// left out: its quantity is 0, it has no unit and its amounts are 0 (false, as before, for every house that took part).
type PoolShare struct {
	ShareNo    int
	ShipmentID string
	JobID      string
	NotUsed    bool
	Quantity   int64
	Unit       string
	Amount     kernel.Money
	Local      kernel.Money
}

// PoolAttribution is who bears one house's former share of one cost line under an apportionment (FIN V2.93): the loss
// the house's own job bears and the consolidation's exception cost with the party responsible for it (empty when it is
// 0), each in the line's currency and in the functional currency (together with the shares and the amount to be
// allocated they make up the line's cost); the voucher and the reason; the house's departure as the business date; and
// whether the apportionment only carries forward what an earlier one stated. ByDefault says that nobody stated it:
// F8 DEFAULT_TO_REMAINING made it, with no loss, no exception cost, no party and no voucher (since FIN V2.98). ByRatio
// says that it is carried with its loss and exception cost scaled to its line's cost by F9 KEEP_RATIO (likewise).
type PoolAttribution struct {
	ShipmentID     string
	JobID          string
	Loss           kernel.Money
	LossLocal      kernel.Money
	Exception      kernel.Money
	ExceptionLocal kernel.Money
	Party          string
	PartyReference string
	Voucher        string
	Reason         string
	DepartedAt     time.Time
	Carried        bool
	ByDefault      bool
	ByRatio        bool
}

// ApportionedLine is one line's shares under an apportionment, at the line version it divided, and the line's cost it
// divided (Amount, Local: the version's figures before FIN V0.5). Since FIN V2.93 the line's cost is also divided into
// the attributions of the houses that left and ToAllocate, what remains when no house is live (zero otherwise).
type ApportionedLine struct {
	LineNo          int
	LineVersion     int
	Method          string
	Reason          string // the MANUAL reason
	Amount          kernel.Money
	Local           kernel.Money
	Shares          []PoolShare
	Attributions    []PoolAttribution
	ToAllocate      kernel.Money
	ToAllocateLocal kernel.Money
}

// HouseLoss is a house's re-routing loss on a cost line, borne by its own job; ExceptionByParty is the consolidation's
// exception cost the attributions of a line state for one party; FormerShare is a house that left the consolidation and
// its used share of a line under the apportionment in force, pending finance's confirmation: it is nobody's cost in the
// profit of the house's job, and is listed apart (the amounts in the line's currency and in the functional currency).
type (
	HouseLoss struct {
		ShipmentID string
		JobID      string
		Amount     kernel.Money
		Local      kernel.Money
	}
	ExceptionByParty struct {
		Party  string
		Amount kernel.Money
		Local  kernel.Money
	}
	FormerShare struct {
		ShipmentID string
		JobID      string
		Amount     kernel.Money
		Local      kernel.Money
		End        string // DETACHED, TRANSFERRED or SHIPMENT_CANCELLED
		EndedAt    time.Time
	}
)

// LineAttributions is what the latest apportionment of a cost line leaves out of its shares (FIN V2.93): the losses
// (house, job), the exception costs by party with the ones whose party is PENDING apart, the amount to be allocated
// and the former shares pending finance's confirmation. With the line's shares they make up its cost.
type LineAttributions struct {
	LineNo                int
	Losses                []HouseLoss
	Exceptions            []ExceptionByParty
	ExceptionPending      kernel.Money
	ExceptionPendingLocal kernel.Money
	ToAllocate            kernel.Money
	ToAllocateLocal       kernel.Money
	FormerShares          []FormerShare
}

// PoolApportionment is one immutable division of pool cost lines over the house jobs: the PhysicalCostBasis v1
// revision and stage it used, and its lines. A line's current shares are those of the latest apportionment covering it.
type PoolApportionment struct {
	ApportionmentNo int
	BasisRevision   int64
	Stage           string // ACTUAL or PLANNED
	By              string // who submitted it
	At              time.Time
	EnteredAt       time.Time
	Lines           []ApportionedLine
	PreviewNo       int // the preview it was submitted from (FIN V2.91); 0 on one made before
	// SettingsVersion is the version of FIN's settings in force when it was submitted, the same as its preview's (FIN
	// V2.97); 0 when no change set was in force, and on one made before.
	SettingsVersion int
}

// PoolPreview is one preview of an apportionment (FIN V2.91): the figures it computed, the basis revision and stage it
// computed them on, who previewed it and when, the SHA-256 of the fingerprint of what it was computed on, and the
// apportionment that submitted it (0 while it is not submitted). A preview is not an apportionment: nothing in a job's
// cost or in the pool's current shares comes from it.
type PoolPreview struct {
	PreviewNo         int
	BasisRevision     int64
	Stage             string // ACTUAL or PLANNED
	By                string
	At                time.Time
	EnteredAt         time.Time
	Lines             []ApportionedLine
	FingerprintSHA256 string
	SubmittedAs       int
	// SettingsVersion is the version of FIN's settings the preview was made under, which its fingerprint holds (FIN
	// V2.97); 0 when no change set was in force, and on one made before.
	SettingsVersion int
}

// WorkUnit is a container of a consolidation that PHY reads in rework.
type WorkUnit struct {
	UnitNo      int
	ExecutionID string // the stuffing opened for rework
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
	Previews           []PoolPreview
	Closures           []PoolClosure
	// Attributions say, per cost line apportioned, what its latest apportionment leaves out of the shares (FIN V2.93).
	// A former share is told by PHY's basis, which the query reads as it reads WorkUnits.
	Attributions []LineAttributions
	// InWork is true while PHY reads a container of the consolidation in rework (FIN V2.91): the pool is frozen, and
	// the last submitted apportionment in Apportionments is the one that stands, in work. WorkUnits are the containers.
	InWork    bool
	WorkUnits []WorkUnit
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
	// PendingInProfit is the part of PoolCost, and so of Profit, that is a former share pending finance's confirmation
	// (FIN V2.97, F6 INCLUDED_MARKED); zero under SEPARATE.
	PendingInProfit kernel.Money
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
	// Since FIN V2.93, per cost line and in total (functional currency): the houses' re-routing losses, the exception
	// costs (ExceptionsPending those whose party is not established, which are part of Exceptions), what is still to be
	// allocated, and the former shares pending finance's confirmation, which are in no house's cost. The costs are
	// conserved: Pooled = Apportioned + Retained + Losses + Exceptions + ToAllocated + FormerSharesPending +
	// Unapportioned.
	Lines               []LineAttributions
	Losses              kernel.Money
	Exceptions          kernel.Money
	ExceptionsPending   kernel.Money
	ToAllocated         kernel.Money
	FormerSharesPending kernel.Money
	// Since FIN V2.97: the basis the houses' profit is told on, from the legal entity's setting F6 (SEPARATE, or
	// INCLUDED_MARKED, under which a former share pending is in its house's PoolCost and Profit and marked in
	// HouseProfit.PendingInProfit), and the version of the settings the answer was read under. Figures under the two
	// bases are not to be compared.
	ProfitBasis     string
	SettingsVersion int
	// InWork is true while PHY reads a container of the consolidation in rework: what the houses bear is the last
	// submitted apportionment, in work. WorkUnits are the containers.
	InWork    bool
	WorkUnits []WorkUnit
}

// GetConsolCostPool asks for a pool; GetConsolProfit for a consolidation's economics through its pool (ErrNotFound
// while the consolidation has no pool).
type (
	GetConsolCostPool struct{ TenantID, PoolID string }
	GetConsolProfit   struct{ TenantID, ConsolidationID string }
)

// FIN V2.94: what a cost pool still needs, and what a preview would change before it is submitted.

// Who acts on a pending item, and its kinds, in the order the list gives them.
const (
	ActorFinance    = "FINANCE"
	ActorOperations = "OPERATIONS"

	PendingInWork             = "IN_WORK"
	PendingFormerSharePending = "FORMER_SHARE_PENDING"
	PendingToBeAllocated      = "TO_BE_ALLOCATED"
	PendingExceptionCause     = "EXCEPTION_CAUSE_PENDING"
	PendingReopenToAttribute  = "REOPEN_TO_ATTRIBUTE"
	PendingLineNotApportioned = "LINE_NOT_APPORTIONED"
	PendingLineNotOnBasis     = "LINE_NOT_ON_CURRENT_BASIS"
	PendingCostNotFinal       = "COST_NOT_FINAL"
	PendingInvoiceHeld        = "INVOICE_HELD"
)

// GetConsolCostPoolPendingItems lists what a pool needs. It is read-only and reads the pool and PHY's basis outside any
// transaction, as the pool's other queries do; when PHY does not answer it fails and lists nothing (an empty list would
// say that nothing is pending). An unknown pool yields ErrNotFound.
type GetConsolCostPoolPendingItems struct{ TenantID, PoolID string }

// PoolPendingItem is one pending item of a pool, in the order of the list (by kind, line, house). LineNo, ShipmentID
// and UnitNo say where it applies (zero or empty when it applies to no line, house or container); Amount and
// AmountLocal are the amount it concerns in the line's currency and in the functional currency (zero values when it
// concerns none); Message says what to do next and whom to ask; Actor is FINANCE or OPERATIONS.
type PoolPendingItem struct {
	Kind        string
	LineNo      int
	ShipmentID  string
	UnitNo      int
	Amount      kernel.Money
	AmountLocal kernel.Money
	Message     string
	Actor       string
}

// GetConsolCostPreviewEffect asks what submitting a stored preview would change. It is read-only, needs no particular
// person and changes nothing; it reads the pool, PHY's basis and the houses' open jobs outside FIN's transactions. An
// unknown pool or preview yields ErrNotFound.
type GetConsolCostPreviewEffect struct {
	TenantID, PoolID string
	PreviewNo        int
}

// PreviewLineEffect is one line of the preview: its cost in force now and in the preview and what the preview divides
// it into. Conserved says whether the shares, the losses, the exception costs, the amount to be allocated and what is
// retained sum to the preview's cost, in the line's currency and in the functional currency: a figure shown, not a
// refusal. CostChanged says that the line's cost now is not the one the preview divided.
type PreviewLineEffect struct {
	LineNo      int
	Method      string
	Reason      string
	CostNow     kernel.Money
	Cost        kernel.Money
	CostChanged bool
	Shares      kernel.Money
	Losses      kernel.Money
	Exceptions  kernel.Money
	ToAllocate  kernel.Money
	// ToAllocateNow is what the line in force now has to be allocated, to compare with ToAllocate (the preview's).
	ToAllocateNow kernel.Money
	Retained      kernel.Money
	Conserved     bool
}

// PreviewHouseEffect is one house's amount on one line: in force now (kind SHARE, FORMER_SHARE_PENDING, LOSS or NONE)
// and in the preview (SHARE, LOSS or NONE), the difference, and the change to the house's job profit if the preview is
// submitted, in the functional currency: a share or a loss lowers the profit by its amount, a former share pending is
// not in it, so attributing it a loss lowers it by the loss and attributing it nothing leaves it.
type PreviewHouseEffect struct {
	LineNo          int
	ShipmentID      string
	JobID           string
	Live            bool
	Former          bool
	NowKind         string
	Now             kernel.Money
	NowLocal        kernel.Money
	PreviewKind     string
	Preview         kernel.Money
	PreviewLocal    kernel.Money
	Difference      kernel.Money
	DifferenceLocal kernel.Money
	ProfitChange    kernel.Money
}

// PreviewHouseTotal is the change to one house's job profit over every line of the preview.
type PreviewHouseTotal struct {
	ShipmentID   string
	JobID        string
	ProfitChange kernel.Money
}

// PreviewExceptionEffect is the exception cost of one party on one line before and after, in the line's currency and in
// the functional currency (party PENDING: the cause is not established yet).
type PreviewExceptionEffect struct {
	LineNo      int
	Party       string
	Before      kernel.Money
	After       kernel.Money
	BeforeLocal kernel.Money
	AfterLocal  kernel.Money
}

// PreviewReason is why a house's figure changes: how and when its place in the consolidation ended and the operator's
// reason (PHY's former house), and what the preview's attribution says (party, reference, voucher, reason, and whether
// it only carries an attribution in force). Since FIN V2.98 ByDefault says that nobody stated the attribution and F8
// DEFAULT_TO_REMAINING made it, ByRatio that it is carried scaled by F9 KEEP_RATIO, and Says what that does.
type PreviewReason struct {
	LineNo          int
	ShipmentID      string
	Former          bool
	End             string // DETACHED, TRANSFERRED or SHIPMENT_CANCELLED
	EndReason       string
	EndedBy         string
	EndedAt         time.Time
	TransferredTo   string
	Attributed      bool
	Party           string
	PartyReference  string
	Voucher         string
	AttributionNote string
	Carried         bool
	ByDefault       bool
	ByRatio         bool
	Says            string
}

// PreviewEffect is what submitting a stored preview would change against the apportionment in force of each of its
// lines. Current is whether the submission would accept the preview now (the facts it was computed on are unchanged)
// and Changes names what differs when it is not (up to five facts, then "and N more"); a preview already submitted
// (SubmittedAs, the apportionment that took it) is not Current and has no Changes. InWork says that PHY reads a
// container in rework, which a submission refuses. The other refusals of a submission are not told here.
type PreviewEffect struct {
	PreviewNo   int
	Stage       string
	PreviewedBy string
	PreviewedAt time.Time
	SubmittedAs int
	Current     bool
	Changes     []string
	InWork      bool
	// PoolClosed says that the pool is CLOSED, which a submission refuses: the preview is not Current, and WayOut says
	// what to do first (reopen the pool with a reason).
	PoolClosed  bool
	WayOut      string
	Lines       []PreviewLineEffect
	Houses      []PreviewHouseEffect
	HouseTotals []PreviewHouseTotal
	Exceptions  []PreviewExceptionEffect
	Reasons     []PreviewReason
}

// GetFinSettings answers FIN's settings in force for a scope (ScopeKind TENANT with no ScopeID, or LEGAL_ENTITY with
// its ID) as of a time (zero: now): each item's value, its parameter, the scope the value comes from (its own, the
// company's, or none when the default applies) and the settings version in force.
type GetFinSettings struct {
	TenantID  string
	ScopeKind string
	ScopeID   string
	AsOf      time.Time
}

// FinSetting is one item's value in force. FromScopeKind is TENANT, LEGAL_ENTITY or, with Default, the default of the
// standard profile, which no change set set. Threshold is in the legal entity's functional currency.
type FinSetting struct {
	Key           string
	Value         string
	Threshold     *kernel.Money
	FromScopeKind string
	FromScopeID   string
	Default       bool
	// ChangeID is the change set that set the value; Exception says that the value of a legal entity is looser than the
	// company's, an approved exception that stays in force when the company tightens.
	ChangeID  string
	Exception bool
}

// FinSettings is the settings in force for a scope at a time, with the version in force (0 before any change set).
type FinSettings struct {
	TenantID  string
	ScopeKind string
	ScopeID   string
	AsOf      time.Time
	Version   int
	Items     []FinSetting
}

// ListFinSettingsChanges lists the tenant's change sets in the order proposed.
type ListFinSettingsChanges struct {
	TenantID string
}

// FinSettingEntryView is an entry of a change set; ThresholdMinor is in the functional currency of the legal entity.
type FinSettingEntryView struct {
	ScopeKind      string
	ScopeID        string
	Key            string
	Value          string
	ThresholdMinor *int64
}

// Statuses of a change set: PROPOSED (waiting for a second person), APPROVED (its time not yet come), IN_FORCE and
// CANCELLED.
const (
	SettingsProposed  = "PROPOSED"
	SettingsApproved  = "APPROVED"
	SettingsInForce   = "IN_FORCE"
	SettingsCancelled = "CANCELLED"
	SettingsRefused   = "REFUSED"
)

// FinSettingsChange is a change set with its history: who proposed it, why and when, whether it loosens, who approved
// it and the version it made (AutoApproved when its proposer did, because it only tightens), who cancelled it. Times
// are FIN's clock at each step; InForceAt is when it takes effect, the later of EffectiveAt and the approval.
type FinSettingsChange struct {
	ChangeID     string
	Status       string
	Reason       string
	Loosening    bool
	Entries      []FinSettingEntryView
	ProposedBy   string
	ProposedAt   time.Time
	EffectiveAt  time.Time
	ApprovedBy   string
	ApprovedAt   time.Time
	Version      int
	InForceAt    time.Time
	AutoApproved bool
	// RiskAcknowledgement is the written acknowledgement of a single administrator who approved their own loosening, and
	// BasisOnboardingID the onboarding of the declaration of the single administrator that approval relies on (V2.102, batch F).
	RiskAcknowledgement string
	BasisOnboardingID   string
	// HeldForOnboarding is the onboarding the change set is held for, empty for an ordinary change set (V2.102, batch F).
	HeldForOnboarding string
	CancelledBy       string
	CancelledAt       time.Time
	CancelReason      string
	// CancelledRole is the permission the canceller acted under: CONFIGURER (ConfigureSettings) or APPROVER (ApproveSettings).
	CancelledRole string
}

// GetFinSettingsCatalogue answers the catalogue of FIN's settings.
type GetFinSettingsCatalogue struct{}

// FinSettingValue is one value of an item; Parameter names the parameter it asks for, if any.
type FinSettingValue struct {
	ID        string
	TitleZH   string
	TitleEN   string
	Parameter string
}

// FinSettingItem is one item of the catalogue, its values from strict to loose.
type FinSettingItem struct {
	Key             string
	TitleZH         string
	TitleEN         string
	Values          []FinSettingValue
	Default         string
	ScopeKinds      []string
	InternalControl bool
	Explanation     string
}

// FinReferenceItem is a fact FIN keeps in the finance policy with its own history (E1 to E4), listed in the catalogue
// though the settings commands do not change it: what it governs, the commands that change it, and whether a change
// needs a second person (ApprovalNote says which).
type FinReferenceItem struct {
	Key           string
	TitleZH       string
	TitleEN       string
	Governs       string
	ChangedBy     string
	NeedsApproval bool
	ApprovalNote  string
}

// FinSettingsCatalogue is the catalogue, and the same as a Markdown table (for the manual). Reference lists E1 to E4.
type FinSettingsCatalogue struct {
	// Version names the catalogue's content (its items, their values in order and defaults, scopes and controls).
	Version   string
	Items     []FinSettingItem
	Reference []FinReferenceItem
	Table     string
}

// ListJobProfits lists the profit of the freight jobs of a legal entity that are not cancelled, in job id order, in
// pages (FIN V2.99): LossOnly keeps the jobs whose profit is below 0, Limit is 1 to 500 and After the job id the page
// follows. The actor needs ReadProfit.
type ListJobProfits struct {
	TenantID      string
	ActorID       string
	LegalEntityID string
	LossOnly      bool
	Limit         int
	After         string
}

// JobProfitRow is one job's profit: revenue, cost (the job's cost and what it bears of pools: Revenue less Cost is
// Profit) and profit in the functional currency, the profit basis (F6), the settings version and the profit version.
type JobProfitRow struct {
	JobID           string
	ShipmentID      string
	CustomerPartyID string
	State           string
	Currency        kernel.CurrencyCode
	Revenue         kernel.Money
	Cost            kernel.Money
	Profit          kernel.Money
	ProfitBasis     string
	SettingsVersion int
	ProfitVersion   string
}

// JobProfitPage is a page of job profits, all read in one snapshot of FIN's rows (PHY's basis of a pool is read just
// before, outside it), as of AsOf on FIN's clock. NextAfter is the After of the next page, empty after the last: with
// LossOnly a page may hold fewer than Limit rows and still have a next page, and a next page may be empty.
type JobProfitPage struct {
	LegalEntityID string
	AsOf          time.Time
	Rows          []JobProfitRow
	NextAfter     string
}

// CostReviewRequest is a request for the review of a loss-making job's costs, with its resolution when it has one. The
// figures are those the requester saw, in the job's functional currency.
type CostReviewRequest struct {
	RequestID       string
	JobID           string
	ShipmentID      string
	LegalEntityID   string
	RequestedBy     string
	RequestedAt     time.Time
	Reason          string
	Revenue         kernel.Money
	Cost            kernel.Money
	Profit          kernel.Money
	ProfitBasis     string
	SettingsVersion int
	ProfitVersion   string
	State           string // OPEN or RESOLVED
	Resolution      *CostReviewResolution
}

// CostReviewResolution is how a request ended.
type CostReviewResolution struct {
	ResolvedBy string
	ResolvedAt time.Time
	Outcome    string
	Note       string
}

// Cost review request states.
const (
	ReviewOpen     = "OPEN"
	ReviewResolved = "RESOLVED"
)

// GetCostReviewRequest reads a request by its id; the actor needs ReadProfit.
type GetCostReviewRequest struct{ TenantID, ActorID, RequestID string }

// GetJobLegalEntity answers the legal entity a job of the tenant was opened under (FIN V2.101), which the job keeps for
// good, so that a caller whose view is limited to some legal entities can be told apart before anything about the job
// is read. The actor needs ReadProfit; an unknown job yields ErrNotFound.
type GetJobLegalEntity struct{ TenantID, ActorID, JobID string }

// ListCostReviewRequests lists the tenant's requests in the order made, of one job when JobID is given, and of one
// State (OPEN or RESOLVED) when given; Limit is 1 to 500 and After the id of the request the page follows.
type ListCostReviewRequests struct {
	TenantID string
	ActorID  string
	JobID    string
	State    string
	Limit    int
	After    string
}

// CostReviewPage is a page of requests; NextAfter is empty after the last.
type CostReviewPage struct {
	Requests  []CostReviewRequest
	NextAfter string
}

// GetFinSettingsChangeImpact tells, before a change of FIN's settings takes effect, what would behave differently if it
// were in force now, compared with what is in force now (FIN V2.100). Name ChangeID, a change set that is proposed or
// approved and not in force yet, or Entries as a proposal would state them (checked as a proposal is; nothing is
// proposed), not both. It is read in one read-only snapshot of FIN's rows (PHY's basis of each open pool just before
// it) and changes nothing. The actor needs ReadProfit, ConfigureSettings or ApproveSettings, any one of them, as
// whoever proposes or approves a change has to see what it does before deciding; the job profits (F6) are given only to
// a holder of ReadProfit, and the others are told how many jobs' profit would change (JobsChanged), without amounts.
type GetFinSettingsChangeImpact struct {
	TenantID string
	ActorID  string
	ChangeID string
	Entries  []FinSettingEntry
}

// FinItemChange is an item of a legal entity's settings that would change, from a value to another; the thresholds are
// in the legal entity's functional currency.
type FinItemChange struct {
	Key           string
	From, To      string
	FromThreshold *kernel.Money
	ToThreshold   *kernel.Money
}

// FinImpactEntity is a legal entity whose resolved settings differ.
type FinImpactEntity struct {
	LegalEntityID string
	Changes       []FinItemChange
}

// FinImpactPreview is an unsubmitted preview of an open pool that would become stale: its settings version is the
// tenant's, so every one of them does. Preview again and submit the new preview.
type FinImpactPreview struct {
	PoolID, LegalEntityID string
	PreviewNo             int
	PreviewedBy           string
	PreviewedAt           time.Time
	SettingsVersion       int
}

// FinImpactClose is an open pool whose close would become refused (Now ALLOWED, After REFUSED) or allowed, with the
// refusal of the one that is refused (F7).
type FinImpactClose struct {
	PoolID, LegalEntityID string
	Now, After            string
	Refusal               string
}

// FinImpactJob is a job whose profit would change, now and after, with the basis each is told on (F6).
type FinImpactJob struct {
	JobID, ShipmentID, LegalEntityID string
	ProfitNow, ProfitAfter           kernel.Money
	BasisNow, BasisAfter             string
}

// FinImpactDefault is a line whose pending former shares of Houses would be defaulted to the cargo that remains (Change
// WOULD_BE_DEFAULTED), or would stop being defaulted and be refused until attributed (WOULD_STOP_BEING_DEFAULTED) (F8).
type FinImpactDefault struct {
	PoolID, LegalEntityID string
	LineNo                int
	Houses                []string
	Change                string
}

// FinImpactCarriedHouse is the amounts of a carried attribution in the line's currency, now and after; AfterKnown is
// false when it would have to be stated again.
type FinImpactCarriedHouse struct {
	ShipmentID                string
	LossNow, ExceptionNow     kernel.Money
	LossAfter, ExceptionAfter kernel.Money
	AfterKnown                bool
}

// FinImpactCarried is a line whose carried attributions are told differently when its cost has changed (F9): Now and
// After are KEPT, RESCALED or STATE_AGAIN.
type FinImpactCarried struct {
	PoolID, LegalEntityID string
	LineNo                int
	Now, After            string
	Houses                []FinImpactCarriedHouse
}

// FinImpactOneStep is a pool that would become able (Change BECOMES_POSSIBLE) or unable (BECOMES_REFUSED) to be
// apportioned in one step, with the refusal of the one that is refused (F4).
type FinImpactOneStep struct {
	PoolID, LegalEntityID string
	Change                string
	Refusal               string
}

// FinImpactCheck is a rule that the submission of an unsubmitted preview would check differently, without saying who
// submits: Rule is F1, F2 or F3, Now and After how it is checked, LineNo and ShipmentID where it applies.
type FinImpactCheck struct {
	PoolID, LegalEntityID string
	PreviewNo             int
	Rule                  string
	LineNo                int
	ShipmentID            string
	Now, After, Note      string
}

// FinSettingsChangeImpact is the answer: what would behave differently if the change were in force at AsOf, with the
// settings version now and after. NotAssessed lists the open pools of a legal entity that changes that were opened
// after PHY's bases were read: ask again.
type FinSettingsChangeImpact struct {
	TenantID      string
	ChangeID      string
	AsOf          time.Time
	VersionNow    int
	VersionAfter  int
	Entities      []FinImpactEntity
	StalePreviews []FinImpactPreview
	Closes        []FinImpactClose
	JobsChanged   int            // how many jobs' profit would change (F6), told to every caller
	Jobs          []FinImpactJob // those jobs with the profit figures, only for a caller who holds ReadProfit
	Defaults      []FinImpactDefault
	Carried       []FinImpactCarried
	OneStep       []FinImpactOneStep
	Checks        []FinImpactCheck
	NotAssessed   []string
}

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
	GetConsolCostPoolPendingItems(ctx context.Context, q GetConsolCostPoolPendingItems) ([]PoolPendingItem, error)
	GetConsolCostPreviewEffect(ctx context.Context, q GetConsolCostPreviewEffect) (PreviewEffect, error)
	GetCreditorInvoice(ctx context.Context, q GetCreditorInvoice) (CreditorInvoice, error)
	ListCreditorInvoices(ctx context.Context, q ListCreditorInvoices) ([]CreditorInvoice, error)
	ListOpenAccruals(ctx context.Context, q ListOpenAccruals) ([]OpenAccrual, error)
	GetCreditorPayment(ctx context.Context, q GetCreditorPayment) (CreditorPayment, error)
	ListCreditorPayments(ctx context.Context, q ListCreditorPayments) ([]CreditorPayment, error)
	GetCreditorAccount(ctx context.Context, q GetCreditorAccount) (CreditorAccount, error)
	GetFapiaoRecord(ctx context.Context, q GetFapiaoRecord) (FapiaoRecord, error)
	ListFapiaoRecords(ctx context.Context, q ListFapiaoRecords) ([]FapiaoRecord, error)
	GetFinSettings(ctx context.Context, q GetFinSettings) (FinSettings, error)
	ListFinSettingsChanges(ctx context.Context, q ListFinSettingsChanges) ([]FinSettingsChange, error)
	GetFinSettingsCatalogue(ctx context.Context, q GetFinSettingsCatalogue) (FinSettingsCatalogue, error)
	GetFinExceptionReport(ctx context.Context, q GetFinExceptionReport) (FinExceptionReport, error)
	GetFinSettingsChangeImpact(ctx context.Context, q GetFinSettingsChangeImpact) (FinSettingsChangeImpact, error)
	ListJobProfits(ctx context.Context, q ListJobProfits) (JobProfitPage, error)
	GetCostReviewRequest(ctx context.Context, q GetCostReviewRequest) (CostReviewRequest, error)
	GetJobLegalEntity(ctx context.Context, q GetJobLegalEntity) (string, error)
	ListCostReviewRequests(ctx context.Context, q ListCostReviewRequests) (CostReviewPage, error)
	GetAfterTheFactReview(ctx context.Context, q GetAfterTheFactReview) (AfterTheFactPage, error)
	ListAfterTheFactPending(ctx context.Context, q ListAfterTheFactPending) (AfterTheFactPending, error)
	ListFinSelfApprovalReviews(ctx context.Context, q ListFinSelfApprovalReviews) (FinSelfApprovalReviews, error)
}

// Reasons an attribution is on the after-the-fact review list (FIN V2.102, batch B). NO_SECOND_PERSON: the person who
// submitted the apportionment is the person who previewed it. NO_VOUCHER: the attribution has no voucher (F3
// ONLY_WHEN_AMOUNT_MOVES allows it when no amount moves, and F8 makes one by default). MADE_BY_DEFAULT: nobody stated
// it; F8 DEFAULT_TO_REMAINING gave the whole former share to the cargo that remains.
const (
	ReviewReasonNoSecondPerson = "NO_SECOND_PERSON"
	ReviewReasonNoVoucher      = "NO_VOUCHER"
	ReviewReasonMadeByDefault  = "MADE_BY_DEFAULT"
)

// GetAfterTheFactReview lists, in pages, every attribution of a legal entity confirmed in an ISO week (YYYY-Www, from
// Monday 00:00 UTC to the next Monday 00:00 UTC, by the apportionments' system time) that has no second person, no
// voucher or was made by default, and answers whether the week is reviewed (FIN V2.102, batch B). It is read-only.
// Limit is 1 to 500 and After the NextAfter of the page before. An attribution an apportionment only carries forward
// is not listed again: it was listed in the week that stated it. The actor needs ReadProfit.
//
// What the records tell: "no second person" is that the submitter of the apportionment and its previewer are one
// person. FIN does not record who asked for the rework that unloaded a house or who ended its place, so a submitter who
// is that person is not told as such: the line names the submitter and the previewer, and the reader compares them with
// PHY's record.
type GetAfterTheFactReview struct {
	TenantID      string
	ActorID       string
	LegalEntityID string
	Week          string
	Limit         int
	After         string
}

// AfterTheFactLine is one attribution of the list: the pool and its consolidation, the line, the house and its job, the
// apportionment that confirmed it and when (the system time that puts it in the week), the amounts it put on the house's
// job (the loss) and on the consolidation (the exception cost, with who is responsible), each in the line's currency and
// in the functional currency, who submitted the apportionment and who previewed it, and why it is listed (Reasons, in
// the order NO_SECOND_PERSON, NO_VOUCHER, MADE_BY_DEFAULT). Voucher is empty when there is none.
type AfterTheFactLine struct {
	PoolID          string
	ConsolidationID string
	LineNo          int
	ShipmentID      string
	JobID           string
	ApportionmentNo int
	PreviewNo       int
	EnteredAt       time.Time
	Loss            kernel.Money
	LossLocal       kernel.Money
	Exception       kernel.Money
	ExceptionLocal  kernel.Money
	Party           string
	Voucher         string
	SubmittedBy     string
	PreviewedBy     string
	Reasons         []string
	// Covered says that a review covers the line, and then CoveredByReview is the number of the latest review that does, with its
	// reviewer, conclusion and time (V2.102, batch F). A line no review covers is one listed after the review of its week.
	Covered           bool
	CoveredByReview   int
	CoveredBy         string
	CoveredConclusion string
	CoveredAt         time.Time
}

// AfterTheFactReviewMark says whether the week is reviewed and, if it is, the latest review: its number, by whom and when,
// the note and the size and digest of the list at that time, with the size and digest of the whole list now and whether they
// match: a list that changed after its review (an attribution recorded later in the same week) does not, and the week may be
// reviewed again (MarkAfterTheFactReviewed), which makes the next review.
type AfterTheFactReviewMark struct {
	Reviewed      bool
	ReviewNo      int
	ReviewedBy    string
	ReviewedAt    time.Time
	Conclusion    string
	Note          string
	ListSize      int
	ListDigest    string
	CurrentSize   int
	CurrentDigest string
	Matches       bool
}

// AfterTheFactPage is a page of the list, read in one snapshot, with the week's review. NextAfter is the After of the
// next page, empty after the last.
type AfterTheFactPage struct {
	LegalEntityID string
	Week          string
	WeekStart     time.Time
	WeekEnd       time.Time
	Lines         []AfterTheFactLine
	NextAfter     string
	Review        AfterTheFactReviewMark
}

// GetFinExceptionReport is FIN's part of the exception report of a tenant for the period From (included) to To
// (excluded): the approved legal-entity exceptions looser than the company and the items in force now looser than the
// standard profile, with who proposed and who approved each, the loosenings that a single administrator approved
// themselves with their acknowledgements and the declarations, the actors who proposed or approved a change set in the
// period, and what each actor submitted or confirmed in FIN in it. Read-only; the actor needs the permission
// ApproveSettings, granted by name only.
type GetFinExceptionReport struct {
	TenantID string
	ActorID  string
	From     time.Time
	To       time.Time
}

// FinExceptionLine is an entry of a scope looser than the company's value (an exception) or than the standard profile's.
type FinExceptionLine struct {
	ScopeKind string
	ScopeID   string
	Key       string
	Value     string
	// ThresholdMinor is the parameter of the value, in the legal entity's functional currency.
	ThresholdMinor *int64
	// ThanValue is the company's value for an exception, the standard profile's for a looser item.
	ThanValue  string
	ChangeID   string
	ProposedBy string
	ApprovedBy string
}

// FinSelfApproval is a loosening that its own proposer approved as the single administrator, with the declaration it relies on (its
// onboarding, the administrator it names and when it was declared: V2.102, batch F).
type FinSelfApproval struct {
	ChangeID           string
	ApprovedBy         string
	ApprovedAt         time.Time
	Acknowledgement    string
	BasisOnboardingID  string
	BasisAdministrator string
	BasisDeclaredAt    time.Time
	Entries            []FinSettingEntryView
}

// FinSettingsChanger is an actor who proposed or approved change sets in the period.
type FinSettingsChanger struct {
	ActorID  string
	Proposed int
	Approved int
}

// FinActorWork is what an actor did in FIN in the period: the apportionments of consolidation cost pools they
// submitted, and of those the ones that state an attribution of their own, which is to confirm it.
type FinActorWork struct {
	ActorID   string
	Submitted int
	Confirmed int
}

// FinSingleAdministratorDeclaration is a declaration of a single administrator.
type FinSingleAdministratorDeclaration struct {
	OnboardingID    string
	ProposedBy      string
	ConfirmedBy     string
	AdministratorID string
	DeclaredAt      time.Time
}

// FinExceptionReport is FIN's part of the exception report.
type FinExceptionReport struct {
	TenantID           string
	From               time.Time
	To                 time.Time
	AsOf               time.Time
	Exceptions         []FinExceptionLine
	LooserThanStandard []FinExceptionLine
	SelfApproved       []FinSelfApproval
	Declarations       []FinSingleAdministratorDeclaration
	Changers           []FinSettingsChanger
	Work               []FinActorWork
}

// ListAfterTheFactPending answers, for a legal entity and the ISO weeks from FromWeek to ToWeek (YYYY-Www, both included, at most 53
// weeks), what still waits for review (FIN V2.102, batch F): every listed line of an ended week that no review covers, and the ended
// weeks with no review. A week that has not ended is left out. It is read-only, in one snapshot. The actor needs ReviewAfterTheFact,
// granted by name only, and ReadProfit, as the list shows what a loss cost.
type ListAfterTheFactPending struct {
	TenantID      string
	ActorID       string
	LegalEntityID string
	FromWeek      string
	ToWeek        string
}

// AfterTheFactWeek is an ended week with no review: its list has ListedLines lines now (0 when nothing is listed).
type AfterTheFactWeek struct {
	Week        string
	WeekStart   time.Time
	WeekEnd     time.Time
	ListedLines int
}

// AfterTheFactPendingLine is a listed line no review covers, in its week.
type AfterTheFactPendingLine struct {
	Week string
	AfterTheFactLine
}

// AfterTheFactPending is what still waits for review.
type AfterTheFactPending struct {
	LegalEntityID      string
	FromWeek           string
	ToWeek             string
	AsOf               time.Time
	WeeksWithoutReview []AfterTheFactWeek
	Lines              []AfterTheFactPendingLine
}

// ListFinSelfApprovalReviews answers, for a period of approvals (From included, To excluded), the self-approved change sets that no
// one has reviewed and the reviewed ones with their reviewer, conclusion and note (FIN V2.102, batch F). It is read-only. The actor
// needs ReviewSettingsExceptions, granted by name only.
type ListFinSelfApprovalReviews struct {
	TenantID string
	ActorID  string
	From     time.Time
	To       time.Time
}

// FinSelfApprovalReview is a self-approved change set with its review.
type FinSelfApprovalReview struct {
	FinSelfApproval
	ReviewedBy string
	ReviewedAt time.Time
	Conclusion string
	Note       string
}

// FinSelfApprovalReviews is the self-approved change sets of a period: Pending have no review.
type FinSelfApprovalReviews struct {
	TenantID string
	From     time.Time
	To       time.Time
	AsOf     time.Time
	Pending  []FinSelfApproval
	Reviewed []FinSelfApprovalReview
}
