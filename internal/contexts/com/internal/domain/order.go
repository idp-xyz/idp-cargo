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

// CustomerSnapshot is the TransactionPartySnapshot of the ordering party: the ORG party at the revision ORG
// confirmed when the order was recorded, with the particulars and the role in force then. The ordering party
// is the quote's recipient, in the role the quote went to (R7): CUSTOMER, OVERSEAS_AGENT, SHIPPER or
// CONSIGNEE. Later ORG changes never alter it.
type CustomerSnapshot struct {
	PartyID         string
	PartyRevision   int64
	LegalName       string
	CountryCode     string
	RoleCode        string
	RoleValidity    kernel.ValidityInterval
	RoleEvidenceRef string
}

// Ordering roles: the roles a quote may go to (R7).
const (
	RoleCustomer      = "CUSTOMER"
	RoleOverseasAgent = "OVERSEAS_AGENT"
	RoleShipper       = "SHIPPER"
	RoleConsignee     = "CONSIGNEE"
)

func orderingRole(r string) bool {
	return r == RoleCustomer || r == RoleOverseasAgent || r == RoleShipper || r == RoleConsignee
}

// Contracts of the RATE snapshot an order relies on.
const (
	QuoteSnapshotV1 = "QuoteSnapshot.v1"
	QuoteSnapshotV2 = "QuoteSnapshot.v2"
)

// AcceptedQuote is COM's copy of the RATE snapshot an order relies on: QuoteSnapshot v2 for orders
// recorded since COM V0.5, v1 before. TermsHash is the hex SHA-256 of the snapshot's canonical encoding
// under TermsContract. RecipientRole is the role the quote went to (CUSTOMER for v1) and Terms the
// commercial terms it agreed (none for v1).
type AcceptedQuote struct {
	QuoteID          string
	QuoteRevision    int64
	LegalEntityID    LegalEntityID
	CustomerPartyID  string
	CustomerRevision int64 // the revision ORG confirmed when the quote was accepted
	RecipientRole    string
	Total            kernel.Money
	Validity         kernel.ValidityInterval
	AcceptedAt       time.Time
	EvidenceRef      string
	Terms            CommercialTerms
	TermsContract    string
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
	OrderClosed             OrderStatus = "CLOSED"
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
	eventLineAmended         = "CustomerOrderLineAmended.v1"
	eventLineAdded           = "CustomerOrderLineAdded.v1"
	eventPartySet            = "OrderPartySet.v1"
	eventTransportChanged    = "OrderTransportChanged.v1"
	eventOrderClosed         = "CustomerOrderClosed.v1"
	eventContactsErased      = "CustomerOrderContactsErased.v1"
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
	LineNo            int   // set for line events
	LineVersion       int64 // set for line events
	PartyRole         string
	PartyVersion      int64
	PartyKind         string
	Overridden        bool  // the party version carries an address or contact given on the order (C9; screening trigger, X1)
	TransportVersion  int64 // set for transport events
}

// OrderDraft is what a new order is recorded from. Lines are numbered in the order given. Requested are the
// commercial terms given on the order, to complete what the quote left open (C8). Policy is the order policy
// in force for the quote's legal entity (C1, C2). Transport and InitialParties are optional (C9, C11).
type OrderDraft struct {
	TenantID       TenantID
	OrderID        OrderID
	Quote          AcceptedQuote
	Customer       CustomerSnapshot
	Lines          []OrderLine
	Requested      CommercialTerms
	Policy         OrderPolicy
	RecordedBy     string
	Transport      Transport
	InitialParties []OrderParty
}

