package domain

import (
	"cmp"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5: rate evaluation (R6). Evaluate is a pure function of its input, the published versions it is given and
// EngineVersion: the same input and versions always give the same lines. RATE never invents a price: a charge it
// cannot rate is a gap, never zero.

// EngineVersion names the evaluation rules, so a snapshot can be replayed.
const EngineVersion = "RATE-ENGINE-1"

// SnapshotPurpose is what an evaluation is for.
type SnapshotPurpose string

const (
	PurposeQuoteLine          SnapshotPurpose = "QUOTE_LINE"
	PurposeShipmentEstimate   SnapshotPurpose = "SHIPMENT_ESTIMATE"
	PurposeConsolCostEstimate SnapshotPurpose = "CONSOL_COST_ESTIMATE"
)

func (p SnapshotPurpose) Valid() bool {
	return p == PurposeQuoteLine || p == PurposeShipmentEstimate || p == PurposeConsolCostEstimate
}

// Gap is why a snapshot line has no amount.
type Gap string

const (
	GapNone           Gap = ""
	GapNoRate         Gap = "NO_RATE"          // no published entry for the lane on that side
	GapCallForPricing Gap = "CALL_FOR_PRICING" // the line is priced by a person
	GapBaseMissing    Gap = "BASE_MISSING"     // a PERCENT_OF line's base charge was not rated
	GapBaseCurrency   Gap = "BASE_CURRENCY"    // the base charge is in another currency
)

// Cargo is what an evaluation rates: pieces, gross weight in grams, volume in cubic centimetres and documents.
type Cargo struct {
	Pieces     int64
	GrossGrams int64
	VolumeCM3  int64
	Documents  int64
}

// EvaluationInput is what an evaluation is asked. Origin, Destination and Via are UN/LOCODEs resolved for the mode.
// Customer names the customer of a SELL evaluation (its CUSTOMER card wins over the tariff); Carrier restricts a BUY
// evaluation to that supplier's cards and SELL entries to those naming no carrier or that one.
type EvaluationInput struct {
	Purpose        SnapshotPurpose
	LegalEntityID  LegalEntityID
	Sides          []Side
	Customer       CustomerRef
	Carrier        string
	Mode           Mode
	ContainerMode  ContainerMode
	Origin         string
	Destination    string
	Via            string
	Edition        string
	ServiceLevel   string
	Commodity      string
	At             time.Time // the business time rates must be valid at
	Cargo          Cargo
	SubjectKind    string // QUOTE, SHIPMENT or CONSOLIDATION; empty for a quote not yet created
	SubjectID      string
	SubjectVersion int64
}

// PublishedVersion is a version rating may use, with its card's header.
type PublishedVersion struct {
	RateCardID    RateCardID
	Side          Side
	Scope         CardScope
	LegalEntityID LegalEntityID
	PartyID       string
	No            int
	Validity      VersionValidity
	Entries       []RateEntry
}

// Candidate is a card version and entry an evaluation considered, and what became of it.
type Candidate struct {
	Side       Side
	RateCardID RateCardID
	Version    int
	EntryNo    int
	Taken      bool
	Reason     string
	Total      []kernel.Money // BUY: the card's total per currency, for the lowest-total choice
}

// SnapshotLine is a rated charge, or a gap.
type SnapshotLine struct {
	Side           Side
	LineNo         int
	ChargeCode     ChargeCode
	RateCardID     RateCardID
	Version        int
	EntryNo        int
	RateLineNo     int
	Calculator     Calculator
	Unit           RateUnit
	Quantity       int64 // base units after the step and the minimum quantity
	Charged        int64 // the quantity charged: Quantity, or a higher band's lower bound that costs less
	BandFrom       int64 // the lower bound of the band used; 0 for the line's own rate
	Rate           UnitRate
	PercentBP      int64
	BaseAmount     int64
	MinimumApplied bool
	MaximumApplied bool
	Amount         kernel.Money
	Gap            Gap
}

var ErrInvalidEvaluation = errors.New("rate: invalid rate evaluation")

func (in EvaluationInput) check() error {
	sides := slices.Clone(in.Sides)
	slices.Sort(sides)
	switch {
	case !in.Purpose.Valid():
		return fmt.Errorf("%w: purpose %q", ErrInvalidEvaluation, in.Purpose)
	case !identifier(string(in.LegalEntityID)):
		return fmt.Errorf("%w: a legal entity", ErrInvalidEvaluation)
	case len(sides) == 0 || len(slices.Compact(sides)) != len(in.Sides) || !sides[0].Valid() || !sides[len(sides)-1].Valid():
		return fmt.Errorf("%w: SELL, BUY or both, once each", ErrInvalidEvaluation)
	case in.Customer.PartyID != "" && (!identifier(in.Customer.PartyID) || in.Customer.PartyRevision < 1):
		return fmt.Errorf("%w: a customer ORG confirmed", ErrInvalidEvaluation)
	case in.Carrier != "" && !identifier(in.Carrier):
		return fmt.Errorf("%w: carrier %q", ErrInvalidEvaluation, in.Carrier)
	case !(in.Mode == ModeAir && in.ContainerMode == ContainerLoose) && !(in.Mode == ModeSea && in.ContainerMode == ContainerLCL):
		return fmt.Errorf("%w: S2 rates AIR LOOSE and SEA LCL", ErrInvalidEvaluation)
	case !locode(in.Origin) || !locode(in.Destination) || (in.Via != "" && !locode(in.Via)) || in.Origin == in.Destination:
		return fmt.Errorf("%w: an origin, a destination and an optional via, UN/LOCODEs", ErrInvalidEvaluation)
	case !ServiceLevelCode(in.ServiceLevel) || in.Commodity != CommodityGeneral:
		return fmt.Errorf("%w: a service level code and the GENERAL commodity class", ErrInvalidEvaluation)
	case in.At.IsZero():
		return fmt.Errorf("%w: a business time", ErrInvalidEvaluation)
	case in.Cargo.Pieces < 0 || in.Cargo.GrossGrams < 0 || in.Cargo.VolumeCM3 < 0 || in.Cargo.Documents < 0:
		return fmt.Errorf("%w: cargo quantities are not negative", ErrInvalidEvaluation)
	case in.SubjectKind != "" && in.SubjectKind != "QUOTE" && in.SubjectKind != "SHIPMENT" && in.SubjectKind != "CONSOLIDATION":
		return fmt.Errorf("%w: subject kind %q", ErrInvalidEvaluation, in.SubjectKind)
	case (in.SubjectKind == "") != (in.SubjectID == "") || (in.SubjectID != "" && !identifier(in.SubjectID)) || in.SubjectVersion < 0:
		return fmt.Errorf("%w: a subject is a kind and an id", ErrInvalidEvaluation)
	}
	return nil
}

// match is an entry that serves the input, with how specific it is.
type match struct {
	v     *PublishedVersion
	e     *RateEntry
	via   bool // the entry names the input's via
	carry bool // the entry names the input's carrier
}

func (m match) less(o match) int {
	if m.via != o.via {
		return map[bool]int{true: -1, false: 1}[m.via]
	}
	if m.carry != o.carry {
		return map[bool]int{true: -1, false: 1}[m.carry]
	}
	return cmp.Or(cmp.Compare(m.v.RateCardID, o.v.RateCardID), cmp.Compare(m.v.No, o.v.No), cmp.Compare(m.e.No, o.e.No))
}

func matches(in EvaluationInput, versions []*PublishedVersion) []match {
	var out []match
	for _, v := range versions {
		if !v.Validity.contains(in.At) {
			continue
		}
		for i := range v.Entries {
			e := &v.Entries[i]
			switch {
			case e.Mode != in.Mode || e.ContainerMode != in.ContainerMode || e.Origin != in.Origin || e.Destination != in.Destination,
				e.ServiceLevel != in.ServiceLevel || e.Commodity != in.Commodity,
				e.Via != "" && e.Via != in.Via,
				e.Carrier != "" && e.Carrier != in.Carrier,
				!e.window(v.Validity).contains(in.At):
				continue
			}
			out = append(out, match{v: v, e: e, via: e.Via != "", carry: e.Carrier != ""})
		}
	}
	slices.SortFunc(out, match.less)
	return out
}

// picked is a rate line chosen for a charge code, with the entry it came from.
type picked struct {
	m match
	l RateLine
}

// pick takes, per charge code, the line of the most specific entry that has it; codes in skip are left out.
func pick(ms []match, skip map[ChargeCode]bool) (map[ChargeCode]picked, map[int]bool) {
	out, used := map[ChargeCode]picked{}, map[int]bool{}
	for i, m := range ms {
		for _, l := range m.e.Lines {
			if _, ok := out[l.ChargeCode]; ok || skip[l.ChargeCode] {
				continue
			}
			out[l.ChargeCode] = picked{m, l}
			used[i] = true
		}
	}
	return out, used
}

// Evaluate rates the input on the published versions given and returns what it considered and the lines.
//
// SELL (OD-1 A): the customer's CUSTOMER cards first, then the TARIFF for the charge codes they lack; per charge
// code the line of the most specific entry (one naming the via, then one naming the carrier, then by card and
// entry). BUY (OD-2 A): with a carrier given only that supplier's cards; otherwise every supplier card is rated
// and the lowest total taken among those rated in full in one currency, ties by card id (RATE converts nothing:
// when the totals are in different currencies the first card by id is taken and the snapshot says so).
func Evaluate(in EvaluationInput, versions []PublishedVersion) ([]Candidate, []SnapshotLine, error) {
	if err := in.check(); err != nil {
		return nil, nil, err
	}
	var cands []Candidate
	var lines []SnapshotLine
	sides := slices.Clone(in.Sides)
	slices.SortFunc(sides, func(a, b Side) int { return cmp.Compare(b, a) }) // SELL before BUY
	for _, side := range sides {
		var c []Candidate
		var l []SnapshotLine
		var err error
		if side == SideSell {
			c, l, err = evaluateSell(in, versions)
		} else {
			c, l, err = evaluateBuy(in, versions)
		}
		if err != nil {
			return nil, nil, err
		}
		cands, lines = append(cands, c...), append(lines, l...)
	}
	for i := range lines {
		lines[i].LineNo = i + 1
	}
	return cands, lines, nil
}

func scoped(in EvaluationInput, versions []PublishedVersion, keep func(*PublishedVersion) bool) []*PublishedVersion {
	var out []*PublishedVersion
	for i := range versions {
		v := &versions[i]
		if v.LegalEntityID == in.LegalEntityID && keep(v) {
			out = append(out, v)
		}
	}
	return out
}

func evaluateSell(in EvaluationInput, versions []PublishedVersion) ([]Candidate, []SnapshotLine, error) {
	customer := matches(in, scoped(in, versions, func(v *PublishedVersion) bool {
		return v.Side == SideSell && v.Scope == ScopeCustomer && in.Customer.PartyID != "" && v.PartyID == in.Customer.PartyID
	}))
	tariff := matches(in, scoped(in, versions, func(v *PublishedVersion) bool { return v.Side == SideSell && v.Scope == ScopeTariff }))
	fromCustomer, usedC := pick(customer, nil)
	skip := map[ChargeCode]bool{}
	for code := range fromCustomer {
		skip[code] = true
	}
	fromTariff, usedT := pick(tariff, skip)
	var cands []Candidate
	for _, group := range []struct {
		ms     []match
		used   map[int]bool
		reason string
	}{{customer, usedC, "CUSTOMER_CARD"}, {tariff, usedT, "TARIFF"}} {
		for i, m := range group.ms {
			c := Candidate{Side: SideSell, RateCardID: m.v.RateCardID, Version: m.v.No, EntryNo: m.e.No, Taken: group.used[i], Reason: group.reason}
			if !c.Taken {
				c.Reason = "CHARGES_TAKEN_ELSEWHERE"
			}
			cands = append(cands, c)
		}
	}
	all := map[ChargeCode]picked{}
	for code, p := range fromCustomer {
		all[code] = p
	}
	for code, p := range fromTariff {
		all[code] = p
	}
	if len(all) == 0 {
		return cands, []SnapshotLine{{Side: SideSell, Gap: GapNoRate}}, nil
	}
	lines, err := rateLines(SideSell, all, in.Cargo)
	return cands, lines, err
}

func evaluateBuy(in EvaluationInput, versions []PublishedVersion) ([]Candidate, []SnapshotLine, error) {
	cards := scoped(in, versions, func(v *PublishedVersion) bool {
		return v.Side == SideBuy && v.Scope == ScopeSupplier && (in.Carrier == "" || v.PartyID == in.Carrier)
	})
	// A BUY entry's carrier is its supplier: entries are matched without the carrier, which chose the cards.
	byCard := map[RateCardID][]*PublishedVersion{}
	var ids []RateCardID
	for _, v := range cards {
		if byCard[v.RateCardID] == nil {
			ids = append(ids, v.RateCardID)
		}
		byCard[v.RateCardID] = append(byCard[v.RateCardID], v)
	}
	slices.Sort(ids)
	type rated struct {
		id       RateCardID
		ms       []match
		used     map[int]bool
		lines    []SnapshotLine
		complete bool
		totals   map[kernel.CurrencyCode]int64
	}
	var all []rated
	plain := in
	plain.Carrier = ""
	for _, id := range ids {
		ms := matches(plain, byCard[id])
		if len(ms) == 0 {
			continue
		}
		p, used := pick(ms, nil)
		lines, err := rateLines(SideBuy, p, in.Cargo)
		if err != nil {
			return nil, nil, err
		}
		r := rated{id: id, ms: ms, used: used, lines: lines, complete: true, totals: map[kernel.CurrencyCode]int64{}}
		for _, l := range lines {
			if l.Gap != GapNone {
				r.complete = false
				continue
			}
			if r.totals[l.Amount.Currency], err = addMinor(r.totals[l.Amount.Currency], l.Amount.Minor); err != nil {
				return nil, nil, err
			}
		}
		all = append(all, r)
	}
	if len(all) == 0 {
		return nil, []SnapshotLine{{Side: SideBuy, Gap: GapNoRate}}, nil
	}
	chosen, reason := 0, "ONLY_CARD"
	if len(all) > 1 {
		var eligible []int
		currencies := map[kernel.CurrencyCode]bool{}
		for i, r := range all {
			if r.complete && len(r.totals) == 1 {
				eligible = append(eligible, i)
				for c := range r.totals {
					currencies[c] = true
				}
			}
		}
		switch {
		case len(eligible) == 0:
			reason = "NO_CARD_RATED_IN_FULL_FIRST_BY_ID"
		case len(currencies) > 1:
			chosen, reason = eligible[0], "CURRENCIES_DIFFER_FIRST_BY_ID"
		default:
			chosen, reason = eligible[0], "LOWEST_TOTAL"
			for _, i := range eligible[1:] {
				if total(all[i].totals) < total(all[chosen].totals) {
					chosen = i
				}
			}
		}
	}
	var cands []Candidate
	for i, r := range all {
		var totals []kernel.Money
		for c, v := range r.totals {
			totals = append(totals, kernel.Money{Currency: c, Minor: v})
		}
		slices.SortFunc(totals, func(a, b kernel.Money) int { return cmp.Compare(a.Currency, b.Currency) })
		for j, m := range r.ms {
			c := Candidate{Side: SideBuy, RateCardID: r.id, Version: m.v.No, EntryNo: m.e.No, Taken: i == chosen && r.used[j], Total: totals}
			switch {
			case i != chosen:
				c.Reason = "OTHER_SUPPLIER_CARD_TAKEN"
			case !c.Taken:
				c.Reason = "CHARGES_TAKEN_ELSEWHERE"
			case in.Carrier != "":
				c.Reason = "CARRIER_GIVEN"
			default:
				c.Reason = reason
			}
			cands = append(cands, c)
		}
	}
	return cands, all[chosen].lines, nil
}

func total(m map[kernel.CurrencyCode]int64) int64 {
	for _, v := range m {
		return v
	}
	return 0
}

func addMinor(a, b int64) (int64, error) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, kernel.ErrAmountOverflow
	}
	return s, nil
}

