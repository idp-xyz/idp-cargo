package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5: rate cards (R6). A card belongs to a legal entity and a side: SELL tariffs for any customer, SELL
// customer cards of one customer (published by accepting an AGREEMENT quote), and BUY supplier cards of one
// carrier, co-loader or overseas agent. Prices change only by a new version: a published version never changes,
// and no two published versions of a card overlap in validity. A version holds entries (lanes), each with lines
// (one per charge code) and weight breaks.

type RateCardID string

// Side is whether a card sells to customers or buys from suppliers.
type Side string

const (
	SideSell Side = "SELL"
	SideBuy  Side = "BUY"
)

func (s Side) Valid() bool { return s == SideSell || s == SideBuy }

// CardScope is whose rates a card holds.
type CardScope string

const (
	ScopeTariff   CardScope = "TARIFF"   // SELL: the company tariff for any customer of the legal entity
	ScopeCustomer CardScope = "CUSTOMER" // SELL: client rates of one customer, from an accepted AGREEMENT quote
	ScopeSupplier CardScope = "SUPPLIER" // BUY: the rates of one carrier, co-loader or overseas agent
)

func (s CardScope) sideOf() Side {
	switch s {
	case ScopeTariff, ScopeCustomer:
		return SideSell
	case ScopeSupplier:
		return SideBuy
	}
	return ""
}

// VersionStatus is where a version of a card stands.
type VersionStatus string

const (
	VersionDraft     VersionStatus = "DRAFT"
	VersionPublished VersionStatus = "PUBLISHED"
	VersionWithdrawn VersionStatus = "WITHDRAWN"
)

// ContainerMode is how cargo moves within its mode: loose for AIR, LCL for SEA (FCL is out of S2).
type ContainerMode string

const (
	ContainerLoose ContainerMode = "LOOSE"
	ContainerLCL   ContainerMode = "LCL"
)

// CommodityGeneral is the only commodity class S2 rates.
const CommodityGeneral = "GENERAL"

// UnitRate is an amount per unit in minor units of its currency times 10^-Scale, so rates finer than a minor unit
// (CNY 12.355 per kg is 12355 at scale 1) need no knowledge of the currency's minor digits.
type UnitRate struct {
	Minor int64
	Scale int
}

// RateBreak is a band of a PER_UNIT line: from From base units on, Rate (at the line's scale) applies.
type RateBreak struct {
	From int64
	Rate int64
}

// RateLine prices one charge code of an entry.
//
// FLAT: Rate is the amount per shipment. PER_UNIT: Rate is per unit of Unit for quantities below the first break;
// quantities are rounded up to Step base units and raised to MinQuantity; KG_CHARGEABLE takes the greater of the
// gross weight and the volume divided by VolumeFactor (cubic centimetres per kilogram), FREIGHT_TONNE the greater of
// gross tonnes and cubic metres times 10^6 / VolumeFactor (cubic centimetres per tonne). PERCENT_OF: PercentBP basis
// points of the amount of BaseCode in the same evaluation and side, in the same currency only. Minimum and Maximum
// (0: none) bound the amount, in minor units. A CallForPricing line has no rate: rating reports a gap.
type RateLine struct {
	No             int
	ChargeCode     ChargeCode
	Currency       kernel.CurrencyCode
	Calculator     Calculator
	Unit           RateUnit
	Rate           UnitRate
	Minimum        int64
	Maximum        int64
	Step           int64
	MinQuantity    int64
	VolumeFactor   int64
	PercentBP      int64
	BaseCode       ChargeCode
	CallForPricing bool
	Breaks         []RateBreak
}