// OrderState is a stored order, for persistence. Lines are the lines as recorded and LineChanges every line
// version written since, in order (C10). Obligations holds every version of every obligation and
// Reservations every reservation, released ones included, so the order keeps its whole history (C6).
// EnteredAt is when COM recorded the order (T1); it is zero for orders recorded before COM V0.5.
type OrderState struct {
	OrderDraft
	Terms        OrderTerms
	RecordedAt   time.Time
	EnteredAt    time.Time
	Status       OrderStatus
	LineChanges  []LineVersion
	Obligations  []Obligation
	Reservations []Reservation
	CancelledBy  string
	CancelledAt  time.Time
	ClosedBy     string
	ClosedAt     time.Time
	// Retention is set once the order is closed or cancelled, Erasure once its contacts are erased (P1).
	Retention *ContactRetention
	Erasure   *ContactErasure
	// Every version of the order's transport and parties, in the order written (C9, C11, C12).
	TransportVersions []TransportVersion
	Parties           []OrderParty
}

// CustomerOrder is the aggregate root. What the quote, the ordering party, the terms and the policy say
// never changes; lines change only by new versions (C10). Obligation versions and reservations are only
// added, and a reservation is released at most once.
type CustomerOrder struct {
	state           OrderState
	lines           lineIndex
	storedChanges   int   // line versions already persisted
	stored          int   // obligation versions already persisted
	reserved        int   // reservations already persisted
	released        []int // persisted reservations released since, by index
	storedParties   int   // party versions already persisted
	storedTransport int   // transport versions already persisted
	retained        bool  // the retention is persisted
	erased          bool  // the erasure is persisted
	pending         []Event
}

const (
	maxText = 500 // the longest legal name ORG accepts
	maxID   = 128 // the longest identifier cargo_com stores
)

// RecordOrder records an order against an accepted quote, for the quote's recipient in the role the quote
// went to, as ORG confirmed it at the instant (R7). ordersOfQuote is the number of orders not cancelled that
// the quote already backs; the policy bounds it (C1) and how long after the quote's validity the order may
// be recorded (C2). The order freezes the terms the quote agreed, completed by those requested (C8).
// entered is when COM records the order (T1).
func RecordOrder(d OrderDraft, ordersOfQuote int, at, entered time.Time) (*CustomerOrder, error) {
	if at.IsZero() || entered.IsZero() || ordersOfQuote < 0 {
		return nil, ErrInvalidOrder
	}
	d.Lines = append([]OrderLine(nil), d.Lines...)
	for i := range d.Lines {
		d.Lines[i].LineNo = i + 1
	}
	terms, err := AgreeTerms(d.Quote.Terms, d.Requested)
	if err != nil {
		return nil, err
	}
	s := OrderState{OrderDraft: d, Terms: terms, RecordedAt: at.UTC(), EnteredAt: entered.UTC(), Status: OrderRecorded}
	s.Requested = CommercialTerms{}
	if err = s.validateCommitment(); err != nil {
		return nil, err
	}
	if ordersOfQuote >= d.Policy.MaxOrdersPerQuote {
		return nil, fmt.Errorf("%w: quote %s backs %d of at most %d", ErrQuoteFullyOrdered, d.Quote.QuoteID, ordersOfQuote, d.Policy.MaxOrdersPerQuote)
	}
	o := &CustomerOrder{state: s}
	if o.lines, err = applyLineChanges(s.Lines, s.RecordedAt, nil); err != nil {
		return nil, err
	}
	o.state.Transport, o.state.InitialParties = Transport{}, nil
	if !d.Transport.valid() {
		return nil, fmt.Errorf("%w: transport %+v", ErrInvalidOrder, d.Transport)
	}
	if d.Transport != (Transport{}) {
		o.state.TransportVersions = []TransportVersion{{Transport: d.Transport, Version: 1, ChangedBy: d.RecordedBy, ChangedAt: at.UTC(), EnteredAt: entered.UTC()}}
	}
	o.record(eventOrderRecorded, at, nil, "")
	parties := map[string]OrderParty{}
	for _, p := range d.InitialParties {
		if _, twice := parties[p.Role]; twice {
			return nil, fmt.Errorf("%w: %s given twice", ErrInvalidParty, p.Role)
		}
		p.Version, p.SetBy, p.SetAt, p.EnteredAt = 1, d.RecordedBy, at.UTC(), entered.UTC()
		if p, err = p.normalized(); err != nil {
			return nil, err
		}
		parties[p.Role] = p
		o.state.Parties = append(o.state.Parties, p)
	}
	if err = partyRules(d.Transport, parties); err != nil {
		return nil, err
	}
	for _, p := range o.state.Parties {
		o.recordParty(p, at)
	}
	return o, nil
}

