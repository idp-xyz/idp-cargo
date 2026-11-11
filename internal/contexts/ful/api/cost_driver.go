package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// CostDriver v1 (FUL -> FIN): the fulfilment facts FIN costs a shipment on, identifiers and quantities only, never
// amounts. V0.4 publishes the first kind, SHIPMENT_PLANNED: the shipment's plan, legal entity, customer and the
// orders of its live parts with the accepted quote COM's obligation snapshot named when FUL planned them. The
// kinds for carried legs, consol shares at departure and rebooking come with leg allocations.

// Cost driver kinds.
const DriverShipmentPlanned = "SHIPMENT_PLANNED"

// CostOrder is an order a shipment plans parts of, with its accepted quote; QuoteID is empty for parts planned
// before FUL kept it (V2.39). An order whose parts name different quotes appears once per quote.
type CostOrder struct {
	OrderID       string
	QuoteID       string
	QuoteRevision int64
	Packs         int64 // of the order's parts in this shipment
}

// CostDriver is CostDriver v1 of kind SHIPMENT_PLANNED. A cancelled shipment has no orders or scope.
type CostDriver struct {
	TenantID        string
	ShipmentID      string
	Kind            string
	Revision        int64
	Status          string // StatusDraft, StatusPlanned or StatusCancelled
	PlanVersion     int
	TransportMode   string
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
	LegalEntityID   string
	CustomerPartyID string
	Packs           int64
	GrossWeight     kernel.Quantity
	Volume          kernel.Quantity
	Orders          []CostOrder
	CancelledAt     time.Time
}

// GetShipmentCostDriver asks for a shipment's cost driver. TenantID must come from a verified principal. An
// unknown shipment yields ErrNotFound.
type GetShipmentCostDriver struct {
	TenantID   string
	ShipmentID string
}

// CostDriverQueries is FUL's read side for FIN.
type CostDriverQueries interface {
	GetShipmentCostDriver(ctx context.Context, q GetShipmentCostDriver) (CostDriver, error)
}
