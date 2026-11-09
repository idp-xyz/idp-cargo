package application

import (
	"context"
	"strings"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

// GetShipmentPlan reads a shipment for ShipmentPlanSnapshot v1, without locking it.
type GetShipmentPlan struct {
	Tx        TxRunner
	Shipments ShipmentStore
}

func (h GetShipmentPlan) Handle(ctx context.Context, tenant ful.TenantID, id ful.ShipmentID) (*ful.Shipment, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if h.Tx == nil || h.Shipments == nil {
		return nil, 0, ErrMissingDependency
	}
	if tenant == "" {
		return nil, 0, ErrUnauthenticated
	}
	if strings.TrimSpace(string(id)) == "" {
		return nil, 0, ErrInvalidQuery
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(tenant) {
		return nil, 0, ErrUnauthorized
	}
	var shipment *ful.Shipment
	var revision int64
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		shipment, revision, err = h.Shipments.Read(txCtx, tenant, id)
		return err
	})
	return shipment, revision, err
}