func (s OrderState) validateCommitment() error {
	q, c := s.Quote, s.Customer
	switch {
	case blank(string(s.TenantID)), blank(string(s.OrderID)), blank(s.RecordedBy), s.RecordedAt.IsZero():
		return ErrInvalidOrder
	case blank(q.QuoteID), q.QuoteRevision < 1, blank(string(q.LegalEntityID)), blank(q.CustomerPartyID), q.CustomerRevision < 1,
		!orderingRole(q.RecipientRole), !q.Total.Currency.Valid(), q.Total.Minor <= 0, !q.Validity.Valid(), q.Validity.Until.IsZero(),
		!q.Validity.Contains(q.AcceptedAt), blank(q.EvidenceRef), !hexDigest(q.TermsHash),
		q.TermsContract != QuoteSnapshotV1 && q.TermsContract != QuoteSnapshotV2:
		return fmt.Errorf("%w: accepted quote", ErrInvalidOrder)
	case q.TermsContract == QuoteSnapshotV1 && (q.RecipientRole != RoleCustomer || q.Terms != CommercialTerms{}):
		return fmt.Errorf("%w: a QuoteSnapshot v1 has no recipient role or terms", ErrInvalidOrder)
	case blank(c.PartyID), c.PartyRevision < 1, !text(c.LegalName), !countryCode(c.CountryCode), !c.RoleValidity.Valid(),
		blank(c.RoleEvidenceRef):
		return fmt.Errorf("%w: ordering party snapshot", ErrInvalidOrder)
	case c.PartyID != q.CustomerPartyID:
		return fmt.Errorf("%w: the ordering party is not the quote's recipient", ErrQuoteMismatch)
	case c.RoleCode != q.RecipientRole:
		return fmt.Errorf("%w: ORG confirmed role %s, the quote went to %s", ErrQuoteMismatch, c.RoleCode, q.RecipientRole)
	case c.PartyRevision < q.CustomerRevision:
		return fmt.Errorf("%w: ORG revision %d is older than %d, the revision at acceptance", ErrQuoteMismatch, c.PartyRevision, q.CustomerRevision)
	case !c.RoleValidity.Contains(s.RecordedAt):
		return fmt.Errorf("%w: the ordering role was not in force when the order was recorded", ErrInvalidOrder)
	case !s.Policy.valid():
		return fmt.Errorf("%w: order policy", ErrInvalidOrder)
	case s.Policy.Scope == PolicyLegalEntity && s.Policy.LegalEntityID != q.LegalEntityID:
		return fmt.Errorf("%w: the order policy belongs to legal entity %s", ErrInvalidOrder, s.Policy.LegalEntityID)
	case s.RecordedAt.Before(q.AcceptedAt) || s.RecordedAt.Before(q.Validity.From) || !s.RecordedAt.Before(s.Policy.lastInstant(q.Validity.Until)):
		return ErrOutsideQuoteValidity
	case !s.Terms.valid() || s.Terms.QuoteTerms() != q.Terms:
		return fmt.Errorf("%w: commercial terms", ErrInvalidOrder)
	case len(s.Lines) == 0:
		return fmt.Errorf("%w: no lines", ErrInvalidOrder)
	}
	for i, l := range s.Lines {
		if l.LineNo != i+1 || !l.valid() {
			return fmt.Errorf("%w: line %d", ErrInvalidOrder, i+1)
		}
	}
	return nil
}

