package application

import (
	"context"
	"fmt"
	"time"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

type ApproveQuotePriceCommand struct {
	CommandID        string
	Scope            Scope
	QuoteID          rate.QuoteID
	ExpectedRevision int64
	ReasonRef        string
	OccurredAt       time.Time
}

// ApproveQuotePrice records the caller's price approval for a quote below its margin floor.
type ApproveQuotePrice struct {
	Deps
}

func (h ApproveQuotePrice) Handle(ctx context.Context, c ApproveQuotePriceCommand) (Receipt, error) {
	if err := h.check(ctx, c.Scope, c.CommandID, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	hash, err := requestHash(c)
	if err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*rate.Quote, int64, error) {
		if err := h.Auth.RequireQuoteAction(txCtx, c.Scope, ActionApproveQuotePrice, c.QuoteID); err != nil {
			return nil, 0, fmt.Errorf("%w: %v", ErrUnauthorized, err)
		}
		quote, revision, err := h.Quotes.Load(txCtx, c.Scope.TenantID, c.QuoteID)
		if err != nil {
			return nil, 0, err
		}
		if revision != c.ExpectedRevision {
			return nil, 0, rate.ErrConflict
		}
		if err = quote.ApprovePrice(c.Scope.ActorID, c.ReasonRef, c.OccurredAt); err != nil {
			return nil, 0, err
		}
		newRevision, err := h.Quotes.Save(txCtx, quote, revision)
		return quote, newRevision, err
	})
}
