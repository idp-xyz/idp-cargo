package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

var ErrInvalidQuery = errors.New("com application: invalid query")

// ObligationReader reads the order that holds one version of an obligation, as one consistent view and
// without locking it.
type ObligationReader interface {
	ReadObligation(ctx context.Context, tenant com.TenantID, id com.ObligationID, version int64) (*com.CustomerOrder, error)
}

// GetObligationSnapshot returns one version of an obligation and the order that holds it.
type GetObligationSnapshot struct {
	Tx     TxRunner
	Orders ObligationReader
}

func (h GetObligationSnapshot) Handle(ctx context.Context, tenant com.TenantID, id com.ObligationID, version int64) (*com.CustomerOrder, com.Obligation, error) {
	if err := ctx.Err(); err != nil {
		return nil, com.Obligation{}, err
	}
	if h.Tx == nil || h.Orders == nil {
		return nil, com.Obligation{}, ErrMissingDependency
	}
	if tenant == "" {
		return nil, com.Obligation{}, ErrUnauthenticated
	}
	if strings.TrimSpace(string(id)) == "" || version < 1 {
		return nil, com.Obligation{}, ErrInvalidQuery
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(tenant) {
		return nil, com.Obligation{}, ErrUnauthorized
	}
	var order *com.CustomerOrder
	var obligation com.Obligation
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		o, err := h.Orders.ReadObligation(txCtx, tenant, id, version)
		if err != nil {
			return err
		}
		ob, ok := o.Obligation(id, version)
		if !ok {
			return fmt.Errorf("com application: order %s does not hold obligation %s version %d", o.ID(), id, version)
		}
		order, obligation = o, ob
		return nil
	})
	if err != nil {
		return nil, com.Obligation{}, err
	}
	return order, obligation, nil
}
