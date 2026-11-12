package domain

import (
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.6: creditor payments. FIN keeps what is paid of creditor invoices from bank or ERP evidence (OD-2 B, the
// payable side of V0.3's settlement): a payment to a creditor of a legal entity is recorded with its evidence and
// allocated to the creditor's matched invoices, in part or across several; what is not allocated stays on account (paid
// in advance). The cash book, payment runs, bank reconciliation and exchange differences stay in the ERP.

// Payment states.
const (
	PaymentRecorded  = "RECORDED"
	PaymentCancelled = "CANCELLED"
)

var (
	ErrNotPayable  = fmt.Errorf("fin: FIN_NOT_PAYABLE: not a creditor invoice of the payment's legal entity, creditor and currency that is matched, not disputed and approved where needed")
	ErrInvoicePaid = fmt.Errorf("fin: FIN_INVOICE_PAID: payments are allocated to the creditor invoice; reverse their allocations first")
)

// PaymentState is a stored creditor payment, for persistence. Its allocations name creditor invoices.
type PaymentState struct {
	TenantID         TenantID
	PaymentID        string
	LegalEntityID    string
	CreditorPartyID  string
	CreditorRevision int64
	Currency         kernel.CurrencyCode
	Amount           int64 // what the creditor is paid
	PaidOn           time.Time
	EvidenceSource   string
	EvidenceRef      string
	State            string
	RecordedBy       string
	RecordedAt       time.Time
	EnteredAt        time.Time
	CancelReason     string
	CancelledBy      string
	CancelledOn      time.Time
	CancelledAt      time.Time
	CancelEnteredAt  time.Time
	Allocations      []Allocation
}

// Unallocated is what the payment has on account: its amount less its live allocations.
func (s PaymentState) Unallocated() int64 {
	left := s.Amount
	for _, a := range s.Allocations {
		if a.Live() {
			left -= a.Amount
		}
	}
	return left
}

// CreditorPayment is the aggregate root of money a legal entity paid a creditor, recorded from bank or ERP evidence,
// with its allocations to the creditor's invoices.
type CreditorPayment struct {
	state       PaymentState
	allocations int   // allocations stored
	reversed    []int // stored allocations reversed since the payment was loaded
	pending     []Event
}

const (
	eventPaymentRecorded  = "CreditorPaymentRecorded.v1"
	eventPaymentAllocated = "CreditorPaymentAllocated.v1"
	eventPaymentUndone    = "CreditorPaymentAllocationReversed.v1"
	eventPaymentCancelled = "CreditorPaymentCancelled.v1"
)

// PaymentInput is a payment as asked, with the creditor's revision ORG confirmed in a creditor role.
type PaymentInput struct {
	ID               string
	LegalEntityID    string
	CreditorPartyID  string
	CreditorRevision int64
	Amount           kernel.Money
	PaidOn           time.Time
	EvidenceSource   string
	EvidenceRef      string
}

// RecordPayment records money paid to a creditor: a positive amount, the civil date paid (not after the day it is
// recorded, in any time zone, and not more than 366 days before) and the evidence as a source and a reference.
func RecordPayment(tenant TenantID, in PaymentInput, by string, at time.Time) (*CreditorPayment, error) {
	switch {
	case tenant == "" || !identifier(in.ID) || !identifier(in.LegalEntityID) || !identifier(in.CreditorPartyID) || in.CreditorRevision < 1 || blank(by) ||
		at.IsZero():
		return nil, fmt.Errorf("%w: a payment names its id, legal entity, creditor, author and time", ErrInvalid)
	case !in.Amount.Currency.Valid() || in.Amount.Minor <= 0 || in.Amount.Minor > MaxAmount:
		return nil, fmt.Errorf("%w: a payment has a positive amount in a currency", ErrInvalid)
	case !date(in.PaidOn) || at.Before(in.PaidOn.Add(-14*time.Hour)) || !at.Before(in.PaidOn.Add(maxReceiptAge+36*time.Hour)):
		return nil, fmt.Errorf("%w: the date paid is not after the day the payment is recorded and not more than 366 days before", ErrInvalid)
	case (in.EvidenceSource != EvidenceBank && in.EvidenceSource != EvidenceERP) || !receiptEvidencePattern.MatchString(in.EvidenceRef):
		return nil, fmt.Errorf("%w: a payment has its evidence as the source BANK or ERP and a reference (letters, digits, space and . _ : / # -, "+
			"200 characters, not ending with a space)", ErrInvalid)
	}
	p := &CreditorPayment{state: PaymentState{TenantID: tenant, PaymentID: in.ID, LegalEntityID: in.LegalEntityID, CreditorPartyID: in.CreditorPartyID,
		CreditorRevision: in.CreditorRevision, Currency: in.Amount.Currency, Amount: in.Amount.Minor, PaidOn: in.PaidOn.UTC(),
		EvidenceSource: in.EvidenceSource, EvidenceRef: in.EvidenceRef, State: PaymentRecorded, RecordedBy: by, RecordedAt: at.UTC()}}
	p.record(eventPaymentRecorded, at, 0, "")
	return p, nil
}

func (p *CreditorPayment) lastChange() time.Time {
	last := later(p.state.RecordedAt, p.state.CancelledAt)
	for _, a := range p.state.Allocations {
		last = later(later(last, a.At), a.ReversedAt)
	}
	return last
}

// change checks what every change of a recorded payment needs: an author, a time not before the payment's last change
// and a civil date of that time.
func (p *CreditorPayment) change(on time.Time, by string, at time.Time) error {
	switch {
	case blank(by) || at.IsZero() || at.Before(p.lastChange()):
		return fmt.Errorf("%w: a change names its author and a time not before the payment's last change", ErrInvalid)
	case p.state.State != PaymentRecorded:
		return fmt.Errorf("%w: payment %s is %s", ErrInvalidTransition, p.state.PaymentID, p.state.State)
	case !dateOf(on, at):
		return fmt.Errorf("%w: the date is the date of the change", ErrInvalid)
	}
	return nil
}

// PayableInvoice is what an allocation needs to know of a creditor invoice, read once the invoice is locked: its
// legal entity, creditor, currency, date, state, whether a variance awaits approval, its total and what live
// allocations pay of it.
type PayableInvoice struct {
	InvoiceID         string
	SupplierInvoiceNo string
	LegalEntityID     string
	CreditorPartyID   string
	Currency          kernel.CurrencyCode
	InvoiceDate       time.Time
	State             string
	AwaitingApproval  bool
	Total             int64
	Paid              int64
}

// Open is what the invoice still has to be paid.
func (n PayableInvoice) Open() int64 { return n.Total - n.Paid }

// Allocate pays creditor invoices from the payment: each a MATCHED invoice, not awaiting a variance approval (an
// invoice disputed or not yet checked is held from payment), of the payment's legal entity, creditor and currency
// (FIN_NOT_PAYABLE), named once, with a positive amount within what the payment has unallocated and the invoice has
// open (FIN_OVER_ALLOCATED). The date is not before the date paid or an invoice's date.
func (p *CreditorPayment) Allocate(items []AllocationItem, invoices []PayableInvoice, on time.Time, by string, at time.Time) error {
	if err := p.change(on, by, at); err != nil {
		return err
	}
	st := &p.state
	if len(items) == 0 || len(items) > MaxAllocations || on.Before(st.PaidOn) {
		return fmt.Errorf("%w: an allocation names 1 to %d creditor invoices and a date not before the date paid", ErrInvalid, MaxAllocations)
	}
	left := st.Unallocated()
	seen := map[string]bool{}
	var added []Allocation
	for _, it := range items {
		i := slices.IndexFunc(invoices, func(n PayableInvoice) bool { return n.InvoiceID == it.InvoiceID })
		switch {
		case !identifier(it.InvoiceID) || seen[it.InvoiceID] || it.Amount.Currency != st.Currency || it.Amount.Minor <= 0:
			return fmt.Errorf("%w: an allocation names each creditor invoice once with a positive amount in %s", ErrInvalid, st.Currency)
		case i < 0:
			return fmt.Errorf("%w: no creditor invoice %s", ErrNotPayable, it.InvoiceID)
		}
		seen[it.InvoiceID] = true
		n := invoices[i]
		switch {
		case n.State != CreditorMatched || n.AwaitingApproval || n.LegalEntityID != st.LegalEntityID || n.CreditorPartyID != st.CreditorPartyID ||
			n.Currency != st.Currency:
			return fmt.Errorf("%w: creditor invoice %s (%s) is %s", ErrNotPayable, n.InvoiceID, n.SupplierInvoiceNo, n.State)
		case on.Before(n.InvoiceDate):
			return fmt.Errorf("%w: creditor invoice %s is dated %s, after the allocation", ErrInvalid, n.SupplierInvoiceNo, n.InvoiceDate.Format(time.DateOnly))
		case it.Amount.Minor > left:
			return fmt.Errorf("%w: payment %s has %d unallocated", ErrOverAllocated, st.PaymentID, left)
		case it.Amount.Minor > n.Open():
			return fmt.Errorf("%w: creditor invoice %s has %d open", ErrOverAllocated, n.SupplierInvoiceNo, n.Open())
		}
		left -= it.Amount.Minor
		added = append(added, Allocation{No: len(st.Allocations) + len(added) + 1, InvoiceID: n.InvoiceID, Amount: it.Amount.Minor, Date: on.UTC(), By: by,
			At: at.UTC()})
	}
	for _, a := range added {
		st.Allocations = append(st.Allocations, a)
		p.record(eventPaymentAllocated, at, a.No, a.InvoiceID)
	}
	return nil
}

// ReverseAllocation reverses one live allocation with a reason, dated on or after it: its invoice is open again for
// the amount, which is back on account.
func (p *CreditorPayment) ReverseAllocation(no int, reason string, on time.Time, by string, at time.Time) error {
	if err := p.change(on, by, at); err != nil {
		return err
	}
	st := &p.state
	switch {
	case no < 1 || no > len(st.Allocations) || !text(reason):
		return fmt.Errorf("%w: a reversal names an allocation of the payment and a reason", ErrInvalid)
	case !st.Allocations[no-1].Live():
		return fmt.Errorf("%w: allocation %d of payment %s is reversed already", ErrInvalidTransition, no, st.PaymentID)
	case on.Before(st.Allocations[no-1].Date):
		return fmt.Errorf("%w: a reversal is dated on or after its allocation", ErrInvalid)
	}
	a := &st.Allocations[no-1]
	a.ReversedBy, a.ReverseReason, a.ReversedOn, a.ReversedAt = by, reason, on.UTC(), at.UTC()
	if no <= p.allocations {
		p.reversed = append(p.reversed, no)
	}
	p.record(eventPaymentUndone, at, no, a.InvoiceID)
	return nil
}

// Cancel cancels a payment entered in error or returned by the bank, once it has no live allocation, dated on or after
// the date paid and every reversal.
func (p *CreditorPayment) Cancel(reason string, on time.Time, by string, at time.Time) error {
	if err := p.change(on, by, at); err != nil {
		return err
	}
	st := &p.state
	if (reason != ReceiptEnteredInError && reason != ReceiptReturned) || on.Before(st.PaidOn) {
		return fmt.Errorf("%w: a cancellation has the reason ENTERED_IN_ERROR or PAYMENT_RETURNED and a date not before the date paid", ErrInvalid)
	}
	for _, a := range st.Allocations {
		switch {
		case a.Live():
			return fmt.Errorf("%w: payment %s pays creditor invoices; reverse its allocations first", ErrInvalidTransition, st.PaymentID)
		case on.Before(a.ReversedOn):
			return fmt.Errorf("%w: a cancellation is dated on or after the payment's reversals", ErrInvalid)
		}
	}
	st.State, st.CancelReason, st.CancelledBy, st.CancelledOn, st.CancelledAt = PaymentCancelled, reason, by, on.UTC(), at.UTC()
	p.record(eventPaymentCancelled, at, 0, "")
	return nil
}

func (p *CreditorPayment) record(kind string, at time.Time, version int, invoice string) {
	p.pending = append(p.pending, Event{Type: kind, At: at.UTC(), TenantID: p.state.TenantID, SubjectID: p.state.PaymentID,
		LegalEntityID: p.state.LegalEntityID, Version: version, CreditorPartyID: p.state.CreditorPartyID, InvoiceID: invoice})
}

// Stamp records when FIN entered what was written since the payment was loaded (T1).
func (p *CreditorPayment) Stamp(entered time.Time) {
	e := entered.UTC()
	st := &p.state
	if st.EnteredAt.IsZero() {
		st.EnteredAt = e
	}
	if !st.CancelledAt.IsZero() && st.CancelEnteredAt.IsZero() {
		st.CancelEnteredAt = e
	}
	for i := range st.Allocations {
		a := &st.Allocations[i]
		if a.EnteredAt.IsZero() {
			a.EnteredAt = e
		}
		if !a.ReversedAt.IsZero() && a.ReverseEnteredAt.IsZero() {
			a.ReverseEnteredAt = e
		}
	}
}

// RestorePayment rebuilds a stored payment.
func RestorePayment(st PaymentState) (*CreditorPayment, error) {
	st.Allocations = append([]Allocation(nil), st.Allocations...)
	for i, a := range st.Allocations {
		if a.No != i+1 || a.Amount <= 0 {
			return nil, fmt.Errorf("%w: allocation %d of payment %s", ErrInvalid, i+1, st.PaymentID)
		}
	}
	if st.Unallocated() < 0 || (st.State != PaymentRecorded && st.State != PaymentCancelled) || (st.State == PaymentCancelled) == st.CancelledAt.IsZero() {
		return nil, fmt.Errorf("%w: payment %s", ErrInvalid, st.PaymentID)
	}
	return &CreditorPayment{state: st, allocations: len(st.Allocations)}, nil
}

func (p *CreditorPayment) State() PaymentState {
	st := p.state
	st.Allocations = append([]Allocation(nil), st.Allocations...)
	return st
}

// Added returns the allocations written since the payment was loaded and the stored ones reversed since.
func (p *CreditorPayment) Added() (added, reversed []Allocation) {
	st := p.State()
	for _, no := range p.reversed {
		reversed = append(reversed, st.Allocations[no-1])
	}
	return st.Allocations[p.allocations:], reversed
}

func (p *CreditorPayment) Pending() []Event { return append([]Event(nil), p.pending...) }

func (p *CreditorPayment) Acknowledge() {
	p.pending, p.allocations, p.reversed = nil, len(p.state.Allocations), nil
}
