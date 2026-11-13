package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
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
	// V0.5 part 3b: an AGREEMENT names the DRAFT customer card version it prices with, and no lines.
	Kind       rate.QuoteKind     `json:",omitzero"`
	Agreement  *rate.AgreementRef `json:",omitzero"`
	OccurredAt time.Time
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
	Customers  Customers
	Margins    MarginPolicy
	Codes      ChargeCodeStore
	Snapshots  SnapshotStore
	Cards      RateCardStore   // AGREEMENT quotes
	Agreements AgreementQuotes // AGREEMENT quotes
}

func (h CreateQuote) Handle(ctx context.Context, c CreateQuoteCommand) (Receipt, error) {
	if err := h.check(ctx, c.Scope, c.CommandID, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	if h.Customers == nil || h.Margins == nil || h.Codes == nil || h.Snapshots == nil {
		return Receipt{}, ErrMissingDependency
	}
	if c.Kind == rate.KindOneOff { // the default kind hashes as an omitted one, so a retry that names it replays
		c.Kind = ""
	}
	// authorized before a replay too: a replay answers only an actor still allowed, and a key in use tells no one else
	if err := h.Auth.RequireQuoteAction(ctx, c.Scope, ActionCreateQuote, c.QuoteID); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if c.Kind == rate.KindAgreement && c.Agreement != nil {
		// an agreement fixes the customer's draft until it expires: only one who maintains sell rates may name it
		if err := h.Auth.RequireQuoteAction(ctx, c.Scope, ActionMaintainSellRates, rate.QuoteID(c.Agreement.RateCardID)); err != nil {
			return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
		}
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
	if c.Kind == rate.KindAgreement && (h.Cards == nil || h.Agreements == nil) {
		return Receipt{}, ErrMissingDependency
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
	if threshold < 0 || threshold > 10000 {
		return Receipt{}, fmt.Errorf("%w: the rate card discount threshold of %s is %d basis points, not 0 to 10000", rate.ErrInvalidQuote,
			c.LegalEntityID, threshold)
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (*rate.Quote, int64, error) {
		draft := rate.QuoteDraft{TenantID: c.Scope.TenantID, QuoteID: c.QuoteID, LegalEntityID: c.LegalEntityID, Customer: customer, Recipient: recipient,
			Currency: c.Currency, Lines: append([]rate.Line(nil), c.Lines...), ExchangeRates: c.ExchangeRates, Terms: c.Terms, Validity: c.Validity,
			MarginFloorBP: floor, Author: c.Scope.ActorID, Scope: c.QuoteScope, DiscountThresholdBP: threshold, Kind: c.Kind, Agreement: c.Agreement}
		price := h.priceLines
		if c.Kind == rate.KindAgreement {
			price = h.priceAgreement
		}
		if err := price(txCtx, &draft, c.OccurredAt); err != nil {
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
// before the quote, for no other subject than the quote. A snapshot's SELL line prices one line of the quote. A quote
// without a scope takes the lane and cargo of its rated lines' snapshot.
func (h CreateQuote) priceLines(ctx context.Context, d *rate.QuoteDraft, at time.Time) error {
	codes, err := h.Codes.List(ctx, d.TenantID)
	if err != nil {
		return err
	}
	type snapshotLine struct {
		snapshot string
		line     int
	}
	snapshots := map[string]rate.RateSnapshotState{}
	used := map[snapshotLine]bool{}
	for i := range d.Lines {
		l := &d.Lines[i]
		code, ok := listed(codes).ChargeCode(l.ChargeCode)
		switch {
		case !ok:
			return fmt.Errorf("%w: %s", rate.ErrChargeCodeUnknown, l.ChargeCode)
		case !code.Active:
			return fmt.Errorf("%w: %s is inactive", rate.ErrChargeCodeUnknown, l.ChargeCode)
		}
		version := code.Version
		l.CodeVersion = &version
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
			switch {
			case errors.Is(err, bentorepo.ErrNotFound):
				return fmt.Errorf("%w: snapshot %s is not recorded", rate.ErrInvalidQuote, l.Rated.SnapshotID)
			case err != nil:
				return fmt.Errorf("rate: reading snapshot %s: %w", l.Rated.SnapshotID, err)
			}
			st = s.State()
			if err = h.fits(d, st, at); err != nil {
				return err
			}
			snapshots[l.Rated.SnapshotID] = st
		}
		rated := *l.Rated
		rated.SellLineNo, rated.CostLineNo = 0, 0
		var sell rate.SnapshotLine
		for _, sl := range st.Lines {
			if sl.ChargeCode != l.ChargeCode || sl.Gap != rate.GapNone {
				continue
			}
			if sl.Side == rate.SideSell {
				sell, rated.SellLineNo, rated.RatedSell = sl, sl.LineNo, sl.Amount
			} else {
				rated.CostLineNo, l.Cost = sl.LineNo, sl.Amount
			}
		}
		if rated.SellLineNo == 0 {
			return fmt.Errorf("%w: snapshot %s rated no sell of %s", rate.ErrInvalidQuote, rated.SnapshotID, l.ChargeCode)
		}
		key := snapshotLine{rated.SnapshotID, rated.SellLineNo}
		if used[key] {
			return fmt.Errorf("%w: line %d of snapshot %s prices one line of a quote", rate.ErrInvalidQuote, rated.SellLineNo, rated.SnapshotID)
		}
		used[key] = true
		if l.Source == rate.SourceRated {
			l.Sell = rated.RatedSell
		}
		// rate on quantity is the sell only for a RATED line that no minimum or maximum bounded; any other is a lump sum
		l.Basis = rate.LineBasis{Kind: rate.BasisLumpSum}
		if l.Source == rate.SourceRated && sell.Calculator == rate.CalcPerUnit && !sell.MinimumApplied && !sell.MaximumApplied {
			l.Basis = rate.LineBasis{Kind: rate.BasisPerUnit, Unit: sell.Unit, Rate: sell.Rate, Quantity: sell.Charged}
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
	case in.SubjectKind != "" && (in.SubjectKind != "QUOTE" || in.SubjectID != string(d.QuoteID)):
		return fmt.Errorf("%w: snapshot %s was recorded for %s %s", rate.ErrInvalidQuote, st.SnapshotID, in.SubjectKind, in.SubjectID)
	case *d.Scope != scope || in.ServiceLevel != d.Terms.ServiceLevel:
		return fmt.Errorf("%w: snapshot %s rates another lane, cargo or service level than the quote", rate.ErrInvalidQuote, st.SnapshotID)
	case in.At.After(at) || at.Sub(in.At) > h.Window.MaxBackdate:
		return fmt.Errorf("%w: snapshot %s was rated at %s, not within the record window before the quote", rate.ErrInvalidQuote, st.SnapshotID,
			in.At.Format(time.RFC3339))
	}
	return nil
}

// priceAgreement completes an AGREEMENT: its version is a DRAFT of the customer's CUSTOMER card, valid over the quote's
// validity, every entry of the scope's mode and publishable;
// each lane is checked at its reference quantity against the supplier cards valid when it first rates (OD-4). The card
// row stays locked, so the draft's entries do not change before the quote commits.
func (h CreateQuote) priceAgreement(ctx context.Context, d *rate.QuoteDraft, at time.Time) error {
	if d.Agreement == nil || d.Scope == nil {
		return fmt.Errorf("%w: an AGREEMENT names its customer card version and its mode", rate.ErrInvalidQuote)
	}
	codes, err := h.Codes.List(ctx, d.TenantID)
	if err != nil {
		return err
	}
	card, _, err := h.Cards.Load(ctx, d.TenantID, d.Agreement.RateCardID)
	if errors.Is(err, bentorepo.ErrNotFound) {
		return fmt.Errorf("%w: rate card %s is not recorded", rate.ErrInvalidQuote, d.Agreement.RateCardID)
	}
	if err != nil {
		return err
	}
	v, err := card.AgreementVersion(d.Agreement.Version, d.LegalEntityID, d.Customer.PartyID, d.Validity, *d.Scope, listed(codes))
	if err != nil {
		return err
	}
	others, err := customerVersions(ctx, h.Cards, h.Agreements, d.TenantID, d.LegalEntityID, d.Customer.PartyID)
	if err != nil {
		return err
	}
	if err = rate.CheckCustomerLanes(d.Agreement.RateCardID, v, others); err != nil {
		return err
	}
	st := card.State()
	d.AgreementChecks = nil
	for _, e := range v.Entries {
		buys, err := h.Cards.Published(ctx, d.TenantID, d.LegalEntityID, rate.AgreementCheckTime(v, e, at),
			Lane{Mode: e.Mode, ContainerMode: e.ContainerMode, Origin: e.Origin, Destination: e.Destination})
		if err != nil {
			return err
		}
		check, err := rate.CheckAgreementEntry(*d, st, v, e, buys, at)
		if err != nil {
			return err
		}
		d.AgreementChecks = append(d.AgreementChecks, check)
	}
	return nil
}

// customerVersions are the versions of the customer's CUSTOMER cards of the legal entity that rate, or that are DRAFTs
// an AGREEMENT quote that has not expired names, with their entries.
func customerVersions(ctx context.Context, cards RateCardStore, agreements AgreementQuotes, tenant rate.TenantID, entity rate.LegalEntityID,
	party string) ([]rate.CustomerVersion, error) {
	states, _, err := cards.List(ctx, tenant, CardFilter{Side: rate.SideSell, Scope: rate.ScopeCustomer, LegalEntityID: entity, PartyID: party})
	if err != nil {
		return nil, err
	}
	var out []rate.CustomerVersion
	for _, st := range states {
		for _, v := range st.Versions {
			window, ok := v.Effective()
			if !ok && v.Status == rate.VersionDraft {
				if _, ok, err = agreements.QuotedBy(ctx, tenant, st.RateCardID, v.No); err != nil {
					return nil, err
				}
				window = v.Validity
			}
			if !ok {
				continue
			}
			entries, err := cards.Entries(ctx, tenant, st.RateCardID, v.No)
			if err != nil {
				return nil, err
			}
			out = append(out, rate.CustomerVersion{RateCardID: st.RateCardID, No: v.No, Window: window, Entries: entries})
		}
	}
	return out, nil
}
