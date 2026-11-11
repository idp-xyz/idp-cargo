package ful

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

var _ api.CostDriverQueries = (*Module)(nil)

// GetShipmentCostDriver answers CostDriver v1 of kind SHIPMENT_PLANNED from the shipment's current plan and live parts.
func (m *Module) GetShipmentCostDriver(ctx context.Context, q api.GetShipmentCostDriver) (api.CostDriver, error) {
	shipment, revision, err := m.shipment(ctx, api.GetShipmentPlanSnapshot{TenantID: q.TenantID, ShipmentID: q.ShipmentID})
	if err != nil {
		return api.CostDriver{}, err
	}
	st, plan := shipment.State(), shipment.Latest()
	out := api.CostDriver{TenantID: string(st.TenantID), ShipmentID: string(st.ShipmentID), Kind: api.DriverShipmentPlanned, Revision: revision,
		Status: string(st.Status), PlanVersion: plan.No, TransportMode: string(plan.Route.Mode), Origin: plan.Route.Origin,
		Destination: plan.Route.Destination, LegalEntityID: st.LegalEntityID, CustomerPartyID: st.CustomerPartyID}
	if st.Status == domain.ShipmentCancelled {
		out.CancelledAt = st.CancelledAt
		return out, nil
	}
	out.Packs, out.GrossWeight, out.Volume = plan.Scope.Packs, plan.Scope.GrossWeight, plan.Scope.Volume
	type key struct{ order, quote string }
	index := map[key]int{}
	for _, p := range shipment.Parts() {
		k := key{p.OrderID, p.QuoteID}
		i, ok := index[k]
		if !ok {
			i = len(out.Orders)
			index[k] = i
			out.Orders = append(out.Orders, api.CostOrder{OrderID: p.OrderID, QuoteID: p.QuoteID, QuoteRevision: p.QuoteRevision})
		}
		out.Orders[i].Packs += p.Part.Packs
	}
	return out, nil
}
