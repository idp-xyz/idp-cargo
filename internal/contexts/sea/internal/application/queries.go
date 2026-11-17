package application

import (
	"context"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
)

// Reads answers SEA's queries without locks, each in one snapshot.
type Reads struct {
	Tx        TxRunner
	Movements MovementStore
	Visits    VisitStore
	Series    SeriesStore
	Numbers   Numbers
	Register  HBLRegister
}

// MovementRead is a movement as read, with its revision and its container visits in the order they were assigned.
type MovementRead struct {
	Movement *sea.Movement
	Revision int64
	Visits   []VisitRead
}

// Movement reads a movement with its revision and its container visits.
func (r Reads) Movement(ctx context.Context, tenant sea.TenantID, id sea.MovementID) (MovementRead, error) {
	if tenant == "" || id == "" {
		return MovementRead{}, ErrInvalidQuery
	}
	var out MovementRead
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		out, err = r.movement(txCtx, tenant, id)
		return err
	})
	return out, err
}

func (r Reads) movement(txCtx context.Context, tenant sea.TenantID, id sea.MovementID) (MovementRead, error) {
	m, revision, err := r.Movements.Read(txCtx, tenant, id)
	if err != nil {
		return MovementRead{}, err
	}
	visits, err := r.visitsOf(txCtx, tenant, id)
	return MovementRead{Movement: m, Revision: revision, Visits: visits}, err
}

// ByMasterBill reads the movement a number is the current master bill reference of, for an issuer.
func (r Reads) ByMasterBill(ctx context.Context, tenant sea.TenantID, issuer, number string) (MovementRead, error) {
	number = sea.NormalizeBillNumber(number)
	if tenant == "" || issuer == "" || !sea.ValidBillNumber(number) {
		return MovementRead{}, ErrInvalidQuery
	}
	var out MovementRead
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		id, found, err := r.Movements.ByMasterBill(txCtx, tenant, issuer, number)
		if err != nil {
			return err
		}
		if !found {
			return bentorepo.ErrNotFound
		}
		out, err = r.movement(txCtx, tenant, id)
		return err
	})
	return out, err
}

// HouseBill finds where an HBL number of a legal entity belongs.
func (r Reads) HouseBill(ctx context.Context, tenant sea.TenantID, legalEntity, hbl string) (Registration, error) {
	if tenant == "" || legalEntity == "" || !sea.ValidHBL(hbl) {
		return Registration{}, ErrInvalidQuery
	}
	var out Registration
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		reg, found, err := r.Register.Find(txCtx, tenant, legalEntity, hbl)
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

// SeriesOf reads a legal entity's HBL series.
func (r Reads) SeriesOf(ctx context.Context, tenant sea.TenantID, legalEntity string) (*sea.Series, int64, error) {
	if tenant == "" || legalEntity == "" {
		return nil, 0, ErrInvalidQuery
	}
	var s *sea.Series
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		s, revision, err = r.Series.Read(txCtx, tenant, legalEntity)
		return err
	})
	return s, revision, err
}