// RateEntry is a lane of a version. Origin, Destination and Via are UN/LOCODEs resolved for the mode against the
// location reference data edition Edition. Carrier is an optional carrier party of a SELL entry; a BUY entry's
// carrier is the card's supplier. Validity, when given, narrows the version's.
type RateEntry struct {
	No            int
	Mode          Mode
	ContainerMode ContainerMode
	Origin        string
	Destination   string
	Via           string
	Edition       string
	Carrier       string
	ServiceLevel  string
	Commodity     string
	TransitDays   int
	Frequency     string
	Validity      kernel.ValidityInterval
	Lines         []RateLine
}

// VersionValidity is a version's half-open validity [From, Until); a zero Until leaves it open.
type VersionValidity struct {
	From  time.Time
	Until time.Time
}

func (v VersionValidity) contains(t time.Time) bool {
	return !t.Before(v.From) && (v.Until.IsZero() || t.Before(v.Until))
}

func (v VersionValidity) overlaps(o VersionValidity) bool {
	return (o.Until.IsZero() || v.From.Before(o.Until)) && (v.Until.IsZero() || o.From.Before(v.Until))
}

// RateCardVersion is a version of a card: its header and, once loaded, its entries.
type RateCardVersion struct {
	No             int
	Status         VersionStatus
	Validity       VersionValidity
	Currency       kernel.CurrencyCode // the default currency of new lines
	SourceRef      string              // the supplier's tariff document, a contract number or the accepting quote
	FilingRef      string              // SEA SELL: the rate filing reference, when filed
	Preparers      []string            // who drafted it and everyone who set its entries since, in order
	PreparedAt     time.Time           // when its entries were last set
	PublishedBy    string
	PublishedAt    time.Time
	ApprovedBy     string // SELL TARIFF: the pricing manager who approved it (the publisher)
	SelfApproved   bool
	WithdrawnBy    string
	WithdrawnAt    time.Time
	WithdrawReason string
	Entries        []RateEntry
	loaded         bool // entries read (DRAFT versions and those asked for)
}

// Effective is when a version rates: a PUBLISHED one its validity; a WITHDRAWN one its validity up to the time the
// withdrawal was recorded (from then on it rates nothing, and a later version may start); a DRAFT none.
func (v RateCardVersion) Effective() (VersionValidity, bool) {
	switch v.Status {
	case VersionPublished:
		return v.Validity, true
	case VersionWithdrawn:
		eff := v.Validity
		if eff.Until.IsZero() || v.WithdrawnAt.Before(eff.Until) {
			eff.Until = v.WithdrawnAt
		}
		return eff, eff.From.Before(eff.Until)
	}
	return VersionValidity{}, false
}

// RateCardState is a card's header and its versions.
type RateCardState struct {
	TenantID      TenantID
	RateCardID    RateCardID
	Side          Side
	Scope         CardScope
	LegalEntityID LegalEntityID
	PartyID       string // the customer of a CUSTOMER card, the supplier of a SUPPLIER card
	PartyRevision int64
	Name          string
	CreatedBy     string
	CreatedAt     time.Time
	Versions      []RateCardVersion
}

// RateCard is the aggregate root of a card and its versions.
type RateCard struct {
	state          RateCardState
	stored         int // versions stored when loaded
	headerChanged  map[int]bool
	contentChanged map[int]bool
	pending        []Event
}

// Event types of rate cards.
const (
	EventRateCardVersionPublished = "RateCardVersionPublished.v1"
	EventRateCardVersionWithdrawn = "RateCardVersionWithdrawn.v1"
)

// Limits of a version, and of the quantities a line and a cargo state, so that no quantity arithmetic overflows.
const (
	MaxEntries  = 500
	MaxLines    = 100
	MaxBreaks   = 20
	MaxStep     = 1_000_000_000      // base units, and cubic centimetres per kilogram or tonne
	MaxQuantity = 1_000_000_000_000  // base units: a million tonnes in grams
	MaxVolume   = 10_000_000_000_000 // cubic centimetres: ten million cubic metres
	MaxCount    = 1_000_000_000      // pieces or documents
)

