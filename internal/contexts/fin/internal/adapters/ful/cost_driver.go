// Package ful is FIN's anticorruption layer for FUL's CostDriver v1 (FUL -> FIN). FUL's error kinds are kept as
// text only.
package ful

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
)

// Shipments reads CostDriver v1 of kind SHIPMENT_PLANNED. Call it outside FIN's transactions.
type Shipments struct{ FUL fulapi.CostDriverQueries }

var _ app.Shipments = Shipments{}

func (s Shipments) CostDriver(ctx context.Context, tenant fin.TenantID, shipment string) (app.CostDriver, error) {
	d, err := s.FUL.GetShipmentCostDriver(ctx, fulapi.GetShipmentCostDriver{TenantID: string(tenant), ShipmentID: shipment})
	if errors.Is(err, fulapi.ErrNotFound) {
		return app.CostDriver{}, fmt.Errorf("%w: shipment %s: %v", app.ErrUnknownSource, shipment, err.Error())
	}
	if err != nil {
		return app.CostDriver{}, err
	}
	if d.ShipmentID != shipment || d.Kind != fulapi.DriverShipmentPlanned {
		return app.CostDriver{}, fmt.Errorf("%w: FUL answered for another shipment or kind", app.ErrUnknownSource)
	}
	out := app.CostDriver{ShipmentID: d.ShipmentID, Status: d.Status, PlanVersion: d.PlanVersion, TransportMode: d.TransportMode, Origin: d.Origin,
		Destination: d.Destination, LegalEntityID: d.LegalEntityID, CustomerPartyID: d.CustomerPartyID, Packs: d.Packs, GrossWeight: d.GrossWeight,
		Volume: d.Volume}
	for _, o := range d.Orders {
		out.Orders = append(out.Orders, app.CostOrder{OrderID: o.OrderID, QuoteID: o.QuoteID, QuoteRevision: o.QuoteRevision})
	}
	return out, nil
}
