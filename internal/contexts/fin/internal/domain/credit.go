package domain

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Credit account states and the CreditStatus v1 answers (MD-12: never a party status in ORG).
const (
	AccountActive = "ACTIVE"
	AccountOnHold = "ON_HOLD"

	CreditOK        = "OK"
	CreditOnHold    = "ON_HOLD"
	CreditNoAccount = "NO_ACCOUNT"
)

// Payment term bases and hold reasons (OD-8 A: every hold is placed by a person; the reason says why).
const (
	TermsFromInvoiceDate = "INVOICE_DATE"
	TermsFromMonthEnd    = "END_OF_MONTH"

	HoldManual    = "MANUAL"
	HoldOverLimit = "OVER_LIMIT"
	HoldOverdue   = "OVERDUE"
)

// TermsVersion is an immutable version of a debtor's credit terms: the limit in the functional currency and the
// payment terms.
type TermsVersion struct {
	No           int
	Limit        kernel.Money
	PaymentDays  int
	PaymentBasis string
	By           string
	At           time.Time
	EnteredAt    time.Time
}

// Hold is a credit hold, released once.
type Hold struct {
	No               int
	Code             string
	Reason           string
	By               string
	At               time.Time
	EnteredAt        time.Time
	ReleasedBy       string
	ReleasedAt       time.Time
	ReleaseReason    string
	ReleaseEnteredAt time.Time
}

// CreditState is a stored account, for persistence.
type CreditState struct {
	TenantID      TenantID
	LegalEntityID string
	DebtorPartyID string
	Currency      kernel.CurrencyCode
	State         string
	Terms         []TermsVersion
	Holds         []Hold
}

// CreditAccount is the aggregate root of a debtor's credit with one legal entity (C7, MD-12): terms and manual
// holds. A hold blocks the issue of transport documents in DOC, never quotes, orders, shipments or charges.
type CreditAccount struct {
	state    CreditState
	terms    int // terms versions stored
	holds    int // holds stored
	released int // holds stored released
	pending  []Event
}

const (
	eventTermsSet     = "CreditTermsSet.v1"
	eventHoldPlaced   = "CreditHoldPlaced.v1"
	eventHoldReleased = "CreditHoldReleased.v1"
)

// OpenAccount opens a debtor's account in the legal entity's functional currency; its first terms follow.
func OpenAccount(tenant TenantID, legalEntity, debtor string, currency kernel.CurrencyCode) (*CreditAccount, error) {
	if tenant == "" || !identifier(legalEntity) || !identifier(debtor) || !currency.Valid() {
		return nil, fmt.Errorf("%w: an account names its legal entity, debtor and currency", ErrInvalid)
	}
	return &CreditAccount{state: CreditState{TenantID: tenant, LegalEntityID: legalEntity, DebtorPartyID: debtor, Currency: currency,
		State: AccountActive}}, nil
}

func (a *CreditAccount) lastChange() time.Time {
	var last time.Time
	for _, t := range a.state.Terms {
		last = later(last, t.At)
	}
	for _, h := range a.state.Holds {
		last = later(later(last, h.At), h.ReleasedAt)
	}
	return last
}

func (a *CreditAccount) check(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(a.lastChange()) {
		return fmt.Errorf("%w: a change names its author and a time not before the account's last change", ErrInvalid)
	}
	return nil
}

// SetTerms records the next terms version: a limit in the account's currency, not negative, and payment days
// (0 to 365) from the invoice date or the end of its month.
func (a *CreditAccount) SetTerms(limit kernel.Money, days int, basis, by string, at time.Time) error {
	if err := a.check(by, at); err != nil {
		return err
	}
	if limit.Currency != a.state.Currency || limit.Minor < 0 || days < 0 || days > 365 || (basis != TermsFromInvoiceDate && basis != TermsFromMonthEnd) {
		return fmt.Errorf("%w: terms name a limit in %s, payment days and the basis INVOICE_DATE or END_OF_MONTH", ErrInvalid, a.state.Currency)
	}
	a.state.Terms = append(a.state.Terms, TermsVersion{No: len(a.state.Terms) + 1, Limit: limit, PaymentDays: days, PaymentBasis: basis, By: by, At: at.UTC()})
	a.record(eventTermsSet, at, len(a.state.Terms))
	return nil
}

// PlaceHold holds an ACTIVE account with a reason.
func (a *CreditAccount) PlaceHold(code, reason, by string, at time.Time) error {
	if err := a.check(by, at); err != nil {
		return err
	}
	switch {
	case a.state.State != AccountActive:
		return fmt.Errorf("%w: the account is %s", ErrInvalidTransition, a.state.State)
	case code != HoldManual && code != HoldOverLimit && code != HoldOverdue, !text(reason):
		return fmt.Errorf("%w: a hold names MANUAL, OVER_LIMIT or OVERDUE and a reason", ErrInvalid)
	}
	a.state.Holds = append(a.state.Holds, Hold{No: len(a.state.Holds) + 1, Code: code, Reason: reason, By: by, At: at.UTC()})
	a.state.State = AccountOnHold
	a.record(eventHoldPlaced, at, len(a.state.Holds))
	return nil
}