var (
	ErrInvalidRateCard = errors.New("rate: invalid rate card")
	ErrVersionOverlap  = errors.New("rate: RATE_VERSION_OVERLAP: another published version of the card is valid then")
	ErrNotPublishable  = errors.New("rate: RATE_NOT_PUBLISHABLE: the version cannot be published")
)

// RateCardDraft is what a new card states.
type RateCardDraft struct {
	TenantID      TenantID
	RateCardID    RateCardID
	Side          Side
	Scope         CardScope
	LegalEntityID LegalEntityID
	PartyID       string
	PartyRevision int64
	Name          string
}

// NewRateCard opens a card with no version.
func NewRateCard(d RateCardDraft, by string, at time.Time) (*RateCard, error) {
	switch {
	case d.TenantID == "" || !identifier(string(d.RateCardID)) || !identifier(string(d.LegalEntityID)):
		return nil, fmt.Errorf("%w: a card needs a tenant, an id and a legal entity", ErrInvalidRateCard)
	case !d.Side.Valid() || d.Scope.sideOf() != d.Side:
		return nil, fmt.Errorf("%w: a SELL card is a TARIFF or a CUSTOMER card, a BUY card a SUPPLIER card", ErrInvalidRateCard)
	case d.Scope == ScopeTariff && d.PartyID != "":
		return nil, fmt.Errorf("%w: a tariff is for any customer and names no party", ErrInvalidRateCard)
	case d.Scope != ScopeTariff && (!identifier(d.PartyID) || d.PartyRevision < 1):
		return nil, fmt.Errorf("%w: a %s card names a party ORG confirmed", ErrInvalidRateCard, d.Scope)
	case !text(d.Name, 200):
		return nil, fmt.Errorf("%w: a name of 1 to 200 characters", ErrInvalidRateCard)
	case strings.TrimSpace(by) == "" || at.IsZero():
		return nil, fmt.Errorf("%w: a card needs an actor and a time", ErrInvalidRateCard)
	}
	return &RateCard{state: RateCardState{TenantID: d.TenantID, RateCardID: d.RateCardID, Side: d.Side, Scope: d.Scope,
		LegalEntityID: d.LegalEntityID, PartyID: d.PartyID, PartyRevision: d.PartyRevision, Name: strings.TrimSpace(d.Name), CreatedBy: by,
		CreatedAt: at.UTC()}}, nil
}

// RestoreRateCard rebuilds a stored card; versions whose entries were read carry them.
func RestoreRateCard(st RateCardState, loaded map[int]bool) *RateCard {
	c := &RateCard{state: st, stored: len(st.Versions)}
	c.state.Versions = slices.Clone(st.Versions)
	for i := range c.state.Versions {
		c.state.Versions[i].loaded = loaded[c.state.Versions[i].No]
	}
	return c
}

func (c *RateCard) State() RateCardState {
	st := c.state
	st.Versions = slices.Clone(c.state.Versions)
	return st
}
func (c *RateCard) Stored() int                { return c.stored }
func (c *RateCard) Pending() []Event           { return append([]Event(nil), c.pending...) }
func (c *RateCard) Acknowledge()               { c.pending = nil }
func (c *RateCard) HeaderChanged(no int) bool  { return c.headerChanged[no] }
func (c *RateCard) ContentChanged(no int) bool { return c.contentChanged[no] }

func (c *RateCard) version(no int) (*RateCardVersion, error) {
	for i := range c.state.Versions {
		if c.state.Versions[i].No == no {
			return &c.state.Versions[i], nil
		}
	}
	return nil, fmt.Errorf("%w: card %s has no version %d", ErrInvalidRateCard, c.state.RateCardID, no)
}

func (c *RateCard) mark(no int, content bool) {
	if c.headerChanged == nil {
		c.headerChanged, c.contentChanged = map[int]bool{}, map[int]bool{}
	}
	c.headerChanged[no] = true
	if content {
		c.contentChanged[no] = true
	}
}

