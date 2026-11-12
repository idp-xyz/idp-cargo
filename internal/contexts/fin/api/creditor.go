package api

import (
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.5 (FB4): creditor invoices matched to accruals, cost variance approval (OD-9 B), the release of accruals, and
// the closing of consol cost pools.

// Accrual kinds a creditor invoice matches, and the accrual statuses beyond ACCRUED: PARTLY_MATCHED while live matches
// that are not final consume part of it and the rest stays accrued, MATCHED once a final match closed it, RELEASED once
// a person released what no invoice will consume.
const (
	TargetJobCost  = "JOB_COST"
	TargetPoolCost = "POOL_COST"

	StatusPartlyMatched = "PARTLY_MATCHED"
	StatusMatched       = "MATCHED"
	StatusReleased      = "RELEASED"
)

// Creditor invoice states and the kinds of incoming tax document.
const (
	CreditorRecorded  = "RECORDED"
	CreditorMatched   = "MATCHED"
	CreditorDisputed  = "DISPUTED"
	CreditorCancelled = "CANCELLED"

	TaxDocSpecialFapiao  = "SPECIAL_FAPIAO"
	TaxDocOrdinaryFapiao = "ORDINARY_FAPIAO"
	TaxDocForeignInvoice = "FOREIGN_INVOICE"
)

// Pool states.
const (
	PoolOpen   = "OPEN"
	PoolClosed = "CLOSED"
)

// SetCostVarianceThreshold records the next version of a legal entity's cost variance threshold (OD-9 B): a final
// match whose variance against its accrual, in the functional currency, is beyond the greater of Amount (minor units of
// the functional currency, from 0) and RateBP basis points (0 to 10000) of the accrual's local amount needs approval.
// Without a threshold every variance does. A version is in force from OccurredAt; the next is not set earlier.
type SetCostVarianceThreshold struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	ExpectedRevision int64
	Amount           int64
	RateBP           int
	OccurredAt       time.Time
}

// CreditorInvoiceLine is a line of a supplier's invoice: a charge code, the supplier's reference (a MAWB, container or
// booking number; optional) and a positive amount in the invoice's currency.
type CreditorInvoiceLine struct {
	ChargeCode  string
	Description string
	SupplierRef string
	Amount      int64
}

// RecordCreditorInvoice records a supplier's invoice to a legal entity with a finance policy, as received. The
// creditor holds the CARRIER, CO_LOADER or OVERSEAS_AGENT role for the legal entity in ORG. SupplierInvoiceNo is the
// supplier's own number, once per legal entity and creditor among invoices not cancelled (FIN_SUPPLIER_INVOICE_USED);
// InvoiceDate (YYYY-MM-DD) is not after the day of the command and not more than 366 days before; one Currency; the
// incoming tax document (SPECIAL_FAPIAO, ORDINARY_FAPIAO or FOREIGN_INVOICE) with its number, or neither; 1 to 200
// lines.
type RecordCreditorInvoice struct {
	CommandID         string
	TenantID          string
	ActorID           string
	InvoiceID         string
	LegalEntityID     string
	CreditorPartyID   string
	SupplierInvoiceNo string
	InvoiceDate       string
	Currency          kernel.CurrencyCode
	TaxDocumentKind   string
	TaxDocumentNumber string
	Lines             []CreditorInvoiceLine
	OccurredAt        time.Time
}

// AccrualRef names an accrual: a job's cost side (Kind JOB_COST, ID the job, LineNo its charge line) or a pool's cost
// line (Kind POOL_COST, ID the pool).
type AccrualRef struct {
	Kind   string
	ID     string
	LineNo int
}

// CreditorMatch is part of an invoice line matched to one accrual. Final closes the accrual: what all live matches
// consume less the accrual is the variance. A match not final consumes at most what the accrual has open and leaves
// the rest accrued for a later invoice (FIN_MATCH_EXCEEDS). Reason is required when the accrual's charge code is not
// the line's.
type CreditorMatch struct {
	LineNo int
	Target AccrualRef
	Amount int64
	Final  bool
	Reason string
}

// MatchCreditorInvoice matches every line of a RECORDED invoice in full (FIN_MATCH_INCOMPLETE), each accrual once, to
// accruals of open jobs and pools of its legal entity (FIN_MATCH_TARGET): in the invoice's currency
// (FIN_MATCH_CURRENCY; revise an accrual into it first), naming no creditor or the invoice's (FIN_MATCH_CREDITOR), and
// neither matched final nor released (FIN_ACCRUAL_CLOSED). Each match converts at the ACCOUNTING rate in force
// (FIN_RATE_MISSING). A final variance beyond the threshold in force needs ApproveCostVariance.
type MatchCreditorInvoice struct {
	CommandID        string
	TenantID         string
	ActorID          string
	InvoiceID        string
	ExpectedRevision int64
	Matches          []CreditorMatch
	OccurredAt       time.Time
}

