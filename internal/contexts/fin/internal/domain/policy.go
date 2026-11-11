package domain

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Rate types (UL FXSnapshot): never mixed, never a fallback for one another. SETTLEMENT stays with the ERP.
const (
	RateEstimate   = "ESTIMATE"
	RateInvoice    = "INVOICE"
	RateAccounting = "ACCOUNTING"
)

func validRateType(t string) bool {
	return t == RateEstimate || t == RateInvoice || t == RateAccounting
}

// RateEntry converts one currency into the functional currency: one unit of From is Rate × 10^−Scale units of
// the functional currency, in [ValidFrom, ValidUntil). Its rate never changes; its period may be ended earlier once,
// with a reason, to correct it (no use of it may lie at or after the new end).
type RateEntry struct {
	No           int
	Type         string
	From         kernel.CurrencyCode
	Rate         int64
	Scale        int
	FromDigits   int
	ToDigits     int
	ValidFrom    time.Time
	ValidUntil   time.Time
	Source       string
	By           string
	At           time.Time
	EnteredAt    time.Time
	EndedBy      string
	EndedAt      time.Time
	EndReason    string
	EndEnteredAt time.Time
}

func (e RateEntry) covers(at time.Time) bool {
	return !at.Before(e.ValidFrom) && at.Before(e.ValidUntil)
}

func (e RateEntry) overlaps(o RateEntry) bool {
	return e.ValidFrom.Before(o.ValidUntil) && o.ValidFrom.Before(e.ValidUntil)
}

// maxRatePeriod bounds an entry's period: rates are set per day, month or year (OD-5 B).
const maxRatePeriod = 366 * 24 * time.Hour

// FxSnapshot is the conversion a local amount was derived with, enough to recompute it.
type FxSnapshot struct {
	EntryNo    int
	Type       string
	Rate       int64
	Scale      int
	FromDigits int
	ToDigits   int
	Source     string
}

// PolicyState is a stored policy, for persistence.
type PolicyState struct {
	TenantID           TenantID
	LegalEntityID      string
	FunctionalCurrency kernel.CurrencyCode
	BookCode           string
	OpenedBy           string
	OpenedAt           time.Time
	EnteredAt          time.Time
	Rates              []RateEntry
}

// FinancePolicy is the aggregate root of a legal entity's finance settings: the functional currency and book,
// fixed once opened (UL-08), and the exchange rate entries FIN converts with.
type FinancePolicy struct {
	state   PolicyState
	stored  int
	ended   []int // entries ended since the policy was loaded
	pending []Event
}

const (
	eventPolicyOpened = "FinancePolicyOpened.v1"
	eventRateRecorded = "ExchangeRateRecorded.v1"
	eventRateEnded    = "ExchangeRateEnded.v1"
)

// OpenPolicy opens a legal entity's policy with its functional currency, whose minor digits the kernel must know,
// and book code.
func OpenPolicy(tenant TenantID, legalEntity string, currency kernel.CurrencyCode, book, by string, at time.Time) (*FinancePolicy, error) {
	_, known := kernel.MinorDigits(currency)
	if tenant == "" || !identifier(legalEntity) || !known || !identifier(book) || len(book) > 32 || blank(by) || at.IsZero() {
		return nil, fmt.Errorf("%w: a policy names its legal entity, a functional currency with known minor digits and its book", ErrInvalid)
	}
	p := &FinancePolicy{state: PolicyState{TenantID: tenant, LegalEntityID: legalEntity, FunctionalCurrency: currency, BookCode: book, OpenedBy: by,
		OpenedAt: at.UTC()}}
	p.record(eventPolicyOpened, at, 0)
	return p, nil
}

// RecordRate adds a rate entry for a period of at most a year; one of the same type and currency may not be in force
// for any part of it. Both currencies' minor digits are their ISO 4217 ones.
func (p *FinancePolicy) RecordRate(e RateEntry, by string, at time.Time) error {
	st := &p.state
	fromDigits, fromKnown := kernel.MinorDigits(e.From)
	toDigits, _ := kernel.MinorDigits(st.FunctionalCurrency)
	switch {
	case !validRateType(e.Type) || !fromKnown || e.From == st.FunctionalCurrency || e.Rate <= 0 || e.Scale < 0 || e.Scale > 12 || e.ValidFrom.IsZero() ||
		!e.ValidUntil.After(e.ValidFrom) || e.ValidUntil.Sub(e.ValidFrom) > maxRatePeriod || !text(e.Source) || blank(by) || at.IsZero():
		return fmt.Errorf("%w: a rate names its type (ESTIMATE, INVOICE, ACCOUNTING), another currency with known minor digits, a positive rate "+
			"with its scale, a period of at most a year and its source", ErrInvalid)
	case e.FromDigits != fromDigits || e.ToDigits != toDigits:
		return fmt.Errorf("%w: %s has %d minor digits and %s %d", ErrInvalid, e.From, fromDigits, st.FunctionalCurrency, toDigits)
	}
	for _, o := range st.Rates {
		if o.Type == e.Type && o.From == e.From && o.overlaps(e) {
			return fmt.Errorf("%w: entry %d", ErrRateOverlap, o.No)
		}
	}
	e.No, e.By, e.At, e.ValidFrom, e.ValidUntil = len(st.Rates)+1, by, at.UTC(), e.ValidFrom.UTC(), e.ValidUntil.UTC()
	e.EndedBy, e.EndedAt, e.EndReason, e.EndEnteredAt = "", time.Time{}, "", time.Time{}
	st.Rates = append(st.Rates, e)
	p.record(eventRateRecorded, at, e.No)
	return nil
}