// VersionInput is what drafting a version states.
type VersionInput struct {
	Validity  VersionValidity
	Currency  kernel.CurrencyCode
	SourceRef string
	FilingRef string
}

// DraftVersion opens the next version as a DRAFT, with the entries of another version copied when given. A card
// has at most one DRAFT version at a time.
func (c *RateCard) DraftVersion(in VersionInput, copied []RateEntry, by string, at time.Time) (int, error) {
	for _, v := range c.state.Versions {
		if v.Status == VersionDraft {
			return 0, fmt.Errorf("%w: version %d of card %s is still a draft", ErrInvalidTransition, v.No, c.state.RateCardID)
		}
	}
	if err := in.check(c.state); err != nil {
		return 0, err
	}
	if strings.TrimSpace(by) == "" || at.IsZero() {
		return 0, fmt.Errorf("%w: a draft needs an actor and a time", ErrInvalidRateCard)
	}
	no := len(c.state.Versions) + 1
	v := RateCardVersion{No: no, Status: VersionDraft, Validity: in.Validity.utc(), Currency: in.Currency, SourceRef: strings.TrimSpace(in.SourceRef),
		FilingRef: strings.TrimSpace(in.FilingRef), Preparers: []string{by}, PreparedAt: at.UTC(), loaded: true}
	if len(copied) > 0 {
		v.Entries = cloneEntries(copied)
		if err := checkEntries(c.state, v); err != nil {
			return 0, err
		}
	}
	c.state.Versions = append(c.state.Versions, v)
	c.mark(no, true)
	return no, nil
}

func (in VersionInput) check(st RateCardState) error {
	switch {
	case in.Validity.From.IsZero() || (!in.Validity.Until.IsZero() && !in.Validity.From.Before(in.Validity.Until)):
		return fmt.Errorf("%w: a validity [from, until) with until after from, or open", ErrInvalidRateCard)
	case !in.Currency.Valid():
		return fmt.Errorf("%w: currency %q", ErrInvalidRateCard, in.Currency)
	case utf8.RuneCountInString(in.SourceRef) > 200 || (st.Side == SideBuy && strings.TrimSpace(in.SourceRef) == ""):
		return fmt.Errorf("%w: a source reference of up to 200 characters; a supplier card names the supplier's tariff or contract", ErrInvalidRateCard)
	case utf8.RuneCountInString(in.FilingRef) > 100 || (in.FilingRef != "" && st.Side != SideSell):
		return fmt.Errorf("%w: a filing reference of up to 100 characters, on SELL cards only", ErrInvalidRateCard)
	}
	return nil
}

func (v VersionValidity) utc() VersionValidity {
	if !v.Until.IsZero() {
		v.Until = v.Until.UTC()
	}
	return VersionValidity{From: v.From.UTC(), Until: v.Until}
}

// SetEntries replaces the entries of a DRAFT version.
func (c *RateCard) SetEntries(no int, entries []RateEntry, by string, at time.Time) error {
	v, err := c.version(no)
	if err != nil {
		return err
	}
	if v.Status != VersionDraft {
		return fmt.Errorf("%w: version %d of card %s is %s; prices change by a new version", ErrInvalidTransition, no, c.state.RateCardID, v.Status)
	}
	if strings.TrimSpace(by) == "" || at.IsZero() {
		return fmt.Errorf("%w: a change needs an actor and a time", ErrInvalidRateCard)
	}
	next := *v
	next.Entries = cloneEntries(entries)
	if err = checkEntries(c.state, next); err != nil {
		return err
	}
	if !slices.Contains(next.Preparers, by) {
		next.Preparers = append(slices.Clone(next.Preparers), by)
	}
	next.PreparedAt = at.UTC()
	*v = next
	c.mark(no, true)
	return nil
}

// Catalogue answers what publication needs to know of charge codes and service levels.
type Catalogue interface {
	ChargeCode(code ChargeCode) (ChargeCodeVersion, bool)
	ServiceLevelListed(code string) bool
}

