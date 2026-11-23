package application

import (
	"context"

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// Reads answers PHY's queries from one snapshot each, without locks.
type Reads struct {
	Tx             TxRunner
	Consolidations ConsolidationStore
	Memberships    MembershipStore
	Lots           LotStore
	Executions     ExecutionStore
	Discrepancies  DiscrepancyList
}

// Consolidation reads a consolidation with its live houses and the stuffings of its load units.
func (r Reads) Consolidation(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) (*phy.Consolidation, int64, []House,
	map[int]UnitUse, error) {
	if tenant == "" || id == "" {
		return nil, 0, nil, nil, ErrInvalidQuery
	}
	var c *phy.Consolidation
	var revision int64
	var houses []House
	var uses map[int]UnitUse
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if c, revision, err = r.Consolidations.Read(txCtx, tenant, id); err != nil {
			return err
		}
		if houses, err = r.Consolidations.LiveHouses(txCtx, tenant, id); err != nil {
			return err
		}
		uses, err = r.Executions.UnitUses(txCtx, tenant, id)
		return err
	})
	return c, revision, houses, uses, err
}

// Membership reads a shipment's membership ledger and, when an entry is live, its consolidation.
func (r Reads) Membership(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) (*phy.Membership, int64, *phy.Consolidation, int64, error) {
	if tenant == "" || shipment == "" {
		return nil, 0, nil, 0, ErrInvalidQuery
	}
	var m *phy.Membership
	var c *phy.Consolidation
	var revision, consolRevision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if m, revision, err = r.Memberships.Read(txCtx, tenant, shipment); err != nil {
			return err
		}
		if live, ok := m.Live(); ok {
			c, consolRevision, err = r.Consolidations.Read(txCtx, tenant, live.ConsolidationID)
		}
		return err
	})
	return m, revision, c, consolRevision, err
}

// Open lists OPEN consolidations by planned departure.
func (r Reads) Open(ctx context.Context, tenant phy.TenantID, f ListFilter) ([]Summary, error) {
	if f.Limit == 0 {
		f.Limit = 50
	}
	if tenant == "" || f.Limit < 1 || f.Limit > 200 || (f.Mode != "" && !f.Mode.Valid()) ||
		(!f.DepartureFrom.IsZero() && !f.DepartureUntil.IsZero() && !f.DepartureFrom.Before(f.DepartureUntil)) {
		return nil, ErrInvalidQuery
	}
	var out []Summary
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		out, err = r.Consolidations.ListOpen(txCtx, tenant, f)
		return err
	})
	return out, err
}

// Lot reads a cargo lot with its live loaded packs.
func (r Reads) Lot(ctx context.Context, tenant phy.TenantID, id phy.CargoLotID) (LotRead, error) {
	if tenant == "" || id == "" {
		return LotRead{}, ErrInvalidQuery
	}
	var out LotRead
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if out.Lot, out.Revision, err = r.Lots.Read(txCtx, tenant, id); err != nil {
			return err
		}
		load, err := r.Lots.Loaded(txCtx, tenant, id, "")
		out.Loaded = load.Packs
		return err
	})
	return out, err
}

// Execution reads a load execution.
func (r Reads) Execution(ctx context.Context, tenant phy.TenantID, id phy.LoadExecutionID) (ExecutionRead, error) {
	if tenant == "" || id == "" {
		return ExecutionRead{}, ErrInvalidQuery
	}
	var out ExecutionRead
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		out.Execution, out.Revision, err = r.Executions.Read(txCtx, tenant, id)
		return err
	})
	return out, err
}

