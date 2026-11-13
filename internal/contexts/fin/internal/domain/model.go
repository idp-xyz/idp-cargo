// Package domain is the model of FIN (Freight Job Economics) V0.2, N1 step 8: each legal entity's finance policy
// (functional currency, book, exchange rates, charge tax treatments and number series), the freight job of a house
// shipment with its estimated charges from the accepted quote or entered by hand and the debit and credit notes that
// bill them, debtors' billing profiles, and the credit accounts of debtors with their manual holds (C7). It depends
// on the standard library and the kernel only.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type TenantID string

var (
	ErrInvalid           = errors.New("fin: invalid")
	ErrInvalidTransition = errors.New("fin: invalid state transition")
	ErrConflict          = errors.New("fin: concurrent revision conflict")
	ErrRateMissing       = errors.New("fin: FIN_RATE_MISSING: no exchange rate of the type in force converts the currency into the functional currency; record one first")
	ErrRateOverlap       = errors.New("fin: FIN_RATE_OVERLAP: an exchange rate of the type and currency is in force for part of the period already")
	ErrBasisApplied      = errors.New("fin: FIN_BASIS_APPLIED: the basis is applied already, or the job or pool is priced otherwise: a quote prices an order on one job, a job is priced by its quotes or by one rated estimate, a pool by one rated estimate")
	ErrNotPriceable      = errors.New("fin: FIN_NOT_PRICEABLE: the basis does not price the job or pool: a quote backs no order of the shipment for the job's legal entity and customer, or RATE rated nothing for it")
	ErrNotOpenable       = errors.New("fin: FIN_NOT_OPENABLE: the shipment cannot have a job of the legal entity")
	ErrPartyRole         = errors.New("fin: FIN_PARTY_ROLE: ORG does not confirm the party in a role the side needs for the legal entity")
	ErrSideVersion       = errors.New("fin: the side's version is not the one expected")
	ErrRateInUse         = errors.New("fin: FIN_RATE_IN_USE: a charge was converted at the entry at or after the new end")
	ErrInvoiced          = errors.New("fin: FIN_INVOICED: the sell side is on an issued debit note; a credit note returns it first")
	ErrNoIssuedDocument  = errors.New("fin: FIN_NO_ISSUED_DOCUMENT: DOC reports no issued house document for the shipment; debit notes bill issued documents only")
	ErrNoBillingProfile  = errors.New("fin: FIN_NO_BILLING_PROFILE: the debtor has no billing profile; set one first")
	ErrProfileStale      = errors.New("fin: FIN_BILLING_PROFILE_STALE: ORG's name or taxpayer number of the debtor differs from its billing profile; set a new version")
	ErrTaxMissing        = errors.New("fin: FIN_TAX_TREATMENT_MISSING: the legal entity has no tax treatment of the charge code in force at the invoice date")
	ErrTaxInUse          = errors.New("fin: FIN_TAX_TREATMENT_IN_USE: a debit note of the legal entity bills the charge code at or after the date")
	ErrNoSeries          = errors.New("fin: FIN_NO_SERIES: the legal entity has no number series of the kind; register one first")
	ErrSeriesExhausted   = errors.New("fin: FIN_SERIES_EXHAUSTED: the number series has no number left")
	ErrSettled           = errors.New("fin: FIN_SETTLED: receipts settle more of the debit note than the credit note would leave on it; reverse the allocation first")
	ErrOverAllocated     = errors.New("fin: FIN_OVER_ALLOCATED: the amount is more than the receipt or payment has unallocated or the note or invoice has open")
	ErrNotAllocatable    = errors.New("fin: FIN_NOT_ALLOCATABLE: not an issued debit note of the receipt's legal entity, debtor and currency")
	ErrEvidenceUsed      = errors.New("fin: FIN_EVIDENCE_USED: a recorded receipt or payment of the legal entity has this evidence reference already")
)

const maxText = 500

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxText }

func identifier(s string) bool {
	return !blank(s) && s == strings.TrimSpace(s) && utf8.RuneCountInString(s) <= 128
}

// ValidChargeCode checks a charge code's form; RATE's catalogue (RB12) will list them.
func ValidChargeCode(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= 64 }

// ParseDate reads a civil date (YYYY-MM-DD) as midnight UTC.
func ParseDate(s string) (time.Time, error) {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: date %q is not YYYY-MM-DD", ErrInvalid, s)
	}
	return d, nil
}

// date reports whether t is a civil date: midnight UTC.
func date(t time.Time) bool { return !t.IsZero() && t.Equal(t.UTC().Truncate(24*time.Hour)) }

// dateOf reports whether a civil date is the date of an instant in some time zone (UTC-12 to UTC+14): FIN keeps no
// time zone of the legal entity, so a date the operator gives is checked only against the instant it was given at.
func dateOf(d, at time.Time) bool {
	return date(d) && !at.Before(d.Add(-14*time.Hour)) && at.Before(d.Add(36*time.Hour))
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Event is a FIN integration event: identifiers and versions only, never amounts, names or tax data.
type Event struct {
	Type          string
	At            time.Time
	TenantID      TenantID
	SubjectID     string
	LegalEntityID string
	Version       int
	ShipmentID    string
	DebtorPartyID string
	LineNo        int    // a charge line's number, for line and side events
	Side          string // SELL or COST, for side events
	JobID         string // the job of a debit or credit note, for note events
	InvoiceID     string // the debit note of an allocation, for allocation events
	// CreditorPartyID is the creditor of a creditor invoice, for its events.
	CreditorPartyID string
}

// MaxAmount bounds every amount and local amount in minor units (10^15, ten trillion units of a two-digit
// currency), so that sums over a job's lines or a debtor's jobs stay far from overflowing (it would take some 9,000
// sides at the bound).
const MaxAmount = 1_000_000_000_000_000
