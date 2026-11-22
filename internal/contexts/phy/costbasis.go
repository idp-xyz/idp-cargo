package phy

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// basisReads is reads for the cost basis: its transaction is one snapshot (REPEATABLE READ, READ ONLY, set before the
// first statement), so the live houses, the houses that left, the executions and the lots are one state of the
// database. A house detached while the basis is read is in the live houses or in the former houses, never in neither
// (V2.92).
func (m *Module) basisReads(tenant string) application.Reads {
	r := m.reads(tenant)
	r.Tx = bento.TenantTransactor{DB: m.db, Tenant: tenant, Snapshot: true}
	return r
}

// GetPhysicalCostBasis answers PhysicalCostBasis v1: the consolidation and, of every live house, the scope PHY froze at
// attach and what its tenders actually loaded, from one snapshot. Loaded measures follow PhysicalLoadEvidence v1: lines
// measured for themselves count as measured, apportioned lines at their lot's latest measurement; weights and volumes
// in mixed units are converted to grams and cubic centimetres and added (V2.84), and a volume not known for a line
// makes the house's volume read zero. Of every house it also says what is loaded in each load unit (HouseUnitLoad,
// measured as the totals are, over every stuffing the lot is loaded in: a lot's measure is divided over them), each
// load unit carries the state and revision of its stuffing's record, and each unit with a stuffing has its packing list
// with a digest (UnitPackingList), built from the same figures, and the unit's cargo against its container's default
// limits (UnitPackingList.Limits). Since V2.92 it also lists the houses that left the consolidation (FormerHouses) and
// the stuffings given up after a rework (GivenUpStuffings), in the same snapshot; neither is part of a digest or of
// Revision.
func (m *Module) GetPhysicalCostBasis(ctx context.Context, q api.GetPhysicalCostBasis) (api.PhysicalCostBasis, error) {
	c, revision, houses, former, executions, lots, err := m.basisReads(q.TenantID).CostBasis(ctx, domain.TenantID(q.TenantID),
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
		// Its reworks are read apart (V2.92).
		if st := r.Execution.State(); st.LoadUnitNo > 0 && st.State == domain.ExecutionCancelled && len(st.Reworks) > 0 {
			out.GivenUpStuffings = append(out.GivenUpStuffings, api.GivenUpStuffing{UnitNo: st.LoadUnitNo, ExecutionID: string(st.ExecutionID),
				Reworks: reworkViews(st.Reworks)})
		}
		if st := r.Execution.State(); st.LoadUnitNo > 0 && st.State != domain.ExecutionCancelled {
			uses[st.LoadUnitNo] = application.UnitUse{ExecutionID: st.ExecutionID, State: st.State, Revision: r.Revision}
			lines := unitLines[st.LoadUnitNo]
			slices.SortFunc(lines, func(a, b api.UnitPackingLine) int { return strings.Compare(a.ShipmentID, b.ShipmentID) })
			list := api.UnitPackingList{UnitNo: st.LoadUnitNo, ExecutionID: string(st.ExecutionID), ExecutionState: string(st.State), Lines: lines,
				Reworks: reworkViews(st.Reworks)}
			if check, ok := r.Execution.CheckLimits(c, byID, allotment); ok {
				list.Limits = api.UnitLimitsCheck{Checked: check.Checked, Unchecked: string(check.Unchecked), CargoGrams: check.CargoGrams,
					CargoCubicCm: check.CargoCubicCm, OverMaxGross: check.OverMaxGross, OverCapacity: check.OverCapacity}
			}
			list.Digest = packingListDigest(list)
			out.PackingLists = append(out.PackingLists, list)
		}
	}
	slices.SortFunc(out.PackingLists, func(a, b api.UnitPackingList) int { return a.UnitNo - b.UnitNo })
	slices.SortFunc(out.GivenUpStuffings, func(a, b api.GivenUpStuffing) int {
		return cmp.Or(a.UnitNo-b.UnitNo, strings.Compare(a.ExecutionID, b.ExecutionID))
	})
	for _, f := range former {
		out.FormerHouses = append(out.FormerHouses, formerHouse(f, executions))
	}
	for _, u := range c.LiveLoadUnits() {
		out.LoadUnits = append(out.LoadUnits, loadUnit(u, uses))
	}
	return out, nil
}

// formerHouse tells how a house left the consolidation and its rework: the entry's, else the one the reversal lines
// name, with the unit and the asker as the stuffing's reworks give them (a stuffing given up is among the executions as
// well).
func formerHouse(f application.FormerHouse, executions []application.ExecutionRead) api.FormerHouse {
	e := f.Entry
	out := api.FormerHouse{ShipmentID: string(f.ShipmentID), End: string(e.End), TransferredTo: string(f.To), Reason: string(e.Reason), ReasonText: e.ReasonText,
		EndedBy: e.EndedBy, EndedAt: e.EndedAt, EndedEnteredAt: e.EndEnteredAt, ReworkFrom: api.ReworkFromNone}
	switch {
	case e.ReworkNo != 0:
		out.ReworkFrom, out.ReworkExecutionID, out.ReworkNo = api.ReworkFromEntry, string(e.ReworkExecutionID), e.ReworkNo
	case f.Unloaded:
		out.ReworkFrom, out.ReworkExecutionID, out.ReworkNo = api.ReworkFromReversal, string(f.Reversal.ExecutionID), f.Reversal.No
	default:
		return out
	}
	for _, r := range executions {
		st := r.Execution.State()
		if string(st.ExecutionID) != out.ReworkExecutionID {
			continue
		}
		out.ReworkUnitNo = st.LoadUnitNo
		for _, w := range st.Reworks {
			if w.No == out.ReworkNo {
				out.ReworkAskedBy = w.AskedBy
			}
		}
	}
	return out
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
