// Package domain is the model of RATE (Rate, Quote & Commercial Rules): quotes whose lines may be priced
// in several currencies and are totalled in the quote's currency at frozen exchange rates, the reasons a
// quote needs a price approval, acceptance within the quote's validity with typed evidence, expiry, and
// the frozen accepted quote. It depends on the standard library and the kernel only.
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

// RecipientRole is the ORG partner role in which a party receives a quote (decision R7).
type RecipientRole string

const (
	RecipientCustomer      RecipientRole = "CUSTOMER"
	RecipientOverseasAgent RecipientRole = "OVERSEAS_AGENT"
	RecipientShipper       RecipientRole = "SHIPPER"
	RecipientConsignee     RecipientRole = "CONSIGNEE"
)

func (r RecipientRole) Valid() bool {
	switch r {
	case RecipientCustomer, RecipientOverseasAgent, RecipientShipper, RecipientConsignee:
		return true
	}
	return false
}

// EvidenceKind is how the customer accepted a quote (decision R5). EvidenceUnrecorded marks quotes
// accepted before evidence kinds were recorded; no new acceptance takes it.
type EvidenceKind string

const (
	EvidenceSignedDocument EvidenceKind = "SIGNED_DOCUMENT"
	EvidenceEmail          EvidenceKind = "EMAIL"
	EvidenceCustomerPortal EvidenceKind = "CUSTOMER_PORTAL"
	EvidenceUnrecorded     EvidenceKind = "UNRECORDED"
)

func (k EvidenceKind) acceptable() bool {
	return k == EvidenceSignedDocument || k == EvidenceEmail || k == EvidenceCustomerPortal
}

type QuoteStatus string

const (
	QuoteAwaitingApproval QuoteStatus = "AWAITING_APPROVAL"
	QuotePriced           QuoteStatus = "PRICED"
	QuoteAccepted         QuoteStatus = "ACCEPTED"
	QuoteExpired          QuoteStatus = "EXPIRED"
)

// ApprovalReason is why a quote waits for a price approval (decisions R2, R9).
type ApprovalReason string

const (
	ReasonBelowMarginFloor ApprovalReason = "BELOW_MARGIN_FLOOR"
	ReasonDiscount         ApprovalReason = "DISCOUNT"
)

var (
	ErrInvalidQuote      = errors.New("rate: invalid quote")
	ErrInvalidTransition = errors.New("rate: invalid state transition")
	ErrSelfApproval      = errors.New("rate: the quote's author may not approve its price")
	ErrOutsideValidity   = errors.New("rate: instant outside the quote's validity")
	ErrConflict          = errors.New("rate: concurrent revision conflict")
)

// Line is one priced charge (decisions R8, R9). Sell and cost each carry their currency. A zero sell
// amount is a charge included elsewhere; a negative one is a discount, which has no cost. Cost stays inside
// RATE; only sell amounts leave it.
type Line struct {
	ChargeCode ChargeCode
	Sell       kernel.Money
	Cost       kernel.Money
	// V0.5 part 3a: where the price comes from, how it was priced, and the catalogue version of the code (0 for a starter
	// code the tenant never changed; nil for a line priced before the catalogue); CodeUnlisted marks a free-text code of a
	// quote priced before the catalogue. They are left out of a command's request hash when unset, so that commands
	// recorded before V0.5 hash as they did.
	Source       LineSource `json:",omitzero"`
	Rated        *RatedRef  `json:",omitzero"`
	Basis        LineBasis  `json:",omitzero"`
	CodeVersion  *int       `json:",omitzero"`
	CodeUnlisted bool       `json:",omitzero"`
}

// Discount reports whether the line lowers the price.
func (l Line) Discount() bool { return l.Sell.Minor < 0 }

// PriceApproval records who authorized a quote that needs approval, and the reason. SelfApproved is set
// when the author approved it, which a designated role may do (decision R3).
type PriceApproval struct {
	ApprovedBy   string
	ApprovedAt   time.Time
	ReasonRef    string
	SelfApproved bool
}

// Acceptance records the customer's acceptance at AcceptedAt, the business time, with its evidence and the
// customer revision ORG confirmed then. RecordedAt is when RATE recorded it (decision T1); it is zero only
// for quotes accepted before recording times were kept.
type Acceptance struct {
	AcceptedBy       string
	AcceptedAt       time.Time
	RecordedAt       time.Time
	EvidenceKind     EvidenceKind
	EvidenceRef      string
	CustomerRevision int64
}

// Event carries identifiers only; the Outbox adapter maps it to a Bento envelope.
type Event struct {
	Type      string
	At        time.Time
	TenantID  TenantID
	SubjectID string
	Version   int // the charge code's or rate card's version; 0 for quote events
}

