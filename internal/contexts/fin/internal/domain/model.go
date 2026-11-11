// Package domain is the model of FIN (Freight Job Economics) V0.1, the first part of N1 step 8: each legal
// entity's finance policy (functional currency, book and exchange rates), the freight job of a house shipment with
// its estimated charges from the accepted quote or entered by hand, and the credit accounts of debtors with their
// manual holds (C7). It depends on the standard library and the kernel only.
package domain

import (
	"errors"
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
	ErrBasisApplied      = errors.New("fin: FIN_BASIS_APPLIED: the quote is applied to the order on a job already; another job of the order takes manual lines")
	ErrNotPriceable      = errors.New("fin: FIN_NOT_PRICEABLE: the quote does not back an order of the shipment for the job's legal entity and customer")
	ErrNotOpenable       = errors.New("fin: FIN_NOT_OPENABLE: the shipment cannot have a job of the legal entity")
	ErrPartyRole         = errors.New("fin: FIN_PARTY_ROLE: ORG does not confirm the party in a role the side needs for the legal entity")
	ErrSideVersion       = errors.New("fin: the side's version is not the one expected")
	ErrRateInUse         = errors.New("fin: FIN_RATE_IN_USE: a charge was converted at the entry at or after the new end")
)

const maxText = 500

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxText }

func identifier(s string) bool {
	return !blank(s) && s == strings.TrimSpace(s) && utf8.RuneCountInString(s) <= 128
}

// ValidChargeCode checks a charge code's form; RATE's catalogue (RB12) will list them.
func ValidChargeCode(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= 64 }

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
}

// MaxAmount bounds every amount and local amount in minor units (10^15, ten trillion units of a two-digit
// currency), so that sums over a job's lines or a debtor's jobs stay far from overflowing (it would take some 9,000
// sides at the bound).
const MaxAmount = 1_000_000_000_000_000
