// Package domain is the model of COM (Commercial Commitment): a customer order recorded against an
// accepted quote, the customer's transaction snapshot, the fulfillment obligations its lines are allocated
// to, their revocation, and the reservations FUL shipments hold on them. It depends on the standard
// library and the kernel only.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

type TenantID string
type OrderID string
type ObligationID string
type LegalEntityID string

// CustomerSnapshot is the TransactionPartySnapshot of the customer: the ORG party at the revision ORG
// confirmed when the order was recorded, with the particulars and the role in force then. Later ORG
// changes never alter it.
type CustomerSnapshot struct {
	PartyID         string
	PartyRevision   int64
	LegalName       string
	CountryCode     string
	RoleValidity    kernel.ValidityInterval
	RoleEvidenceRef string
}

// AcceptedQuote is COM's copy of the QuoteSnapshot v1 an order relies on. TermsHash is the hex SHA-256 of
// the snapshot's canonical encoding.
type AcceptedQuote struct {
	QuoteID          string
	QuoteRevision    int64
	LegalEntityID    LegalEntityID
	CustomerPartyID  string
	CustomerRevision int64 // the revision ORG confirmed when the quote was accepted
	Total            kernel.Money
	Validity         kernel.ValidityInterval
	AcceptedAt       time.Time
	EvidenceRef      string
	TermsHash        string
}

// OrderLine is one goods requirement: packs of one package type with their declared gross weight and,
// optionally, volume (C3). Lines are numbered from 1.
type OrderLine struct {
	LineNo      int
	Description string
	PackageType PackageType
	Measure     Measure
}

type OrderStatus string

const (
	OrderRecorded           OrderStatus = "RECORDED"
	OrderPartiallyAllocated OrderStatus = "PARTIALLY_ALLOCATED"
	OrderAllocated          OrderStatus = "ALLOCATED"
	OrderCancelled          OrderStatus = "CANCELLED"
)

var (
	ErrInvalidOrder         = errors.New("com: invalid customer order")
	ErrQuoteMismatch        = errors.New("com: the order does not match the accepted quote")
	ErrOutsideQuoteValidity = errors.New("com: instant outside the accepted quote's validity")
	ErrInvalidAllocation    = errors.New("com: invalid allocation")
	ErrOverAllocation       = errors.New("com: allocation exceeds what the line has left")
	ErrInvalidTransition    = errors.New("com: invalid state transition")
	ErrInvalidChange        = errors.New("com: invalid change to the order")
	ErrUnknownObligation    = errors.New("com: the order holds no such obligation version")
	ErrObligationNotActive  = errors.New("com: the obligation version is revoked or superseded")
	ErrObligationReserved   = errors.New("com: a shipment holds the obligation")
	ErrNotReserved          = errors.New("com: the shipment does not hold the obligation version")
	ErrConflict             = errors.New("com: concurrent revision conflict")
)

// Event types, as package api publishes them.
const (
	eventOrderRecorded       = "CustomerOrderRecorded.v1"
	eventOrderCancelled      = "CustomerOrderCancelled.v1"
	eventObligationAllocated = "FulfillmentObligationAllocated.v1"
	eventObligationRevoked   = "FulfillmentObligationRevoked.v1"
	eventObligationReserved  = "ObligationReserved.v1"
	eventReservationReleased = "ObligationReservationReleased.v1"
)

// Event carries identifiers only; the Outbox adapter maps it to a Bento envelope.
type Event struct {
	Type              string
	At                time.Time
	TenantID          TenantID
	SubjectID         string
	ObligationID      ObligationID
	ObligationVersion int64
	ShipmentID        string
}

// OrderDraft is what a new order is recorded from. Lines are numbered in the order given.
type OrderDraft struct {
	TenantID   TenantID
	OrderID    OrderID
	Quote      AcceptedQuote
	Customer   CustomerSnapshot
	Lines      []OrderLine
	RecordedBy string
}

// OrderState is a stored order, for persistence. Obligations holds every version of every obligation and
// Reservations every reservation, released ones included, so the order keeps its whole history (C6).
type OrderState struct {
	OrderDraft
	RecordedAt   time.Time
	Status       OrderStatus
	Obligations  []Obligation
	Reservations []Reservation
	CancelledBy  string
	CancelledAt  time.Time
}

// CustomerOrder is the aggregate root. Its commitment never changes. Obligation versions and reservations
// are only added, and a reservation is released at most once.
type CustomerOrder struct {
	state    OrderState
	stored   int   // obligation versions already persisted
	reserved int   // reservations already persisted
	released []int // persisted reservations released since, by index
	pending  []Event
}

const (
	maxText = 500 // the longest legal name ORG accepts
	maxID   = 128 // the longest identifier cargo_com stores
)

// RecordOrder records an order against an accepted quote, for the quote's customer as ORG confirmed it
// at the instant.
func RecordOrder(d OrderDraft, at time.Time) (*CustomerOrder, error) {
	if at.IsZero() {
		return nil, ErrInvalidOrder
	}
	d.Lines = append([]OrderLine(nil), d.Lines...)
	for i := range d.Lines {
		d.Lines[i].LineNo = i + 1
	}
	s := OrderState{OrderDraft: d, RecordedAt: at.UTC(), Status: OrderRecorded}
	if err := s.validateCommitment(); err != nil {
		return nil, err
	}
	o := &CustomerOrder{state: s}
	o.record(eventOrderRecorded, at, nil, "")
	return o, nil
}

