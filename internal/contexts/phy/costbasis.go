package phy

import (
	"context"
	"slices"
	"strings"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// GetPhysicalCostBasis answers PhysicalCostBasis v1: the consolidation and, of every live house, the scope
// PHY froze at attach and what its tenders actually loaded, from one snapshot. Loaded measures follow
// PhysicalLoadEvidence v1: lines measured for themselves count as measured, apportioned lines at their lot's
// latest measurement; quantities in mixed units are not added and read zero.
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
	for _, h := range houses {
		byID := map[domain.CargoLotID]*domain.CargoLot{}
		for _, r := range lots[h.ShipmentID] {
			byID[r.Lot.ID()] = r.Lot
		}
		e := h.Entry
		house := api.HouseCostBasis{ShipmentID: string(h.ShipmentID), MembershipRevision: h.MembershipRevision,
			Packs: e.Scope.Packs, GrossWeight: e.Scope.GrossWeight, Volume: e.Scope.Volume}
		weight, volume := sum{}, sum{}
		for _, r := range executions {
			x := r.Execution
			packs, w, v := x.HouseMeasure(h.ShipmentID, byID)
			house.LoadedPacks += packs
			weight.add(w)
			volume.add(v)
			for _, l := range x.State().Lines {
				if l.ShipmentID != h.ShipmentID {
					continue
				}
				if live, _, _ := x.LineLive(l.No); live > 0 && l.At.After(house.LastLoadedAt) {
					house.LastLoadedAt = l.At
				}
			}
		}
		house.LoadedGrossWeight, house.LoadedVolume = weight.quantity(), volume.quantity()
		out.Houses = append(out.Houses, house)
	}
	slices.SortFunc(out.Houses, func(a, b api.HouseCostBasis) int { return strings.Compare(a.ShipmentID, b.ShipmentID) })
	uses := map[int]application.UnitUse{}
	for _, r := range executions {
		if st := r.Execution.State(); st.LoadUnitNo > 0 {
			uses[st.LoadUnitNo] = application.UnitUse{ExecutionID: st.ExecutionID, State: st.State}
		}
	}
	for _, u := range c.LiveLoadUnits() {
		out.LoadUnits = append(out.LoadUnits, loadUnit(u, uses))
	}
	return out, nil
}

// sum adds quantities of one unit; mixed units make the sum unknown, which reads as a zero quantity.
type sum struct {
	unit    kernel.UnitCode
	amount  int64
	unknown bool
}

func (s *sum) add(q kernel.Quantity) {
	switch {
	case q.Unit == "" && q.Amount == 0:
	case s.unit == "" || s.unit == q.Unit:
		s.unit, s.amount = q.Unit, s.amount+q.Amount
	default:
		s.unknown = true
	}
}

func (s *sum) quantity() kernel.Quantity {
	if s.unknown {
		return kernel.Quantity{}
	}
	return kernel.Quantity{Unit: s.unit, Amount: s.amount}
}
