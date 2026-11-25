package domain

import (
	"fmt"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.97: the settings FIN's rules read (step C1b-1 of the configuration of the rules); V2.98 adds F4, F8 and F9
// (step C1b-2). The application reads the settings in force for the pool's legal entity inside the command's
// transaction and hands the domain what the rules need, as RuleSettings; the domain does not know the ledger. The zero
// value is the standard profile, the rules as V2.96 had them, so that a pool with no settings set behaves as before.
// The values are those of FIN's catalogue.
const (
	F1Unrestricted          = "UNRESTRICTED"
	F1AboveThreshold        = "NOT_ABOVE_THRESHOLD"
	F2SubmitterAndPreviewer = "SUBMITTER_AND_PREVIEWER"
	F3OnlyWhenAmountMoves   = "ONLY_WHEN_AMOUNT_MOVES"
	F4OneStep               = "ONE_STEP_WITHOUT_OPENED_OR_LEFT"
	F6IncludedMarked        = "INCLUDED_MARKED"
	F7Blocks                = "BLOCKS"
	F8DefaultToRemaining    = "DEFAULT_TO_REMAINING"
	F9KeepRatio             = "KEEP_RATIO"
	F9Reconfirm             = "RECONFIRM"
)

// RuleSettings is what the rules of a pool read of the settings of its legal entity: the version in force (0 when no
// change set is in force), F1 with its threshold in the functional currency's minor units (read only under
// NOT_ABOVE_THRESHOLD), F2, F3, F4, F6, F7, F8 and F9. An empty value is the item's default.
type RuleSettings struct {
	Version     int
	F1          string
	F1Threshold int64
	F2          string
	F3          string
	F4          string
	F6          string
	F7          string
	F8          string
	F9          string
}

// confirmer is a person the confirmation rules are checked for, with how a refusal names them.
type confirmer struct {
	name  string
	label string
	hint  string
}

// confirmers are the people checked against the bars of F1: the submitter and, under F2 SUBMITTER_AND_PREVIEWER, the
// person who made the preview when that is another person.
func (r RuleSettings) confirmers(submitter, previewer string) []confirmer {
	out := []confirmer{{name: submitter, label: submitter}}
	if r.F2 == F2SubmitterAndPreviewer && previewer != "" && previewer != submitter {
		out = append(out, confirmer{name: previewer, label: "the previewer " + previewer,
			hint: "; the previewer is checked too under F2 SUBMITTER_AND_PREVIEWER: preview again as another finance user, who is not barred"})
	}
	return out
}

// bars tells whether the rules of F1 apply to an amount at risk, in the functional currency's minor units: always under
// NOT_ALLOWED, never under UNRESTRICTED, and under NOT_ABOVE_THRESHOLD only above the threshold (an amount equal to the
// threshold is not above it).
func (r RuleSettings) bars(local int64) bool {
	switch r.F1 {
	case F1Unrestricted:
		return false
	case F1AboveThreshold:
		return local > r.F1Threshold
	}
	return true
}

// aboveNote says why a bar applies, for a refusal: empty under NOT_ALLOWED.
func (r RuleSettings) aboveNote(currency kernel.CurrencyCode, local int64) string {
	if r.F1 != F1AboveThreshold {
		return ""
	}
	return fmt.Sprintf(" (%s is above the threshold of %s set by F1 NOT_ABOVE_THRESHOLD)", money(currency, local), money(currency, r.F1Threshold))
}