// QuoteDraft is what a new quote is priced from. Currency is the currency the quote totals in; lines in
// another currency need one of ExchangeRates.
type QuoteDraft struct {
	TenantID      TenantID
	QuoteID       QuoteID
	LegalEntityID LegalEntityID
	Customer      CustomerRef
	Recipient     RecipientRole
	Currency      kernel.CurrencyCode
	Lines         []Line
	ExchangeRates []ExchangeRate
	Terms         CommercialTerms
	Validity      kernel.ValidityInterval
	MarginFloorBP int // minimum margin in basis points of the sell total
	Author        string
	// V0.5 part 3a: the lane and cargo the quote is priced for (required with a rated line), and how far below the rated
	// sell, in basis points of it, an overridden line may sell before it needs approval (RATE_CARD_DISCOUNT).
	Scope               *QuoteScope
	DiscountThresholdBP int
	// V0.5 part 3b: the kind (empty reads as ONE_OFF), and for an AGREEMENT its customer card version and the margin
	// check of each of its lanes (OD-4).
	Kind            QuoteKind
	Agreement       *AgreementRef
	AgreementChecks []AgreementCheck
}

// QuoteState is a stored quote, for persistence.
type QuoteState struct {
	QuoteDraft
	Status     QuoteStatus
	Approval   *PriceApproval
	Acceptance *Acceptance
	ExpiredAt  time.Time // zero unless EXPIRED
}

// Quote is the aggregate root. Once accepted or expired it does not change.
type Quote struct {
	state   QuoteState
	pending []Event
}