// Publication is who publishes and how a SELL tariff is approved.
type Publication struct {
	By          string
	At          time.Time
	ApprovedBy  string // SELL TARIFF: the approver, the publisher
	SelfApprove bool   // SELL TARIFF: the approver prepared the version, as an authorized role may (R3)
}

// Publish makes a DRAFT version valid for rating: every entry and line complete, charge codes listed, active and
// applying to the entry's mode, service levels listed, and no other published version of the card valid at any
// instant of its validity. A CUSTOMER version is published only by accepting its AGREEMENT quote.
func (c *RateCard) Publish(no int, cat Catalogue, p Publication) error {
	v, err := c.version(no)
	if err != nil {
		return err
	}
	switch {
	case v.Status != VersionDraft:
		return fmt.Errorf("%w: version %d of card %s is %s", ErrInvalidTransition, no, c.state.RateCardID, v.Status)
	case !v.loaded:
		return fmt.Errorf("%w: the entries of version %d were not read", ErrInvalidRateCard, no)
	case c.state.Scope == ScopeCustomer:
		return fmt.Errorf("%w: a CUSTOMER card's version is published by accepting its AGREEMENT quote", ErrNotPublishable)
	case len(v.Entries) == 0:
		return fmt.Errorf("%w: version %d has no entry", ErrNotPublishable, no)
	case strings.TrimSpace(p.By) == "" || p.At.IsZero():
		return fmt.Errorf("%w: a publication needs an actor and a time", ErrInvalidRateCard)
	case c.state.Scope == ScopeTariff && (strings.TrimSpace(p.ApprovedBy) == "" || p.ApprovedBy != p.By):
		return fmt.Errorf("%w: a SELL tariff is approved by the pricing manager who publishes it", ErrNotPublishable)
	case c.state.Scope == ScopeTariff && slices.Contains(v.Preparers, p.ApprovedBy) != p.SelfApprove:
		return fmt.Errorf("%w: the approval of a version its approver drafted or set is a self-approval", ErrNotPublishable)
	}
	if err := checkPublishable(c.state, *v, cat); err != nil {
		return err
	}
	for _, o := range c.state.Versions {
		if eff, ok := o.Effective(); o.No != no && ok && eff.overlaps(v.Validity) {
			return fmt.Errorf("%w: version %d", ErrVersionOverlap, o.No)
		}
	}
	v.Status, v.PublishedBy, v.PublishedAt = VersionPublished, p.By, p.At.UTC()
	if c.state.Scope == ScopeTariff {
		v.ApprovedBy, v.SelfApproved = p.ApprovedBy, p.SelfApprove
	}
	c.mark(no, false)
	c.pending = append(c.pending, Event{Type: EventRateCardVersionPublished, At: v.PublishedAt, TenantID: c.state.TenantID,
		SubjectID: string(c.state.RateCardID), Version: no})
	return nil
}

// checkPublishable checks what publication needs of a version's entries: each complete, its service level listed, and
// its charge codes listed, active, applying to the entry's mode and not discounts (R9).
func checkPublishable(st RateCardState, v RateCardVersion, cat Catalogue) error {
	if err := checkEntries(st, v); err != nil {
		return err
	}
	for _, e := range v.Entries {
		if !cat.ServiceLevelListed(e.ServiceLevel) {
			return fmt.Errorf("%w: entry %d: service level %s is not listed", ErrNotPublishable, e.No, e.ServiceLevel)
		}
		for _, l := range e.Lines {
			for _, code := range []ChargeCode{l.ChargeCode, l.BaseCode} {
				if code == "" {
					continue
				}
				cv, ok := cat.ChargeCode(code)
				switch {
				case !ok:
					return fmt.Errorf("%w: entry %d line %d: %s", ErrChargeCodeUnknown, e.No, l.No, code)
				case !cv.Active && code == l.ChargeCode:
					return fmt.Errorf("%w: entry %d line %d: charge code %s is inactive", ErrNotPublishable, e.No, l.No, code)
				case !cv.AppliesTo(e.Mode) && code == l.ChargeCode:
					return fmt.Errorf("%w: entry %d line %d: charge code %s does not apply to %s", ErrNotPublishable, e.No, l.No, code, e.Mode)
				case cv.Group == GroupDiscount && code == l.ChargeCode:
					return fmt.Errorf("%w: entry %d line %d: %s is a discount, a quote line, never a rate (R9)", ErrNotPublishable, e.No, l.No, code)
				}
			}
		}
	}
	return nil
}

// Withdraw takes a PUBLISHED version out of rating from the time recorded, with a reason; snapshots that used it
// keep it, and a later version may start then.
func (c *RateCard) Withdraw(no int, reason, by string, at time.Time) error {
	v, err := c.version(no)
	if err != nil {
		return err
	}
	switch {
	case v.Status != VersionPublished:
		return fmt.Errorf("%w: version %d of card %s is %s", ErrInvalidTransition, no, c.state.RateCardID, v.Status)
	case !text(reason, 500):
		return fmt.Errorf("%w: a withdrawal needs a reason of 1 to 500 characters", ErrInvalidRateCard)
	case strings.TrimSpace(by) == "" || at.IsZero():
		return fmt.Errorf("%w: a withdrawal needs an actor and a time", ErrInvalidRateCard)
	}
	v.Status, v.WithdrawnBy, v.WithdrawnAt, v.WithdrawReason = VersionWithdrawn, by, at.UTC(), strings.TrimSpace(reason)
	c.mark(no, false)
	c.pending = append(c.pending, Event{Type: EventRateCardVersionWithdrawn, At: v.WithdrawnAt, TenantID: c.state.TenantID,
		SubjectID: string(c.state.RateCardID), Version: no})
	return nil
}

func cloneEntries(in []RateEntry) []RateEntry {
	out := make([]RateEntry, len(in))
	for i, e := range in {
		out[i] = e
		out[i].Lines = make([]RateLine, len(e.Lines))
		for j, l := range e.Lines {
			out[i].Lines[j] = l
			out[i].Lines[j].Breaks = slices.Clone(l.Breaks)
		}
	}
	return out
}

// checkEntries checks a version's entries: numbered from 1, each a complete lane with lines numbered from 1, one
// line per charge code, and no two entries of the same lane valid at the same time.
func checkEntries(st RateCardState, v RateCardVersion) error {
	if len(v.Entries) > MaxEntries {
		return fmt.Errorf("%w: at most %d entries", ErrInvalidRateCard, MaxEntries)
	}
	for i, e := range v.Entries {
		if e.No != i+1 {
			return fmt.Errorf("%w: entries are numbered from 1 without gaps", ErrInvalidRateCard)
		}
		if err := checkEntry(st, v, e); err != nil {
			return fmt.Errorf("entry %d: %w", e.No, err)
		}
		for _, o := range v.Entries[:i] {
			if o.lane() == e.lane() && o.window(v.Validity).overlaps(e.window(v.Validity)) {
				return fmt.Errorf("%w: entries %d and %d are the same lane at the same time", ErrInvalidRateCard, o.No, e.No)
			}
		}
	}
	return nil
}

type lane struct {
	mode                      Mode
	container                 ContainerMode
	origin, destination, via  string
	carrier, level, commodity string
}

func (e RateEntry) lane() lane {
	return lane{e.Mode, e.ContainerMode, e.Origin, e.Destination, e.Via, e.Carrier, e.ServiceLevel, e.Commodity}
}

// window is when the entry is valid: its own validity, or the version's.
func (e RateEntry) window(v VersionValidity) VersionValidity {
	if e.Validity.From.IsZero() {
		return v
	}
	return VersionValidity{From: e.Validity.From, Until: e.Validity.Until}
}

