package domain

import (
	"fmt"
	"regexp"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// CommercialTerms are the Incoterms rule with its edition and named place, and the service level a quote
// is priced for (decision C8). They are optional on a quote; an accepted quote passes them to the order,
// where they may still be completed and are then frozen.
type CommercialTerms struct {
	IncotermsRule    string // such as FOB or DAP
	IncotermsVersion string // 2010 or 2020
	NamedPlace       string // such as "Shanghai port" for FOB
	ServiceLevel     string // a code of the service-level list, such as STANDARD
	// ServiceLevelUnlisted marks a service level recorded as free text before the list existed (C8,
	// 2026-11-09). It is kept as recorded and never accepted on a new quote.
	ServiceLevelUnlisted bool
}

// ServiceLevel is an entry of RATE's service-level list: the commercial catalogue of service levels a
// quote may be priced for (C8).
type ServiceLevel struct {
	Code string
	Name string
}

// DefaultServiceLevels is the list S1 builds in for every tenant. Tenants maintain their own list before
// the pilot (E3); integrations map these codes, such as STANDARD to CargoWise's STD.
var DefaultServiceLevels = []ServiceLevel{
	{Code: "STANDARD", Name: "Standard"},
	{Code: "EXPRESS", Name: "Express"},
	{Code: "ECONOMY", Name: "Economy"},
	{Code: "DEFERRED", Name: "Deferred"},
}

// serviceLevelCode is the form of a service-level code: two to sixteen upper-case letters, digits or
// underscores, starting with a letter.
var serviceLevelCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,15}$`)

// ServiceLevelCode reports whether code has the form of a service-level code.
func ServiceLevelCode(code string) bool { return serviceLevelCode.MatchString(code) }

// ListedServiceLevel returns the entry of the list with the code.
func ListedServiceLevel(code string) (ServiceLevel, bool) {
	for _, l := range DefaultServiceLevels {
		if l.Code == code {
			return l, true
		}
	}
	return ServiceLevel{}, false
}

// Empty reports whether no term is set.
func (t CommercialTerms) Empty() bool { return t == CommercialTerms{} }

// validate accepts no terms, or an Incoterms rule of its edition with a named place, and an optional
// service level; or a service level alone. A service level is a listed code, or the free text of a quote
// recorded before the list existed.
func (t CommercialTerms) validate() error {
	incoterm := t.IncotermsRule != "" || t.IncotermsVersion != "" || t.NamedPlace != ""
	switch {
	case incoterm && (!kernel.ValidIncoterm(t.IncotermsVersion, t.IncotermsRule) || blank(t.NamedPlace) || utf8.RuneCountInString(t.NamedPlace) > 200):
		return fmt.Errorf("%w: Incoterms %q %q at %q", ErrInvalidQuote, t.IncotermsRule, t.IncotermsVersion, t.NamedPlace)
	case t.ServiceLevelUnlisted && (blank(t.ServiceLevel) || utf8.RuneCountInString(t.ServiceLevel) > 64):
		return fmt.Errorf("%w: unlisted service level %q", ErrInvalidQuote, t.ServiceLevel)
	case !t.ServiceLevelUnlisted && t.ServiceLevel != "":
		if _, listed := ListedServiceLevel(t.ServiceLevel); !listed {
			return fmt.Errorf("%w: service level %q is not on the service-level list", ErrInvalidQuote, t.ServiceLevel)
		}
	}
	return nil
}
