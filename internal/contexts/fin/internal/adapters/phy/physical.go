// Package phy is FIN's anticorruption layer for PHY's PhysicalCostBasis v1 (PHY -> FIN). PHY's error kinds are kept
// as text only.
package phy

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// Physical reads PhysicalCostBasis v1. Call it outside FIN's transactions.
type Physical struct {
	PHY phyapi.PhysicalCostBasisQueries
}

var _ app.Physical = Physical{}

func (p Physical) CostBasis(ctx context.Context, tenant fin.TenantID, consolidation string) (fin.CostBasis, error) {
	b, err := p.PHY.GetPhysicalCostBasis(ctx, phyapi.GetPhysicalCostBasis{TenantID: string(tenant), ConsolidationID: consolidation})
	if errors.Is(err, phyapi.ErrNotFound) {
		return fin.CostBasis{}, fmt.Errorf("%w: consolidation %s: %v", app.ErrUnknownSource, consolidation, err.Error())
	}
	if err != nil {
		return fin.CostBasis{}, err
	}
	if b.ConsolidationID != consolidation {
		return fin.CostBasis{}, fmt.Errorf("%w: PHY answered for another consolidation", app.ErrUnknownSource)
	}
	out := fin.CostBasis{ConsolidationID: b.ConsolidationID, Revision: b.Revision, Kind: b.Kind,
		OperatingLegalEntityID: b.OperatingLegalEntityID, State: b.State}
	for _, h := range b.Houses {
		out.Houses = append(out.Houses, fin.HouseFacts{ShipmentID: h.ShipmentID, Packs: h.Packs, GrossWeight: h.GrossWeight, Volume: h.Volume,
			LoadedPacks: h.LoadedPacks, LoadedGrossWeight: h.LoadedGrossWeight, LoadedVolume: h.LoadedVolume, LastLoadedAt: h.LastLoadedAt})
	}
	return out, nil
}
