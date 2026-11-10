package application

import (
	"context"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type SetOrderPolicyCommand struct {
	CommandID         string
	Scope             Scope
	LegalEntityID     com.LegalEntityID // empty for the tenant default
	ExpectedVersion   int64             // the version the caller read, 0 for none
	MaxOrdersPerQuote int
	GraceDays         int
}

// SetOrderPolicy records the next version of the order policy of a legal entity, or of the tenant default
// (C1, C2). It takes effect when COM records it; it cannot be backdated.
type SetOrderPolicy struct {
	Deps
	Policies Policies
}

func (h SetOrderPolicy) Handle(ctx context.Context, c SetOrderPolicyCommand) (Receipt, error) {
	if h.Policies == nil || h.Clock == nil {
		return Receipt{}, ErrMissingDependency
	}
	now := h.Clock.Now().UTC().Truncate(time.Microsecond)
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, now, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionSetOrderPolicy, ""); err != nil {
		return Receipt{}, err
	}
	key := string(c.LegalEntityID)
	if key == "" {
		key = TenantPolicyKey
	}
	var result Receipt
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		r, ok, err := h.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
		if err != nil {
			return err
		}
		if ok {
			if r.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			result = r
			return nil
		}
		latest, err := h.Policies.Latest(txCtx, c.Scope.TenantID, c.LegalEntityID)
		if err != nil {
			return err
		}
		setting, err := com.SetOrderPolicy(c.Scope.TenantID, c.LegalEntityID, latest, c.ExpectedVersion, c.MaxOrdersPerQuote, c.GraceDays, c.Scope.ActorID, now)
		if err != nil {
			return err
		}
		if err = h.Policies.Insert(txCtx, setting); err != nil {
			return err
		}
		result = Receipt{CommandID: c.CommandID, ResultType: ResultOrderPolicy, PolicyKey: key, NewRevision: setting.Version, RequestHash: hash}
		return h.Receipts.Save(txCtx, c.Scope.TenantID, result)
	})
	if err != nil {
		return Receipt{}, err
	}
	return result, nil
}
