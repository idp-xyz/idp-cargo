// Package ful is PHY's anticorruption layer for FUL's LoadIntent v1 (relationship 7). FUL's error kinds are
// kept as text only.
package ful

import (
	"context"
	"errors"
	"fmt"

	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// LoadIntents reads a shipment's LoadIntent v1 from FUL. Call it outside PHY's transactions.
type LoadIntents struct{ FUL fulapi.LoadIntentQueries }

var _ app.LoadIntents = LoadIntents{}

func (l LoadIntents) LoadIntent(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) (phy.LoadIntent, error) {
	got, err := l.FUL.GetLoadIntent(ctx, fulapi.GetShipmentPlanSnapshot{TenantID: string(tenant), ShipmentID: string(shipment)})
	if errors.Is(err, fulapi.ErrNotFound) {
		return phy.LoadIntent{}, fmt.Errorf("%w: %s: %v", app.ErrUnknownShipment, shipment, err.Error())
	}
	if err != nil {
		return phy.LoadIntent{}, err
	}
	if got.TenantID != string(tenant) || got.ShipmentID != string(shipment) || got.Stage != fulapi.StageMain {
		return phy.LoadIntent{}, fmt.Errorf("%w: FUL answered for another shipment or stage", phy.ErrInvalidIntent)
	}
	intent := phy.LoadIntent{ShipmentID: shipment, Revision: got.Revision, Status: phy.IntentStatus(got.Status), PlanVersion: got.PlanVersion,
		LegalEntityID: got.LegalEntityID, Mode: phy.Mode(got.TransportMode), ContainerMode: got.ContainerMode,
		Route: phy.Route{Origin: got.Origin, Destination: got.Destination, OriginIATA: got.OriginIATA, DestinationIATA: got.DestinationIATA,
			LocationEdition: got.LocationEdition},
		DepartureWindow: got.DepartureWindow, Scope: phy.CargoScope{Packs: got.Packs, GrossWeight: got.GrossWeight, Volume: got.Volume},
		ConsolidationID: phy.ConsolidationID(got.ConsolidationID), EntryNo: got.MembershipEntryNo}
	for _, p := range got.Parts {
		intent.Parts = append(intent.Parts, phy.Part{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID,
			Packs: p.Packs, PackageType: p.PackageType, GrossWeight: p.GrossWeight, Volume: p.Volume})
	}
	return intent, nil
}
