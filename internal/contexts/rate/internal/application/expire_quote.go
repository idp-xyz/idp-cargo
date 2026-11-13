package application

import (
	"context"
	"fmt"
	"time"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

type ExpireQuoteCommand struct {
	CommandID        string
	Scope            Scope
	QuoteID          rate.QuoteID
	ExpectedRevision int64
	OccurredAt       time.Time
}

// ExpireQuote ends a quote that was not accepted once its validity has passed (R4). The scheduled job in
// cargo-worker (S2) calls it with its own principal.
type ExpireQuote struct{ Deps }

func (h ExpireQuote) Handle(ctx context.Context, c ExpireQuoteCommand) (Receipt, error) {
	if err := h.check(ctx, c.Scope, c.CommandID, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	// authorized before a replay too
	if err := h.Auth.RequireQuoteAction(ctx, c.Scope, ActionExpireQuote, c.QuoteID); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	hash, err := requestHash(c)
	if err != nil {
		return Receipt{}, err
	}
	if r, ok, err := h.replay(ctx, c.Scope.TenantID, c.CommandID, hash); err != nil || ok {
		return r, err
	}
	if _, err = h.admit(c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*rate.Quote, int64, error) {
		if err := h.Auth.RequireQuoteAction(txCtx, c.Scope, ActionExpireQuote, c.QuoteID); err != nil {
			return nil, 0, fmt.Errorf("%w: %v", ErrUnauthorized, err)
		}
		quote, revision, err := h.Quotes.Load(txCtx, c.Scope.TenantID, c.QuoteID)
		if err != nil {
			return nil, 0, err
		}
		if revision != c.ExpectedRevision {
			return nil, 0, rate.ErrConflict
		}
		if err = quote.Expire(c.OccurredAt); err != nil {
			return nil, 0, err
		}
		newRevision, err := h.Quotes.Save(txCtx, quote, revision)
		return quote, newRevision, err
	})
}
