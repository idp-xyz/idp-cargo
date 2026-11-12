package api

import (
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.6: creditor payments. FIN keeps what is paid of creditor invoices from bank or ERP evidence (OD-2 B); the
// cash book, payment runs, bank reconciliation and exchange differences stay in the ERP.

// Creditor payment states.
const (
	PaymentRecorded  = "RECORDED"
	PaymentCancelled = "CANCELLED"
)

// RecordCreditorPayment records money a legal entity paid a creditor, from bank or ERP evidence. Amount is what the
// creditor is paid; PaidOn the civil date paid (YYYY-MM-DD), not after the day of the command and not more than 366
// days before it. EvidenceRef is a reference (a bank statement line, an ERP voucher), never free text; a legal entity
// records a piece of evidence once (FIN_EVIDENCE_USED) until that payment is cancelled. The creditor must hold the
// CARRIER, CO_LOADER or OVERSEAS_AGENT role for the legal entity in ORG; one whose roles have ended is still paid while
// a matched invoice of the legal entity is open. A payment in another currency than the invoices it pays is entered at
// the amount it pays in their currency.
type RecordCreditorPayment struct {
	CommandID       string
	TenantID        string
	ActorID         string
	PaymentID       string
	LegalEntityID   string
	CreditorPartyID string
	Amount          kernel.Money
	PaidOn          string
	EvidenceSource  string
	EvidenceRef     string
	OccurredAt      time.Time
}

// PaymentAllocation is an amount of a payment for a creditor invoice.
type PaymentAllocation struct {
	InvoiceID string
	Amount    kernel.Money
}

// AllocateCreditorPayment pays creditor invoices from a payment: each a MATCHED invoice, not awaiting a variance
// approval (an invoice recorded, disputed or awaiting approval is held from payment), of the payment's legal entity,
// creditor and currency (FIN_NOT_PAYABLE), named once, with a positive amount within what the payment has unallocated
// and the invoice has open (FIN_OVER_ALLOCATED). Date is the civil date of the command, not before the date paid or an
// invoice's date. What stays unallocated is on account (paid in advance).
type AllocateCreditorPayment struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PaymentID        string
	ExpectedRevision int64
	Allocations      []PaymentAllocation
	Date             string
	OccurredAt       time.Time
}

// ReverseCreditorPaymentAllocation reverses one allocation of a payment with a reason, once: its invoice is open
// again for the amount, which is back on account. Date is the civil date of the command, on or after the allocation's.
type ReverseCreditorPaymentAllocation struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PaymentID        string
	ExpectedRevision int64
	AllocationNo     int
	Reason           string
	Date             string
	OccurredAt       time.Time
}

// CancelCreditorPayment cancels a payment that pays nothing (its allocations reversed first), entered in error or
// returned by the bank. Date is the civil date of the command.
type CancelCreditorPayment struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PaymentID        string
	ExpectedRevision int64
	Reason           string // ReceiptEnteredInError or ReceiptReturned
	Date             string
	OccurredAt       time.Time
}

// CreditorPaymentAllocation is an allocation of a payment, reversed at most once.
type CreditorPaymentAllocation struct {
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

// CreditorPayment is money paid to a creditor with its allocations; Unallocated is what is on account.
type CreditorPayment struct {
	TenantID         string
	PaymentID        string
	Revision         int64
	LegalEntityID    string
	CreditorPartyID  string
	CreditorRevision int64
	Amount           kernel.Money
	Unallocated      kernel.Money
	PaidOn           string
	EvidenceSource   string
	EvidenceRef      string
	State            string
	RecordedBy       string
	RecordedAt       time.Time
	CancelReason     string
	CancelledBy      string
	CancelledOn      string
	CancelledAt      time.Time
	Allocations      []CreditorPaymentAllocation
}

// CreditorInvoicePayment is a live allocation paying a creditor invoice.
type CreditorInvoicePayment struct {
	PaymentID    string
	AllocationNo int
	Amount       kernel.Money
	Date         string
}

// CreditorOpenItem is a creditor invoice not cancelled with something open: Payable when it is matched, not disputed
// and approved where needed; DaysSinceInvoice counts from its invoice date to AsOf.
type CreditorOpenItem struct {
	InvoiceID         string
	SupplierInvoiceNo string
	InvoiceDate       string
	State             string
	AwaitingApproval  bool
	Payable           bool
	Total             kernel.Money
	Paid              kernel.Money
	Open              kernel.Money
	DaysSinceInvoice  int
}

// CreditorAccount is what a legal entity owes a creditor: the open items, what is open per currency, and what payments
// have on account (paid in advance) per currency.
type CreditorAccount struct {
	LegalEntityID   string
	CreditorPartyID string
	AsOf            string
	OpenItems       []CreditorOpenItem
	Open            []kernel.Money
	OnAccount       []kernel.Money
}

// GetCreditorPayment asks for a payment; ListCreditorPayments lists a creditor's payments by a legal entity in the
// order paid; GetCreditorAccount states what the legal entity owes the creditor (AsOf YYYY-MM-DD, today when empty).
type (
	GetCreditorPayment   struct{ TenantID, PaymentID string }
	ListCreditorPayments struct{ TenantID, LegalEntityID, CreditorPartyID string }
	GetCreditorAccount   struct{ TenantID, LegalEntityID, CreditorPartyID, AsOf string }
)
