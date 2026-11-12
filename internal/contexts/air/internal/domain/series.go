package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// SeriesState is a stored HAWB series, for persistence.
type SeriesState struct {
	TenantID      TenantID
	LegalEntityID string
	Template      string
	FirstNumber   int64
	SeedPeriod    string
	By            string
	At            time.Time
	EnteredAt     time.Time
}

// Series is a legal entity's HAWB numbering (decision N1-HAWB, MD-28): a numbering template of idp-platform-go and the
// first number, fixed once. The platform's sequence draws the numbers; the template's language is the platform's, so
// AIR's application checks it and that it renders HAWB numbers.
type Series struct {
	state   SeriesState
	pending []Event
	added   bool
}

// RegisterSeries gives a legal entity its HAWB numbering: the template, the first number and the period the first
// number seeds (that of the registration).
func RegisterSeries(tenant TenantID, legalEntity, template string, first int64, seedPeriod, by string, at time.Time) (*Series, error) {
	st := SeriesState{TenantID: tenant, LegalEntityID: legalEntity, Template: template, FirstNumber: first, SeedPeriod: seedPeriod, By: by, At: at.UTC()}
	if !identifier(string(tenant)) || blank(by) || at.IsZero() || !st.valid() {
		return nil, ErrInvalidSeries
	}
	s := &Series{state: st, added: true}
	s.pending = append(s.pending, Event{Type: "HawbSeriesRegistered.v1", At: at.UTC(), TenantID: tenant, SubjectID: legalEntity})
	return s, nil
}

func (st SeriesState) valid() bool {
	return identifier(st.LegalEntityID) && strings.TrimSpace(st.Template) == st.Template && st.Template != "" && utf8.RuneCountInString(st.Template) <= 40 &&
		st.FirstNumber >= 1 && len(st.SeedPeriod) <= 8
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