// PriceQuote creates a quote. A margin below the floor or a discount line makes it wait for a price
// approval; otherwise it can be accepted at once.
func PriceQuote(d QuoteDraft, at time.Time) (*Quote, error) {
	if at.IsZero() {
		return nil, ErrInvalidQuote
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	if d.Terms.ServiceLevelUnlisted {
		return nil, fmt.Errorf("%w: a new quote takes a service level from the list", ErrInvalidQuote)
	}
	for _, l := range d.Lines {
		if l.CodeUnlisted || !ValidChargeCode(l.ChargeCode) {
			return nil, fmt.Errorf("%w: a new line takes a code of the catalogue, not %q", ErrInvalidQuote, l.ChargeCode)
		}
	}
	if !at.Before(d.Validity.Until) {
		return nil, fmt.Errorf("%w: validity has already ended", ErrInvalidQuote)
	}
	d.Lines = append([]Line(nil), d.Lines...)
	d.ExchangeRates = append([]ExchangeRate(nil), d.ExchangeRates...)
	d.AgreementChecks = append([]AgreementCheck(nil), d.AgreementChecks...)
	q := &Quote{state: QuoteState{QuoteDraft: d, Status: QuotePriced}}
	reasons, err := q.ApprovalReasons()
	if err != nil {
		return nil, err
	}
	if len(reasons) > 0 {
		q.state.Status = QuoteAwaitingApproval
	}
	q.record("QuotePriced.v1", at)
	return q, nil
}

func (d QuoteDraft) validate() error {
	switch {
	case d.kind() != KindOneOff && d.kind() != KindAgreement:
		return fmt.Errorf("%w: quote kind %q", ErrInvalidQuote, d.Kind)
	case d.kind() == KindOneOff && (d.Agreement != nil || len(d.AgreementChecks) > 0):
		return fmt.Errorf("%w: a ONE_OFF quote names no agreement", ErrInvalidQuote)
	case blank(string(d.TenantID)), blank(string(d.QuoteID)), blank(string(d.LegalEntityID)), blank(d.Customer.PartyID),
		d.Customer.PartyRevision < 1, !d.Recipient.Valid(), !d.Currency.Valid(), d.kind() == KindOneOff && len(d.Lines) == 0, blank(d.Author),
		d.MarginFloorBP < 0, d.MarginFloorBP > 10000, d.DiscountThresholdBP < 0, d.DiscountThresholdBP > 10000:
		return ErrInvalidQuote
	case !d.Validity.Valid() || d.Validity.Until.IsZero():
		return fmt.Errorf("%w: a quote needs a bounded validity", ErrInvalidQuote)
	}
	if err := d.Terms.validate(); err != nil {
		return err
	}
	if d.kind() == KindAgreement {
		if err := d.validateAgreement(); err != nil {
			return err
		}
		// the exchange rates convert the lanes' sell and buy rates for the floor check
		_, err := newConverter(d.Currency, d.ExchangeRates)
		return err
	}
	if d.Scope != nil {
		if err := d.Scope.validate(); err != nil {
			return err
		}
	}
	c, err := newConverter(d.Currency, d.ExchangeRates)
	if err != nil {
		return err
	}
	used := map[kernel.CurrencyCode]bool{}
	for _, l := range d.Lines {
		if blank(string(l.ChargeCode)) || !l.Sell.Currency.Valid() || !l.Cost.Currency.Valid() || l.Cost.Minor < 0 || (l.Discount() && l.Cost.Minor != 0) {
			return fmt.Errorf("%w: line %q", ErrInvalidQuote, l.ChargeCode)
		}
		if err := l.validateSource(d.Scope); err != nil {
			return err
		}
		for _, m := range []kernel.Money{l.Sell, l.Cost} {
			if _, err = c.convert(m); err != nil {
				return err
			}
			used[m.Currency] = true
		}
	}
	for _, r := range d.ExchangeRates {
		if !used[r.From] {
			return fmt.Errorf("%w: no line is in %s, the exchange rate is not used", ErrInvalidQuote, r.From)
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
	case QuoteAwaitingApproval, QuotePriced, QuoteAccepted, QuoteExpired:
	default:
		return nil, fmt.Errorf("%w: status %q", ErrInvalidQuote, s.Status)
	}
	s.Lines = append([]Line(nil), s.Lines...)
	s.ExchangeRates = append([]ExchangeRate(nil), s.ExchangeRates...)
	s.AgreementChecks = append([]AgreementCheck(nil), s.AgreementChecks...)
	q := &Quote{state: s}
	reasons, err := q.ApprovalReasons()
	if err != nil {
		return nil, err
	}
	needs := len(reasons) > 0
	a := s.Acceptance
	switch {
	case s.Status == QuoteAwaitingApproval && (s.Approval != nil || !needs):
		return nil, fmt.Errorf("%w: inconsistent approval state", ErrInvalidQuote)
	case (s.Status == QuoteAccepted) != (a != nil):
		return nil, fmt.Errorf("%w: inconsistent acceptance state", ErrInvalidQuote)
	case (s.Status == QuoteExpired) != !s.ExpiredAt.IsZero(), s.Status == QuoteExpired && s.ExpiredAt.Before(s.Validity.Until):
		return nil, fmt.Errorf("%w: inconsistent expiry", ErrInvalidQuote)
	case s.Status != QuoteAwaitingApproval && s.Status != QuoteExpired && needs && s.Approval == nil:
		return nil, fmt.Errorf("%w: a quote that needs approval has none", ErrInvalidQuote)
	case s.Approval != nil && s.Approval.SelfApproved != (s.Approval.ApprovedBy == s.Author):
		return nil, fmt.Errorf("%w: inconsistent self-approval flag", ErrInvalidQuote)
	case a != nil && (a.EvidenceKind != EvidenceUnrecorded && !a.EvidenceKind.acceptable() || (a.EvidenceKind == EvidenceUnrecorded) != a.RecordedAt.IsZero()):
		return nil, fmt.Errorf("%w: acceptance evidence or recording time", ErrInvalidQuote)
	}
	return q, nil
}

// ApprovePrice authorizes a quote that needs a price approval. The author may approve it only if
// mayApproveOwn says the author holds a role designated for that (decision R3); the approval then says so.
func (q *Quote) ApprovePrice(approver, reasonRef string, mayApproveOwn bool, at time.Time) error {
	if q.state.Status != QuoteAwaitingApproval || at.IsZero() {
		return ErrInvalidTransition
	}
	if blank(approver) || blank(reasonRef) {
		return ErrInvalidQuote
	}
	self := approver == q.state.Author
	if self && !mayApproveOwn {
		return ErrSelfApproval
	}
	q.state.Approval = &PriceApproval{ApprovedBy: approver, ApprovedAt: at.UTC(), ReasonRef: reasonRef, SelfApproved: self}
	q.state.Status = QuotePriced
	q.record("QuotePriceApproved.v1", at)
	return nil
}

// Accept records the customer's acceptance at the business time at, within the quote's validity, with
// typed evidence (decision R5). recordedAt is when RATE records it (decision T1). customerRevision is the
// party revision ORG confirmed at that instant; it cannot be older than the one the quote was priced for.
func (q *Quote) Accept(acceptedBy string, kind EvidenceKind, evidenceRef string, customerRevision int64, at, recordedAt time.Time) error {
	if q.state.Status != QuotePriced || at.IsZero() || recordedAt.IsZero() {
		return ErrInvalidTransition
	}
	if blank(acceptedBy) || !kind.acceptable() || blank(evidenceRef) || customerRevision < q.state.Customer.PartyRevision {
		return ErrInvalidQuote
	}
	if !q.state.Validity.Contains(at) {
		return ErrOutsideValidity
	}
	q.state.Acceptance = &Acceptance{AcceptedBy: acceptedBy, AcceptedAt: at.UTC(), RecordedAt: recordedAt.UTC(), EvidenceKind: kind,
		EvidenceRef: evidenceRef, CustomerRevision: customerRevision}
	q.state.Status = QuoteAccepted
	q.record("QuoteAccepted.v1", at)
	return nil
}

// Expire ends a quote that was not accepted once its validity has passed (decision R4). A scheduled job
// calls it; an expired quote can no longer be approved or accepted.
func (q *Quote) Expire(at time.Time) error {
	if (q.state.Status != QuotePriced && q.state.Status != QuoteAwaitingApproval) || at.IsZero() {
		return ErrInvalidTransition
	}
	if at.Before(q.state.Validity.Until) {
		return fmt.Errorf("%w: the quote is valid until %s", ErrInvalidTransition, q.state.Validity.Until.Format(time.RFC3339))
	}
	q.state.ExpiredAt, q.state.Status = at.UTC(), QuoteExpired
	q.record("QuoteExpired.v1", at)
	return nil
}

// Converted returns each line's sell amount in the quote currency, in line order.
func (q *Quote) Converted() ([]kernel.Money, error) {
	c, err := newConverter(q.state.Currency, q.state.ExchangeRates)
	if err != nil {
		return nil, err
	}
	out := make([]kernel.Money, len(q.state.Lines))
	for i, l := range q.state.Lines {
		if out[i], err = c.convert(l.Sell); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Totals returns the sell and cost totals in the quote currency, converting each line separately.
func (q *Quote) Totals() (sell, cost kernel.Money, err error) {
	c, err := newConverter(q.state.Currency, q.state.ExchangeRates)
	if err != nil {
		return kernel.Money{}, kernel.Money{}, err
	}
	sell, cost = kernel.Money{Currency: q.state.Currency}, kernel.Money{Currency: q.state.Currency}
	for _, l := range q.state.Lines {
		s, err := c.convert(l.Sell)
		if err == nil {
			sell, err = sell.Add(s)
		}
		if err != nil {
			return kernel.Money{}, kernel.Money{}, err
		}
		k, err := c.convert(l.Cost)
		if err == nil {
			cost, err = cost.Add(k)
		}
		if err != nil {
			return kernel.Money{}, kernel.Money{}, err
		}
	}
	if sell.Minor < 0 {
		return kernel.Money{}, kernel.Money{}, fmt.Errorf("%w: discounts exceed the charges", ErrInvalidQuote)
	}
	return sell, cost, nil
}

// ApprovalReasons returns why the quote needs a price approval: a margin below the floor, a discount line,
// or both; none means it can be accepted without one.
func (q *Quote) ApprovalReasons() ([]ApprovalReason, error) {
	if q.state.kind() == KindAgreement {
		return q.agreementReasons(), nil
	}
	below, err := q.BelowMarginFloor()
	if err != nil {
		return nil, err
	}
	var reasons []ApprovalReason
	if below {
		reasons = append(reasons, ReasonBelowMarginFloor)
	}
	for _, l := range q.state.Lines {
		if l.Discount() {
			reasons = append(reasons, ReasonDiscount)
			break
		}
	}
	for _, l := range q.state.Lines {
		if l.rateCardDiscount(q.state.DiscountThresholdBP) {
			reasons = append(reasons, ReasonRateCardDiscount)
			break
		}
	}
	return reasons, nil
}

// BelowMarginFloor reports whether (sell − cost) / sell is below the floor in the quote currency,
// compared exactly as (sell − cost) × 10000 < floor × sell.
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
	if s.Scope != nil {
		scope := *s.Scope
		s.Scope = &scope
	}
	s.ExchangeRates = append([]ExchangeRate(nil), s.ExchangeRates...)
	s.AgreementChecks = append([]AgreementCheck(nil), s.AgreementChecks...)
	if s.Agreement != nil {
		a := *s.Agreement
		s.Agreement = &a
	}
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
func (q *Quote) Kind() QuoteKind     { return q.state.kind() }
func (q *Quote) Author() string      { return q.state.Author }
func (q *Quote) Pending() []Event    { return append([]Event(nil), q.pending...) }

// Acknowledge is permitted only after the transaction holding the quote and its Outbox events commits.
func (q *Quote) Acknowledge() { q.pending = nil }

func (q *Quote) record(kind string, at time.Time) {
	q.pending = append(q.pending, Event{Type: kind, At: at.UTC(), TenantID: q.state.TenantID, SubjectID: string(q.state.QuoteID)})
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }
