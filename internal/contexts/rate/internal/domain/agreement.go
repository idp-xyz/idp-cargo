package domain

import (
	"fmt"
	"math/bits"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5 part 3b: AGREEMENT quotes (R6, C1 option 3, N1-RATE OD-4 A, OD-5 A). An AGREEMENT quote prices by lane and
// calculation rule, not by fixed lines: it names a DRAFT version of the customer's CUSTOMER SELL card, one entry per
// lane (as a CargoWise quotation carries one rate entry per trade lane), valid exactly over the quote's validity.
// Accepting the quote publishes that version, and later shipments of the customer are rated with it (OD-1 A). The
// margin floor (R1) is checked per lane at a reference quantity against the valid buy rates (OD-4 A).

// QuoteKind is a ONE_OFF quote (fixed lines for one shipment, or the orders C1 allows) or an AGREEMENT.
type QuoteKind string

const (
	KindOneOff    QuoteKind = "ONE_OFF"
	KindAgreement QuoteKind = "AGREEMENT"
)

// ReasonAgreementBelowFloor: a lane of an agreement sells below the margin floor against the valid buy rates at its
// reference quantity, or no buy rate prices it there in full (OD-4).
const ReasonAgreementBelowFloor ApprovalReason = "AGREEMENT_BELOW_FLOOR"

// AgreementRef is the customer card version an AGREEMENT quote prices with.
type AgreementRef struct {
	RateCardID RateCardID
	Version    int
}

// AgreementCheck is the margin check of one lane (entry) of an agreement (OD-4): at At, on the reference cargo, the
// entry's sell and the valid buy rates' cost, each line converted into the quote currency at the quote's rates and
// summed. CostRated is false when no supplier card rated the lane there in full; Cost is then zero.
type AgreementCheck struct {
	EntryNo   int
	At        time.Time
	Sell      kernel.Money
	Cost      kernel.Money
	CostRated bool
}

// ReferenceCargo is the cargo an agreement's lanes are checked on (OD-4): 100 kg chargeable for AIR (the +100 kg
// break), 1 W/M for LCL (one tonne and one cubic metre), with one piece and one document.
func ReferenceCargo(m Mode) Cargo {
	if m == ModeSea {
		return Cargo{Pieces: 1, GrossGrams: 1_000_000, VolumeCM3: 1_000_000, Documents: 1}
	}
	return Cargo{Pieces: 1, GrossGrams: 100_000, Documents: 1}
}

// kind reads a quote stored before kinds existed as ONE_OFF.
func (d QuoteDraft) kind() QuoteKind {
	if d.Kind == "" {
		return KindOneOff
	}
	return d.Kind
}

// validateAgreement checks what an AGREEMENT states: a card version, no lines, a scope of its mode only, and one check
// per lane.
func (d QuoteDraft) validateAgreement() error {
	a, s := d.Agreement, d.Scope
	switch {
	case a == nil || !identifier(string(a.RateCardID)) || a.Version < 1:
		return fmt.Errorf("%w: an AGREEMENT names its customer card version", ErrInvalidQuote)
	case len(d.Lines) > 0:
		return fmt.Errorf("%w: an AGREEMENT prices by its card's lanes, not by lines", ErrInvalidQuote)
	case s == nil || !(s.Mode == ModeAir && s.ContainerMode == ContainerLoose) && !(s.Mode == ModeSea && s.ContainerMode == ContainerLCL),
		s.Origin != "" || s.Destination != "" || s.Via != "" || s.Carrier != "" || s.Cargo != (Cargo{}):
		return fmt.Errorf("%w: an AGREEMENT's scope is its mode, AIR LOOSE or SEA LCL; its lanes are its card's entries", ErrInvalidQuote)
	case len(d.AgreementChecks) == 0:
		return fmt.Errorf("%w: an AGREEMENT is checked lane by lane", ErrInvalidQuote)
	}
	seen := map[int]bool{}
	for _, c := range d.AgreementChecks {
		switch {
		case c.EntryNo < 1 || seen[c.EntryNo] || c.At.IsZero():
			return fmt.Errorf("%w: one check per entry, with its time", ErrInvalidQuote)
		case c.Sell.Currency != d.Currency || c.Cost.Currency != d.Currency || c.Sell.Minor < 0 || c.Cost.Minor < 0 || (!c.CostRated && c.Cost.Minor != 0):
			return fmt.Errorf("%w: entry %d is checked in the quote currency, the cost only when rated", ErrInvalidQuote, c.EntryNo)
		}
		seen[c.EntryNo] = true
	}
	return nil
}

// below reports whether a lane fails the margin floor: no cost rated in full, no sell, or (sell − cost) × 10000 <
// floor × sell.
func (c AgreementCheck) below(floorBP int) bool {
	if !c.CostRated || c.Sell.Minor <= 0 || c.Cost.Minor > c.Sell.Minor {
		return true
	}
	hiMargin, loMargin := bits.Mul64(uint64(c.Sell.Minor-c.Cost.Minor), 10000)
	hiFloor, loFloor := bits.Mul64(uint64(floorBP), uint64(c.Sell.Minor))
	return hiMargin < hiFloor || (hiMargin == hiFloor && loMargin < loFloor)
}

// AgreementVersion checks that a card version can be an agreement for a quote: a DRAFT version, with entries, of a
// CUSTOMER SELL card of the quote's legal entity and customer, valid exactly over the quote's validity, every entry of
// the scope's mode, its codes and service levels publishable. It returns the version.
func (c *RateCard) AgreementVersion(no int, entity LegalEntityID, customer string, validity kernel.ValidityInterval, scope QuoteScope,
	cat Catalogue) (RateCardVersion, error) {
	st := c.state
	if st.Scope != ScopeCustomer || st.Side != SideSell || st.LegalEntityID != entity || st.PartyID != customer {
		return RateCardVersion{}, fmt.Errorf("%w: card %s is not a CUSTOMER card of %s for %s", ErrInvalidQuote, st.RateCardID, customer, entity)
	}
	v, err := c.version(no)
	if err != nil {
		return RateCardVersion{}, fmt.Errorf("%w: %v", ErrInvalidQuote, err)
	}
	switch {
	case v.Status != VersionDraft:
		return RateCardVersion{}, fmt.Errorf("%w: version %d of card %s is %s; an agreement takes a DRAFT", ErrInvalidQuote, no, st.RateCardID, v.Status)
	case !v.loaded || len(v.Entries) == 0:
		return RateCardVersion{}, fmt.Errorf("%w: version %d of card %s has no entry", ErrInvalidQuote, no, st.RateCardID)
	case !v.Validity.From.Equal(validity.From) || !v.Validity.Until.Equal(validity.Until):
		return RateCardVersion{}, fmt.Errorf("%w: version %d of card %s is valid over [%s, %s), not the quote's validity", ErrInvalidQuote, no,
			st.RateCardID, v.Validity.From.Format(time.RFC3339), v.Validity.Until.Format(time.RFC3339))
	}
	for _, e := range v.Entries {
		if e.Mode != scope.Mode || e.ContainerMode != scope.ContainerMode {
			return RateCardVersion{}, fmt.Errorf("%w: entry %d is %s %s, the agreement %s %s", ErrInvalidQuote, e.No, e.Mode, e.ContainerMode, scope.Mode,
				scope.ContainerMode)
		}
	}
	if err = checkPublishable(st, *v, cat); err != nil {
		return RateCardVersion{}, err
	}
	out := *v
	out.Entries = cloneEntries(v.Entries)
	return out, nil
}

// CheckAgreementEntry rates one entry of an agreement's version at the reference cargo (OD-4): its SELL lines from the
// entry alone, its cost from the supplier cards given (the entry's carrier's when it names one, else the lowest total,
// OD-2 A), at the later of at and the entry's start. Each line is converted into the quote currency.
func CheckAgreementEntry(q QuoteDraft, card RateCardState, v RateCardVersion, e RateEntry, buys []PublishedVersion, at time.Time) (AgreementCheck, error) {
	from := AgreementCheckTime(v, e, at)
	in := EvaluationInput{Purpose: PurposeQuoteLine, LegalEntityID: q.LegalEntityID, Sides: []Side{SideSell, SideBuy}, Customer: q.Customer,
		Carrier: e.Carrier, Mode: e.Mode, ContainerMode: e.ContainerMode, Origin: e.Origin, Destination: e.Destination, Via: e.Via, Edition: e.Edition,
		ServiceLevel: e.ServiceLevel, Commodity: e.Commodity, At: from, Cargo: ReferenceCargo(e.Mode)}
	own := PublishedVersion{RateCardID: card.RateCardID, Side: SideSell, Scope: ScopeCustomer, LegalEntityID: card.LegalEntityID, PartyID: card.PartyID,
		No: v.No, Validity: VersionValidity{From: from, Until: v.Validity.Until}, Entries: []RateEntry{e}}
	versions := []PublishedVersion{own}
	for _, b := range buys {
		if b.Side == SideBuy && b.Scope == ScopeSupplier {
			versions = append(versions, b)
		}
	}
	_, lines, err := Evaluate(in, versions)
	if err != nil {
		return AgreementCheck{}, err
	}
	conv, err := newConverter(q.Currency, q.ExchangeRates)
	if err != nil {
		return AgreementCheck{}, err
	}
	check := AgreementCheck{EntryNo: e.No, At: from, Sell: kernel.Money{Currency: q.Currency}, Cost: kernel.Money{Currency: q.Currency}, CostRated: true}
	for _, l := range lines {
		if l.Gap != GapNone {
			if l.Side == SideBuy {
				check.CostRated = false
			}
			continue
		}
		m, err := conv.convert(l.Amount)
		if err != nil {
			return AgreementCheck{}, fmt.Errorf("%w (entry %d)", err, e.No)
		}
		sum := &check.Sell
		if l.Side == SideBuy {
			sum = &check.Cost
		}
		if *sum, err = sum.Add(m); err != nil {
			return AgreementCheck{}, err
		}
	}
	if !check.CostRated {
		check.Cost.Minor = 0
	}
	return check, nil
}

// AgreementCheckTime is when a lane of an agreement is checked: the later of the quote's business time and the
// entry's start, the first instant the lane rates under the agreement.
func AgreementCheckTime(v RateCardVersion, e RateEntry, at time.Time) time.Time {
	if from := e.window(v.Validity).From; from.After(at) {
		return from
	}
	return at
}

// PublishAgreement publishes a CUSTOMER version as its AGREEMENT quote is accepted: the version is still the DRAFT
// the quote names, valid over the quote's validity, publishable, and no other version of the card rates at any
// instant of its validity. The acceptance is the evidence; whoever recorded it publishes.
func (c *RateCard) PublishAgreement(no int, quote QuoteID, validity kernel.ValidityInterval, cat Catalogue, by string, at time.Time) error {
	v, err := c.version(no)
	if err != nil {
		return err
	}
	switch {
	case c.state.Scope != ScopeCustomer:
		return fmt.Errorf("%w: card %s is not a CUSTOMER card", ErrNotPublishable, c.state.RateCardID)
	case v.Status != VersionDraft:
		return fmt.Errorf("%w: version %d of card %s is %s", ErrInvalidTransition, no, c.state.RateCardID, v.Status)
	case !v.loaded || len(v.Entries) == 0:
		return fmt.Errorf("%w: version %d has no entry", ErrNotPublishable, no)
	case !v.Validity.From.Equal(validity.From) || !v.Validity.Until.Equal(validity.Until):
		return fmt.Errorf("%w: version %d is not valid over quote %s's validity", ErrNotPublishable, no, quote)
	case strings.TrimSpace(by) == "" || at.IsZero():
		return fmt.Errorf("%w: a publication needs an actor and a time", ErrInvalidRateCard)
	}
	if err = checkPublishable(c.state, *v, cat); err != nil {
		return err
	}
	for _, o := range c.state.Versions {
		if eff, ok := o.Effective(); o.No != no && ok && eff.overlaps(v.Validity) {
			return fmt.Errorf("%w: version %d", ErrVersionOverlap, o.No)
		}
	}
	v.Status, v.PublishedBy, v.PublishedAt = VersionPublished, by, at.UTC()
	c.mark(no, false)
	c.pending = append(c.pending, Event{Type: EventRateCardVersionPublished, At: v.PublishedAt, TenantID: c.state.TenantID,
		SubjectID: string(c.state.RateCardID), Version: no})
	return nil
}

// agreementReasons is AGREEMENT_BELOW_FLOOR when a lane fails the floor.
func (q *Quote) agreementReasons() []ApprovalReason {
	if slices.ContainsFunc(q.state.AgreementChecks, func(c AgreementCheck) bool { return c.below(q.state.MarginFloorBP) }) {
		return []ApprovalReason{ReasonAgreementBelowFloor}
	}
	return nil
}
