package domain

import (
	"fmt"
	"unicode/utf8"
)

// CommercialTerms are the Incoterms rule with its edition and named place, and the service level a quote
// is priced for (decision C8). They are optional on a quote; an accepted quote passes them to the order,
// where they may still be completed and are then frozen.
type CommercialTerms struct {
	IncotermsRule    string // such as FOB or DAP
	IncotermsVersion string // 2010 or 2020
	NamedPlace       string // such as "Shanghai port" for FOB
	ServiceLevel     string // the service level code agreed, such as STANDARD
}

var incoterms = map[string]map[string]bool{
	"2020": {"EXW": true, "FCA": true, "CPT": true, "CIP": true, "DAP": true, "DPU": true, "DDP": true, "FAS": true, "FOB": true, "CFR": true, "CIF": true},
	"2010": {"EXW": true, "FCA": true, "CPT": true, "CIP": true, "DAT": true, "DAP": true, "DDP": true, "FAS": true, "FOB": true, "CFR": true, "CIF": true},
}

// Empty reports whether no term is set.
func (t CommercialTerms) Empty() bool { return t == CommercialTerms{} }

// validate accepts no terms, or an Incoterms rule of its edition with a named place, and an optional
// service level; or a service level alone.
func (t CommercialTerms) validate() error {
	incoterm := t.IncotermsRule != "" || t.IncotermsVersion != "" || t.NamedPlace != ""
	switch {
	case incoterm && (!incoterms[t.IncotermsVersion][t.IncotermsRule] || blank(t.NamedPlace) || utf8.RuneCountInString(t.NamedPlace) > 200):
		return fmt.Errorf("%w: Incoterms %q %q at %q", ErrInvalidQuote, t.IncotermsRule, t.IncotermsVersion, t.NamedPlace)
	case t.ServiceLevel != "" && (blank(t.ServiceLevel) || utf8.RuneCountInString(t.ServiceLevel) > 64):
		return fmt.Errorf("%w: service level %q", ErrInvalidQuote, t.ServiceLevel)
	}
	return nil
}
