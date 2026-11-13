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

// ApproveQuotePrice records the caller's price approval for a quote that needs one. The quote's author may
// approve it only when authorized for ActionApproveOwnQuotePrice (R3).
type ApproveQuotePrice struct {
	Deps
}

func (h ApproveQuotePrice) Handle(ctx context.Context, c ApproveQuotePriceCommand) (Receipt, error) {
	if err := h.check(ctx, c.Scope, c.CommandID, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	// authorized before a replay too; the quote's author approves only with ApproveOwnQuotePrice (R3)
	if err := h.Auth.RequireQuoteAction(ctx, c.Scope, ActionApproveQuotePrice, c.QuoteID); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	var author string
	if err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		quote, _, err := h.Quotes.Load(txCtx, c.Scope.TenantID, c.QuoteID)
		if err == nil {
			author = quote.Author()
		}
		return err
	}); err != nil {
		return Receipt{}, err
	}
	if author == c.Scope.ActorID && h.Auth.RequireQuoteAction(ctx, c.Scope, ActionApproveOwnQuotePrice, c.QuoteID) != nil {
		return Receipt{}, rate.ErrSelfApproval
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
		own := quote.Author() == c.Scope.ActorID && h.Auth.RequireQuoteAction(txCtx, c.Scope, ActionApproveOwnQuotePrice, c.QuoteID) == nil
		if err = quote.ApprovePrice(c.Scope.ActorID, c.ReasonRef, own, c.OccurredAt); err != nil {
			return nil, 0, err
		}
		newRevision, err := h.Quotes.Save(txCtx, quote, revision)
		return quote, newRevision, err
	})
}
