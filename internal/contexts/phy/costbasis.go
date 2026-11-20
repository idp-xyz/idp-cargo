package phy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// GetPhysicalCostBasis answers PhysicalCostBasis v1: the consolidation and, of every live house, the scope PHY froze at
// attach and what its tenders actually loaded, from one snapshot. Loaded measures follow PhysicalLoadEvidence v1: lines
// measured for themselves count as measured, apportioned lines at their lot's latest measurement; weights and volumes
// in mixed units are converted to grams and cubic centimetres and added (V2.84), and a volume not known for a line
// makes the house's volume read zero. Of every house it also says what is loaded in each load unit (HouseUnitLoad,
// measured as the totals are, over every stuffing the lot is loaded in: a lot's measure is divided over them), each
// load unit carries the state and revision of its stuffing's record, and each unit with a stuffing has its packing list
// with a digest (UnitPackingList), built from the same figures, and the unit's cargo against its container's default
// limits (UnitPackingList.Limits).
func (m *Module) GetPhysicalCostBasis(ctx context.Context, q api.GetPhysicalCostBasis) (api.PhysicalCostBasis, error) {
	c, revision, houses, executions, lots, err := m.reads(q.TenantID).CostBasis(ctx, domain.TenantID(q.TenantID),
		domain.ConsolidationID(q.ConsolidationID))
	if err != nil {
		return api.PhysicalCostBasis{}, classify(err)
	}
	st := c.State()
	out := api.PhysicalCostBasis{TenantID: string(st.TenantID), ConsolidationID: string(st.ConsolidationID), Revision: revision,
		Mode: string(st.Mode), Kind: string(st.Kind), OperatingLegalEntityID: st.OperatingLegalEntityID, State: string(st.State),
		Houses: []api.HouseCostBasis{}}
	unitLines := map[int][]api.UnitPackingLine{}
	// A lot's measure is divided over every execution that holds live packs of it. A shipment is live in one
	// consolidation only, and its packs leave a consolidation before it does (migration 000002,
	// membership_entry_loaded),
	// so this consolidation's executions are every execution that can hold live packs of its houses' lots.
	byID := map[domain.CargoLotID]*domain.CargoLot{}
	for _, rs := range lots {
		for _, r := range rs {
			byID[r.Lot.ID()] = r.Lot
		}
	}
	all := make([]*domain.LoadExecution, len(executions))
	for i, r := range executions {
		all[i] = r.Execution
	}
	allotment := domain.AllotLots(all, byID)
	for _, h := range houses {
		e := h.Entry
		house := api.HouseCostBasis{ShipmentID: string(h.ShipmentID), MembershipRevision: h.MembershipRevision,
			Packs: e.Scope.Packs, GrossWeight: e.Scope.GrossWeight, Volume: e.Scope.Volume}
		var weight domain.WeightSum
		var volume domain.VolumeSum
		for _, r := range executions {
			x := r.Execution
			packs, w, v := x.HouseMeasureAllotted(h.ShipmentID, byID, allotment)
			house.LoadedPacks += packs
			// A stuffing that is not cancelled keeps its unit (the database refuses to remove it) and a cancelled one
			// has
			// no live packs, so every unit with packs of a house is a live one.
			if no := x.State().LoadUnitNo; packs > 0 && no > 0 {
				house.Units = append(house.Units, api.HouseUnitLoad{UnitNo: no, ExecutionID: string(x.State().ExecutionID),
					LoadedPacks: packs, LoadedGrossWeight: w, LoadedVolume: v})
				unitLines[no] = append(unitLines[no], api.UnitPackingLine{ShipmentID: string(h.ShipmentID), LoadedPacks: packs,
					LoadedGrossWeight: w, LoadedVolume: v})
			}
			weight.Add(w)
			volume.Add(v)
			for _, l := range x.State().Lines {
				if l.ShipmentID != h.ShipmentID {
					continue
				}
				if live, _, _ := x.LineLive(l.No); live > 0 && l.At.After(house.LastLoadedAt) {
					house.LastLoadedAt = l.At
				}
			}
		}
		house.LoadedGrossWeight, house.LoadedVolume = weight.Quantity(), volume.Quantity()
		slices.SortFunc(house.Units, func(a, b api.HouseUnitLoad) int { return a.UnitNo - b.UnitNo })
		out.Houses = append(out.Houses, house)
	}
	slices.SortFunc(out.Houses, func(a, b api.HouseCostBasis) int { return strings.Compare(a.ShipmentID, b.ShipmentID) })
	uses := map[int]application.UnitUse{}
	for _, r := range executions {
		// A cancelled stuffing was given up with no live packs: its unit is PLANNED again, and it has no packing list.
		if st := r.Execution.State(); st.LoadUnitNo > 0 && st.State != domain.ExecutionCancelled {
			uses[st.LoadUnitNo] = application.UnitUse{ExecutionID: st.ExecutionID, State: st.State, Revision: r.Revision}
			lines := unitLines[st.LoadUnitNo]
			slices.SortFunc(lines, func(a, b api.UnitPackingLine) int { return strings.Compare(a.ShipmentID, b.ShipmentID) })
			list := api.UnitPackingList{UnitNo: st.LoadUnitNo, ExecutionID: string(st.ExecutionID), ExecutionState: string(st.State), Lines: lines}
			if check, ok := r.Execution.CheckLimits(c, byID, allotment); ok {
				list.Limits = api.UnitLimitsCheck{Checked: check.Checked, Unchecked: string(check.Unchecked), CargoGrams: check.CargoGrams,
					CargoCubicCm: check.CargoCubicCm, OverMaxGross: check.OverMaxGross, OverCapacity: check.OverCapacity}
			}
			list.Digest = packingListDigest(list)
			out.PackingLists = append(out.PackingLists, list)
		}
	}
	slices.SortFunc(out.PackingLists, func(a, b api.UnitPackingList) int { return a.UnitNo - b.UnitNo })
	for _, u := range c.LiveLoadUnits() {
		out.LoadUnits = append(out.LoadUnits, loadUnit(u, uses))
	}
	return out, nil
}

// packingListDigest is the SHA-256, in hexadecimal, of the canonical text of a unit's packing list: the three lines
//
//	unit <UnitNo>\n
//	execution <ExecutionID as a Go quoted string>\n
//	state <ExecutionState as a Go quoted string>\n
//
// and then, for each line in shipment order, one line
//
//	line <ShipmentID quoted> <LoadedPacks> <weight unit quoted> <weight amount> <volume unit quoted> <volume amount>\n
//
// with numbers in decimal. Nothing else enters the text, so the digest does not depend on map order, and a later
// version keeps it unless it changes this form.
func packingListDigest(l api.UnitPackingList) string {
	var b strings.Builder
	fmt.Fprintf(&b, "unit %d\nexecution %q\nstate %q\n", l.UnitNo, l.ExecutionID, l.ExecutionState)
	for _, x := range l.Lines {
		fmt.Fprintf(&b, "line %q %d %q %d %q %d\n", x.ShipmentID, x.LoadedPacks, string(x.LoadedGrossWeight.Unit), x.LoadedGrossWeight.Amount,
			string(x.LoadedVolume.Unit), x.LoadedVolume.Amount)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