// rateLines computes the chosen lines of a side, in the order of their charge codes; PERCENT_OF lines after the
// charges they are a percentage of.
func rateLines(side Side, chosen map[ChargeCode]picked, cargo Cargo) ([]SnapshotLine, error) {
	codes := make([]ChargeCode, 0, len(chosen))
	for code := range chosen {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	out := make([]SnapshotLine, len(codes))
	at := map[ChargeCode]int{}
	for i, code := range codes {
		p := chosen[code]
		l := p.l
		out[i] = SnapshotLine{Side: side, ChargeCode: code, RateCardID: p.m.v.RateCardID, Version: p.m.v.No, EntryNo: p.m.e.No, RateLineNo: l.No,
			Calculator: l.Calculator, Unit: l.Unit, Amount: kernel.Money{Currency: l.Currency}}
		at[code] = i
		if l.CallForPricing {
			out[i].Gap = GapCallForPricing
			continue
		}
		if l.Calculator == CalcPercentOf {
			continue
		}
		if err := rateLine(&out[i], l, cargo); err != nil {
			return nil, err
		}
	}
	for i, code := range codes {
		l := chosen[code].l
		if l.Calculator != CalcPercentOf || l.CallForPricing {
			continue
		}
		out[i].PercentBP = l.PercentBP
		j, ok := at[l.BaseCode]
		switch {
		case !ok || out[j].Gap != GapNone || out[j].Calculator == CalcPercentOf:
			out[i].Gap = GapBaseMissing
		case out[j].Amount.Currency != l.Currency:
			out[i].Gap = GapBaseCurrency
		default:
			out[i].BaseAmount = out[j].Amount.Minor
			amount := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(out[j].Amount.Minor), big.NewInt(l.PercentBP)), big.NewInt(10_000))
			if err := bound(&out[i], l, amount); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Quantity is what a PER_UNIT line rates the cargo on, in base units, before the step and the minimum quantity.
func Quantity(l RateLine, c Cargo) int64 {
	switch l.Unit {
	case UnitKgGross:
		return c.GrossGrams
	case UnitKgChargeable:
		return max(c.GrossGrams, ceilDiv(c.VolumeCM3*1000, l.VolumeFactor))
	case UnitCBM:
		return c.VolumeCM3
	case UnitFreightTonne:
		return max(ceilDiv(c.GrossGrams, 1000), ceilDiv(c.VolumeCM3*1000, l.VolumeFactor))
	case UnitPiece:
		return c.Pieces
	case UnitDocument:
		return c.Documents
	}
	return 0
}

func ceilDiv(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// rateLine computes a FLAT or PER_UNIT line (OD-3): the band the quantity falls in prices the whole quantity, a
// higher band's lower bound is charged instead when that costs less, then the minimum and the maximum apply; the
// amount is rounded half away from zero once.
func rateLine(s *SnapshotLine, l RateLine, c Cargo) error {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(l.Rate.Scale)), nil)
	if l.Calculator == CalcFlat {
		s.Rate = l.Rate
		return bound(s, l, new(big.Rat).SetFrac(big.NewInt(l.Rate.Minor), scale))
	}
	if c.VolumeCM3 > (1<<62)/1000 {
		return fmt.Errorf("%w: volume", kernel.ErrAmountOverflow)
	}
	q := Quantity(l, c)
	q = ceilDiv(q, l.Step) * l.Step
	q = max(q, l.MinQuantity)
	s.Quantity, s.Charged = q, q
	bands := append([]RateBreak{{From: 0, Rate: l.Rate.Minor}}, l.Breaks...)
	at := 0
	for i, b := range bands {
		if b.From <= q {
			at = i
		}
	}
	denominator := new(big.Int).Mul(scale, big.NewInt(l.Unit.Base()))
	price := func(rate, qty int64) *big.Rat {
		return new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(rate), big.NewInt(qty)), denominator)
	}
	best := price(bands[at].Rate, q)
	s.BandFrom, s.Rate = bands[at].From, UnitRate{Minor: bands[at].Rate, Scale: l.Rate.Scale}
	for _, b := range bands[at+1:] {
		if a := price(b.Rate, b.From); a.Cmp(best) < 0 {
			best, s.Charged, s.BandFrom, s.Rate = a, b.From, b.From, UnitRate{Minor: b.Rate, Scale: l.Rate.Scale}
		}
	}
	return bound(s, l, best)
}