// Evidence reads a shipment's lots, the load executions with lines of it and, for stuffings, the limits of the
// units they fill, in one transaction (read committed: each read sees what was committed when it ran).
func (r Reads) Evidence(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) ([]LotRead, []ExecutionRead,
	map[phy.LoadExecutionID]phy.UnitLimits, error) {
	if tenant == "" || shipment == "" {
		return nil, nil, nil, ErrInvalidQuery
	}
	var lots []LotRead
	var executions []ExecutionRead
	units := map[phy.LoadExecutionID]phy.UnitLimits{}
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if lots, err = r.Lots.ByShipment(txCtx, tenant, shipment); err != nil {
			return err
		}
		if executions, err = r.Executions.ByShipment(txCtx, tenant, shipment); err != nil {
			return err
		}
		consols := map[phy.ConsolidationID]*phy.Consolidation{}
		for _, x := range executions {
			st := x.Execution.State()
			if st.LoadUnitNo == 0 {
				continue
			}
			c, ok := consols[st.ConsolidationID]
			if !ok {
				if c, _, err = r.Consolidations.Read(txCtx, tenant, st.ConsolidationID); err != nil {
					return err
				}
				consols[st.ConsolidationID] = c
			}
			if us := c.State().LoadUnits; st.LoadUnitNo <= len(us) {
				units[st.ExecutionID] = us[st.LoadUnitNo-1].Limits
			}
		}
		return nil
	})
	return lots, executions, units, err
}

// CostBasis reads a consolidation, its live houses, the houses that left it, its load executions and the houses' lots
// in one transaction, for PhysicalCostBasis v1 (the caller makes it a snapshot).
func (r Reads) CostBasis(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) (*phy.Consolidation, int64, []House,
	[]FormerHouse, []ExecutionRead, map[phy.ShipmentID][]LotRead, error) {
	if tenant == "" || id == "" {
		return nil, 0, nil, nil, nil, nil, ErrInvalidQuery
	}
	var c *phy.Consolidation
	var revision int64
	var houses []House
	var former []FormerHouse
	var executions []ExecutionRead
	lots := map[phy.ShipmentID][]LotRead{}
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if c, revision, err = r.Consolidations.Read(txCtx, tenant, id); err != nil {
			return err
		}
		if houses, err = r.Consolidations.LiveHouses(txCtx, tenant, id); err != nil {
			return err
		}
		if former, err = r.Consolidations.FormerHouses(txCtx, tenant, id); err != nil {
			return err
		}
		// An entry ended while the consolidation was OPEN, or by the shipment's cancellation, names no rework even when
		// a rework unloaded the house: the reversal lines say which, those PHY entered since the entry began.
		for i, f := range former {
			if f.Entry.ReworkNo != 0 {
				continue
			}
			if former[i].Reversal, former[i].Unloaded, err = r.Executions.HouseReworkSince(txCtx, tenant, id, f.ShipmentID, f.Entry.EnteredAt); err != nil {
				return err
			}
		}
		if executions, err = r.Executions.ByConsolidation(txCtx, tenant, id); err != nil {
			return err
		}
		for i, h := range houses {
			if lots[h.ShipmentID], err = r.Lots.ByShipment(txCtx, tenant, h.ShipmentID); err != nil {
				return err
			}
			// The rework that last unloaded a live house since its entry began, by the reads that name it for a house
			// that left (V2.95): in the same snapshot.
			if houses[i].Reversal, houses[i].Unloaded, err = r.Executions.HouseReworkSince(txCtx, tenant, id, h.ShipmentID, h.Entry.EnteredAt); err != nil {
				return err
			}
		}
		return nil
	})
	return c, revision, houses, former, executions, lots, err
}

// OpenDiscrepancies lists open discrepancies by the time they were raised.
func (r Reads) OpenDiscrepancies(ctx context.Context, tenant phy.TenantID, f DiscrepancyFilter) ([]OpenDiscrepancy, error) {
	if f.Limit == 0 {
		f.Limit = 50
	}
	if tenant == "" || f.Limit < 1 || f.Limit > 200 {
		return nil, ErrInvalidQuery
	}
	var out []OpenDiscrepancy
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		out, err = r.Discrepancies.ListOpen(txCtx, tenant, f)
		return err
	})
	return out, err
}
