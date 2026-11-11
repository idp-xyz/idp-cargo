// Package ful is AIR's anticorruption layer for FUL's AirOperationPlan v1 (relationship 9). FUL's error kinds are
// kept as text only.
package ful

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/air/internal/application"
	air "go.idp.xyz/idp-cargo/internal/contexts/air/internal/domain"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
)

// Plans reads AirOperationPlan v1. Call it outside AIR's transactions.
type Plans struct {
	FUL fulapi.AirOperationPlanQueries
}

var _ app.Plans = Plans{}

func (p Plans) Plan(ctx context.Context, tenant air.TenantID, shipment string) (air.Plan, error) {
	got, err := p.FUL.GetAirOperationPlan(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: string(tenant), ShipmentID: shipment})
	if errors.Is(err, fulapi.ErrNotFound) {
		return air.Plan{}, fmt.Errorf("%w: %s: %v", app.ErrUnknownShipment, shipment, err.Error())
	}
	if err != nil {
		return air.Plan{}, err
	}
	if got.TenantID != string(tenant) || got.ShipmentID != shipment || got.Stage != fulapi.StageMain {
		return air.Plan{}, fmt.Errorf("%w: FUL answered for another shipment or stage", app.ErrUnknownShipment)
	}
	out := air.Plan{ShipmentID: shipment, Revision: got.Revision, Status: got.Status, PlanVersion: got.PlanVersion, LegalEntityID: got.LegalEntityID,
		Mode: got.TransportMode, Origin: got.Origin, Destination: got.Destination, Window: got.DepartureWindow, Pieces: got.Packs,
		GrossWeight: got.GrossWeight, Volume: got.Volume, ConsolidationID: got.ConsolidationID, EvidenceVersion: got.EvidenceVersion,
		MeasuredBasis: got.MeasuredBasis, MeasuredPieces: got.MeasuredPacks, MeasuredWeight: got.MeasuredGrossWeight, MeasuredVolume: got.MeasuredVolume,
		Security: got.SecurityStatus}
	for _, part := range got.Parts {
		out.Parts = append(out.Parts, air.Part{ObligationID: part.ObligationID, ObligationVersion: part.ObligationVersion, OrderID: part.OrderID,
			Packs: part.Packs, PackageType: part.PackageType, GrossWeight: part.GrossWeight, Volume: part.Volume, GoodsDescription: part.GoodsDescription})
	}
	for _, s := range got.Screenings {
		out.Screenings = append(out.Screenings, air.Screening{LotID: s.LotID, Outcome: s.Outcome, Screener: s.Screener, Method: s.Method,
			Reference: s.Reference, ScreenedAt: s.ScreenedAt.UTC()})
	}
	return out, nil
}
