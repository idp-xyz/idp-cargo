// Package domain is the model of COM (Commercial Commitment): a customer order recorded against an
// accepted quote, the customer's transaction snapshot, and the fulfillment obligations its lines are
// allocated to. It depends on the standard library and the kernel only.
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

// OrderLine is one goods or service requirement. Lines are numbered from 1.
type OrderLine struct {
	LineNo      int
	Description string
	Quantity    kernel.Quantity
}

// Obligation is a fulfillment obligation: part or all of one line's quantity that COM commits to fulfil.
// It never changes; a change will need a new version.
type Obligation struct {
	ObligationID ObligationID
	Version      int64
	LineNo       int
	Quantity     kernel.Quantity
	AllocatedBy  string
	AllocatedAt  time.Time
}

type OrderStatus string

const (
	OrderRecorded           OrderStatus = "RECORDED"
	OrderPartiallyAllocated OrderStatus = "PARTIALLY_ALLOCATED"
	OrderAllocated          OrderStatus = "ALLOCATED"
)

var (
	ErrInvalidOrder         = errors.New("com: invalid customer order")
	ErrQuoteMismatch        = errors.New("com: the order does not match the accepted quote")
	ErrOutsideQuoteValidity = errors.New("com: instant outside the accepted quote's validity")
	ErrInvalidAllocation    = errors.New("com: invalid allocation")
	ErrOverAllocation       = errors.New("com: allocation exceeds the line's remaining quantity")
	ErrInvalidTransition    = errors.New("com: invalid state transition")
	ErrConflict             = errors.New("com: concurrent revision conflict")
)

// Event carries identifiers only; the Outbox adapter maps it to a Bento envelope.
type Event struct {
	Type              string
	At                time.Time
	TenantID          TenantID
	SubjectID         string
	ObligationID      ObligationID
	ObligationVersion int64
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

// OrderState is a stored order, for persistence.
type OrderState struct {
	OrderDraft
	RecordedAt  time.Time
	Status      OrderStatus
	Obligations []Obligation
}

// CustomerOrder is the aggregate root. Its commitment never changes; allocation only adds obligations.
type CustomerOrder struct {
	state   OrderState
	stored  int // obligations already persisted
	pending []Event
}

// maxText matches the longest legal name ORG accepts.
const maxText = 500

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
	o.record("CustomerOrderRecorded.v1", at, nil)
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
		if l.LineNo != i+1 || !text(l.Description) || !l.Quantity.Unit.Valid() || l.Quantity.Amount <= 0 {
			return fmt.Errorf("%w: line %d", ErrInvalidOrder, i+1)
		}
	}
	return nil
}

// RestoreOrder rebuilds a stored order without emitting events.
func RestoreOrder(s OrderState) (*CustomerOrder, error) {
	switch s.Status {
	case OrderRecorded, OrderPartiallyAllocated, OrderAllocated:
	default:
		return nil, fmt.Errorf("%w: status %q", ErrInvalidOrder, s.Status)
	}
	if err := s.validateCommitment(); err != nil {
		return nil, err
	}
	s.Lines = append([]OrderLine(nil), s.Lines...)
	s.Obligations = append([]Obligation(nil), s.Obligations...)
	o := &CustomerOrder{state: s, stored: len(s.Obligations)}
	seen := map[ObligationID]bool{}
	for _, ob := range s.Obligations {
		if blank(string(ob.ObligationID)) || seen[ob.ObligationID] || ob.Version < 1 || ob.LineNo < 1 || ob.LineNo > len(s.Lines) ||
			ob.Quantity.Amount <= 0 || blank(ob.AllocatedBy) || ob.AllocatedAt.Before(s.RecordedAt) {
			return nil, fmt.Errorf("%w: obligation %q", ErrInvalidOrder, ob.ObligationID)
		}
		seen[ob.ObligationID] = true
	}
	for no := 1; no <= len(s.Lines); no++ {
		if remaining, err := o.Remaining(no); err != nil || remaining.Amount < 0 {
			return nil, fmt.Errorf("%w: line %d is over-allocated", ErrInvalidOrder, no)
		}
	}
	if derived := o.derivedStatus(); s.Status != derived {
		return nil, fmt.Errorf("%w: status %s, but the allocations make it %s", ErrInvalidOrder, s.Status, derived)
	}
	return o, nil
}

