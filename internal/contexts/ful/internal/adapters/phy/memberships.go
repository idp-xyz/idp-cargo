// Package phy is FUL's anticorruption layer for PHY's ConsolMembership v1 (relationship 8). PHY's error kinds
// are kept as text only.
package phy

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// Memberships reads a shipment's consol membership from PHY. Call it outside FUL's transactions.
type Memberships struct {
	PHY phyapi.ConsolMembershipQueries
}

var _ app.ConsolMemberships = Memberships{}

func (m Memberships) Membership(ctx context.Context, tenant ful.TenantID, shipment ful.ShipmentID) (app.ConsolMembership, error) {
	got, err := m.PHY.GetConsolMembership(ctx, phyapi.GetConsolMembership{TenantID: string(tenant), ShipmentID: string(shipment)})
	if errors.Is(err, phyapi.ErrNotFound) {
		return app.ConsolMembership{}, fmt.Errorf("%w: PHY has no membership of %s: %v", app.ErrMembershipMismatch, shipment, err.Error())
	}
	if err != nil {
		return app.ConsolMembership{}, err
	}
	if got.TenantID != string(tenant) || got.ShipmentID != string(shipment) || got.Stage != phyapi.StageMain {
		return app.ConsolMembership{}, fmt.Errorf("%w: PHY answered for another shipment or stage", app.ErrMembershipMismatch)
	}
	out := app.ConsolMembership{Live: got.Live, Ended: map[string]ful.AttachmentEnd{}}
	if got.Live {
		c := got.Consolidation
		if c.ConsolidationID != got.Entry.ConsolidationID {
			return app.ConsolMembership{}, fmt.Errorf("%w: the live entry and its consolidation differ", app.ErrMembershipMismatch)
		}
		out.Entry = ful.Membership{ConsolidationID: c.ConsolidationID, EntryNo: got.Entry.EntryNo, Revision: got.Revision,
			AttachedAt: got.Entry.AttachedAt, Mode: ful.TransportMode(c.Mode), Origin: c.Origin, Destination: c.Destination}
	}
	for _, e := range got.History {
		if e.End != "" {
			out.Ended[e.ConsolidationID] = ful.AttachmentEnd(e.End)
		}
	}
	return out, nil
}
