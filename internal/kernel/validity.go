// Package kernel holds value objects whose meaning is identical in every bounded context. It imports
// the standard library only, and a change here affects every context that uses it.
package kernel

import "time"

// ValidityInterval is the half-open interval [From, Until). A zero Until means open-ended.
type ValidityInterval struct {
	From  time.Time
	Until time.Time
}

// Valid reports whether the interval has a start and, if bounded, ends after it starts.
func (v ValidityInterval) Valid() bool {
	return !v.From.IsZero() && (v.Until.IsZero() || v.From.Before(v.Until))
}

// Contains reports whether t lies in [From, Until).
func (v ValidityInterval) Contains(t time.Time) bool {
	return !t.Before(v.From) && (v.Until.IsZero() || t.Before(v.Until))
}

// Overlaps reports whether the intervals share an instant; [a, b) and [b, c) do not.
func (v ValidityInterval) Overlaps(o ValidityInterval) bool {
	return (v.Until.IsZero() || o.From.Before(v.Until)) && (o.Until.IsZero() || v.From.Before(o.Until))
}
