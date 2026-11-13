package application

import (
	"context"
	"fmt"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

// QuoteOnShipment re-evaluates an accepted ONE_OFF quote on the cargo shipped (N1-RATE OD-7 A). It reads each rated
// per-unit line's snapshot and the card line it was rated from, which never change once published; call it within a
// tenant transaction.
type QuoteOnShipment struct {
	Snapshots SnapshotStore
	Cards     RateCardStore
}

type cardVersion struct {
	card    rate.RateCardID
	version int
}

// Lines answers the quote's lines on the cargo: per-unit lines at their frozen rates on the quantity shipped, the rest as
// quoted.
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
				if e.No == sl.EntryNo && l.No == sl.RateLineNo {
					return l, nil
				}
			}
		}
		return rate.RateLine{}, fmt.Errorf("rate: card %s version %d has no entry %d line %d", sl.RateCardID, sl.Version, sl.EntryNo, sl.RateLineNo)
	}
	rules := map[int]rate.RatedRule{}
	for i, l := range st.Lines {
		if l.Basis.Kind != rate.BasisPerUnit || l.Rated == nil {
			continue
		}
		snap, ok := snapshots[l.Rated.SnapshotID]
		if !ok {
			s, err := h.Snapshots.Read(ctx, st.TenantID, l.Rated.SnapshotID)
			if err != nil {
				return nil, err
			}
			snap = s.State()
			snapshots[l.Rated.SnapshotID] = snap
		}
		line := func(no int) (rate.SnapshotLine, error) {
			for _, sl := range snap.Lines {
				if sl.LineNo == no {
					return sl, nil
				}
			}
			return rate.SnapshotLine{}, fmt.Errorf("rate: snapshot %s has no line %d", snap.SnapshotID, no)
		}
		sell, err := line(l.Rated.SellLineNo)
		if err != nil {
			return nil, err
		}
		var rule rate.RatedRule
		if rule.Sell, err = cardLine(sell); err != nil {
			return nil, err
		}
		if l.Rated.CostLineNo > 0 {
			buy, err := line(l.Rated.CostLineNo)
			if err != nil {
				return nil, err
			}
			if buy.Calculator == rate.CalcPerUnit && !buy.MinimumApplied && !buy.MaximumApplied {
				costLine, err := cardLine(buy)
				if err != nil {
					return nil, err
				}
				rule.Cost, rule.CostRate, rule.CostUnit = &costLine, buy.Rate, buy.Unit
			}
		}
		rules[i+1] = rule
	}
	return q.OnShipment(cargo, rules)
}
