package application

import (
	"context"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	air "go.idp.xyz/idp-cargo/internal/contexts/air/internal/domain"
)

// Reads answers AIR's queries without locks, each in one snapshot.
type Reads struct {
	Tx        TxRunner
	Movements MovementStore
	Series    SeriesStore
	Numbers   Numbers
	Register  HAWBRegister
}

// Movement reads a movement with its revision.
func (r Reads) Movement(ctx context.Context, tenant air.TenantID, id air.MovementID) (*air.Movement, int64, error) {
	if tenant == "" || id == "" {
		return nil, 0, ErrInvalidQuery
	}
	var m *air.Movement
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		m, revision, err = r.Movements.Read(txCtx, tenant, id)
		return err
	})
	return m, revision, err
}

// ByMAWB reads the movement a MAWB number is assigned to.
func (r Reads) ByMAWB(ctx context.Context, tenant air.TenantID, mawb string) (*air.Movement, int64, error) {
	if tenant == "" || !air.ValidMAWB(mawb) {
		return nil, 0, ErrInvalidQuery
	}
	var m *air.Movement
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		id, found, err := r.Movements.ByMAWB(txCtx, tenant, mawb)
		if err != nil {
			return err
		}
		if !found {
			return bentorepo.ErrNotFound
		}
		m, revision, err = r.Movements.Read(txCtx, tenant, id)
		return err
	})
	return m, revision, err
}

// HouseWaybill finds where a HAWB number of a legal entity belongs.
func (r Reads) HouseWaybill(ctx context.Context, tenant air.TenantID, legalEntity, hawb string) (Registration, error) {
	if tenant == "" || legalEntity == "" || !air.ValidHAWB(hawb) {
		return Registration{}, ErrInvalidQuery
	}
	var out Registration
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		reg, found, err := r.Register.Find(txCtx, tenant, legalEntity, hawb)
		if err != nil {
			return err
		}
		if !found {
			return bentorepo.ErrNotFound
		}
		out = reg
		return nil
	})
	return out, err
}

// SeriesOf reads a legal entity's HAWB series.
func (r Reads) SeriesOf(ctx context.Context, tenant air.TenantID, legalEntity string) (*air.Series, int64, error) {
	if tenant == "" || legalEntity == "" {
		return nil, 0, ErrInvalidQuery
	}
	var s *air.Series
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		s, revision, err = r.Series.Read(txCtx, tenant, legalEntity)
		return err
	})
	return s, revision, err
}
