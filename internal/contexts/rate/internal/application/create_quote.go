package application

import (
	"context"
	"fmt"
	"time"

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type CreateQuoteCommand struct {
	CommandID       string
	Scope           Scope
	QuoteID         rate.QuoteID
	LegalEntityID   rate.LegalEntityID
	CustomerPartyID string
	Recipient       rate.RecipientRole `json:",omitzero"` // empty: CUSTOMER
	Currency        kernel.CurrencyCode
	Lines           []rate.Line
	ExchangeRates   []rate.ExchangeRate  `json:",omitzero"`
	Terms           rate.CommercialTerms `json:",omitzero"`
	Validity        kernel.ValidityInterval
	QuoteScope      *rate.QuoteScope `json:",omitzero"`
	OccurredAt      time.Time
}

// DiscountPolicy supplies how far below the rated sell, in basis points of it, an overridden quote line of a legal
// entity may sell before the quote needs approval (RATE_CARD_DISCOUNT). A MarginPolicy without it allows none.
type DiscountPolicy interface {
	RateCardDiscountBP(ctx context.Context, tenant rate.TenantID, entity rate.LegalEntityID) (int, error)
}

// CreateQuote prices a quote for a party that ORG confirms in the recipient role (R7) for the legal
// entity.
type CreateQuote struct {
	Deps
	Customers Customers
	Margins   MarginPolicy
	Codes     ChargeCodeStore
	Snapshots SnapshotStore
}

func (h CreateQuote) Handle(ctx context.Context, c CreateQuoteCommand) (Receipt, error) {
	if err := h.check(ctx, c.Scope, c.CommandID, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	if h.Customers == nil || h.Margins == nil || h.Codes == nil || h.Snapshots == nil {
		return Receipt{}, ErrMissingDependency
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
	if err = h.Auth.RequireQuoteAction(ctx, c.Scope, ActionCreateQuote, c.QuoteID); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	recipient := c.Recipient
	if recipient == "" {
		recipient = rate.RecipientCustomer
	}
	if !recipient.Valid() {
		return Receipt{}, fmt.Errorf("%w: recipient role %q", rate.ErrInvalidQuote, recipient)
	}
	customer, err := h.Customers.VerifyRecipient(ctx, c.Scope.TenantID, c.CustomerPartyID, c.LegalEntityID, recipient, c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	floor, err := h.Margins.MarginFloorBP(ctx, c.Scope.TenantID, c.LegalEntityID)
	if err != nil {
		return Receipt{}, err
	}
	threshold := 0
	if p, ok := h.Margins.(DiscountPolicy); ok {
		if threshold, err = p.RateCardDiscountBP(ctx, c.Scope.TenantID, c.LegalEntityID); err != nil {
			return Receipt{}, err
		}
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*rate.Quote, int64, error) {
		draft := rate.QuoteDraft{TenantID: c.Scope.TenantID, QuoteID: c.QuoteID, LegalEntityID: c.LegalEntityID, Customer: customer, Recipient: recipient,
			Currency: c.Currency, Lines: append([]rate.Line(nil), c.Lines...), ExchangeRates: c.ExchangeRates, Terms: c.Terms, Validity: c.Validity,
			MarginFloorBP: floor, Author: c.Scope.ActorID, Scope: c.QuoteScope, DiscountThresholdBP: threshold}
		if err := h.priceLines(txCtx, &draft, c.OccurredAt); err != nil {
			return nil, 0, err
		}
		quote, err := rate.PriceQuote(draft, c.OccurredAt)
		if err != nil {
			return nil, 0, err
		}
		revision, err := h.Quotes.Insert(txCtx, quote)
		return quote, revision, err
	})
}

// priceLines completes the draft's lines: every code is a listed, active code of the catalogue (applying to the scope's
// mode), and a rated line takes its rated sell, its cost when a BUY line priced it and its basis from a RateSnapshot of
// purpose QUOTE_LINE recorded for the quote's legal entity, customer, scope and service level within the record window
// before the quote. A quote without a scope takes the lane and cargo of its rated lines' snapshot.
func (h CreateQuote) priceLines(ctx context.Context, d *rate.QuoteDraft, at time.Time) error {
	codes, err := h.Codes.List(ctx, d.TenantID)
	if err != nil {
		return err
	}
	snapshots := map[string]rate.RateSnapshotState{}
	for i := range d.Lines {
		l := &d.Lines[i]
		code, ok := listed(codes).ChargeCode(l.ChargeCode)
		switch {
		case !ok:
			return fmt.Errorf("%w: %s", rate.ErrChargeCodeUnknown, l.ChargeCode)
		case !code.Active:
			return fmt.Errorf("%w: charge code %s is inactive", rate.ErrInvalidQuote, l.ChargeCode)
		}
		l.CodeVersion = code.Version
		if l.Source == "" {
			l.Source = rate.SourceManual
		}
		if l.Source == rate.SourceManual {
			continue
		}
		if l.Rated == nil || l.Rated.SnapshotID == "" {
			return fmt.Errorf("%w: a rated line %s names its snapshot", rate.ErrInvalidQuote, l.ChargeCode)
		}
		st, ok := snapshots[l.Rated.SnapshotID]
		if !ok {
			s, err := h.Snapshots.Read(ctx, d.TenantID, l.Rated.SnapshotID)
			if err != nil {
				return fmt.Errorf("%w: snapshot %s: %v", rate.ErrInvalidQuote, l.Rated.SnapshotID, err)
			}
			st = s.State()
			if err = h.fits(d, st, at); err != nil {
				return err
			}
			snapshots[l.Rated.SnapshotID] = st
		}
		rated := *l.Rated
		rated.SellLineNo, rated.CostLineNo = 0, 0
		for _, sl := range st.Lines {
			if sl.ChargeCode != l.ChargeCode || sl.Gap != rate.GapNone {
				continue
			}
			if sl.Side == rate.SideSell {
				rated.SellLineNo, rated.RatedSell = sl.LineNo, sl.Amount
				l.Basis = rate.LineBasis{Kind: rate.BasisLumpSum}
				if sl.Calculator == rate.CalcPerUnit {
					l.Basis = rate.LineBasis{Kind: rate.BasisPerUnit, Unit: sl.Unit, Rate: sl.Rate, Quantity: sl.Charged}
				}
			} else {
				rated.CostLineNo, l.Cost = sl.LineNo, sl.Amount
			}
		}
		if rated.SellLineNo == 0 {
			return fmt.Errorf("%w: snapshot %s rated no sell of %s", rate.ErrInvalidQuote, rated.SnapshotID, l.ChargeCode)
		}
		if l.Source == rate.SourceRated {
			l.Sell = rated.RatedSell
		}
		l.Rated = &rated
	}
	if d.Scope != nil {
		for _, l := range d.Lines {
			if code, _ := listed(codes).ChargeCode(l.ChargeCode); !code.AppliesTo(d.Scope.Mode) {
				return fmt.Errorf("%w: charge code %s does not apply to %s", rate.ErrInvalidQuote, l.ChargeCode, d.Scope.Mode)
			}
		}
	}
	return nil
}

// fits checks that a snapshot was recorded for the quote, and gives a quote without a scope or service level those of
// the snapshot.
func (h CreateQuote) fits(d *rate.QuoteDraft, st rate.RateSnapshotState, at time.Time) error {
	in := st.Input
	scope := rate.QuoteScope{Mode: in.Mode, ContainerMode: in.ContainerMode, Origin: in.Origin, Destination: in.Destination, Via: in.Via,
		Carrier: in.Carrier, Cargo: in.Cargo}
	if d.Scope == nil {
		d.Scope = &scope
	}
	if d.Terms.ServiceLevel == "" {
		d.Terms.ServiceLevel = in.ServiceLevel
	}
	switch {
	case in.Purpose != rate.PurposeQuoteLine || in.LegalEntityID != d.LegalEntityID || in.Customer.PartyID != d.Customer.PartyID:
		return fmt.Errorf("%w: snapshot %s rates another purpose, legal entity or customer", rate.ErrInvalidQuote, st.SnapshotID)
	case *d.Scope != scope || in.ServiceLevel != d.Terms.ServiceLevel:
		return fmt.Errorf("%w: snapshot %s rates another lane, cargo or service level than the quote", rate.ErrInvalidQuote, st.SnapshotID)
	case in.At.After(at) || at.Sub(in.At) > h.Window.MaxBackdate:
		return fmt.Errorf("%w: snapshot %s was rated at %s, not within the record window before the quote", rate.ErrInvalidQuote, st.SnapshotID,
			in.At.Format(time.RFC3339))
	}
	return nil
}
