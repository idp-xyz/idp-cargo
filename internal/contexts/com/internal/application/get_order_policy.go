package application

import (
	"context"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

// GetOrderPolicy returns the order policy an order of a legal entity follows (C1, C2).
type GetOrderPolicy struct {
	Tx       TxRunner
	Policies Policies
}

func (h GetOrderPolicy) Handle(ctx context.Context, tenant com.TenantID, entity com.LegalEntityID) (com.OrderPolicy, error) {
	if err := ctx.Err(); err != nil {
		return com.OrderPolicy{}, err
	}
	if h.Tx == nil || h.Policies == nil {
		return com.OrderPolicy{}, ErrMissingDependency
	}
	if tenant == "" {
		return com.OrderPolicy{}, ErrUnauthenticated
	}
	if entity == "" {
		return com.OrderPolicy{}, ErrInvalidQuery
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(tenant) {
		return com.OrderPolicy{}, ErrUnauthorized
	}
	var policy com.OrderPolicy
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		policy, err = h.Policies.InForce(txCtx, tenant, entity)
		return err
	})
	return policy, err
}