// RestoreOrder rebuilds a stored order without emitting events.
func RestoreOrder(s OrderState) (*CustomerOrder, error) {
	switch s.Status {
	case OrderRecorded, OrderPartiallyAllocated, OrderAllocated, OrderCancelled, OrderClosed:
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
	s.LineChanges = append([]LineVersion(nil), s.LineChanges...)
	s.Obligations = append([]Obligation(nil), s.Obligations...)
	s.Reservations = append([]Reservation(nil), s.Reservations...)
	s.Retention, s.Erasure = copyOf(s.Retention), copyOf(s.Erasure)
	lines, err := applyLineChanges(s.Lines, s.RecordedAt, s.LineChanges)
	if err != nil {
		return nil, err
	}
	o := &CustomerOrder{state: s, lines: lines, storedChanges: len(s.LineChanges), stored: len(s.Obligations), reserved: len(s.Reservations),
		retained: s.Retention != nil, erased: s.Erasure != nil}
	if err := o.validateObligations(); err != nil {
		return nil, err
	}
	if err := o.validateReservations(); err != nil {
		return nil, err
	}
	if err := o.validateRetention(); err != nil {
		return nil, err
	}
	if derived := o.derivedStatus(); s.Status != derived {
		return nil, fmt.Errorf("%w: status %s, but the obligations make it %s", ErrInvalidOrder, s.Status, derived)
	}
	if err := o.restoreParties(); err != nil {
		return nil, err
	}
	return o, nil
}

// restoreParties checks the stored transport and party versions: numbered from 1 without gaps, each valid on
// its own, and the current ones fitting the current transport.
func (o *CustomerOrder) restoreParties() error {
	s := &o.state
	s.TransportVersions = append([]TransportVersion(nil), s.TransportVersions...)
	s.Parties = append([]OrderParty(nil), s.Parties...)
	for i, v := range s.TransportVersions {
		if v.Version != int64(i+1) || !v.valid() || v.Transport == (Transport{}) || blank(v.ChangedBy) || v.ChangedAt.IsZero() || (i > 0 && blank(v.Reason)) {
			return fmt.Errorf("%w: transport version %d", ErrInvalidOrder, i+1)
		}
	}
	next := map[string]int64{}
	for i, p := range s.Parties {
		next[p.Role]++
		if p.Version != next[p.Role] || blank(p.SetBy) || p.SetAt.IsZero() || (p.Version > 1 && blank(p.Reason)) {
			return fmt.Errorf("%w: %s version %d", ErrInvalidOrder, p.Role, p.Version)
		}
		normal, err := p.normalized()
		if err != nil {
			return fmt.Errorf("%w: stored party: %v", ErrInvalidOrder, err)
		}
		s.Parties[i] = normal
	}
	if err := partyRules(o.Transport(), o.currentParties()); err != nil {
		return fmt.Errorf("%w: stored parties: %v", ErrInvalidOrder, err)
	}
	o.storedParties, o.storedTransport = len(s.Parties), len(s.TransportVersions)
	return nil
}

// State returns a copy of the order for persistence and read models.
func (o *CustomerOrder) State() OrderState {
	s := o.state
	s.Lines = append([]OrderLine(nil), s.Lines...)
	s.LineChanges = append([]LineVersion(nil), s.LineChanges...)
	s.Obligations = append([]Obligation(nil), s.Obligations...)
	s.Reservations = append([]Reservation(nil), s.Reservations...)
	s.TransportVersions = append([]TransportVersion(nil), s.TransportVersions...)
	s.Parties = append([]OrderParty(nil), s.Parties...)
	s.Retention, s.Erasure = copyOf(s.Retention), copyOf(s.Erasure)
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
	o.pending, o.storedChanges, o.stored, o.reserved, o.released = nil, len(o.state.LineChanges), len(o.state.Obligations), len(o.state.Reservations), nil
	o.storedParties, o.storedTransport = len(o.state.Parties), len(o.state.TransportVersions)
	o.retained, o.erased = o.state.Retention != nil, o.state.Erasure != nil
}

func (o *CustomerOrder) event(kind string, at time.Time) Event {
	return Event{Type: kind, At: at.UTC(), TenantID: o.state.TenantID, SubjectID: string(o.state.OrderID)}
}

func (o *CustomerOrder) record(kind string, at time.Time, ob *Obligation, shipment string) {
	e := o.event(kind, at)
	e.ShipmentID = shipment
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
