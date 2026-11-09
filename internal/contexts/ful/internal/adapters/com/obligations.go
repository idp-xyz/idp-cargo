// Package com is FUL's anticorruption layer for COM: it turns ObligationSnapshot v2 into the obligation
// reference a shipment plans, and refuses a version that COM does not report plannable for the shipment.
package com

import (
	"context"
	"errors"
	"fmt"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

// Obligations reads obligation versions through COM's query side. Call it outside FUL's transactions.
type Obligations struct{ COM comapi.Queries }

// Plannable returns the obligation version if COM reports it ACTIVE, the obligation's latest version,
// and reserved for the shipment.
func (o Obligations) Plannable(ctx context.Context, tenant ful.TenantID, id ful.ObligationID, version int64, shipment ful.ShipmentID) (ful.ObligationRef, error) {
	s, err := o.COM.GetObligationSnapshotV2(ctx, comapi.GetObligationSnapshot{TenantID: string(tenant), ObligationID: string(id), Version: version})
	if errors.Is(err, comapi.ErrNotFound) || errors.Is(err, comapi.ErrRejected) {
		// COM's answer is kept as text only, so its error kinds never become FUL's: an obligation COM does
		// not know is not plannable, it does not make the shipment "not found".
		return ful.ObligationRef{}, fmt.Errorf("%w: %v", app.ErrNotPlannable, err)
	}
	if err != nil {
		return ful.ObligationRef{}, err
	}
	switch {
	case s.TenantID != string(tenant) || s.ObligationID != string(id) || s.ObligationVersion != version:
		return ful.ObligationRef{}, fmt.Errorf("%w: COM answered for another obligation version", app.ErrNotPlannable)
	case s.Status != comapi.ObligationActive || s.LatestVersion != version:
		return ful.ObligationRef{}, fmt.Errorf("%w: COM reports %s version %d %s, latest version %d", app.ErrNotPlannable, id, version, s.Status, s.LatestVersion)
	case s.HeldByShipmentID != string(shipment):
		return ful.ObligationRef{}, fmt.Errorf("%w: COM reports %s reserved for shipment %q, not %s", app.ErrNotPlannable, id, s.HeldByShipmentID, shipment)
	}
	return ful.ObligationRef{
		ObligationID: id, ObligationVersion: version, OrderID: s.OrderID, CustomerPartyID: s.Customer.PartyID,
		LegalEntityID: s.LegalEntityID, Packs: s.Packs, PackageType: s.PackageType, GrossWeight: s.GrossWeight, Volume: s.Volume,
	}, nil
}
