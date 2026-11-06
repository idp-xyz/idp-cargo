// Package domain is the model of RATE (Rate, Quote & Commercial Rules): quotes priced in one currency,
// the margin floor below which a quote needs a price approval, acceptance within the quote's validity,
// and the frozen accepted quote. It depends on the standard library and the kernel only.
package domain

import (
	"errors"
	"fmt"
	"math/bits"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

type TenantID string
type QuoteID string
type LegalEntityID string
type ChargeCode string

// CustomerRef is RATE's copy of the ORG party reference a quote is priced for.
type CustomerRef struct {
	PartyID       string
	PartyRevision int64
}

type QuoteStatus string

const (
	QuoteAwaitingApproval QuoteStatus = "AWAITING_APPROVAL"
	QuotePriced           QuoteStatus = "PRICED"
	QuoteAccepted         QuoteStatus = "ACCEPTED"
)

var (
	ErrInvalidQuote      = errors.New("rate: invalid quote")
	ErrInvalidTransition = errors.New("rate: invalid state transition")
	ErrSelfApproval      = errors.New("rate: a price approval needs an approver other than the quote's author")
	ErrOutsideValidity   = errors.New("rate: instant outside the quote's validity")
	ErrConflict          = errors.New("rate: concurrent revision conflict")
)

// Line is one priced charge. Cost stays inside RATE; only sell amounts leave it.
type Line struct {
	ChargeCode ChargeCode
	Sell       kernel.Money
	Cost       kernel.Money
}

// PriceApproval records who authorized a quote below its margin floor, and the reason.
type PriceApproval struct {
	ApprovedBy string
	ApprovedAt time.Time
	ReasonRef  string
}

// Acceptance records the customer's acceptance and the customer revision ORG confirmed at that instant.
type Acceptance struct {
	AcceptedBy       string
	AcceptedAt       time.Time
	EvidenceRef      string
	CustomerRevision int64
}

// Event carries identifiers only; the Outbox adapter maps it to a Bento envelope.
type Event struct {
	Type      string
	At        time.Time
	TenantID  TenantID
	SubjectID string
}

// QuoteDraft is what a new quote is priced from.
type QuoteDraft struct {
	TenantID      TenantID
	QuoteID       QuoteID
	LegalEntityID LegalEntityID
	Customer      CustomerRef
	Currency      kernel.CurrencyCode
	Lines         []Line
	Validity      kernel.ValidityInterval
	MarginFloorBP int // minimum margin in basis points of the sell total
	Author        string
}

// QuoteState is a stored quote, for persistence.
type QuoteState struct {
	QuoteDraft
	Status     QuoteStatus
	Approval   *PriceApproval
	Acceptance *Acceptance
}

// Quote is the aggregate root. Once accepted it does not change.
type Quote struct {
	state   QuoteState
	pending []Event
}

// PriceQuote creates a quote. A margin below the floor makes it wait for a price approval; otherwise
// it can be accepted at once.
func PriceQuote(d QuoteDraft, at time.Time) (*Quote, error) {
	if at.IsZero() {
		return nil, ErrInvalidQuote
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	if !at.Before(d.Validity.Until) {
		return nil, fmt.Errorf("%w: validity has already ended", ErrInvalidQuote)
	}
	d.Lines = append([]Line(nil), d.Lines...)
	q := &Quote{state: QuoteState{QuoteDraft: d, Status: QuotePriced}}
	below, err := q.BelowMarginFloor()
	if err != nil {
		return nil, err
	}
	if below {
		q.state.Status = QuoteAwaitingApproval
	}
	q.record("QuotePriced.v1", at)
	return q, nil
}

func (d QuoteDraft) validate() error {
	switch {
	case blank(string(d.TenantID)), blank(string(d.QuoteID)), blank(string(d.LegalEntityID)), blank(d.Customer.PartyID),
		d.Customer.PartyRevision < 1, !d.Currency.Valid(), len(d.Lines) == 0, blank(d.Author),
		d.MarginFloorBP < 0, d.MarginFloorBP > 10000:
		return ErrInvalidQuote
	case !d.Validity.Valid() || d.Validity.Until.IsZero():
		return fmt.Errorf("%w: a quote needs a bounded validity", ErrInvalidQuote)
	}
	for _, l := range d.Lines {
		if blank(string(l.ChargeCode)) || l.Sell.Currency != d.Currency || l.Cost.Currency != d.Currency || l.Sell.Minor <= 0 || l.Cost.Minor < 0 {
			return fmt.Errorf("%w: line %q", ErrInvalidQuote, l.ChargeCode)
		}
	}
	return nil
}

// RestoreQuote rebuilds a stored quote without emitting events.
func RestoreQuote(s QuoteState) (*Quote, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	switch s.Status {
	case QuoteAwaitingApproval, QuotePriced, QuoteAccepted:
	default:
		return nil, fmt.Errorf("%w: status %q", ErrInvalidQuote, s.Status)
	}
	s.Lines = append([]Line(nil), s.Lines...)
	q := &Quote{state: s}
	below, err := q.BelowMarginFloor()
	if err != nil {
		return nil, err
	}
	switch {
	case s.Status == QuoteAwaitingApproval && (s.Approval != nil || s.Acceptance != nil || !below):
		return nil, fmt.Errorf("%w: inconsistent approval state", ErrInvalidQuote)
	case s.Status == QuotePriced && s.Acceptance != nil, s.Status == QuoteAccepted && s.Acceptance == nil:
		return nil, fmt.Errorf("%w: inconsistent acceptance state", ErrInvalidQuote)
	case s.Status != QuoteAwaitingApproval && below && s.Approval == nil:
		return nil, fmt.Errorf("%w: below the margin floor without a price approval", ErrInvalidQuote)
	}
	return q, nil
}

// ApprovePrice authorizes a quote below its margin floor. The approver must not be the quote's author.
func (q *Quote) ApprovePrice(approver, reasonRef string, at time.Time) error {
	if q.state.Status != QuoteAwaitingApproval || at.IsZero() {
		return ErrInvalidTransition
	}
	if blank(approver) || blank(reasonRef) {
		return ErrInvalidQuote
	}
	if approver == q.state.Author {
		return ErrSelfApproval
	}
	q.state.Approval = &PriceApproval{ApprovedBy: approver, ApprovedAt: at.UTC(), ReasonRef: reasonRef}
	q.state.Status = QuotePriced
	q.record("QuotePriceApproved.v1", at)
	return nil
}

// Accept records the customer's acceptance within the quote's validity. customerRevision is the party
// revision ORG confirmed at that instant; it cannot be older than the one the quote was priced for.
func (q *Quote) Accept(acceptedBy, evidenceRef string, customerRevision int64, at time.Time) error {
	if q.state.Status != QuotePriced || at.IsZero() {
		return ErrInvalidTransition
	}
	if blank(acceptedBy) || blank(evidenceRef) || customerRevision < q.state.Customer.PartyRevision {
		return ErrInvalidQuote
	}
	if !q.state.Validity.Contains(at) {
		return ErrOutsideValidity
	}
	q.state.Acceptance = &Acceptance{AcceptedBy: acceptedBy, AcceptedAt: at.UTC(), EvidenceRef: evidenceRef, CustomerRevision: customerRevision}
	q.state.Status = QuoteAccepted
	q.record("QuoteAccepted.v1", at)
	return nil
}

// Totals returns the sell and cost totals.
func (q *Quote) Totals() (sell, cost kernel.Money, err error) {
	sell, cost = kernel.Money{Currency: q.state.Currency}, kernel.Money{Currency: q.state.Currency}
	for _, l := range q.state.Lines {
		if sell, err = sell.Add(l.Sell); err != nil {
			return kernel.Money{}, kernel.Money{}, err
		}
		if cost, err = cost.Add(l.Cost); err != nil {
			return kernel.Money{}, kernel.Money{}, err
		}
	}
	return sell, cost, nil
}

// BelowMarginFloor reports whether (sell − cost) / sell is below the floor, compared exactly as
// (sell − cost) × 10000 < floor × sell.
func (q *Quote) BelowMarginFloor() (bool, error) {
	sell, cost, err := q.Totals()
	if err != nil {
		return false, err
	}
	margin, err := sell.Sub(cost)
	if err != nil {
		return false, err
	}
	if margin.Minor < 0 {
		return true, nil
	}
	hiMargin, loMargin := bits.Mul64(uint64(margin.Minor), 10000)
	hiFloor, loFloor := bits.Mul64(uint64(q.state.MarginFloorBP), uint64(sell.Minor))
	return hiMargin < hiFloor || (hiMargin == hiFloor && loMargin < loFloor), nil
}

// State returns a copy of the quote for persistence and read models.
func (q *Quote) State() QuoteState {
	s := q.state
	s.Lines = append([]Line(nil), s.Lines...)
	if s.Approval != nil {
		a := *s.Approval
		s.Approval = &a
	}
	if s.Acceptance != nil {
		a := *s.Acceptance
		s.Acceptance = &a
	}
	return s
}

func (q *Quote) ID() QuoteID         { return q.state.QuoteID }
func (q *Quote) Tenant() TenantID    { return q.state.TenantID }
func (q *Quote) Status() QuoteStatus { return q.state.Status }
func (q *Quote) Pending() []Event    { return append([]Event(nil), q.pending...) }

// Acknowledge is permitted only after the transaction holding the quote and its Outbox events commits.
func (q *Quote) Acknowledge() { q.pending = nil }

func (q *Quote) record(kind string, at time.Time) {
	q.pending = append(q.pending, Event{Type: kind, At: at.UTC(), TenantID: q.state.TenantID, SubjectID: string(q.state.QuoteID)})
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }
