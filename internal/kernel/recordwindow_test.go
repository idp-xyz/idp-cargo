package kernel

import (
	"errors"
	"testing"
	"time"
)

func TestRecordWindowAdmitsLateRecordingWithinItsBounds(t *testing.T) {
	recorded := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	w := DefaultRecordWindow
	for name, tc := range map[string]struct {
		occurred time.Time
		ok       bool
	}{
		"now":                     {recorded, true},
		"yesterday's e-mail":      {recorded.Add(-24 * time.Hour), true},
		"exactly seven days late": {recorded.Add(-7 * 24 * time.Hour), true},
		"eight days late":         {recorded.Add(-8 * 24 * time.Hour), false},
		"a clock 4 minutes ahead": {recorded.Add(4 * time.Minute), true},
		"an hour in the future":   {recorded.Add(time.Hour), false},
		"no business time":        {time.Time{}, false},
	} {
		if err := w.Check(tc.occurred, recorded); (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrOutsideRecordWindow)) {
			t.Errorf("%s: err = %v, want ok %v", name, err, tc.ok)
		}
	}
	if err := w.Check(recorded, time.Time{}); !errors.Is(err, ErrOutsideRecordWindow) {
		t.Errorf("no recording time: err = %v", err)
	}
	if err := (RecordWindow{MaxBackdate: -time.Hour}).Check(recorded, recorded); !errors.Is(err, ErrOutsideRecordWindow) {
		t.Errorf("a negative window admits nothing: err = %v", err)
	}
	if err := (RecordWindow{}).Check(recorded, recorded); err != nil {
		t.Errorf("a zero window admits only the recording instant: %v", err)
	}
}
