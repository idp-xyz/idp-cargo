package application

import (
	"context"
	"errors"
	"fmt"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

// QuoteOnShipment re-evaluates an accepted ONE_OFF quote on the cargo shipped (N1-RATE OD-7 A). It reads each RATED
// line's snapshot and the card lines its snapshot lines were rated from, which never change once published; call it
// within a tenant transaction.
type QuoteOnShipment struct {
	Snapshots SnapshotStore
	Cards     RateCardStore
}

type cardVersion struct {
	card    rate.RateCardID
	version int
}

// errStoredQuote marks what RATE stored for a quote but cannot read back as it rated it: an internal failure, never a
// rejection of the caller.
var errStoredQuote = errors.New("rate: a quote's snapshot or card line cannot be read as rated")

// Lines answers the quote's lines on the cargo: RATED per-unit and percentage lines as rated on it, the rest as quoted.
func (h QuoteOnShipment) Lines(ctx context.Context, q *rate.Quote, cargo rate.Cargo) ([]rate.ShippedLine, error) {
	if h.Snapshots == nil || h.Cards == nil {
		return nil, ErrMissingDependency
	}
	st := q.State()
	snapshots := map[string]rate.RateSnapshotState{}
	entries := map[cardVersion][]rate.RateEntry{}
	cardLine := func(sl rate.SnapshotLine) (rate.RateLine, error) {
		key := cardVersion{sl.RateCardID, sl.Version}
		es, ok := entries[key]
		if !ok {
			var err error
			if es, err = h.Cards.Entries(ctx, st.TenantID, sl.RateCardID, sl.Version); err != nil {
				return rate.RateLine{}, err
			}
			entries[key] = es
		}
		for _, e := range es {
			for _, l := range e.Lines {
				if e.No == sl.EntryNo && l.No == sl.RateLineNo && l.ChargeCode == sl.ChargeCode {
					return l, nil
				}
			}
		}
		return rate.RateLine{}, fmt.Errorf("card %s version %d has no entry %d line %d of %s", sl.RateCardID, sl.Version, sl.EntryNo, sl.RateLineNo,
			sl.ChargeCode)
	}
	rules := map[int]rate.RatedRule{}
	for i, l := range st.Lines {
		if l.Source != rate.SourceRated || l.Rated == nil {
			continue
		}
		rule, err := func() (rate.RatedRule, error) {
			snap, ok := snapshots[l.Rated.SnapshotID]
			if !ok {
				s, err := h.Snapshots.Read(ctx, st.TenantID, l.Rated.SnapshotID)
				if err != nil {
					return rate.RatedRule{}, err
				}
				snap = s.State()
				snapshots[l.Rated.SnapshotID] = snap
			}
			// unit and percentage rules of a snapshot line, from the card line it was rated from
			side := func(no int) (*rate.UnitRule, *rate.PercentRule, error) {
				for _, sl := range snap.Lines {
					if sl.LineNo != no {
						continue
					}
					if sl.Calculator != rate.CalcPerUnit && sl.Calculator != rate.CalcPercentOf {
						return nil, nil, nil
					}
					card, err := cardLine(sl)
					if err != nil {
						return nil, nil, err
					}
					if sl.Calculator == rate.CalcPerUnit {
						return &rate.UnitRule{Line: card}, nil, nil
					}
					return nil, &rate.PercentRule{Line: card, BP: sl.PercentBP}, nil
				}
				return nil, nil, fmt.Errorf("snapshot %s has no line %d", snap.SnapshotID, no)
			}
			var rule rate.RatedRule
			var err error
			if rule.Sell, rule.SellPercent, err = side(l.Rated.SellLineNo); err != nil {
				return rate.RatedRule{}, err
			}
			if l.Rated.CostLineNo > 0 {
				if rule.Cost, rule.CostPercent, err = side(l.Rated.CostLineNo); err != nil {
					return rate.RatedRule{}, err
				}
			}
			return rule, nil
		}()
		if err != nil {
			return nil, fmt.Errorf("%w: quote %s line %d: %v", errStoredQuote, st.QuoteID, i+1, err)
		}
		rules[i+1] = rule
	}
	return q.OnShipment(cargo, rules)
}
