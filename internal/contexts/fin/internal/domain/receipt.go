package domain

import (
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Receipt states, evidence sources and cancellation reasons. FIN keeps the settlement status of debit notes from bank
// or ERP evidence (OD-2 B); the cash book, bank reconciliation and exchange differences stay in the ERP.
const (
	ReceiptRecorded  = "RECORDED"
	ReceiptCancelled = "CANCELLED"

	EvidenceBank = "BANK"
	EvidenceERP  = "ERP"

	ReceiptEnteredInError = "ENTERED_IN_ERROR"
	ReceiptReturned       = "PAYMENT_RETURNED"
)

const maxReceiptAge = 366 * 24 * time.Hour // a date received further back is taken for a typing error

// MaxAllocations is how many debit notes one command allocates a receipt to.
const MaxAllocations = 200

// Allocation settles part of a debit note from a receipt, in the receipt's currency; it is reversed at most once.
type Allocation struct {
	No               int
	InvoiceID        string
	Amount           int64
	Date             time.Time // the civil date it counts from
	By               string
	At               time.Time
	EnteredAt        time.Time
	ReversedBy       string
	ReverseReason    string
	ReversedOn       time.Time // the civil date the reversal counts from
	ReversedAt       time.Time
	ReverseEnteredAt time.Time
}

// Live reports whether the allocation is not reversed.
func (a Allocation) Live() bool { return a.ReversedAt.IsZero() }

// liveOn reports whether the allocation counts at the end of a civil date.
func (a Allocation) liveOn(d time.Time) bool {
	return !a.Date.After(d) && (a.ReversedAt.IsZero() || a.ReversedOn.After(d))
}

// ReceiptState is a stored debtor receipt, for persistence.
type ReceiptState struct {
	TenantID        TenantID
	ReceiptID       string
	LegalEntityID   string
	DebtorPartyID   string
	DebtorRevision  int64
	Currency        kernel.CurrencyCode
	Amount          int64 // credited to the legal entity's bank account
	BankCharges     int64 // deducted on the way and borne by the legal entity; the debtor is credited with both
	ReceivedOn      time.Time
	EvidenceSource  string
	EvidenceRef     string
	State           string
	RecordedBy      string
	RecordedAt      time.Time
	EnteredAt       time.Time
	CancelReason    string
	CancelledBy     string
	CancelledOn     time.Time
	CancelledAt     time.Time
	CancelEnteredAt time.Time
	Allocations     []Allocation
}

// Credited is what the debtor is credited with: the amount received and the bank charges the legal entity bears.
func (s ReceiptState) Credited() int64 { return s.Amount + s.BankCharges }

// Unallocated is what the receipt has on account: credited less its live allocations.
func (s ReceiptState) Unallocated() int64 {
	left := s.Credited()
	for _, a := range s.Allocations {
		if a.Live() {
			left -= a.Amount
		}
	}
	return left
}

// DebtorReceipt is the aggregate root of money received from a debtor of a legal entity, recorded from bank or ERP
// evidence, with its allocations to the debtor's debit notes. What is not allocated stays on account.
type DebtorReceipt struct {
	state       ReceiptState
	allocations int   // allocations stored
	reversed    []int // stored allocations reversed since the receipt was loaded
	pending     []Event
}

const (
	eventReceiptRecorded  = "DebtorReceiptRecorded.v1"
	eventReceiptAllocated = "DebtorReceiptAllocated.v1"
	eventAllocationUndone = "DebtorReceiptAllocationReversed.v1"
	eventReceiptCancelled = "DebtorReceiptCancelled.v1"
)

// ReceiptInput is a receipt as asked, with the debtor's revision ORG confirmed in a debtor role.
type ReceiptInput struct {
	ID             string
	LegalEntityID  string
	DebtorPartyID  string
	DebtorRevision int64
	Amount         kernel.Money
	BankCharges    kernel.Money // zero, or in the amount's currency
	ReceivedOn     time.Time
	EvidenceSource string
	EvidenceRef    string
}

// RecordReceipt records money received: a positive amount, bank charges of the same currency not above it, the civil
// date it was received (not after the day it is recorded, in any time zone, and not more than 366 days before) and
// the evidence as a source and a reference.
func RecordReceipt(tenant TenantID, in ReceiptInput, by string, at time.Time) (*DebtorReceipt, error) {
	charges := in.BankCharges
	if charges == (kernel.Money{}) {
		charges.Currency = in.Amount.Currency
	}
	switch {
	case tenant == "" || !identifier(in.ID) || !identifier(in.LegalEntityID) || !identifier(in.DebtorPartyID) || in.DebtorRevision < 1 || blank(by) || at.IsZero():
		return nil, fmt.Errorf("%w: a receipt names its id, legal entity, debtor, author and time", ErrInvalid)
	case !in.Amount.Currency.Valid() || in.Amount.Minor <= 0 || in.Amount.Minor > MaxAmount:
		return nil, fmt.Errorf("%w: a receipt has a positive amount in a currency", ErrInvalid)
	case charges.Currency != in.Amount.Currency || charges.Minor < 0 || charges.Minor > in.Amount.Minor:
		return nil, fmt.Errorf("%w: bank charges are in the amount's currency and not more than the amount", ErrInvalid)
	case !date(in.ReceivedOn) || at.Before(in.ReceivedOn.Add(-14*time.Hour)) || !at.Before(in.ReceivedOn.Add(maxReceiptAge+36*time.Hour)):
		return nil, fmt.Errorf("%w: the date received is not after the day the receipt is recorded and not more than 366 days before", ErrInvalid)
	case (in.EvidenceSource != EvidenceBank && in.EvidenceSource != EvidenceERP) || !evidencePattern.MatchString(in.EvidenceRef):
		return nil, fmt.Errorf("%w: a receipt has its evidence as the source BANK or ERP and a reference (letters, digits, space and . _ : / # -, "+
			"200 characters)", ErrInvalid)
	}
	r := &DebtorReceipt{state: ReceiptState{TenantID: tenant, ReceiptID: in.ID, LegalEntityID: in.LegalEntityID, DebtorPartyID: in.DebtorPartyID,
		DebtorRevision: in.DebtorRevision, Currency: in.Amount.Currency, Amount: in.Amount.Minor, BankCharges: charges.Minor, ReceivedOn: in.ReceivedOn.UTC(),
		EvidenceSource: in.EvidenceSource, EvidenceRef: in.EvidenceRef, State: ReceiptRecorded, RecordedBy: by, RecordedAt: at.UTC()}}
	r.record(eventReceiptRecorded, at, 0, "")
	return r, nil
}

func (r *DebtorReceipt) lastChange() time.Time {
	last := later(r.state.RecordedAt, r.state.CancelledAt)
	for _, a := range r.state.Allocations {
		last = later(later(last, a.At), a.ReversedAt)
	}
	return last
}

// change checks what every change of a recorded receipt needs: an author, a time not before the receipt's last change
// and a civil date of that time.
func (r *DebtorReceipt) change(on time.Time, by string, at time.Time) error {
	switch {
	case blank(by) || at.IsZero() || at.Before(r.lastChange()):
		return fmt.Errorf("%w: a change names its author and a time not before the receipt's last change", ErrInvalid)
	case r.state.State != ReceiptRecorded:
		return fmt.Errorf("%w: receipt %s is %s", ErrInvalidTransition, r.state.ReceiptID, r.state.State)
	case !dateOf(on, at):
		return fmt.Errorf("%w: the date is the date of the change", ErrInvalid)
	}
	return nil
}

// SettlementNote is what an allocation needs to know of a note, read once the note's job is locked: the debit note's
// unreversed lines total Remaining, of which live allocations settle Settled.
type SettlementNote struct {
	InvoiceID     string
	Kind          string
	State         string
	Number        string
	LegalEntityID string
	DebtorPartyID string
	Currency      kernel.CurrencyCode
	InvoiceDate   time.Time
	Remaining     int64
	Settled       int64
}

// Open is what the debit note still has to be settled.
func (n SettlementNote) Open() int64 { return n.Remaining - n.Settled }

// AllocationItem is an amount of the receipt for a debit note.
type AllocationItem struct {
	InvoiceID string
	Amount    kernel.Money
}

// Allocate settles debit notes from the receipt: each an ISSUED debit note of the receipt's legal entity, debtor and
// currency (FIN_NOT_ALLOCATABLE otherwise), named once, with a positive amount within what the receipt has
// unallocated and the note has open (FIN_OVER_ALLOCATED). The date is not before the date received or a note's invoice
// date.
func (r *DebtorReceipt) Allocate(items []AllocationItem, notes []SettlementNote, on time.Time, by string, at time.Time) error {
	if err := r.change(on, by, at); err != nil {
		return err
	}
	st := &r.state
	if len(items) == 0 || len(items) > MaxAllocations || on.Before(st.ReceivedOn) {
		return fmt.Errorf("%w: an allocation names 1 to %d debit notes and a date not before the date received", ErrInvalid, MaxAllocations)
	}
	left := st.Unallocated()
	seen := map[string]bool{}
	var added []Allocation
	for _, it := range items {
		i := slices.IndexFunc(notes, func(n SettlementNote) bool { return n.InvoiceID == it.InvoiceID })
		switch {
		case !identifier(it.InvoiceID) || seen[it.InvoiceID] || it.Amount.Currency != st.Currency || it.Amount.Minor <= 0:
			return fmt.Errorf("%w: an allocation names each debit note once with a positive amount in %s", ErrInvalid, st.Currency)
		case i < 0:
			return fmt.Errorf("%w: no note %s", ErrNotAllocatable, it.InvoiceID)
		}
		seen[it.InvoiceID] = true
		n := notes[i]
		switch {
		case n.Kind != KindDebitNote || n.State != NoteIssued || n.LegalEntityID != st.LegalEntityID || n.DebtorPartyID != st.DebtorPartyID ||
			n.Currency != st.Currency:
			return fmt.Errorf("%w: note %s", ErrNotAllocatable, n.Number)
		case on.Before(n.InvoiceDate):
			return fmt.Errorf("%w: debit note %s is dated %s, after the allocation", ErrInvalid, n.Number, n.InvoiceDate.Format(time.DateOnly))
		case it.Amount.Minor > left:
			return fmt.Errorf("%w: receipt %s has %d unallocated", ErrOverAllocated, st.ReceiptID, left)
		case it.Amount.Minor > n.Open():
			return fmt.Errorf("%w: debit note %s has %d open", ErrOverAllocated, n.Number, n.Open())
		}
		left -= it.Amount.Minor
		added = append(added, Allocation{No: len(st.Allocations) + len(added) + 1, InvoiceID: n.InvoiceID, Amount: it.Amount.Minor, Date: on.UTC(), By: by,
			At: at.UTC()})
	}
	for _, a := range added {
		st.Allocations = append(st.Allocations, a)
		r.record(eventReceiptAllocated, at, a.No, a.InvoiceID)
	}
	return nil
}

// ReverseAllocation reverses one live allocation with a reason, dated on or after it: its debit note is open again
// for the amount, which is back on account.
func (r *DebtorReceipt) ReverseAllocation(no int, reason string, on time.Time, by string, at time.Time) error {
	if err := r.change(on, by, at); err != nil {
		return err
	}
	st := &r.state
	switch {
	case no < 1 || no > len(st.Allocations) || !text(reason):
		return fmt.Errorf("%w: a reversal names an allocation of the receipt and a reason", ErrInvalid)
	case !st.Allocations[no-1].Live():
		return fmt.Errorf("%w: allocation %d of receipt %s is reversed already", ErrInvalidTransition, no, st.ReceiptID)
	case on.Before(st.Allocations[no-1].Date):
		return fmt.Errorf("%w: a reversal is dated on or after its allocation", ErrInvalid)
	}
	a := &st.Allocations[no-1]
	a.ReversedBy, a.ReverseReason, a.ReversedOn, a.ReversedAt = by, reason, on.UTC(), at.UTC()
	if no <= r.allocations {
		r.reversed = append(r.reversed, no)
	}
	r.record(eventAllocationUndone, at, no, a.InvoiceID)
	return nil
}

// Cancel cancels a receipt entered in error or returned by the bank, once it has no live allocation, dated on or after
// the date received and every reversal.
func (r *DebtorReceipt) Cancel(reason string, on time.Time, by string, at time.Time) error {
	if err := r.change(on, by, at); err != nil {
		return err
	}
	st := &r.state
	if (reason != ReceiptEnteredInError && reason != ReceiptReturned) || on.Before(st.ReceivedOn) {
		return fmt.Errorf("%w: a cancellation has the reason ENTERED_IN_ERROR or PAYMENT_RETURNED and a date not before the date received", ErrInvalid)
	}
	for _, a := range st.Allocations {
		switch {
		case a.Live():
			return fmt.Errorf("%w: receipt %s settles debit notes; reverse its allocations first", ErrInvalidTransition, st.ReceiptID)
		case on.Before(a.ReversedOn):
			return fmt.Errorf("%w: a cancellation is dated on or after the receipt's reversals", ErrInvalid)
		}
	}
	st.State, st.CancelReason, st.CancelledBy, st.CancelledOn, st.CancelledAt = ReceiptCancelled, reason, by, on.UTC(), at.UTC()
	r.record(eventReceiptCancelled, at, 0, "")
	return nil
}

func (r *DebtorReceipt) record(kind string, at time.Time, version int, invoice string) {
	r.pending = append(r.pending, Event{Type: kind, At: at.UTC(), TenantID: r.state.TenantID, SubjectID: r.state.ReceiptID,
		LegalEntityID: r.state.LegalEntityID, Version: version, DebtorPartyID: r.state.DebtorPartyID, InvoiceID: invoice})
}

// Stamp records when FIN entered what was written since the receipt was loaded (T1).
func (r *DebtorReceipt) Stamp(entered time.Time) {
	e := entered.UTC()
	st := &r.state
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

// RestoreReceipt rebuilds a stored receipt.
func RestoreReceipt(st ReceiptState) (*DebtorReceipt, error) {
	st.Allocations = append([]Allocation(nil), st.Allocations...)
	for i, a := range st.Allocations {
		if a.No != i+1 || a.Amount <= 0 {
			return nil, fmt.Errorf("%w: allocation %d of receipt %s", ErrInvalid, i+1, st.ReceiptID)
		}
	}
	if st.Unallocated() < 0 || (st.State != ReceiptRecorded && st.State != ReceiptCancelled) || (st.State == ReceiptCancelled) == st.CancelledAt.IsZero() {
		return nil, fmt.Errorf("%w: receipt %s", ErrInvalid, st.ReceiptID)
	}
	return &DebtorReceipt{state: st, allocations: len(st.Allocations)}, nil
}

func (r *DebtorReceipt) State() ReceiptState {
	st := r.state
	st.Allocations = append([]Allocation(nil), st.Allocations...)
	return st
}

// Added returns the allocations written since the receipt was loaded and the stored ones reversed since.
func (r *DebtorReceipt) Added() (added, reversed []Allocation) {
	st := r.State()
	for _, no := range r.reversed {
		reversed = append(reversed, st.Allocations[no-1])
	}
	return st.Allocations[r.allocations:], reversed
}

func (r *DebtorReceipt) Pending() []Event { return append([]Event(nil), r.pending...) }

func (r *DebtorReceipt) Acknowledge() {
	r.pending, r.allocations, r.reversed = nil, len(r.state.Allocations), nil
}
