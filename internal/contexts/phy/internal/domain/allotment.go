package domain

import (
	"math/big"
	"slices"
	"strings"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// A lot whose packs are loaded in more than one load execution (a house split over two containers) has one measure
// to give: the executions' parts of it must add up to it, not be worked out each for itself (found by the
// review of V2.79). AllotLots does it over the executions that hold live packs of the lots, and HouseMeasureAllotted
// reads the result; the figures are computed on every read and nothing is stored.

type allotKey struct {
	execution LoadExecutionID
	lot       CargoLotID
}

type allotPart struct{ weight, volume kernel.Quantity }

// Allotment is the part of each lot's measure that the apportioned live packs of each execution take.
type Allotment struct{ parts map[allotKey]allotPart }

func (a Allotment) part(execution LoadExecutionID, lot CargoLotID) (weight, volume kernel.Quantity, ok bool) {
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

// taken is what lines measured for themselves took of a lot's measure m, in a unit the two can be compared in. When
// every line that took anything is in the unit of m (or m is empty) it is the lot's measure as it is and the amounts
// added as they are, exactly as before V2.84, so with one unit throughout no figure moves. When a line is in another
// unit, everything is converted first, to grams or cubic centimetres (convert, rounding up, as the limits check does)
// and the subtraction is made there, and the measure comes back in that base unit: converting back to the unit of m
// would have to round a figure that the base unit holds exactly. When m or a line is in a unit that cannot be
// converted, the two cannot be compared and the amounts are taken off as they are, as before V2.84: the figure that
// comes of it is not taken for a good one, because the quantity in the unit that cannot be converted (the lot's
// measure, or the line, which the sums add in its own unit) makes the house's sum, and the container's check, say so
// (WeightSum, VolumeSum, unitCargo). A line that took nothing counts for nothing.
func taken(m kernel.Quantity, lines []kernel.Quantity, convert func(kernel.Quantity) (int64, bool), base kernel.UnitCode) (kernel.Quantity, int64) {
	var sum int64
	same := true
	for _, q := range lines {
		sum += q.Amount
		if q.Amount != 0 && q.Unit != m.Unit {
			same = false
		}
	}
	if same || m == (kernel.Quantity{}) {
		return m, sum
	}
	measure, ok := convert(m)
	var total int64
	for _, q := range lines {
		if q.Amount == 0 {
			continue
		}
		v, convertible := convert(q)
		ok = ok && convertible
		total = addCapped(total, v)
	}
	if !ok {
		return m, sum
	}
	return kernel.Quantity{Unit: base, Amount: measure}, total
}

// lotPart is what the apportioned live packs of a lot take of its latest measure m: apportioned, with what the lines
// measured for themselves took of the weight and of the volume (lines) taken off in a unit they can be compared in.
// Each figure is in the unit of the lot's measure, or in grams or cubic centimetres where a line was in another unit.
func lotPart(m Measurement, received, measuredPacks int64, weights, volumes []kernel.Quantity, packs int64) (weight, volume kernel.Quantity) {
	mw, tw := taken(m.GrossWeight, weights, toGrams, "GRM")
	mv, tv := taken(m.Volume, volumes, toCubicCm, "CMQ")
	w, v := apportioned(Measurement{GrossWeight: mw, Volume: mv}, received, measuredPacks, tw, tv, packs)
	return kernel.Quantity{Unit: mw.Unit, Amount: w}, kernel.Quantity{Unit: mv.Unit, Amount: v}
}

// AllotLots divides each measured lot's measure over the executions of xs that hold live packs of it:
//  1. lines measured for themselves count as measured, in whichever execution they are;
//  2. the lot's remaining measure is its latest measurement less what all those lines took, never below zero (a line in
//     another unit than the lot's measure is converted first, see taken), and its remaining packs are its packs
//     received less the packs of those lines;
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
		measuredPacks    int64
		weights, volumes []kernel.Quantity // what the lines measured for themselves took, each in its own unit
		takes            []take
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
				t.measuredPacks = t.measuredPacks + live
				t.weights = append(t.weights, kernel.Quantity{Unit: line.GrossWeight.Unit, Amount: w})
				t.volumes = append(t.volumes, kernel.Quantity{Unit: line.Volume.Unit, Amount: v})
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
		weight, volume := lotPart(m, lot.Received(), t.measuredPacks, t.weights, t.volumes, packs)
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
		weights, volumes = divide(weight.Amount, weights), divide(volume.Amount, volumes)
		for i, k := range t.takes {
			out.parts[allotKey{k.x.state.ExecutionID, id}] = allotPart{kernel.Quantity{Unit: weight.Unit, Amount: weights[i]},
				kernel.Quantity{Unit: volume.Unit, Amount: volumes[i]}}
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
