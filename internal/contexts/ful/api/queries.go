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
// steps. The cargo scope is the sum of the planned parts, which a cancelled shipment no longer has.
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

// PlannedPart is the part of a COM obligation version a shipment plans (F1): its packs, gross weight and
// volume, and what COM committed for the whole obligation.
type PlannedPart struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity // zero when the obligation declares none
	ObligationPacks   int64
	ObligationWeight  kernel.Quantity
	ObligationVolume  kernel.Quantity
}

// ShipmentPlanSnapshotV2 is ShipmentPlanSnapshot v1 with the part of each obligation the shipment plans
// (F1). In v1 the obligations are listed without their parts; the scope is the sum of the parts in both.
type ShipmentPlanSnapshotV2 struct {
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
	Parts            []PlannedPart   // none once cancelled
	PlannedAt        time.Time       // when the current plan version was written
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
	GetShipmentPlanSnapshotV2(ctx context.Context, q GetShipmentPlanSnapshot) (ShipmentPlanSnapshotV2, error)
}
