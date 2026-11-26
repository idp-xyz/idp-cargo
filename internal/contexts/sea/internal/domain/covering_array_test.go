package domain

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The combination tests read the pairwise covering array and the refusal convention from the foundation's test data
// (internal/foundation/settings/settingstest/testdata), because a domain package imports nothing but the standard library and
// the kernel, tests included (tools/dependency-check). The foundation's tests check that the array is what the generator
// writes and that it covers every valid pair.

const settingstestData = "../../../../foundation/settings/settingstest/testdata/"

// config is one configuration by item key; F1's threshold is a level of its own, as the array lists it.
type config map[string]string

type arrayFactor struct {
	name   string
	levels []string
}

const (
	thresholdLevelLow  = "NOT_ABOVE_THRESHOLD@LOW"
	thresholdLevelHigh = "NOT_ABOVE_THRESHOLD@HIGH"
	thresholdLow       = int64(10_000)  // below box A's former share of CNY 500.00
	thresholdHigh      = int64(100_000) // above it
)

func (c config) thresholdOf() int64 {
	switch c["fin.F1"] {
	case thresholdLevelLow:
		return thresholdLow
	case thresholdLevelHigh:
		return thresholdHigh
	}
	return 0
}

func (c config) f1Value() string {
	if c.thresholdOf() > 0 {
		return "NOT_ABOVE_THRESHOLD"
	}
	return c["fin.F1"]
}

func coveringArray(t testing.TB) ([]arrayFactor, []config) {
	t.Helper()
	raw, err := os.ReadFile(settingstestData + "covering_array.txt")
	if err != nil {
		t.Fatal(err)
	}
	var factors []arrayFactor
	var rows []config
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if rest, ok := strings.CutPrefix(line, "factor "); ok {
			name, levels, _ := strings.Cut(rest, ": ")
			factors = append(factors, arrayFactor{name, strings.Split(levels, ",")})
			continue
		}
		c := config{}
		for _, kv := range strings.Fields(line) {
			k, v, _ := strings.Cut(kv, "=")
			c[k] = v
		}
		rows = append(rows, c)
	}
	if len(factors) == 0 || len(rows) == 0 {
		t.Fatal("the covering array is empty")
	}
	return factors, rows
}

var kindPattern, wayOutPattern = func() (*regexp.Regexp, *regexp.Regexp) {
	raw, err := os.ReadFile(settingstestData + "refusal.txt")
	if err != nil {
		panic(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	return regexp.MustCompile(lines[0]), regexp.MustCompile(lines[1])
}()

// refusalConvention answers nil for a refusal that has a kind and a way out, as settingstest.Check does.
func refusalConvention(message string) error {
	if !kindPattern.MatchString(message) {
		return fmt.Errorf("no kind (a code such as FIN_ONE_STEP_REFUSED or a prefix such as fin:): %q", message)
	}
	if !wayOutPattern.MatchString(message) {
		return fmt.Errorf("no way out (an imperative clause after the cause): %q", message)
	}
	return nil
}
