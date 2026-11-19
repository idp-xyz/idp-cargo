package domain

import (
	"math/big"
	"slices"
	"strings"
)

// A lot whose packs are loaded in more than one load execution (a house split over two containers) has one measure
// to give: the executions' parts of it must add up to it, not be worked out each for itself (found by the
// review of V2.79). AllotLots does it over the executions that hold live packs of the lots, and HouseMeasureAllotted
// reads the result; the figures are computed on every read and nothing is stored.

type allotKey struct {
	execution LoadExecutionID
	lot       CargoLotID
}

type allotPart struct{ weight, volume int64 }

// Allotment is the part of each lot's measure that the apportioned live packs of each execution take.
type Allotment struct{ parts map[allotKey]allotPart }

func (a Allotment) part(execution LoadExecutionID, lot CargoLotID) (weight, volume int64, ok bool) {
	p, ok := a.parts[allotKey{execution, lot}]
	return p.weight, p.volume, ok
}

// apportioned is what the apportioned live packs of a lot take of its latest measure m: the measure less what the
// live lines measured for themselves took (never below zero), as the share of the lot's packs received and not
// measured for themselves that the apportioned packs are, rounded down, or all of it when they complete the lot.
// Lines measured for themselves are believed: when they exceed the measure the rest is zero and the lot's total
// exceeds its measure.
func apportioned(m Measurement, received, measuredPacks, measuredWeight, measuredVolume, packs int64) (weight, volume int64) {
	rest := received - measuredPacks
	weight, volume = max(m.GrossWeight.Amount-measuredWeight, 0), max(m.Volume.Amount-measuredVolume, 0)
	if packs < rest {
		weight, volume = share(weight, packs, rest), share(volume, packs, rest)
	}
	return weight, volume
}

// AllotLots divides each measured lot's measure over the executions of xs that hold live packs of it:
//  1. lines measured for themselves count as measured, in whichever execution they are;
//  2. the lot's remaining measure is its latest measurement less what all those lines took, never below zero, and its
//     remaining packs are its packs received less the packs of those lines;
//  3. the apportioned live packs of all the executions together take of the remaining measure their share of the
//     remaining packs, all of it when they complete the lot (one execution alone: exactly HouseMeasure's rule);
//  4. that amount is divided over the executions by their apportioned live packs by the largest remainder, so that
//     the parts add up to it exactly; equal remainders go to the lower load unit number, then the lower execution id.
//
// Weight and volume are each divided so. A lot with no measurement or no packs received has nothing to allot. The
// result does not depend on the order of xs. An execution passed twice counts once (by execution id) and a nil element
// is skipped; no caller does either.
//
// A container's part of a lot is its part of the lot's measure as the lot is loaded now, not a figure fixed when the
// container is sealed: it changes when another container loads or unloads packs of the lot, when packs of it are
// measured for themselves elsewhere, and when the lot is measured again. A lot of 6 packs weighed at 301 kg with 5
// packs in a sealed container 1 gives it 301 x 5/6 = 250.83 rounded down to 250 kg; when container 2 loads the sixth
// pack the parts are 250.83 and 50.17, the floors 250 + 50 leave 1 kg for the larger remainder (.83): 251 + 50, and the
// sealed container's figure, and its packing list's digest, have moved.
func AllotLots(xs []*LoadExecution, lots map[CargoLotID]*CargoLot) Allotment {
	type take struct {
		x     *LoadExecution
		packs int64
	}
	type lotTotals struct {
		measuredPacks, measuredWeight, measuredVolume int64
		takes                                         []take
	}
	byLot := map[CargoLotID]*lotTotals{}
	var order []CargoLotID
	counted := map[LoadExecutionID]bool{}
	for _, x := range xs {
		if x == nil || counted[x.state.ExecutionID] {
			continue // no caller passes an execution twice or a nil one; if one did, it counts once, or not at all
		}
		counted[x.state.ExecutionID] = true
		for _, line := range x.state.Lines {
			live, w, v := x.lineLive(line.No)
			lot, known := lots[line.LotID]
			if live == 0 || !known {
				continue
			}
			if _, measured := lot.Measured(); !measured || lot.Received() <= 0 {
				continue
			}
			t := byLot[line.LotID]
			if t == nil {
				t = &lotTotals{}
				byLot[line.LotID], order = t, append(order, line.LotID)
			}
			if line.Basis != BasisApportioned {
				t.measuredPacks, t.measuredWeight, t.measuredVolume = t.measuredPacks+live, t.measuredWeight+w, t.measuredVolume+v
				continue
			}
			if i := slices.IndexFunc(t.takes, func(k take) bool { return k.x.state.ExecutionID == x.state.ExecutionID }); i >= 0 {
				t.takes[i].packs += live
			} else {
				t.takes = append(t.takes, take{x, live})
			}
		}
	}
	out := Allotment{parts: map[allotKey]allotPart{}}
	for _, id := range order {
		t := byLot[id]
		var packs int64
		for _, k := range t.takes {
			packs += k.packs
		}
		if packs == 0 {
			continue
		}
		lot := lots[id]
		m, _ := lot.Measured()
		weight, volume := apportioned(m, lot.Received(), t.measuredPacks, t.measuredWeight, t.measuredVolume, packs)
		slices.SortFunc(t.takes, func(a, b take) int {
			if a.x.state.LoadUnitNo != b.x.state.LoadUnitNo {
				return a.x.state.LoadUnitNo - b.x.state.LoadUnitNo
			}
			return strings.Compare(string(a.x.state.ExecutionID), string(b.x.state.ExecutionID))
		})
		weights, volumes := make([]int64, len(t.takes)), make([]int64, len(t.takes))
		for i, k := range t.takes {
			weights[i], volumes[i] = k.packs, k.packs
		}
		weights, volumes = divide(weight, weights), divide(volume, volumes)
		for i, k := range t.takes {
			out.parts[allotKey{k.x.state.ExecutionID, id}] = allotPart{weights[i], volumes[i]}
		}
	}
	return out
}

// divide splits total over parts in proportion to their sizes by the largest remainder: each takes its share rounded
// down, and the units that are left go one each to the parts with the largest remainders, equal remainders to the
// part that comes first (the parts are in tie order). The result adds up to total exactly.
func divide(total int64, sizes []int64) []int64 {
	var all int64
	for _, n := range sizes {
		all += n
	}
	out := make([]int64, len(sizes))
	remainders := make([]*big.Int, len(sizes))
	left := total
	for i, n := range sizes {
		product := new(big.Int).Mul(big.NewInt(total), big.NewInt(n))
		quotient, remainder := new(big.Int).QuoRem(product, big.NewInt(all), new(big.Int))
		out[i], remainders[i] = quotient.Int64(), remainder
		left -= out[i]
	}
	index := make([]int, len(sizes))
	for i := range index {
		index[i] = i
	}
	slices.SortStableFunc(index, func(a, b int) int { return remainders[b].Cmp(remainders[a]) })
	for _, i := range index[:left] {
		out[i]++
	}
	return out
}
