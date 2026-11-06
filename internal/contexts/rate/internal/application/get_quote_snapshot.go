package application

import (
	"context"
	"errors"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

var (
	ErrQuoteNotAccepted     = errors.New("rate application: quote not accepted")
	ErrCustomerNotConfirmed = errors.New("rate application: ORG did not confirm the customer")
)

// QuoteReader reads a quote as one consistent view, without locking it.
type QuoteReader interface {
	Read(context.Context, rate.TenantID, rate.QuoteID) (*rate.Quote, int64, error)
}

// GetQuoteSnapshot returns an accepted quote and its revision; any other quote has no snapshot.
type GetQuoteSnapshot struct {
	Tx     TxRunner
	Quotes QuoteReader
}

func (h GetQuoteSnapshot) Handle(ctx context.Context, tenant rate.TenantID, id rate.QuoteID) (*rate.Quote, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if h.Tx == nil || h.Quotes == nil {
		return nil, 0, ErrMissingDependency
	}
	if tenant == "" {
		return nil, 0, ErrUnauthenticated
	}
	if id == "" {
		return nil, 0, rate.ErrInvalidQuote
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(tenant) {
		return nil, 0, ErrUnauthorized
	}
	var accepted *rate.Quote
	var revision int64
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		quote, rev, err := h.Quotes.Read(txCtx, tenant, id)
		if err != nil {
			return err
		}
		if quote.Status() != rate.QuoteAccepted {
			return ErrQuoteNotAccepted
		}
		accepted, revision = quote, rev
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return accepted, revision, nil
}