// ReleaseHold releases the hold of an ON_HOLD account with a reason.
func (a *CreditAccount) ReleaseHold(reason, by string, at time.Time) error {
	if err := a.check(by, at); err != nil {
		return err
	}
	if a.state.State != AccountOnHold {
		return fmt.Errorf("%w: the account is %s", ErrInvalidTransition, a.state.State)
	}
	if !text(reason) {
		return fmt.Errorf("%w: a release needs a reason", ErrInvalid)
	}
	h := &a.state.Holds[len(a.state.Holds)-1]
	h.ReleasedBy, h.ReleasedAt, h.ReleaseReason = by, at.UTC(), reason
	a.state.State = AccountActive
	a.record(eventHoldReleased, at, h.No)
	return nil
}

// Status is the CreditStatus v1 answer: OK or ON_HOLD.
func (a *CreditAccount) Status() string {
	if a.state.State == AccountOnHold {
		return CreditOnHold
	}
	return CreditOK
}

// CurrentTerms returns the terms in force, if any.
func (a *CreditAccount) CurrentTerms() (TermsVersion, bool) {
	if n := len(a.state.Terms); n > 0 {
		return a.state.Terms[n-1], true
	}
	return TermsVersion{}, false
}

// OpenHold returns the hold in force, if any.
func (a *CreditAccount) OpenHold() (Hold, bool) {
	if n := len(a.state.Holds); n > 0 && a.state.State == AccountOnHold {
		return a.state.Holds[n-1], true
	}
	return Hold{}, false
}

// SubjectID names the account in events and decisions.
func (a *CreditAccount) SubjectID() string {
	return a.state.LegalEntityID + "/" + a.state.DebtorPartyID
}

func (a *CreditAccount) record(kind string, at time.Time, version int) {
	a.pending = append(a.pending, Event{Type: kind, At: at.UTC(), TenantID: a.state.TenantID, SubjectID: a.SubjectID(),
		LegalEntityID: a.state.LegalEntityID, Version: version, DebtorPartyID: a.state.DebtorPartyID})
}

// Stamp records when FIN entered what was written since the account was loaded (T1).
func (a *CreditAccount) Stamp(entered time.Time) {
	e := entered.UTC()
	for i := range a.state.Terms {
		if a.state.Terms[i].EnteredAt.IsZero() {
			a.state.Terms[i].EnteredAt = e
		}
	}
	for i := range a.state.Holds {
		h := &a.state.Holds[i]
		if h.EnteredAt.IsZero() {
			h.EnteredAt = e
		}
		if !h.ReleasedAt.IsZero() && h.ReleaseEnteredAt.IsZero() {
			h.ReleaseEnteredAt = e
		}
	}
}

// RestoreAccount rebuilds a stored account.
func RestoreAccount(st CreditState) (*CreditAccount, error) {
	st.Terms, st.Holds = append([]TermsVersion(nil), st.Terms...), append([]Hold(nil), st.Holds...)
	open := false
	for i, h := range st.Holds {
		if h.No != i+1 || (h.ReleasedAt.IsZero() && i != len(st.Holds)-1) {
			return nil, fmt.Errorf("%w: hold %d", ErrInvalid, i+1)
		}
		open = h.ReleasedAt.IsZero()
	}
	if (st.State == AccountOnHold) != open {
		return nil, fmt.Errorf("%w: account state %s", ErrInvalid, st.State)
	}
	return &CreditAccount{state: st, terms: len(st.Terms), holds: len(st.Holds), released: releasedCount(st.Holds)}, nil
}

func releasedCount(holds []Hold) int {
	n := 0
	for _, h := range holds {
		if !h.ReleasedAt.IsZero() {
			n++
		}
	}
	return n
}

func (a *CreditAccount) State() CreditState {
	st := a.state
	st.Terms, st.Holds = append([]TermsVersion(nil), st.Terms...), append([]Hold(nil), st.Holds...)
	return st
}

// Added returns the terms versions and holds written since the account was loaded, and the stored holds released
// since (holds are released in order, the open one last).
func (a *CreditAccount) Added() (terms []TermsVersion, holds []Hold, released []Hold) {
	st := a.State()
	for i := a.released; i < a.holds && i < len(st.Holds); i++ {
		if !st.Holds[i].ReleasedAt.IsZero() {
			released = append(released, st.Holds[i])
		}
	}
	return st.Terms[a.terms:], st.Holds[a.holds:], released
}

func (a *CreditAccount) Pending() []Event { return append([]Event(nil), a.pending...) }

func (a *CreditAccount) Acknowledge() {
	a.pending, a.terms, a.holds, a.released = nil, len(a.state.Terms), len(a.state.Holds), releasedCount(a.state.Holds)
}
