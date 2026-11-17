// Package ful is SEA's anticorruption layer for FUL's OceanOperationPlan v1 (relationship 10). FUL's error kinds are
// kept as text only.
package ful

import (
	"context"
	"errors"
	"fmt"

	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
)

// Plans reads OceanOperationPlan v1. Call it outside SEA's transactions.
type Plans struct {
	FUL fulapi.OceanOperationPlanQueries
}

var _ app.Plans = Plans{}

func (p Plans) Plan(ctx context.Context, tenant sea.TenantID, shipment string) (sea.Plan, error) {
	got, err := p.FUL.GetOceanOperationPlan(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: string(tenant), ShipmentID: shipment})
	if errors.Is(err, fulapi.ErrNotFound) {
		return sea.Plan{}, fmt.Errorf("%w: %s: %v", app.ErrUnknownShipment, shipment, err.Error())
	}
	if err != nil {
		return sea.Plan{}, err
	}
	if got.TenantID != string(tenant) || got.ShipmentID != shipment || got.Stage != fulapi.StageMain {
		return sea.Plan{}, fmt.Errorf("%w: FUL answered for another shipment or stage", app.ErrUnknownShipment)
	}
	out := sea.Plan{ShipmentID: shipment, Revision: got.Revision, Status: got.Status, PlanVersion: got.PlanVersion, LegalEntityID: got.LegalEntityID,
		Mode: got.TransportMode, ContainerMode: got.ContainerMode, Origin: got.Origin, Destination: got.Destination, Window: got.DepartureWindow,
		Packs: got.Packs, GrossWeight: got.GrossWeight, Volume: got.Volume, ConsolidationID: got.ConsolidationID, EvidenceVersion: got.EvidenceVersion,
		MeasuredBasis: got.MeasuredBasis, MeasuredPacks: got.MeasuredPacks, MeasuredWeight: got.MeasuredGrossWeight, MeasuredVolume: got.MeasuredVolume}
	for _, part := range got.Parts {
		out.Parts = append(out.Parts, sea.Part{ObligationID: part.ObligationID, ObligationVersion: part.ObligationVersion, OrderID: part.OrderID,
			Packs: part.Packs, PackageType: part.PackageType, GrossWeight: part.GrossWeight, Volume: part.Volume, GoodsDescription: part.GoodsDescription})
	}
	for _, s := range got.Stuffings {
		out.Stuffings = append(out.Stuffings, sea.Stuffing{ExecutionID: s.ExecutionID, LoadUnitNo: s.LoadUnitNo, TypeCode: s.TypeCode, Packs: s.Packs,
			GrossWeight: s.GrossWeight, Volume: s.Volume, LineNos: append([]int(nil), s.LineNos...), Seal: s.Seal, SealSHA256: s.SealSHA256,
			SealedAt: s.SealedAt.UTC()})
	}
	return out, nil
}