// EndRate ends an entry's period earlier, once, with a reason, so that a corrected entry may follow: the new end lies
// after its start and before its current end. The database refuses the end when a charge was converted at the entry
// at or after it.
func (p *FinancePolicy) EndRate(no int, until time.Time, reason, by string, at time.Time) error {
	st := &p.state
	if no < 1 || no > len(st.Rates) || !text(reason) || blank(by) || at.IsZero() {
		return fmt.Errorf("%w: an end names an entry of the policy and a reason", ErrInvalid)
	}
	e := &st.Rates[no-1]
	switch {
	case !e.EndedAt.IsZero():
		return fmt.Errorf("%w: entry %d was ended on %s", ErrInvalidTransition, no, e.EndedAt.Format(time.RFC3339))
	case !until.After(e.ValidFrom) || !until.Before(e.ValidUntil):
		return fmt.Errorf("%w: the new end lies within the entry's period", ErrInvalid)
	}
	e.ValidUntil, e.EndedBy, e.EndedAt, e.EndReason = until.UTC(), by, at.UTC(), reason
	p.ended = append(p.ended, no)
	p.record(eventRateEnded, at, no)
	return nil
}

// Local converts an amount into the functional currency at the rate of the type in force at an instant; an amount
// in the functional currency needs none.
func (p *FinancePolicy) Local(m kernel.Money, rateType string, at time.Time) (int64, *FxSnapshot, error) {
	if !m.Currency.Valid() {
		return 0, nil, fmt.Errorf("%w: currency %q", ErrInvalid, m.Currency)
	}
	if m.Currency == p.state.FunctionalCurrency || m.Minor == 0 {
		return m.Minor, nil, nil // nothing to convert
	}
	for _, e := range p.state.Rates {
		if e.Type == rateType && e.From == m.Currency && e.covers(at) {
			local, err := kernel.ConvertMinor(m.Minor, e.Rate, e.Scale, e.FromDigits, e.ToDigits)
			if err != nil {
				return 0, nil, err
			}
			return local, &FxSnapshot{EntryNo: e.No, Type: e.Type, Rate: e.Rate, Scale: e.Scale, FromDigits: e.FromDigits, ToDigits: e.ToDigits,
				Source: e.Source}, nil
		}
	}
	return 0, nil, fmt.Errorf("%w: %s %s to %s at %s", ErrRateMissing, rateType, m.Currency, p.state.FunctionalCurrency, at.UTC().Format(time.RFC3339))
}

func (p *FinancePolicy) record(kind string, at time.Time, version int) {
	p.pending = append(p.pending, Event{Type: kind, At: at.UTC(), TenantID: p.state.TenantID, SubjectID: p.state.LegalEntityID,
		LegalEntityID: p.state.LegalEntityID, Version: version})
}

// Stamp records when FIN entered what was added since the policy was loaded (T1).
func (p *FinancePolicy) Stamp(entered time.Time) {
	if p.state.EnteredAt.IsZero() {
		p.state.EnteredAt = entered.UTC()
	}
	for i := range p.state.Rates {
		e := &p.state.Rates[i]
		if e.EnteredAt.IsZero() {
			e.EnteredAt = entered.UTC()
		}
		if !e.EndedAt.IsZero() && e.EndEnteredAt.IsZero() {
			e.EndEnteredAt = entered.UTC()
		}
	}
}

// RestorePolicy rebuilds a stored policy.
func RestorePolicy(st PolicyState) (*FinancePolicy, error) {
	st.Rates = append([]RateEntry(nil), st.Rates...)
	for i, e := range st.Rates {
		if e.No != i+1 {
			return nil, fmt.Errorf("%w: rate entry %d", ErrInvalid, i+1)
		}
	}
	return &FinancePolicy{state: st, stored: len(st.Rates)}, nil
}

func (p *FinancePolicy) State() PolicyState {
	st := p.state
	st.Rates = append([]RateEntry(nil), st.Rates...)
	return st
}

// AddedRates returns the entries recorded since the policy was loaded.
func (p *FinancePolicy) AddedRates() []RateEntry {
	return append([]RateEntry(nil), p.state.Rates[p.stored:]...)
}

func (p *FinancePolicy) Pending() []Event { return append([]Event(nil), p.pending...) }

// EndedRates returns the stored entries ended since the policy was loaded.
func (p *FinancePolicy) EndedRates() []RateEntry {
	var out []RateEntry
	for _, no := range p.ended {
		if no <= p.stored {
			out = append(out, p.state.Rates[no-1])
		}
	}
	return out
}

func (p *FinancePolicy) Acknowledge() { p.pending, p.stored, p.ended = nil, len(p.state.Rates), nil }
