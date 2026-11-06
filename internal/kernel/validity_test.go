package kernel

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

func interval(from, until time.Duration, open bool) ValidityInterval {
	v := ValidityInterval{From: t0.Add(from)}
	if !open {
		v.Until = t0.Add(until)
	}
	return v
}

func TestValidityIntervalValid(t *testing.T) {
	for name, tc := range map[string]struct {
		v    ValidityInterval
		want bool
	}{
		"bounded":           {interval(0, time.Hour, false), true},
		"open-ended":        {interval(0, 0, true), true},
		"no start":          {ValidityInterval{Until: t0}, false},
		"empty":             {interval(0, 0, false), false},
		"ends before start": {interval(time.Hour, 0, false), false},
	} {
		if got := tc.v.Valid(); got != tc.want {
			t.Errorf("%s: Valid() = %v, want %v", name, got, tc.want)
		}
	}
}

func TestValidityIntervalContainsIsHalfOpen(t *testing.T) {
	bounded := interval(0, time.Hour, false)
	for name, tc := range map[string]struct {
		at   time.Duration
		want bool
	}{
		"start":             {0, true},
		"just before end":   {time.Hour - time.Nanosecond, true},
		"end":               {time.Hour, false},
		"just before start": {-time.Nanosecond, false},
	} {
		if got := bounded.Contains(t0.Add(tc.at)); got != tc.want {
			t.Errorf("%s: Contains = %v, want %v", name, got, tc.want)
		}
	}
	if !interval(0, 0, true).Contains(t0.AddDate(100, 0, 0)) {
		t.Error("an open-ended interval contains every later instant")
	}
}

func TestValidityIntervalOverlaps(t *testing.T) {
	for name, tc := range map[string]struct {
		a, b ValidityInterval
		want bool
	}{
		"adjacent":          {interval(0, time.Hour, false), interval(time.Hour, 2*time.Hour, false), false},
		"disjoint":          {interval(0, time.Hour, false), interval(2*time.Hour, 3*time.Hour, false), false},
		"nested":            {interval(0, 3*time.Hour, false), interval(time.Hour, 2*time.Hour, false), true},
		"partial":           {interval(0, 2*time.Hour, false), interval(time.Hour, 3*time.Hour, false), true},
		"open-ended later":  {interval(0, 0, true), interval(5*time.Hour, 6*time.Hour, false), true},
		"open-ended before": {interval(time.Hour, 0, true), interval(0, time.Hour, false), false},
		"both open-ended":   {interval(0, 0, true), interval(time.Hour, 0, true), true},
	} {
		if got, rev := tc.a.Overlaps(tc.b), tc.b.Overlaps(tc.a); got != tc.want || rev != tc.want {
			t.Errorf("%s: Overlaps = %v / %v (reversed), want %v", name, got, rev, tc.want)
		}
	}
}
