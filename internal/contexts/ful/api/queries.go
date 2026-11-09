package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Shipment statuses.
const (
	StatusDraft     = "DRAFT"
	StatusPlanned   = "PLANNED"
	StatusCancelled = "CANCELLED"
)

// ObligationRef is a COM obligation version a shipment plans, with the order it belongs to.
type ObligationRef struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
}

// ShipmentPlanSnapshot is ShipmentPlanSnapshot v1: the current plan of a shipment, for BOOK and later
// steps. The cargo scope is the sum of the planned obligations, which a cancelled shipment no longer has.
// It carries no names (MD-05) and never asserts loading or delivery.
type ShipmentPlanSnapshot struct {
	TenantID         string
	ShipmentID       string
	Revision         int64
	Status           string // StatusDraft, StatusPlanned or StatusCancelled
	PlanVersion      int
	ConfirmedVersion int // the latest confirmed plan version, 0 if none
	CustomerPartyID  string
	LegalEntityID    string
	TransportMode    string
	Origin           kernel.LocationCode
	Destination      kernel.LocationCode
	DepartureWindow  kernel.ValidityInterval
	Packs            int64
	GrossWeight      kernel.Quantity
	Volume           kernel.Quantity // zero when the obligations declare none
	Obligations      []ObligationRef
	PlannedAt        time.Time // when the current plan version was written
}

// GetShipmentPlanSnapshot asks for a shipment's current plan. TenantID must come from a verified
// principal.
type GetShipmentPlanSnapshot struct {
	TenantID   string
	ShipmentID string
}

// Queries is FUL's read side for other contexts. An unknown shipment yields ErrNotFound.
type Queries interface {
	GetShipmentPlanSnapshot(ctx context.Context, q GetShipmentPlanSnapshot) (ShipmentPlanSnapshot, error)
}
