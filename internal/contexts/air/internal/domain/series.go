package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var seriesPrefix = regexp.MustCompile(`^[0-9A-Z]{1,4}$`)

// SeriesState is a stored HAWB series, for persistence.
type SeriesState struct {
	TenantID      TenantID
	LegalEntityID string
	Prefix        string
	Width         int
	Next          int64
	By            string
	At            time.Time
	EnteredAt     time.Time
}

// Series is a legal entity's HAWB series (decision N1-HAWB): a prefix and a zero-padded sequence, drawn in order
// under the series row's lock and never reused.
type Series struct {
	state   SeriesState
	pending []Event
	added   bool
}

// RegisterSeries gives a legal entity its series: a prefix of one to four letters and digits, a width of four to
// eight digits, at most twelve characters in all, and the first number to draw.
func RegisterSeries(tenant TenantID, legalEntity, prefix string, width int, next int64, by string, at time.Time) (*Series, error) {
	st := SeriesState{TenantID: tenant, LegalEntityID: legalEntity, Prefix: prefix, Width: width, Next: next, By: by, At: at.UTC()}
	if !identifier(string(tenant)) || blank(by) || at.IsZero() || !st.valid() {
		return nil, ErrInvalidSeries
	}
	s := &Series{state: st, added: true}
	s.pending = append(s.pending, Event{Type: "HawbSeriesRegistered.v1", At: at.UTC(), TenantID: tenant, SubjectID: legalEntity})
	return s, nil
}

func (st SeriesState) valid() bool {
	return identifier(st.LegalEntityID) && seriesPrefix.MatchString(st.Prefix) && st.Width >= 4 && st.Width <= 8 &&
		len(st.Prefix)+st.Width <= 12 && st.Next >= 1 && len(strconv.FormatInt(st.Next, 10)) <= st.Width+1
}

// Draw returns the next number and advances the series; a series past its last number is exhausted.
func (s *Series) Draw() (string, error) {
	digits := strconv.FormatInt(s.state.Next, 10)
	if len(digits) > s.state.Width {
		return "", ErrSeriesExhausted
	}
	s.state.Next++
	return s.state.Prefix + strings.Repeat("0", s.state.Width-len(digits)) + digits, nil
}

// RestoreSeries rebuilds a stored series.
func RestoreSeries(st SeriesState) (*Series, error) {
	if !st.valid() {
		return nil, fmt.Errorf("%w: stored series of %s", ErrInvalidSeries, st.LegalEntityID)
	}
	return &Series{state: st}, nil
}

func (s *Series) State() SeriesState { return s.state }
func (s *Series) Pending() []Event   { return append([]Event(nil), s.pending...) }
func (s *Series) Acknowledge()       { s.pending, s.added = nil, false }
func (s *Series) Stamp(t time.Time) {
	if s.added {
		s.state.EnteredAt = t.UTC()
	}
}
