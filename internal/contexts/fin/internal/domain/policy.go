package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
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

// Charge tax treatments (china_localization; the tax adviser confirms each charge code): international freight
// forwarding is exempt from VAT and takes ordinary fapiao only; domestic services are taxable at a rate; disbursements
// pass through, pending the tax adviser. A charged amount is what the debtor pays; the fapiao splits out the tax.
const (
	TaxExemptIntlFreight = "EXEMPT_INTL_FREIGHT_FORWARDING"
	TaxTaxable           = "TAXABLE"
	TaxPassThrough       = "PASS_THROUGH_DISBURSEMENT"
)

// TaxTreatment is an immutable version of a charge code's tax treatment in the legal entity, in force from a date
// until the code's next version; a TAXABLE one carries its VAT rate in basis points.
type TaxTreatment struct {
	No            int
	ChargeCode    string
	Treatment     string
	RateBP        int
	EffectiveFrom time.Time
	By            string
	At            time.Time
	EnteredAt     time.Time
}

// Note kinds, each numbered from its own series of the legal entity.
const (
	KindDebitNote  = "DEBIT_NOTE"
	KindCreditNote = "CREDIT_NOTE"
)

var seriesPrefix = regexp.MustCompile(`^[0-9A-Z][0-9A-Z-]{0,9}$`)

// Series is a legal entity's number series of a note kind (MD-28: the numbering rule may come from the platform, the
// numbers issued are FIN's): a prefix and a zero-padded sequence from Start, drawn in order as notes are issued and
// never reused. The policy fixes the series; the next number lives with the series row the notes draw from.
type Series struct {
	Kind      string
	Prefix    string
	Width     int
	Start     int64
	By        string
	At        time.Time
	EnteredAt time.Time
}

// Format writes the number of a sequence value of the series.
func (s Series) Format(seq int64) (string, error) {
	digits := strconv.FormatInt(seq, 10)
	if seq < s.Start || len(digits) > s.Width {
		return "", fmt.Errorf("%w: %s series %s at %d", ErrSeriesExhausted, s.Kind, s.Prefix, seq)
	}
	return s.Prefix + strings.Repeat("0", s.Width-len(digits)) + digits, nil
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
	Taxes              []TaxTreatment
	Series             []Series
}

// FinancePolicy is the aggregate root of a legal entity's finance settings: the functional currency and book,
// fixed once opened (UL-08), the exchange rate entries FIN converts with, the tax treatments of charge codes and
// the number series of notes.
type FinancePolicy struct {
	state   PolicyState
	stored  int
	taxes   int   // tax treatments stored
	series  int   // series stored
	ended   []int // entries ended since the policy was loaded
	pending []Event
}

