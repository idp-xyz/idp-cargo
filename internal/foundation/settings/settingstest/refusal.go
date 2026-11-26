package settingstest

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
)

// The refusal convention of FIN and SEA, which every refusal of a rule follows: the message names its cause with a kind
// ("fin: FIN_PREVIEW_STALE: ...", "settings: refused: SETTINGS_ORDER: ..." or "sea: ...") and says the way out, an
// imperative clause that follows the cause, set off by a semicolon, a colon, a comma, a parenthesis or "or" ("...; preview
// again and submit the new preview", "...: have another approver approve it", "...; or ask a configurer to set F4").
// A way out may also be said of the person who does it ("; another finance user submits", FIN_ATTRIBUTION_CONFIRMER). Check
// accepts a message that has both: a kind (an upper-case code of two words or more joined by underscores such as
// FIN_ONE_STEP_REFUSED, or the package prefix "fin:", "sea:", "settings:" or "org:") and, after the first clause, a verb from
// the list below that opens a clause, or "another/a <person> <verb>s". It does not judge whether the way out is right; the tests that name a way out say that.
//
// The checker is a heuristic by the form of the sentence, not an understanding of it. It cannot tell a listed verb used
// as a noun, or as part of the cause, from a way out: "fin: the pool is closed, cost cannot be moved" and "fin: refused,
// change" pass because "cost" and "change" are on the list and follow a comma, though neither says what to do. It
// does not read what the way out asks, so a wrong way out passes too. A message that has the form is accepted; the tests
// that name a way out ("review it from that Monday") are what hold a message to its way out.
//
//go:embed testdata/refusal.txt
var patterns string

var kindPattern, wayOutPattern = func() (*regexp.Regexp, *regexp.Regexp) {
	lines := strings.Split(strings.TrimSpace(patterns), "\n")
	return regexp.MustCompile(lines[0]), regexp.MustCompile(lines[1])
}()

// Check answers nil for a refusal message that follows the convention, and otherwise what it lacks. The two patterns are
// in testdata/refusal.txt, which the tests of the domain packages read too (a domain package imports nothing of the
// foundation, so they cannot import this one).
func Check(message string) error {
	if !kindPattern.MatchString(message) {
		return fmt.Errorf("no kind (a code such as FIN_ONE_STEP_REFUSED or a prefix such as fin:): %q", message)
	}
	if !wayOutPattern.MatchString(message) {
		return fmt.Errorf("no way out (an imperative clause after the cause): %q", message)
	}
	return nil
}
