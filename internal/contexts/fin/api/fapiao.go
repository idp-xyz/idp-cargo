package api

import (
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.7: fapiao records (FB6). FIN asks for a fapiao on issued debit notes and keeps what the national e-invoice
// platform answered, from evidence an authorized operator records: FIN does not call the platform, and it is not the
// statutory tax record (the platform and the ERP are).

// Fapiao types and record states.
const (
	FapiaoOrdinary = "ORDINARY" // 普通发票
	FapiaoSpecial  = "SPECIAL"  // 增值税专用发票: the buyer deducts its input VAT

	FapiaoRecordRequested   = "REQUESTED"
	FapiaoRecordIssued      = "ISSUED"
	FapiaoRecordFailed      = "FAILED"
	FapiaoRecordRedLettered = "RED_LETTERED"
)

// RequestFapiao asks for a fapiao on issued debit notes (1 to 200, each named once) of one legal entity to one debtor,
// none covered by another requested or issued record (FIN_FAPIAO_COVERS). The debtor holds a debtor role for the legal
// entity in ORG and has a CN_ENTERPRISE billing profile whose current version still names it as ORG does
// (FIN_BILLING_PROFILE_STALE); the record freezes that version's buyer data. A fapiao is in CNY, the legal entity's
// functional currency (FIN_NOT_FAPIAOABLE otherwise, as for notes of another legal entity or debtor). Each note puts on
// the fapiao its lines not reversed by credit notes, per tax treatment and rate, leaving its pass-through disbursements
// off; a note with nothing else is FIN_NOT_FAPIAOABLE, and a SPECIAL fapiao takes no exempt international freight
// forwarding. While the record is requested or issued, no credit note reverses a line of its notes that the fapiao
// states (pass-through disbursements may be credited). A request retried while the first is still committing hears
// ErrConflict and replays its receipt when sent again.
type RequestFapiao struct {
	CommandID     string
	TenantID      string
	ActorID       string
	FapiaoID      string
	LegalEntityID string
	DebtorPartyID string
	Type          string // FapiaoOrdinary or FapiaoSpecial
	InvoiceIDs    []string
	OccurredAt    time.Time
}

// FapiaoEvidence is what the platform answered: the fapiao number (the 20 digits of a fully digital e-invoice, 数电票,
// the platform's only kind since 2024-12-01), its civil date (YYYY-MM-DD), not after the day it is recorded and not
// more than 366 days before, and a reference to the evidence kept (such as the platform file's hash), never free text.
// The platform's numbers are unique nationwide, so a tenant records a number once, issued or red-letter alike
// (FIN_FAPIAO_NUMBER_USED).
type FapiaoEvidence struct {
	Number      string
	Date        string
	EvidenceRef string
}

// RecordFapiaoIssued records the fapiao the platform issued on a REQUESTED record, dated not before the debit notes
// it covers.
type RecordFapiaoIssued struct {
	CommandID        string
	TenantID         string
	ActorID          string
	FapiaoID         string
	ExpectedRevision int64
	Fapiao           FapiaoEvidence
	OccurredAt       time.Time
}

// RecordFapiaoFailed records that the platform refused a REQUESTED record or the operator abandoned it, with a reason
// (500 characters): its notes are covered no longer.
type RecordFapiaoFailed struct {
	CommandID        string
	TenantID         string
	ActorID          string
	FapiaoID         string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// RecordRedLetter records the red-letter fapiao (红字发票) that reversed an ISSUED record's fapiao, with its own number,
// dated not before it, the reference of its red-letter information confirmation form (红字发票信息确认单, filled in for
// every fully digital red-letter fapiao) and a reason: credit notes may then reverse the notes, and a new record may
// cover them.
type RecordRedLetter struct {
	CommandID        string
	TenantID         string
	ActorID          string
	FapiaoID         string
	ExpectedRevision int64
	RedLetter        FapiaoEvidence
	ConfirmationRef  string
	Reason           string
	OccurredAt       time.Time
}

// FapiaoLine is a line of the fapiao: a tax treatment and rate (in basis points, 0 for none) with its gross amount and
// the VAT within it (gross × rate / (1 + rate), rounded half up once per rate).
type FapiaoLine struct {
	Treatment string
	RateBP    int
	Gross     kernel.Money
	Net       kernel.Money
	VAT       kernel.Money
}

// FapiaoAmount is what a covered note puts on the fapiao in a tax treatment and rate.
type FapiaoAmount struct {
	Treatment string
	RateBP    int
	Amount    kernel.Money
}

// FapiaoCoveredNote is a debit note a record covers; Released once the record failed or was red-lettered.
type FapiaoCoveredNote struct {
	InvoiceID   string
	Number      string
	InvoiceDate string
	Amounts     []FapiaoAmount
	Released    bool
}

// FapiaoRecordEvidence is the platform evidence a record keeps, with who recorded it and when.
type FapiaoRecordEvidence struct {
	Number          string
	Date            string
	EvidenceRef     string
	ConfirmationRef string
	RecordedBy      string
	RecordedAt      time.Time
}

// FapiaoRecord is a fapiao asked for on debit notes with the buyer data it was asked with and what the platform
// answered.
type FapiaoRecord struct {
	TenantID       string
	FapiaoID       string
	Revision       int64
	LegalEntityID  string
	DebtorPartyID  string
	DebtorRevision int64
	Type           string
	State          string
	ProfileVersion int
	BuyerTitle     string
	TaxpayerNumber string
	BuyerAddress   string
	BuyerPhone     string
	BankName       string
	BankAccount    string
	Gross          kernel.Money
	VAT            kernel.Money
	Lines          []FapiaoLine
	Notes          []FapiaoCoveredNote
	RequestedBy    string
	RequestedAt    time.Time
	Issued         *FapiaoRecordEvidence
	FailReason     string
	FailedBy       string
	FailedAt       time.Time
	RedLetter      *FapiaoRecordEvidence
	RedReason      string
}

// Fapiao queries.
type (
	GetFapiaoRecord   struct{ TenantID, FapiaoID string }
	ListFapiaoRecords struct{ TenantID, LegalEntityID, DebtorPartyID string }
)