const (
	eventPolicyOpened     = "FinancePolicyOpened.v1"
	eventRateRecorded     = "ExchangeRateRecorded.v1"
	eventRateEnded        = "ExchangeRateEnded.v1"
	eventTaxSet           = "ChargeTaxTreatmentSet.v1"
	eventSeriesRegistered = "InvoiceSeriesRegistered.v1"
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

// SetTaxTreatment records the next tax treatment version of a charge code, in force from a date after the code's
// latest version's. The database refuses it when a debit note of the legal entity bills the code at or after the date
// (FIN_TAX_TREATMENT_IN_USE).
func (p *FinancePolicy) SetTaxTreatment(t TaxTreatment, by string, at time.Time) error {
	switch {
	case !ValidChargeCode(t.ChargeCode) || !date(t.EffectiveFrom) || blank(by) || at.IsZero():
		return fmt.Errorf("%w: a tax treatment names its charge code and the date it is in force from", ErrInvalid)
	case t.Treatment == TaxTaxable && (t.RateBP < 1 || t.RateBP > 3000),
		t.Treatment != TaxTaxable && ((t.Treatment != TaxExemptIntlFreight && t.Treatment != TaxPassThrough) || t.RateBP != 0):
		return fmt.Errorf("%w: a treatment is EXEMPT_INTL_FREIGHT_FORWARDING, PASS_THROUGH_DISBURSEMENT or TAXABLE with a rate of 1 to 3000 "+
			"basis points", ErrInvalid)
	}
	for _, o := range p.state.Taxes {
		if o.ChargeCode == t.ChargeCode && !t.EffectiveFrom.After(o.EffectiveFrom) {
			return fmt.Errorf("%w: version %d of %s is in force from %s; a new version starts later", ErrInvalid, o.No, o.ChargeCode,
				o.EffectiveFrom.Format(time.DateOnly))
		}
	}
	t.No, t.By, t.At, t.EnteredAt = len(p.state.Taxes)+1, by, at.UTC(), time.Time{}
	p.state.Taxes = append(p.state.Taxes, t)
	p.record(eventTaxSet, at, t.No)
	return nil
}

// TaxOn returns the tax treatment of a charge code in force on a date.
func (p *FinancePolicy) TaxOn(code string, d time.Time) (TaxTreatment, bool) {
	var found TaxTreatment
	ok := false
	for _, t := range p.state.Taxes {
		if t.ChargeCode == code && !t.EffectiveFrom.After(d) && (!ok || t.EffectiveFrom.After(found.EffectiveFrom)) {
			found, ok = t, true
		}
	}
	return found, ok
}

// RegisterSeries fixes the legal entity's number series of a note kind, once: a prefix of one to ten capital letters,
// digits and hyphens (not starting with a hyphen), a width of four to ten digits, twenty characters at most, and the
// first number. The two kinds' prefixes never start one another, so their numbers never meet.
func (p *FinancePolicy) RegisterSeries(s Series, by string, at time.Time) error {
	switch {
	case s.Kind != KindDebitNote && s.Kind != KindCreditNote, !seriesPrefix.MatchString(s.Prefix), s.Width < 4 || s.Width > 10,
		len(s.Prefix)+s.Width > 20, s.Start < 1 || len(strconv.FormatInt(s.Start, 10)) > s.Width, blank(by), at.IsZero():
		return fmt.Errorf("%w: a series names DEBIT_NOTE or CREDIT_NOTE, a prefix of capital letters, digits and hyphens, a width of 4 to 10 "+
			"digits (20 characters in all) and a first number that fits", ErrInvalid)
	}
	for _, o := range p.state.Series {
		switch {
		case o.Kind == s.Kind:
			return fmt.Errorf("%w: the %s series is registered with prefix %s", ErrInvalidTransition, o.Kind, o.Prefix)
		case strings.HasPrefix(o.Prefix, s.Prefix) || strings.HasPrefix(s.Prefix, o.Prefix):
			return fmt.Errorf("%w: prefix %s and the %s prefix %s start one another", ErrInvalid, s.Prefix, o.Kind, o.Prefix)
		}
	}
	s.By, s.At, s.EnteredAt = by, at.UTC(), time.Time{}
	p.state.Series = append(p.state.Series, s)
	p.record(eventSeriesRegistered, at, len(p.state.Series))
	return nil
}

// SeriesOf returns the series of a note kind.
func (p *FinancePolicy) SeriesOf(kind string) (Series, bool) {
	for _, s := range p.state.Series {
		if s.Kind == kind {
			return s, true
		}
	}
	return Series{}, false
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
	for i := range p.state.Taxes {
		if p.state.Taxes[i].EnteredAt.IsZero() {
			p.state.Taxes[i].EnteredAt = entered.UTC()
		}
	}
	for i := range p.state.Series {
		if p.state.Series[i].EnteredAt.IsZero() {
			p.state.Series[i].EnteredAt = entered.UTC()
		}
	}
}

// RestorePolicy rebuilds a stored policy.
func RestorePolicy(st PolicyState) (*FinancePolicy, error) {
	st.Rates, st.Taxes, st.Series = append([]RateEntry(nil), st.Rates...), append([]TaxTreatment(nil), st.Taxes...), append([]Series(nil), st.Series...)
	for i, e := range st.Rates {
		if e.No != i+1 {
			return nil, fmt.Errorf("%w: rate entry %d", ErrInvalid, i+1)
		}
	}
	for i, t := range st.Taxes {
		if t.No != i+1 {
			return nil, fmt.Errorf("%w: tax treatment %d", ErrInvalid, i+1)
		}
	}
	return &FinancePolicy{state: st, stored: len(st.Rates), taxes: len(st.Taxes), series: len(st.Series)}, nil
}

func (p *FinancePolicy) State() PolicyState {
	st := p.state
	st.Rates, st.Taxes, st.Series = append([]RateEntry(nil), st.Rates...), append([]TaxTreatment(nil), st.Taxes...), append([]Series(nil), st.Series...)
	return st
}

// AddedTaxes returns the tax treatments set since the policy was loaded.
func (p *FinancePolicy) AddedTaxes() []TaxTreatment {
	return append([]TaxTreatment(nil), p.state.Taxes[p.taxes:]...)
}

// AddedSeries returns the series registered since the policy was loaded.
func (p *FinancePolicy) AddedSeries() []Series {
	return append([]Series(nil), p.state.Series[p.series:]...)
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

func (p *FinancePolicy) Acknowledge() {
	p.pending, p.stored, p.ended, p.taxes, p.series = nil, len(p.state.Rates), nil, len(p.state.Taxes), len(p.state.Series)
}
