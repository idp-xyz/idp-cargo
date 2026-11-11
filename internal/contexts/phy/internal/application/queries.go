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

// Consolidation reads a consolidation with its live houses.
func (r Reads) Consolidation(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) (*phy.Consolidation, int64, []House, error) {
	if tenant == "" || id == "" {
		return nil, 0, nil, ErrInvalidQuery
	}
	var c *phy.Consolidation
	var revision int64
	var houses []House
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if c, revision, err = r.Consolidations.Read(txCtx, tenant, id); err != nil {
			return err
		}
		houses, err = r.Consolidations.LiveHouses(txCtx, tenant, id)
		return err
	})
	return c, revision, houses, err
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
		out.Loaded, err = r.Lots.Loaded(txCtx, tenant, id)
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

// Evidence reads a shipment's lots and the load executions with lines of it, in one snapshot.
func (r Reads) Evidence(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) ([]LotRead, []ExecutionRead, error) {
	if tenant == "" || shipment == "" {
		return nil, nil, ErrInvalidQuery
	}
	var lots []LotRead
	var executions []ExecutionRead
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if lots, err = r.Lots.ByShipment(txCtx, tenant, shipment); err != nil {
			return err
		}
		executions, err = r.Executions.ByShipment(txCtx, tenant, shipment)
		return err
	})
	return lots, executions, err
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