// bound applies the line's minimum and maximum to the exact amount and rounds it half away from zero.
func bound(s *SnapshotLine, l RateLine, amount *big.Rat) error {
	if minimum := new(big.Rat).SetInt64(l.Minimum); amount.Cmp(minimum) < 0 {
		amount, s.MinimumApplied = minimum, true
	}
	if l.Maximum > 0 {
		if maximum := new(big.Rat).SetInt64(l.Maximum); amount.Cmp(maximum) > 0 {
			amount, s.MaximumApplied = maximum, true
		}
	}
	num, den := amount.Num(), amount.Denom()
	rounded := new(big.Int).Quo(new(big.Int).Add(new(big.Int).Mul(num, big.NewInt(2)), den), new(big.Int).Mul(den, big.NewInt(2)))
	if !rounded.IsInt64() {
		return fmt.Errorf("%w: charge %s", kernel.ErrAmountOverflow, s.ChargeCode)
	}
	s.Amount = kernel.Money{Currency: l.Currency, Minor: rounded.Int64()}
	return nil
}

// RateSnapshotState is a recorded evaluation: evidence only, it prices nothing until a quote line, a pricing basis
// or a person uses it.
type RateSnapshotState struct {
	TenantID      TenantID
	SnapshotID    string
	Input         EvaluationInput
	EngineVersion string
	Candidates    []Candidate
	Lines         []SnapshotLine
	EvaluatedBy   string
	RecordedAt    time.Time
}

