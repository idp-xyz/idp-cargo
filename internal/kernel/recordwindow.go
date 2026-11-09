package kernel

import (
	"errors"
	"time"
)

var ErrOutsideRecordWindow = errors.New("kernel: business time outside the window the recording time allows")

// RecordWindow bounds the business time a caller states for a fact against the time the system records it
// (owner decision T1). A fact may be recorded late, such as an acceptance sent by e-mail yesterday and
// entered today, so the business time may be earlier than the recording time by at most MaxBackdate. It may
// be later only by Skew, which absorbs clocks that run slightly ahead. Both times are kept.
type RecordWindow struct {
	MaxBackdate time.Duration
	Skew        time.Duration
}

// DefaultRecordWindow admits facts up to seven days old and five minutes of clock skew.
var DefaultRecordWindow = RecordWindow{MaxBackdate: 7 * 24 * time.Hour, Skew: 5 * time.Minute}

// Valid reports whether the window's bounds are not negative.
func (w RecordWindow) Valid() bool { return w.MaxBackdate >= 0 && w.Skew >= 0 }

// Check returns ErrOutsideRecordWindow unless occurred lies within [recorded − MaxBackdate, recorded + Skew].
func (w RecordWindow) Check(occurred, recorded time.Time) error {
	if !w.Valid() || occurred.IsZero() || recorded.IsZero() || occurred.After(recorded.Add(w.Skew)) || occurred.Before(recorded.Add(-w.MaxBackdate)) {
		return ErrOutsideRecordWindow
	}
	return nil
}