// Allocate commits part or all of a line's remaining quantity to a new obligation at version 1.
func (o *CustomerOrder) Allocate(lineNo int, id ObligationID, qty kernel.Quantity, by string, at time.Time) error {
	if o.state.Status == OrderAllocated {
		return ErrInvalidTransition
	}
	if at.IsZero() || at.Before(o.state.RecordedAt) || blank(string(id)) || blank(by) || qty.Amount <= 0 {
		return ErrInvalidAllocation
	}
	if o.holds(id) {
		return fmt.Errorf("%w: obligation %q already exists", ErrInvalidAllocation, id)
	}
	remaining, err := o.Remaining(lineNo)
	if err != nil {
		return err
	}
	if qty.Unit != remaining.Unit {
		return fmt.Errorf("%w: %w", ErrInvalidAllocation, kernel.ErrUnitMismatch)
	}
	if qty.Amount > remaining.Amount {
		return ErrOverAllocation
	}
	ob := Obligation{ObligationID: id, Version: 1, LineNo: lineNo, Quantity: qty, AllocatedBy: by, AllocatedAt: at.UTC()}
	o.state.Obligations = append(o.state.Obligations, ob)
	o.state.Status = o.derivedStatus()
	o.record("FulfillmentObligationAllocated.v1", at, &ob)
	return nil
}

// Remaining returns the quantity of a line not yet allocated to obligations.
func (o *CustomerOrder) Remaining(lineNo int) (kernel.Quantity, error) {
	if lineNo < 1 || lineNo > len(o.state.Lines) {
		return kernel.Quantity{}, fmt.Errorf("%w: no line %d", ErrInvalidAllocation, lineNo)
	}
	remaining := o.state.Lines[lineNo-1].Quantity
	for _, ob := range o.state.Obligations {
		if ob.LineNo != lineNo {
			continue
		}
		var err error
		if remaining, err = remaining.Sub(ob.Quantity); err != nil {
			return kernel.Quantity{}, err
		}
	}
	return remaining, nil
}

// Obligation returns one version of an obligation of the order.
func (o *CustomerOrder) Obligation(id ObligationID, version int64) (Obligation, bool) {
	for _, ob := range o.state.Obligations {
		if ob.ObligationID == id && ob.Version == version {
			return ob, true
		}
	}
	return Obligation{}, false
}

func (o *CustomerOrder) holds(id ObligationID) bool {
	for _, ob := range o.state.Obligations {
		if ob.ObligationID == id {
			return true
		}
	}
	return false
}

func (o *CustomerOrder) derivedStatus() OrderStatus {
	if len(o.state.Obligations) == 0 {
		return OrderRecorded
	}
	for no := 1; no <= len(o.state.Lines); no++ {
		if remaining, err := o.Remaining(no); err != nil || remaining.Amount != 0 {
			return OrderPartiallyAllocated
		}
	}
	return OrderAllocated
}

// State returns a copy of the order for persistence and read models.
func (o *CustomerOrder) State() OrderState {
	s := o.state
	s.Lines = append([]OrderLine(nil), s.Lines...)
	s.Obligations = append([]Obligation(nil), s.Obligations...)
	return s
}

func (o *CustomerOrder) ID() OrderID         { return o.state.OrderID }
func (o *CustomerOrder) Tenant() TenantID    { return o.state.TenantID }
func (o *CustomerOrder) Status() OrderStatus { return o.state.Status }
func (o *CustomerOrder) Pending() []Event    { return append([]Event(nil), o.pending...) }

// Added returns the obligations allocated since the order was recorded or restored, for persistence.
func (o *CustomerOrder) Added() []Obligation {
	return append([]Obligation(nil), o.state.Obligations[o.stored:]...)
}

// Acknowledge is permitted only after the transaction holding the order and its Outbox events commits.
func (o *CustomerOrder) Acknowledge() { o.pending, o.stored = nil, len(o.state.Obligations) }

func (o *CustomerOrder) record(kind string, at time.Time, ob *Obligation) {
	e := Event{Type: kind, At: at.UTC(), TenantID: o.state.TenantID, SubjectID: string(o.state.OrderID)}
	if ob != nil {
		e.ObligationID, e.ObligationVersion = ob.ObligationID, ob.Version
	}
	o.pending = append(o.pending, e)
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxText }

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