func checkEntry(st RateCardState, v RateCardVersion, e RateEntry) error {
	switch {
	case !(e.Mode == ModeAir && e.ContainerMode == ContainerLoose) && !(e.Mode == ModeSea && e.ContainerMode == ContainerLCL):
		return fmt.Errorf("%w: S2 rates AIR LOOSE and SEA LCL", ErrInvalidRateCard)
	case !locode(e.Origin) || !locode(e.Destination) || (e.Via != "" && !locode(e.Via)) || e.Origin == e.Destination ||
		(e.Via != "" && (e.Via == e.Origin || e.Via == e.Destination)):
		return fmt.Errorf("%w: an origin, a destination and an optional via, distinct UN/LOCODEs", ErrInvalidRateCard)
	case strings.TrimSpace(e.Edition) == "":
		return fmt.Errorf("%w: the locations are resolved against an edition of the location reference data", ErrInvalidRateCard)
	case e.Carrier != "" && (st.Side == SideBuy || !identifier(e.Carrier)):
		return fmt.Errorf("%w: a carrier is named on SELL entries only (a BUY card's carrier is its supplier)", ErrInvalidRateCard)
	case !ServiceLevelCode(e.ServiceLevel):
		return fmt.Errorf("%w: service level %q", ErrInvalidRateCard, e.ServiceLevel)
	case e.Commodity != CommodityGeneral:
		return fmt.Errorf("%w: S2 rates the GENERAL commodity class only", ErrInvalidRateCard)
	case e.TransitDays < 0 || e.TransitDays > 365 || utf8.RuneCountInString(e.Frequency) > 60:
		return fmt.Errorf("%w: transit days 0 to 365 and a frequency of up to 60 characters", ErrInvalidRateCard)
	case len(e.Lines) == 0 || len(e.Lines) > MaxLines:
		return fmt.Errorf("%w: 1 to %d lines", ErrInvalidRateCard, MaxLines)
	}
	if !e.Validity.From.IsZero() || !e.Validity.Until.IsZero() {
		// [From, Until) with Until open or after From, starting within the version and, when both end, ending by it
		w := VersionValidity{From: e.Validity.From, Until: e.Validity.Until}
		if !e.Validity.Valid() || w.From.Before(v.Validity.From) || (!v.Validity.Until.IsZero() && !w.From.Before(v.Validity.Until)) ||
			(!v.Validity.Until.IsZero() && w.Until.After(v.Validity.Until)) {
			return fmt.Errorf("%w: an entry's validity lies within its version's", ErrInvalidRateCard)
		}
	}
	codes := map[ChargeCode]bool{}
	for i, l := range e.Lines {
		if l.No != i+1 {
			return fmt.Errorf("%w: lines are numbered from 1 without gaps", ErrInvalidRateCard)
		}
		if codes[l.ChargeCode] {
			return fmt.Errorf("%w: charge code %s twice", ErrInvalidRateCard, l.ChargeCode)
		}
		codes[l.ChargeCode] = true
		if err := l.check(); err != nil {
			return fmt.Errorf("line %d: %w", l.No, err)
		}
	}
	for _, l := range e.Lines {
		if l.Calculator == CalcPercentOf && !codes[l.BaseCode] {
			return fmt.Errorf("line %d: %w: the base charge %s is not a line of the entry", l.No, ErrInvalidRateCard, l.BaseCode)
		}
	}
	return nil
}

