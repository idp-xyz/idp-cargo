package domain

import (
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var ErrTermsMismatch = errors.New("com: the order's commercial terms contradict the accepted quote")

// CommercialTerms are the Incoterms rule with its edition and named place, and the service level (C8).
type CommercialTerms struct {
	IncotermsRule    string // such as FOB or DAP
	IncotermsVersion string // 2010 or 2020
	NamedPlace       string // such as "Shanghai port" for FOB
	ServiceLevel     string // a code of RATE's service-level list, such as STANDARD
}

// TermSource says where an order's term comes from (C8).
type TermSource string

const (
	TermFromQuote TermSource = "QUOTE" // the accepted quote agreed it
	TermFromOrder TermSource = "ORDER" // the quote left it open and the order completed it
)

// OrderTerms are the commercial terms an order freezes (C8). Each term the accepted quote agreed is taken
// over; a term the quote left open may be completed when the order is recorded. Afterwards they never
// change. A source is empty exactly when its term is absent.
type OrderTerms struct {
	CommercialTerms
	IncotermsSource    TermSource
	ServiceLevelSource TermSource
}

// serviceLevelCode is the form of a service-level code (C8); whether a code is listed is RATE's answer.
var serviceLevelCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,15}$`)

func (t CommercialTerms) incoterm() bool {
	return t.IncotermsRule != "" || t.IncotermsVersion != "" || t.NamedPlace != ""
}

func (t CommercialTerms) sameIncoterm(o CommercialTerms) bool {
	return t.IncotermsRule == o.IncotermsRule && t.IncotermsVersion == o.IncotermsVersion && t.NamedPlace == o.NamedPlace
}

func validIncoterm(t CommercialTerms) bool {
	return kernel.ValidIncoterm(t.IncotermsVersion, t.IncotermsRule) && !blank(t.NamedPlace) && utf8.RuneCountInString(t.NamedPlace) <= 200
}

// AgreeTerms combines the quote's terms with those given on the order. A term the quote agreed may be
// repeated but not changed; a term the quote left open is taken from the order. A service level given on the
// order must have the form of a code; the caller has RATE confirm that it is listed.
func AgreeTerms(quote, given CommercialTerms) (OrderTerms, error) {
	var agreed OrderTerms
	switch {
	case quote.incoterm() && given.incoterm() && !quote.sameIncoterm(given):
		return OrderTerms{}, fmt.Errorf("%w: the quote agreed Incoterms %s %s %q", ErrTermsMismatch, quote.IncotermsRule, quote.IncotermsVersion, quote.NamedPlace)
	case quote.incoterm():
		agreed.IncotermsRule, agreed.IncotermsVersion, agreed.NamedPlace, agreed.IncotermsSource = quote.IncotermsRule, quote.IncotermsVersion, quote.NamedPlace, TermFromQuote
	case given.incoterm():
		if !validIncoterm(given) {
			return OrderTerms{}, fmt.Errorf("%w: Incoterms %q %q at %q", ErrInvalidOrder, given.IncotermsRule, given.IncotermsVersion, given.NamedPlace)
		}
		agreed.IncotermsRule, agreed.IncotermsVersion, agreed.NamedPlace, agreed.IncotermsSource = given.IncotermsRule, given.IncotermsVersion, given.NamedPlace, TermFromOrder
	}
	switch {
	case quote.ServiceLevel != "" && given.ServiceLevel != "" && given.ServiceLevel != quote.ServiceLevel:
		return OrderTerms{}, fmt.Errorf("%w: the quote agreed service level %q", ErrTermsMismatch, quote.ServiceLevel)
	case quote.ServiceLevel != "":
		agreed.ServiceLevel, agreed.ServiceLevelSource = quote.ServiceLevel, TermFromQuote
	case given.ServiceLevel != "":
		if !serviceLevelCode.MatchString(given.ServiceLevel) {
			return OrderTerms{}, fmt.Errorf("%w: service level %q is not a code", ErrInvalidOrder, given.ServiceLevel)
		}
		agreed.ServiceLevel, agreed.ServiceLevelSource = given.ServiceLevel, TermFromOrder
	}
	return agreed, nil
}

// QuoteTerms returns the terms the accepted quote agreed, as far as the order shows them.
func (t OrderTerms) QuoteTerms() CommercialTerms {
	var q CommercialTerms
	if t.IncotermsSource == TermFromQuote {
		q.IncotermsRule, q.IncotermsVersion, q.NamedPlace = t.IncotermsRule, t.IncotermsVersion, t.NamedPlace
	}
	if t.ServiceLevelSource == TermFromQuote {
		q.ServiceLevel = t.ServiceLevel
	}
	return q
}

// valid checks stored terms. A service level the quote agreed may be the free text of a quote recorded
// before RATE's list existed; one completed on the order is a code.
func (t OrderTerms) valid() bool {
	source := func(s TermSource) bool { return s == TermFromQuote || s == TermFromOrder }
	incoterm := t.incoterm()
	switch {
	case incoterm != (t.IncotermsSource != ""), incoterm && (!source(t.IncotermsSource) || !validIncoterm(t.CommercialTerms)):
		return false
	case (t.ServiceLevel != "") != (t.ServiceLevelSource != ""), t.ServiceLevel != "" && !source(t.ServiceLevelSource):
		return false
	case t.ServiceLevelSource == TermFromQuote && (blank(t.ServiceLevel) || utf8.RuneCountInString(t.ServiceLevel) > 64):
		return false
	case t.ServiceLevelSource == TermFromOrder && !serviceLevelCode.MatchString(t.ServiceLevel):
		return false
	}
	return true
}