// RateSnapshot is the aggregate of an evaluation; it never changes once recorded.
type RateSnapshot struct {
	state   RateSnapshotState
	pending []Event
}

// EventRatesEvaluated is published when a snapshot is recorded.
const EventRatesEvaluated = "RatesEvaluated.v1"

// RecordSnapshot evaluates the input on the versions and records the result.
func RecordSnapshot(tenant TenantID, id string, in EvaluationInput, versions []PublishedVersion, by string, recordedAt time.Time) (*RateSnapshot, error) {
	if tenant == "" || !identifier(id) || strings.TrimSpace(by) == "" || recordedAt.IsZero() {
		return nil, fmt.Errorf("%w: a snapshot needs a tenant, an id, an actor and a time", ErrInvalidEvaluation)
	}
	in.Sides = slices.Clone(in.Sides)
	slices.SortFunc(in.Sides, func(a, b Side) int { return cmp.Compare(b, a) })
	in.At = in.At.UTC()
	cands, lines, err := Evaluate(in, versions)
	if err != nil {
		return nil, err
	}
	s := &RateSnapshot{state: RateSnapshotState{TenantID: tenant, SnapshotID: id, Input: in, EngineVersion: EngineVersion, Candidates: cands,
		Lines: lines, EvaluatedBy: by, RecordedAt: recordedAt.UTC()}}
	s.pending = append(s.pending, Event{Type: EventRatesEvaluated, At: s.state.RecordedAt, TenantID: tenant, SubjectID: id})
	return s, nil
}

// RestoreSnapshot rebuilds a stored snapshot.
func RestoreSnapshot(st RateSnapshotState) *RateSnapshot { return &RateSnapshot{state: st} }

func (s *RateSnapshot) State() RateSnapshotState { return s.state }
func (s *RateSnapshot) Pending() []Event         { return append([]Event(nil), s.pending...) }
func (s *RateSnapshot) Acknowledge()             { s.pending = nil }