func (l RateLine) check() error {
	switch {
	case !ValidChargeCode(l.ChargeCode):
		return fmt.Errorf("%w: charge code %q", ErrInvalidRateCard, l.ChargeCode)
	case !l.Currency.Valid():
		return fmt.Errorf("%w: currency %q", ErrInvalidRateCard, l.Currency)
	case !l.Calculator.Valid():
		return fmt.Errorf("%w: calculator %q", ErrInvalidRateCard, l.Calculator)
	case l.Rate.Minor < 0 || l.Rate.Scale < 0 || l.Rate.Scale > 6 || l.Minimum < 0 || l.Maximum < 0 || (l.Maximum > 0 && l.Maximum < l.Minimum):
		return fmt.Errorf("%w: a rate at scale 0 to 6, a minimum and a maximum not below it, none negative", ErrInvalidRateCard)
	case l.Step > MaxStep || l.MinQuantity > MaxQuantity || l.VolumeFactor > MaxStep:
		return fmt.Errorf("%w: a step and a volume factor up to %d, a minimum quantity up to %d", ErrInvalidRateCard, MaxStep, MaxQuantity)
	case len(l.Breaks) > MaxBreaks:
		return fmt.Errorf("%w: at most %d breaks", ErrInvalidRateCard, MaxBreaks)
	case l.CallForPricing && (l.Rate.Minor != 0 || l.Minimum != 0 || l.Maximum != 0 || len(l.Breaks) > 0 || l.PercentBP != 0):
		return fmt.Errorf("%w: a call-for-pricing line has no rate", ErrInvalidRateCard)
	}
	switch l.Calculator {
	case CalcFlat:
		if l.Unit != UnitNone || len(l.Breaks) > 0 || l.Step != 0 || l.MinQuantity != 0 || l.VolumeFactor != 0 || l.PercentBP != 0 || l.BaseCode != "" {
			return fmt.Errorf("%w: a FLAT line has an amount only", ErrInvalidRateCard)
		}
	case CalcPerUnit:
		switch {
		case !l.Unit.Valid():
			return fmt.Errorf("%w: a PER_UNIT line needs a rate unit", ErrInvalidRateCard)
		case l.Step < 1 || l.MinQuantity < 0:
			return fmt.Errorf("%w: a quantity step of at least 1 base unit and a minimum quantity not below 0", ErrInvalidRateCard)
		case (l.Unit == UnitKgChargeable || l.Unit == UnitFreightTonne) != (l.VolumeFactor > 0):
			return fmt.Errorf("%w: KG_CHARGEABLE and FREIGHT_TONNE lines, and only they, have a volume factor", ErrInvalidRateCard)
		case l.PercentBP != 0 || l.BaseCode != "":
			return fmt.Errorf("%w: a PER_UNIT line has no percentage", ErrInvalidRateCard)
		}
		prev := int64(0)
		for _, b := range l.Breaks {
			if b.From <= prev || b.Rate < 0 || b.From > MaxQuantity {
				return fmt.Errorf("%w: breaks ascend from above 0 and have rates not below 0", ErrInvalidRateCard)
			}
			prev = b.From
		}
	case CalcPercentOf:
		switch {
		case l.Unit != UnitNone || len(l.Breaks) > 0 || l.Step != 0 || l.MinQuantity != 0 || l.VolumeFactor != 0 || l.Rate.Minor != 0:
			return fmt.Errorf("%w: a PERCENT_OF line has a percentage and a base charge only", ErrInvalidRateCard)
		case !l.CallForPricing && (l.PercentBP < 1 || l.PercentBP > 1_000_000):
			return fmt.Errorf("%w: a percentage of 1 to 1,000,000 basis points", ErrInvalidRateCard)
		case !ValidChargeCode(l.BaseCode) || l.BaseCode == l.ChargeCode:
			return fmt.Errorf("%w: the base charge is another charge code", ErrInvalidRateCard)
		}
	}
	return nil
}

// locode reports whether s has the form of a UN/LOCODE.
func locode(s string) bool { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: s}.Valid() }

// identifier reports whether s is a non-empty id without surrounding spaces of at most 128 characters.
func identifier(s string) bool {
	return s != "" && s == strings.TrimSpace(s) && utf8.RuneCountInString(s) <= 128
}

func text(s string, max int) bool {
	s = strings.TrimSpace(s)
	return s != "" && utf8.RuneCountInString(s) <= max
}
