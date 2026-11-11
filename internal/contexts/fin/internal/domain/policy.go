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
// the functional currency, in [ValidFrom, ValidUntil) (an open end when ValidUntil is zero). Immutable.
type RateEntry struct {
	No         int
	Type       string
	From       kernel.CurrencyCode
	Rate       int64
	Scale      int
	FromDigits int
	ToDigits   int
	ValidFrom  time.Time
	ValidUntil time.Time
	Source     string
	By         string
	At         time.Time
	EnteredAt  time.Time
}

func (e RateEntry) covers(at time.Time) bool {
	return !at.Before(e.ValidFrom) && (e.ValidUntil.IsZero() || at.Before(e.ValidUntil))
}

func (e RateEntry) overlaps(o RateEntry) bool {
	return (o.ValidUntil.IsZero() || e.ValidFrom.Before(o.ValidUntil)) && (e.ValidUntil.IsZero() || o.ValidFrom.Before(e.ValidUntil))
}

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
	pending []Event
}

const (
	eventPolicyOpened = "FinancePolicyOpened.v1"
	eventRateRecorded = "ExchangeRateRecorded.v1"
)

// OpenPolicy opens a legal entity's policy with its functional currency and book code.
func OpenPolicy(tenant TenantID, legalEntity string, currency kernel.CurrencyCode, book, by string, at time.Time) (*FinancePolicy, error) {
	if tenant == "" || !identifier(legalEntity) || !currency.Valid() || !identifier(book) || len(book) > 32 || blank(by) || at.IsZero() {
		return nil, fmt.Errorf("%w: a policy names its legal entity, functional currency and book", ErrInvalid)
	}
	p := &FinancePolicy{state: PolicyState{TenantID: tenant, LegalEntityID: legalEntity, FunctionalCurrency: currency, BookCode: book, OpenedBy: by,
		OpenedAt: at.UTC()}}
	p.record(eventPolicyOpened, at, 0)
	return p, nil
}

// RecordRate adds a rate entry; one of the same type and currency may not be in force for any part of its period.
func (p *FinancePolicy) RecordRate(e RateEntry, by string, at time.Time) error {
	st := &p.state
	if !validRateType(e.Type) || !e.From.Valid() || e.From == st.FunctionalCurrency || e.Rate <= 0 || e.Scale < 0 || e.Scale > 12 ||
		e.FromDigits < 0 || e.FromDigits > 4 || e.ToDigits < 0 || e.ToDigits > 4 || e.ValidFrom.IsZero() ||
		(!e.ValidUntil.IsZero() && !e.ValidUntil.After(e.ValidFrom)) || !text(e.Source) || blank(by) || at.IsZero() {
		return fmt.Errorf("%w: a rate names its type (ESTIMATE, INVOICE, ACCOUNTING), another currency, a positive rate with its scale and digits, "+
			"a period and its source", ErrInvalid)
	}
	for _, o := range st.Rates {
		if o.ToDigits != e.ToDigits {
			return fmt.Errorf("%w: rates disagree on %s's minor digits", ErrInvalid, st.FunctionalCurrency)
		}
		if o.Type == e.Type && o.From == e.From && o.overlaps(e) {
			return fmt.Errorf("%w: entry %d", ErrRateOverlap, o.No)
		}
	}
	e.No, e.By, e.At, e.ValidFrom = len(st.Rates)+1, by, at.UTC(), e.ValidFrom.UTC()
	if !e.ValidUntil.IsZero() {
		e.ValidUntil = e.ValidUntil.UTC()
	}
	st.Rates = append(st.Rates, e)
	p.record(eventRateRecorded, at, e.No)
	return nil
}

// Local converts an amount into the functional currency at the rate of the type in force at an instant; an amount
// in the functional currency needs none.
func (p *FinancePolicy) Local(m kernel.Money, rateType string, at time.Time) (int64, *FxSnapshot, error) {
	if !m.Currency.Valid() {
		return 0, nil, fmt.Errorf("%w: currency %q", ErrInvalid, m.Currency)
	}
	if m.Currency == p.state.FunctionalCurrency {
		return m.Minor, nil, nil
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
		if p.state.Rates[i].EnteredAt.IsZero() {
			p.state.Rates[i].EnteredAt = entered.UTC()
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

func (p *FinancePolicy) Acknowledge() { p.pending, p.stored = nil, len(p.state.Rates) }