// ApproveCostVariance approves the variance of a MATCHED invoice that needs it, with a reason: by another person than
// the one who matched it, unless the Authorizer also allows ApproveOwnCostVariance (FIN_VARIANCE_APPROVAL); the
// approval keeps the flag.
type ApproveCostVariance struct {
	CommandID        string
	TenantID         string
	ActorID          string
	InvoiceID        string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// DisputeCreditorInvoice disputes a RECORDED or MATCHED invoice with the supplier; while disputed, its matches keep
// the pools they consume from closing. ResolveCreditorInvoiceDispute returns it to the state it was in.
// CancelCreditorInvoice cancels a RECORDED or DISPUTED invoice (a matched one is disputed first): its matches consume
// nothing any more. Disputing and cancelling need the jobs and pools of the matches open (FIN_TARGET_CLOSED).
type DisputeCreditorInvoice struct {
	CommandID        string
	TenantID         string
	ActorID          string
	InvoiceID        string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

type ResolveCreditorInvoiceDispute struct {
	CommandID        string
	TenantID         string
	ActorID          string
	InvoiceID        string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

type CancelCreditorInvoice struct {
	CommandID        string
	TenantID         string
	ActorID          string
	InvoiceID        string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// ReleaseJobAccrual releases what no creditor invoice will consume of a job's cost side, with a reason: the side's
// next version is RELEASED with amount zero, and its cost is what invoices matched. ExpectedSideVersion is the cost
// side's latest version read.
type ReleaseJobAccrual struct {
	CommandID           string
	TenantID            string
	ActorID             string
	JobID               string
	ExpectedRevision    int64
	LineNo              int
	ExpectedSideVersion int
	Reason              string
	OccurredAt          time.Time
}

// ReleasePoolAccrual releases what no creditor invoice will consume of a pool cost line, as ReleaseJobAccrual.
type ReleasePoolAccrual struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PoolID           string
	ExpectedRevision int64
	LineNo           int
	ExpectedVersion  int
	Reason           string
	OccurredAt       time.Time
}

// CloseConsolCostPool closes a pool whose costs are final (FIN_POOL_NOT_CLOSABLE otherwise): every line matched final
// or released, by invoices neither disputed nor awaiting approval, and apportioned at its final cost on PHY's current
// basis with every house loaded, naming the live houses and their current quantities (a line of zero cost needs no
// apportionment). ReopenConsolCostPool reopens it with a reason, such as a late invoice (OD-10 A).
type CloseConsolCostPool struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PoolID           string
	ExpectedRevision int64
	OccurredAt       time.Time
}

type ReopenConsolCostPool struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PoolID           string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// CreditorInvoiceMatch is a match of an invoice: the accrual and its version it consumed, the amount in the invoice's
// currency and in the functional currency at the ACCOUNTING rate, the accrual's figures then and what earlier invoices
// had consumed of it, and for a final match the variance in both currencies.
type CreditorInvoiceMatch struct {
	MatchNo       int
	LineNo        int
	Target        AccrualRef
	TargetVersion int
	Amount        kernel.Money
	Final         bool
	Reason        string
	Local         kernel.Money
	FX            *FxSnapshot
	Accrual       kernel.Money
	AccrualLocal  kernel.Money
	Prior         kernel.Money
	PriorLocal    kernel.Money
	Variance      kernel.Money
	VarianceLocal kernel.Money
}

// CreditorInvoiceDispute is a dispute of an invoice, resolved at most once.
type CreditorInvoiceDispute struct {
	DisputeNo     int
	FromState     string
	Reason        string
	By            string
	At            time.Time
	ResolvedBy    string
	ResolveReason string
	ResolvedAt    time.Time
}

// CreditorInvoice is a supplier's invoice with its matches, approval and disputes.
type CreditorInvoice struct {
	TenantID          string
	InvoiceID         string
	Revision          int64
	LegalEntityID     string
	CreditorPartyID   string
	CreditorRevision  int64
	SupplierInvoiceNo string
	InvoiceDate       string
	Currency          kernel.CurrencyCode
	TaxDocumentKind   string
	TaxDocumentNumber string
	Total             kernel.Money
	State             string
	RecordedBy        string
	RecordedAt        time.Time
	Lines             []CreditorInvoiceLine
	Matches           []CreditorInvoiceMatch
	MatchedBy         string
	MatchedAt         time.Time
	ApprovalRequired  bool
	ThresholdVersion  int // the threshold the variances were weighed against; 0 when none was set
	AwaitingApproval  bool
	ApprovedBy        string
	ApprovalReason    string
	SelfApproved      bool
	ApprovedAt        time.Time
	Disputes          []CreditorInvoiceDispute
	CancelReason      string
	CancelledBy       string
	CancelledAt       time.Time
}

// OpenAccrual is an accrual still expecting an invoice: its job (Reference the shipment) or pool (Reference the
// consolidation), charge code, supplier reference, creditor (empty while not known), latest version, what live
// matches consume and what is open, in its currency and in the functional currency.
type OpenAccrual struct {
	Target          AccrualRef
	Reference       string
	ChargeCode      string
	SupplierRef     string
	CreditorPartyID string
	Version         int
	Status          string
	Amount          kernel.Money
	Local           kernel.Money
	Matched         kernel.Money
	MatchedLocal    kernel.Money
	Open            kernel.Money
	OpenLocal       kernel.Money
}

// VarianceThreshold is a version of a legal entity's cost variance threshold.
type VarianceThreshold struct {
	Version int
	Amount  kernel.Money
	RateBP  int
	SetBy   string
	SetAt   time.Time
}

// PoolClosure is one closing of a pool, reopened at most once with a reason.
type PoolClosure struct {
	ClosureNo    int
	ClosedBy     string
	ClosedAt     time.Time
	ReopenedBy   string
	ReopenReason string
	ReopenedAt   time.Time
}

// GetCreditorInvoice asks for an invoice. ListCreditorInvoices lists a legal entity's invoices in the order dated, of
// one creditor when CreditorPartyID is set, and only those awaiting a variance approval when AwaitingApproval is.
// ListOpenAccruals lists the accruals of a legal entity's open jobs and pools still expecting an invoice, naming the
// creditor or none when CreditorPartyID is set.
type (
	GetCreditorInvoice   struct{ TenantID, InvoiceID string }
	ListCreditorInvoices struct {
		TenantID         string
		LegalEntityID    string
		CreditorPartyID  string
		AwaitingApproval bool
	}
	ListOpenAccruals struct{ TenantID, LegalEntityID, CreditorPartyID string }
)