func (s OrderState) validateCommitment() error {
	q, c := s.Quote, s.Customer
	switch {
	case blank(string(s.TenantID)), blank(string(s.OrderID)), blank(s.RecordedBy), s.RecordedAt.IsZero():
		return ErrInvalidOrder
	case blank(q.QuoteID), q.QuoteRevision < 1, blank(string(q.LegalEntityID)), blank(q.CustomerPartyID), q.CustomerRevision < 1,
		!q.Total.Currency.Valid(), q.Total.Minor <= 0, !q.Validity.Valid(), q.Validity.Until.IsZero(),
		!q.Validity.Contains(q.AcceptedAt), blank(q.EvidenceRef), !hexDigest(q.TermsHash):
		return fmt.Errorf("%w: accepted quote", ErrInvalidOrder)
	case blank(c.PartyID), c.PartyRevision < 1, !text(c.LegalName), !countryCode(c.CountryCode), !c.RoleValidity.Valid(),
		blank(c.RoleEvidenceRef):
		return fmt.Errorf("%w: customer snapshot", ErrInvalidOrder)
	case c.PartyID != q.CustomerPartyID:
		return fmt.Errorf("%w: the customer is not the quote's customer", ErrQuoteMismatch)
	case c.PartyRevision < q.CustomerRevision:
		return fmt.Errorf("%w: ORG revision %d is older than %d, the revision at acceptance", ErrQuoteMismatch, c.PartyRevision, q.CustomerRevision)
	case !c.RoleValidity.Contains(s.RecordedAt):
		return fmt.Errorf("%w: the customer role was not in force when the order was recorded", ErrInvalidOrder)
	case s.RecordedAt.Before(q.AcceptedAt) || !q.Validity.Contains(s.RecordedAt):
		return ErrOutsideQuoteValidity
	case len(s.Lines) == 0:
		return fmt.Errorf("%w: no lines", ErrInvalidOrder)
	}
	for i, l := range s.Lines {
		if l.LineNo != i+1 || !text(l.Description) || !l.PackageType.Valid() || !l.Measure.valid() {
			return fmt.Errorf("%w: line %d", ErrInvalidOrder, i+1)
		}
	}
	return nil
}

// RestoreOrder rebuilds a stored order without emitting events.
func RestoreOrder(s OrderState) (*CustomerOrder, error) {
	switch s.Status {
	case OrderRecorded, OrderPartiallyAllocated, OrderAllocated, OrderCancelled:
	default:
		return nil, fmt.Errorf("%w: status %q", ErrInvalidOrder, s.Status)
	}
	if err := s.validateCommitment(); err != nil {
		return nil, err
	}
	if cancelled := !s.CancelledAt.IsZero(); cancelled == blank(s.CancelledBy) || (cancelled && s.CancelledAt.Before(s.RecordedAt)) {
		return nil, fmt.Errorf("%w: cancellation", ErrInvalidOrder)
	}
	s.Lines = append([]OrderLine(nil), s.Lines...)
	s.Obligations = append([]Obligation(nil), s.Obligations...)
	s.Reservations = append([]Reservation(nil), s.Reservations...)
	o := &CustomerOrder{state: s, stored: len(s.Obligations), reserved: len(s.Reservations)}
	if err := o.validateObligations(); err != nil {
		return nil, err
	}
	if err := o.validateReservations(); err != nil {
		return nil, err
	}
	if derived := o.derivedStatus(); s.Status != derived {
		return nil, fmt.Errorf("%w: status %s, but the obligations make it %s", ErrInvalidOrder, s.Status, derived)
	}
	return o, nil
}

// State returns a copy of the order for persistence and read models.
func (o *CustomerOrder) State() OrderState {
	s := o.state
	s.Lines = append([]OrderLine(nil), s.Lines...)
	s.Obligations = append([]Obligation(nil), s.Obligations...)
	s.Reservations = append([]Reservation(nil), s.Reservations...)
	return s
}

func (o *CustomerOrder) ID() OrderID         { return o.state.OrderID }
func (o *CustomerOrder) Tenant() TenantID    { return o.state.TenantID }
func (o *CustomerOrder) Status() OrderStatus { return o.state.Status }
func (o *CustomerOrder) Pending() []Event    { return append([]Event(nil), o.pending...) }

// Added returns the obligation versions written since the order was recorded or restored, for persistence.
func (o *CustomerOrder) Added() []Obligation {
	return append([]Obligation(nil), o.state.Obligations[o.stored:]...)
}

// Acknowledge is permitted only after the transaction holding the order and its Outbox events commits.
func (o *CustomerOrder) Acknowledge() {
	o.pending, o.stored, o.reserved, o.released = nil, len(o.state.Obligations), len(o.state.Reservations), nil
}

func (o *CustomerOrder) record(kind string, at time.Time, ob *Obligation, shipment string) {
	e := Event{Type: kind, At: at.UTC(), TenantID: o.state.TenantID, SubjectID: string(o.state.OrderID), ShipmentID: shipment}
	if ob != nil {
		e.ObligationID, e.ObligationVersion = ob.ObligationID, ob.Version
	}
	o.pending = append(o.pending, e)
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxText }

func identifier(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxID }

func countryCode(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

func hexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return true
}
