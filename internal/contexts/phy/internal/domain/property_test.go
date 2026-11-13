package domain

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// TestRandomMembershipKeepsItsInvariants runs random attaches, detaches, transfers, refreshes, closes and
// reopens over three consolidations and five houses. Whatever is accepted, every house has at most one live
// entry, every history restores, and a closed consolidation's houses do not change until it is reopened
// (PHY is Core: property tests, implementation backlog PB10).
func TestRandomMembershipKeepsItsInvariants(t *testing.T) {
	for seed := uint64(1); seed <= 200; seed++ {
		r := rand.New(rand.NewPCG(seed, 7))
		consols := []*Consolidation{airConsol(t, "C-0"), airConsol(t, "C-1"), airConsol(t, "C-2")}
		houses := make([]*Membership, 5)
		plans := make([]int, 5)
		for i := range houses {
			houses[i], _ = NewMembership("T", ShipmentID(fmt.Sprintf("SH-%d", i)))
			plans[i] = 1
		}
		at := t0.Add(time.Hour)
		for step := 0; step < 60; step++ {
			at = at.Add(time.Minute)
			h, c, d := r.IntN(5), consols[r.IntN(3)], consols[r.IntN(3)]
			m, id := houses[h], ShipmentID(fmt.Sprintf("SH-%d", h))
			before := liveIn(houses, c.ID())
			closed := c.Current().State == StateClosed
			switch r.IntN(7) { // refusals are expected; only the invariants matter
			case 0, 1:
				_ = m.Attach(c, intent(id, plans[h]), "ops", at)
			case 2:
				_ = m.Detach(c, ReasonCustomerRequest, "", "ops", at)
			case 3:
				_ = m.Transfer(c, d, intent(id, plans[h]), ReasonRollover, "", "ops", at)
			case 4:
				plans[h]++ // FUL replanned and confirmed the shipment again
				_ = m.Refresh(c, intent(id, plans[h]), "ops", at)
			case 5:
				n := liveIn(houses, c.ID())
				_ = c.Close(scopes(len(n)), at, "", "ops", at)
			case 6:
				_ = c.Reopen("late cargo", "ops", at, 0, Stuffed{})
			}
			if closed && c.Current().State == StateClosed && !same(before, liveIn(houses, c.ID())) {
				t.Fatalf("seed %d step %d: the houses of closed %s changed", seed, step, c.ID())
			}
			for i, m := range houses {
				live := 0
				for _, e := range m.State().Entries {
					if e.Live() {
						live++
					}
				}
				if live > 1 {
					t.Fatalf("seed %d step %d: house %d has %d live entries", seed, step, i, live)
				}
				if len(m.State().Entries) > 0 {
					if _, err := RestoreMembership(m.State()); err != nil {
						t.Fatalf("seed %d step %d: house %d does not restore: %v", seed, step, i, err)
					}
				}
			}
			for _, c := range consols {
				if _, err := RestoreConsolidation(c.State()); err != nil {
					t.Fatalf("seed %d step %d: %s does not restore: %v", seed, step, c.ID(), err)
				}
			}
		}
	}
}

// liveIn returns the houses with a live entry in a consolidation, with the entry number.
func liveIn(houses []*Membership, c ConsolidationID) map[ShipmentID]int {
	in := map[ShipmentID]int{}
	for _, m := range houses {
		if e, ok := m.Live(); ok && e.ConsolidationID == c {
			in[m.ShipmentID()] = e.No
		}
	}
	return in
}

func same(a, b map[ShipmentID]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
