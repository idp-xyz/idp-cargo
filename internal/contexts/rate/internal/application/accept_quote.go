package application

import (
	"context"
	"fmt"
	"time"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

type AcceptQuoteCommand struct {
	CommandID        string
	Scope            Scope
	QuoteID          rate.QuoteID
	ExpectedRevision int64
	EvidenceKind     rate.EvidenceKind
	EvidenceRef      string
	OccurredAt       time.Time
}

// AcceptQuote records the customer's acceptance at its business time, with typed evidence (R5), and the
// time RATE records it (T1). ORG must still confirm the recipient role at that instant; the accepted quote
// is frozen.
type AcceptQuote struct {
	Deps
	Customers Customers
	Codes     ChargeCodeStore // AGREEMENT quotes
	Cards     RateCardStore   // AGREEMENT quotes
}

func (h AcceptQuote) Handle(ctx context.Context, c AcceptQuoteCommand) (Receipt, error) {
	if err := h.check(ctx, c.Scope, c.CommandID, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	if h.Customers == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, err := requestHash(c)
	if err != nil {
		return Receipt{}, err
	}
	if r, ok, err := h.replay(ctx, c.Scope.TenantID, c.CommandID, hash); err != nil || ok {
		return r, err
	}
	recorded, err := h.admit(c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	if err = h.Auth.RequireQuoteAction(ctx, c.Scope, ActionAcceptQuote, c.QuoteID); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	var priced rate.QuoteState
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		quote, _, err := h.Quotes.Load(txCtx, c.Scope.TenantID, c.QuoteID)
		if err == nil {
			priced = quote.State()
		}
		return err
	})
	if err != nil {
		return Receipt{}, err
	}
	customer, err := h.Customers.VerifyRecipient(ctx, c.Scope.TenantID, priced.Customer.PartyID, priced.LegalEntityID, priced.Recipient, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*rate.Quote, int64, error) {
		quote, revision, err := h.Quotes.Load(txCtx, c.Scope.TenantID, c.QuoteID)
		if err != nil {
			return nil, 0, err
		}
		if revision != c.ExpectedRevision {
			return nil, 0, rate.ErrConflict
		}
		if err = quote.Accept(c.Scope.ActorID, c.EvidenceKind, c.EvidenceRef, customer.PartyRevision, c.OccurredAt, recorded); err != nil {
			return nil, 0, err
		}
		if quote.Kind() == rate.KindAgreement {
			if err = h.publishAgreement(txCtx, c.Scope, quote, recorded); err != nil {
				return nil, 0, err
			}
		}
		newRevision, err := h.Quotes.Save(txCtx, quote, revision)
		return quote, newRevision, err
	})
}

// publishAgreement publishes the customer card version of an accepted AGREEMENT in the acceptance's transaction, with
// the acceptance as its evidence (RB16): whoever recorded the acceptance publishes it, at the recording time. The
// version keeps the validity the customer accepted; rating uses it only at business times within the record window.
func (h AcceptQuote) publishAgreement(ctx context.Context, s Scope, q *rate.Quote, recorded time.Time) error {
	if h.Codes == nil || h.Cards == nil {
		return ErrMissingDependency
	}
	st := q.State()
	codes, err := h.Codes.List(ctx, st.TenantID)
	if err != nil {
		return err
	}
	card, revision, err := h.Cards.Load(ctx, st.TenantID, st.Agreement.RateCardID)
	if err != nil {
		return err
	}
	if err = card.PublishAgreement(st.Agreement.Version, st.QuoteID, st.Validity, listed(codes), s.ActorID, recorded); err != nil {
		return err
	}
	if _, err = h.Cards.Save(ctx, card, revision); err != nil {
		return err
	}
	for _, e := range card.Pending() {
		if err = h.Outbox.EnqueueRATEEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
