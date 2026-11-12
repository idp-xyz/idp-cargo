package numbering

import (
	"errors"
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// Cargo's subset of the platform's templates: a literal prefix, date parts and one zero-padded sequence; the reset
// period follows the finest date part; a sequence wider than its width is exhausted.
func TestTemplates(t *testing.T) {
	for _, raw := range []string{"{seq:6}", "DN{seq:3}", "DN{seq:11}", "DN{seq}", "DN{seq:6}{seq:6}", "DN{rand:4}{seq:6}", "DN{seqx:6}", "DN{dd}{seq:6}",
		"DN{yyyy:2}{seq:6}", "DN{seq:6}}", "DN{unknown}{seq:6}", "DN-{hh}{seq:6}", "DN{mm}{seq:5}", "DNQ{q}-{seq:4}", "DN{seq:04}"} {
		if _, err := Parse(raw); !errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("%q: %v", raw, err)
		}
	}
	yearly, err := Parse("DN{yy}-{seq:5}")
	if err != nil {
		t.Fatal(err)
	}
	if yearly.Prefix() != "DN" || yearly.Width() != 5 || yearly.Period(day(2026, 12, 31)) != "2026" || yearly.Period(day(2027, 1, 1)) != "2027" {
		t.Fatalf("yearly: %q %d %q", yearly.Prefix(), yearly.Width(), yearly.Period(day(2026, 12, 31)))
	}
	if n, err := yearly.Render(42, day(2027, 1, 1)); err != nil || n != "DN27-00042" {
		t.Fatalf("render: %q %v", n, err)
	}
	if _, err := yearly.Render(100_000, day(2027, 1, 1)); !errors.Is(err, ErrExhausted) {
		t.Fatalf("past the width: %v", err)
	}
	if n, err := yearly.Longest(); err != nil || n != "DN99-99999" {
		t.Fatalf("longest: %q %v", n, err)
	}
	monthly, _ := Parse("SHA{yyyy}{mm}{seq:4}")
	if monthly.Period(day(2026, 11, 30)) != "202611" {
		t.Fatalf("monthly period %q", monthly.Period(day(2026, 11, 30)))
	}
	quarterly, _ := Parse("Q{yyyy}Q{q}-{seq:4}")
	if n, _ := quarterly.Render(7, day(2026, 11, 12)); n != "Q2026Q4-0007" || quarterly.Period(day(2026, 11, 12)) != "2026Q4" {
		t.Fatalf("quarterly %q", n)
	}
	continuous, _ := Parse("SHA{seq:7}")
	if continuous.Resets() || !yearly.Resets() || !monthly.Resets() {
		t.Fatal("resets")
	}
	if continuous.Period(day(2026, 11, 12)) != "" {
		t.Fatal("a continuous sequence has no period")
	}
	if n, _ := continuous.Render(1, day(2026, 11, 12)); n != "SHA0000001" {
		t.Fatalf("continuous %q", n)
	}
	if DocType("DEBIT_NOTE", "LE-1") != "DEBIT_NOTE/LE-1" {
		t.Fatal("doc type")
	}
}
