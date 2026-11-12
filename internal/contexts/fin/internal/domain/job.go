package domain

import (
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Job kinds of V0.1: the export house of an air consolidation or of an ocean LCL consolidation.
const (
	KindAirExportHouse    = "AIR_EXPORT_HOUSE"
	KindSeaExportLCLHouse = "SEA_EXPORT_LCL_HOUSE"
)

// Job states: OPEN; CANCELLED when the shipment was cancelled or the job opened by mistake, with nothing invoiced.
// Closure and reopening come with matching, recognition and postings.
const (
	JobOpen      = "OPEN"
	JobCancelled = "CANCELLED"
)

// Sides, sources and side statuses: a sell side is an estimate not yet invoiced (ESTIMATED, WIP) or on an issued
// debit note (INVOICED); a cost side is an estimate not yet matched (ACCRUED, accrual).
const (
	SideSell = "SELL"
	SideCost = "COST"

	SourceQuote  = "QUOTE"
	SourceManual = "MANUAL"

	StatusEstimated = "ESTIMATED"
	StatusInvoiced  = "INVOICED"
	StatusAccrued   = "ACCRUED"
)

// SideVersion is one immutable version of a charge line's sell or cost side: the debtor or creditor (a cost side's
// creditor may not be known yet), the amount in its currency, the local amount in the functional currency at the rate
// it was converted at (none when the currency is the functional one; ESTIMATE for estimates, the debit note's rate
// for an INVOICED side), the status and, from the second version on, the reason. An INVOICED side names the debit
// note line that billed it.
type SideVersion struct {
	Side          string
	No            int
	PartyID       string
	PartyRevision int64
	Amount        kernel.Money
	Local         int64
	FX            *FxSnapshot
	Status        string
	Reason        string
	InvoiceID     string
	InvoiceLineNo int
	ConvertedAt   time.Time // when its rate was applied, if before At: an estimate a credit note returned
	By            string
	At            time.Time
	EnteredAt     time.Time
}

// converted is when the side's rate was applied.
func (v SideVersion) converted() time.Time {
	if v.ConvertedAt.IsZero() {
		return v.At
	}
	return v.ConvertedAt
}

// ChargeLine is a charge of the job with at most one sell side and one cost side, each a series of versions. A
// QUOTE line names the applied basis and the quote line it comes from.
type ChargeLine struct {
	No          int
	ChargeCode  string
	Description string
	Source      string
	BasisNo     int
	QuoteLineNo int
	Reason      string
	By          string
	At          time.Time
	EnteredAt   time.Time
	Sell        []SideVersion
	Cost        []SideVersion
}

// Basis is a pricing basis applied to the job: a quote for one order of the shipment.
type Basis struct {
	No            int
	BasisID       string
	QuoteID       string
	QuoteRevision int64
	OrderID       string
	By            string
	At            time.Time
	EnteredAt     time.Time
}

// JobState is a stored job, for persistence.
type JobState struct {
	TenantID           TenantID
	JobID              string
	LegalEntityID      string
	ShipmentID         string
	Kind               string
	FunctionalCurrency kernel.CurrencyCode
	BookCode           string
	CustomerPartyID    string
	PlanVersion        int
	State              string
	OpenedBy           string
	OpenedAt           time.Time
	EnteredAt          time.Time
	CancelReason       string
	CancelledBy        string
	CancelledAt        time.Time
	CancelEnteredAt    time.Time
	Bases              []Basis
	Lines              []ChargeLine
	Notes              []Note
}

// FreightJob is the aggregate root of a house shipment's economics under one legal entity and book (UL-02,
// UL-08, OD-1 A): estimated charges and the debit and credit notes billing them (issued through the job, as they
// change its sell sides; each keeps its own id and number); accruals' matches and pool shares later.
type FreightJob struct {
	state     JobState
	stored    stored
	cancelled []string // stored debit notes cancelled by a credit note since the job was loaded
	pending   []Event
}

// stored counts what the database holds of a loaded job: bases, lines, side versions per line and notes.
type stored struct {
	bases, lines, notes int
	sell, cost          map[int]int
}

const (
	eventJobOpened    = "FreightJobOpened.v1"
	eventLineRecorded = "ChargeLineRecorded.v1"
	eventSideRevised  = "ChargeSideRevised.v1"
	eventJobCancelled = "FreightJobCancelled.v1"
	eventBasisApplied = "PricingBasisApplied.v1"
)

// Converter converts an amount into the job's functional currency at the ESTIMATE rate in force at an instant.
type Converter interface {
	Local(m kernel.Money, rateType string, at time.Time) (int64, *FxSnapshot, error)
}

// OpenJob opens a job for a shipment under a legal entity's policy.
func OpenJob(st JobState, policy *FinancePolicy, by string, at time.Time) (*FreightJob, error) {
	p := policy.State()
	switch {
	case st.TenantID == "" || !identifier(st.JobID) || !identifier(st.ShipmentID) || !identifier(st.CustomerPartyID) || st.PlanVersion < 1 ||
		(st.Kind != KindAirExportHouse && st.Kind != KindSeaExportLCLHouse) || blank(by) || at.IsZero():
		return nil, fmt.Errorf("%w: a job names its shipment, kind, customer and plan version", ErrInvalid)
	case p.TenantID != st.TenantID || p.LegalEntityID != st.LegalEntityID:
		return nil, fmt.Errorf("%w: the policy is another legal entity's", ErrInvalid)
	}
	st.FunctionalCurrency, st.BookCode, st.State, st.OpenedBy, st.OpenedAt = p.FunctionalCurrency, p.BookCode, JobOpen, by, at.UTC()
	st.Bases, st.Lines, st.Notes, st.CancelReason, st.CancelledBy, st.CancelledAt = nil, nil, nil, "", "", time.Time{}
	j := &FreightJob{state: st, stored: stored{sell: map[int]int{}, cost: map[int]int{}}}
	j.record(eventJobOpened, at, 0)
	return j, nil
}

func (j *FreightJob) open(by string, at time.Time) error {
	switch {
	case j.state.State != JobOpen:
		return fmt.Errorf("%w: the job is %s", ErrInvalidTransition, j.state.State)
	case blank(by) || at.IsZero():
		return fmt.Errorf("%w: a change names its author and time", ErrInvalid)
	case at.Before(j.lastChange()):
		return fmt.Errorf("%w: business time %s before the job's last change", ErrInvalid, at.UTC().Format(time.RFC3339))
	}
	return nil
}

func (j *FreightJob) lastChange() time.Time {
	last := j.state.OpenedAt
	for _, b := range j.state.Bases {
		last = later(last, b.At)
	}
	for _, l := range j.state.Lines {
		last = later(last, l.At)
		for _, v := range append(append([]SideVersion(nil), l.Sell...), l.Cost...) {
			last = later(last, v.At)
		}
	}
	return last
}

func (j *FreightJob) side(side string, party string, revision int64, amount kernel.Money, status, reason string, conv Converter, no int,
	by string, at time.Time) (SideVersion, error) {
	if !amount.Currency.Valid() || (side == SideSell && !identifier(party)) || (party != "" && !identifier(party)) || revision < 0 {
		return SideVersion{}, fmt.Errorf("%w: a %s side names its currency and, for a sell side, its debtor", ErrInvalid, side)
	}
	if amount.Minor > MaxAmount || amount.Minor < -MaxAmount {
		return SideVersion{}, fmt.Errorf("%w: an amount beyond %d minor units", ErrInvalid, MaxAmount)
	}
	local, fx, err := conv.Local(amount, RateEstimate, at)
	if err != nil {
		return SideVersion{}, err
	}
	if local > MaxAmount || local < -MaxAmount {
		return SideVersion{}, fmt.Errorf("%w: a local amount beyond %d minor units", ErrInvalid, MaxAmount)
	}
	return SideVersion{Side: side, No: no, PartyID: party, PartyRevision: revision, Amount: amount, Local: local, FX: fx, Status: status, Reason: reason,
		By: by, At: at.UTC()}, nil
}

// BasisLine is a line of a pricing basis: sell and cost estimates of one charge.
type BasisLine struct {
	LineNo     int
	ChargeCode string
	Sell       kernel.Money
	Cost       kernel.Money
}

// ApplyBasis applies an accepted quote for one of the shipment's orders: every line becomes a charge line with a
// sell side for the job's customer and, when it has a cost, a cost side, both estimates from the quote. A quote is
// applied to a job once, whichever order (FIN_BASIS_APPLIED), and a quote and order to one job of the tenant (the
// database keeps both).
func (j *FreightJob) ApplyBasis(b Basis, customer string, customerRevision int64, lines []BasisLine, conv Converter, by string, at time.Time) error {
	if err := j.open(by, at); err != nil {
		return err
	}
	switch {
	case !identifier(b.BasisID) || !identifier(b.QuoteID) || b.QuoteRevision < 1 || !identifier(b.OrderID) || len(lines) == 0:
		return fmt.Errorf("%w: a basis names its quote, revision, order and lines", ErrInvalid)
	case customer != j.state.CustomerPartyID:
		return fmt.Errorf("%w: the quote is for %s, the job for %s", ErrNotPriceable, customer, j.state.CustomerPartyID)
	}
	for _, o := range j.state.Bases {
		if o.QuoteID == b.QuoteID {
			return fmt.Errorf("%w: basis %d applies quote %s to the job for order %s", ErrBasisApplied, o.No, o.QuoteID, o.OrderID)
		}
	}
	b.No, b.By, b.At = len(j.state.Bases)+1, by, at.UTC()
	j.state.Bases = append(j.state.Bases, b)
	for _, l := range lines {
		if !ValidChargeCode(l.ChargeCode) || l.LineNo < 1 || l.Cost.Minor < 0 || (l.Sell.Minor < 0 && l.Cost.Minor != 0) {
			return fmt.Errorf("%w: quote line %d", ErrInvalid, l.LineNo)
		}
		line := ChargeLine{No: len(j.state.Lines) + 1, ChargeCode: l.ChargeCode, Source: SourceQuote, BasisNo: b.No, QuoteLineNo: l.LineNo, By: by,
			At: at.UTC()}
		sell, err := j.side(SideSell, customer, customerRevision, l.Sell, StatusEstimated, "", conv, 1, by, at)
		if err != nil {
			return err
		}
		line.Sell = []SideVersion{sell}
		if l.Cost.Minor != 0 {
			cost, err := j.side(SideCost, "", 0, l.Cost, StatusAccrued, "", conv, 1, by, at)
			if err != nil {
				return err
			}
			line.Cost = []SideVersion{cost}
		}
		j.state.Lines = append(j.state.Lines, line)
		j.recordLine(eventLineRecorded, at, line.No, "", 1)
	}
	j.record(eventBasisApplied, at, b.No)
	return nil
}

// SideInput is a side entered by hand: the debtor or creditor (ORG confirmed it), its revision and the amount.
type SideInput struct {
	PartyID       string
	PartyRevision int64
	Amount        kernel.Money
}

// RecordLine adds a MANUAL charge line with a sell side, a cost side or both, and a reason. A manual sell side is
// not negative: a discount is a quote line RATE approved (R9).
func (j *FreightJob) RecordLine(code, description string, sell, cost *SideInput, reason string, conv Converter, by string, at time.Time) error {
	if err := j.open(by, at); err != nil {
		return err
	}
	switch {
	case !ValidChargeCode(code) || (description != "" && !text(description)) || !text(reason) || (sell == nil && cost == nil):
		return fmt.Errorf("%w: a manual line names its charge code, a side and a reason", ErrInvalid)
	case sell != nil && sell.Amount.Minor < 0, cost != nil && cost.Amount.Minor < 0:
		return fmt.Errorf("%w: a manual amount is not negative; a discount comes from an approved quote", ErrInvalid)
	}
	line := ChargeLine{No: len(j.state.Lines) + 1, ChargeCode: code, Description: description, Source: SourceManual, Reason: reason, By: by, At: at.UTC()}
	if sell != nil {
		v, err := j.side(SideSell, sell.PartyID, sell.PartyRevision, sell.Amount, StatusEstimated, "", conv, 1, by, at)
		if err != nil {
			return err
		}
		line.Sell = []SideVersion{v}
	}
	if cost != nil {
		v, err := j.side(SideCost, cost.PartyID, cost.PartyRevision, cost.Amount, StatusAccrued, "", conv, 1, by, at)
		if err != nil {
			return err
		}
		line.Cost = []SideVersion{v}
	}
	j.state.Lines = append(j.state.Lines, line)
	j.recordLine(eventLineRecorded, at, line.No, "", 1)
	return nil
}

// ReviseSide writes the next version of a line's sell or cost side with a reason; expected version 0 adds the
// side a line lacks. A sell side stays non-negative unless the line is a quote's discount.
func (j *FreightJob) ReviseSide(lineNo int, side string, expected int, in SideInput, reason string, conv Converter, by string, at time.Time) error {
	if err := j.open(by, at); err != nil {
		return err
	}
	if lineNo < 1 || lineNo > len(j.state.Lines) || (side != SideSell && side != SideCost) || !text(reason) {
		return fmt.Errorf("%w: a revision names a line of the job, the side and a reason", ErrInvalid)
	}
	line := &j.state.Lines[lineNo-1]
	versions, status := &line.Sell, StatusEstimated
	if side == SideCost {
		versions, status = &line.Cost, StatusAccrued
	}
	if expected != len(*versions) {
		return fmt.Errorf("%w: %s side of line %d is at version %d", ErrSideVersion, side, lineNo, len(*versions))
	}
	if n := len(*versions); n > 0 && (*versions)[n-1].Status == StatusInvoiced {
		return fmt.Errorf("%w: line %d, debit note %s", ErrInvoiced, lineNo, (*versions)[n-1].InvoiceID)
	}
	discountLine := line.Source == SourceQuote && len(line.Sell) > 0 && line.Sell[0].Amount.Minor < 0
	switch {
	case side == SideCost && discountLine:
		return fmt.Errorf("%w: a discount line has no cost (R9)", ErrInvalid)
	case in.Amount.Minor < 0 && !(side == SideSell && discountLine):
		return fmt.Errorf("%w: a %s amount is not negative except on a quote's discount line", ErrInvalid, side)
	}
	v, err := j.side(side, in.PartyID, in.PartyRevision, in.Amount, status, reason, conv, expected+1, by, at)
	if err != nil {
		return err
	}
	*versions = append(*versions, v)
	j.recordLine(eventSideRevised, at, lineNo, side, v.No)
	return nil
}

// Cancel cancels an OPEN job with a reason when none of its sell sides is invoiced (credit notes return them first);
// its applied quotes are free for another job.
func (j *FreightJob) Cancel(reason, by string, at time.Time) error {
	if err := j.open(by, at); err != nil {
		return err
	}
	if !text(reason) {
		return fmt.Errorf("%w: a cancellation needs a reason", ErrInvalid)
	}
	for _, l := range j.state.Lines {
		if n := len(l.Sell); n > 0 && l.Sell[n-1].Status == StatusInvoiced {
			return fmt.Errorf("%w: line %d, debit note %s", ErrInvoiced, l.No, l.Sell[n-1].InvoiceID)
		}
	}
	j.state.State, j.state.CancelReason, j.state.CancelledBy, j.state.CancelledAt = JobCancelled, reason, by, at.UTC()
	j.record(eventJobCancelled, at, 0)
	return nil
}

// Profit is a job's economics in the functional currency, per charge code and in total: revenue from the latest sell
// sides (invoiced ones at their debit note's local amount; Invoiced is that part), cost from the cost estimates.
type Profit struct {
	Currency kernel.CurrencyCode
	Lines    []CodeProfit
	Revenue  int64
	Invoiced int64
	Cost     int64
	Profit   int64
}

// CodeProfit is the estimated revenue, cost and profit of one charge code.
type CodeProfit struct {
	ChargeCode string
	Revenue    int64
	Cost       int64
	Profit     int64
}

// Profit sums the latest versions of the sides.
func (j *FreightJob) Profit() Profit {
	out := Profit{Currency: j.state.FunctionalCurrency}
	index := map[string]int{}
	for _, l := range j.state.Lines {
		i, ok := index[l.ChargeCode]
		if !ok {
			i = len(out.Lines)
			index[l.ChargeCode] = i
			out.Lines = append(out.Lines, CodeProfit{ChargeCode: l.ChargeCode})
		}
		if n := len(l.Sell); n > 0 {
			out.Lines[i].Revenue += l.Sell[n-1].Local
			if l.Sell[n-1].Status == StatusInvoiced {
				out.Invoiced += l.Sell[n-1].Local
			}
		}
		if n := len(l.Cost); n > 0 {
			out.Lines[i].Cost += l.Cost[n-1].Local
		}
	}
	for i := range out.Lines {
		out.Lines[i].Profit = out.Lines[i].Revenue - out.Lines[i].Cost
		out.Revenue += out.Lines[i].Revenue
		out.Cost += out.Lines[i].Cost
	}
	out.Profit = out.Revenue - out.Cost
	return out
}

// Exposure is a debtor's exposure on the job in the functional currency while the job is open: the sell not yet
// billed (ESTIMATED sides, WIP) and the sell billed on debit notes not reversed (INVOICED sides; settlement comes
// later, so every issued debit note stays open).
func (j *FreightJob) Exposure(debtor string) (unbilled, billed int64) {
	if j.state.State != JobOpen {
		return 0, 0
	}
	for _, l := range j.state.Lines {
		if n := len(l.Sell); n > 0 && l.Sell[n-1].PartyID == debtor {
			if l.Sell[n-1].Status == StatusInvoiced {
				billed += l.Sell[n-1].Local
			} else {
				unbilled += l.Sell[n-1].Local
			}
		}
	}
	return unbilled, billed
}

// Debtors lists the debtors of the job's sell sides, as their latest versions name them.
func (j *FreightJob) Debtors() []string {
	var out []string
	for _, l := range j.state.Lines {
		if n := len(l.Sell); n > 0 && !slices.Contains(out, l.Sell[n-1].PartyID) {
			out = append(out, l.Sell[n-1].PartyID)
		}
	}
	return out
}

func (j *FreightJob) record(kind string, at time.Time, version int) {
	j.recordLine(kind, at, 0, "", version)
}

// recordLine records an event of the job; line and side events name the line, the side and its version.
func (j *FreightJob) recordLine(kind string, at time.Time, line int, side string, version int) {
	j.pending = append(j.pending, Event{Type: kind, At: at.UTC(), TenantID: j.state.TenantID, SubjectID: j.state.JobID,
		LegalEntityID: j.state.LegalEntityID, Version: version, ShipmentID: j.state.ShipmentID, LineNo: line, Side: side})
}

// Stamp records when FIN entered what was written since the job was loaded (T1).
func (j *FreightJob) Stamp(entered time.Time) {
	e := entered.UTC()
	st := &j.state
	if st.EnteredAt.IsZero() {
		st.EnteredAt = e
	}
	if st.State == JobCancelled && st.CancelEnteredAt.IsZero() {
		st.CancelEnteredAt = e
	}
	for i := range st.Bases {
		if st.Bases[i].EnteredAt.IsZero() {
			st.Bases[i].EnteredAt = e
		}
	}
	for i := range st.Lines {
		l := &st.Lines[i]
		if l.EnteredAt.IsZero() {
			l.EnteredAt = e
		}
		for k := range l.Sell {
			if l.Sell[k].EnteredAt.IsZero() {
				l.Sell[k].EnteredAt = e
			}
		}
		for k := range l.Cost {
			if l.Cost[k].EnteredAt.IsZero() {
				l.Cost[k].EnteredAt = e
			}
		}
	}
	for i := range st.Notes {
		if st.Notes[i].EnteredAt.IsZero() {
			st.Notes[i].EnteredAt = e
		}
	}
}

// RestoreJob rebuilds a stored job.
func RestoreJob(st JobState) (*FreightJob, error) {
	st.Bases = append([]Basis(nil), st.Bases...)
	st.Lines = slices.Clone(st.Lines)
	st.Notes = slices.Clone(st.Notes)
	s := stored{bases: len(st.Bases), lines: len(st.Lines), notes: len(st.Notes), sell: map[int]int{}, cost: map[int]int{}}
	notes := map[string]*Note{}
	for i := range st.Notes {
		n := &st.Notes[i]
		n.Lines = append([]NoteLine(nil), n.Lines...)
		notes[n.ID] = n
		for k, l := range n.Lines {
			if l.No != k+1 || l.JobLineNo < 1 || l.JobLineNo > len(st.Lines) {
				return nil, fmt.Errorf("%w: line %d of note %s", ErrInvalid, k+1, n.ID)
			}
		}
	}
	for i, b := range st.Bases {
		if b.No != i+1 {
			return nil, fmt.Errorf("%w: basis %d", ErrInvalid, i+1)
		}
	}
	for i := range st.Lines {
		l := &st.Lines[i]
		l.Sell, l.Cost = append([]SideVersion(nil), l.Sell...), append([]SideVersion(nil), l.Cost...)
		if l.No != i+1 || len(l.Sell)+len(l.Cost) == 0 {
			return nil, fmt.Errorf("%w: charge line %d", ErrInvalid, i+1)
		}
		for k, v := range l.Sell {
			if v.No != k+1 || v.Side != SideSell {
				return nil, fmt.Errorf("%w: sell side of line %d", ErrInvalid, i+1)
			}
			if v.Status == StatusInvoiced {
				n := notes[v.InvoiceID]
				if n == nil || v.InvoiceLineNo < 1 || v.InvoiceLineNo > len(n.Lines) || n.Lines[v.InvoiceLineNo-1].JobLineNo != l.No {
					return nil, fmt.Errorf("%w: invoiced sell side of line %d", ErrInvalid, i+1)
				}
			}
		}
		for k, v := range l.Cost {
			if v.No != k+1 || v.Side != SideCost {
				return nil, fmt.Errorf("%w: cost side of line %d", ErrInvalid, i+1)
			}
		}
		s.sell[l.No], s.cost[l.No] = len(l.Sell), len(l.Cost)
	}
	return &FreightJob{state: st, stored: s}, nil
}

// State returns a copy for persistence and read models.
func (j *FreightJob) State() JobState {
	st := j.state
	st.Bases = append([]Basis(nil), st.Bases...)
	st.Lines = slices.Clone(st.Lines)
	for i := range st.Lines {
		st.Lines[i].Sell = append([]SideVersion(nil), st.Lines[i].Sell...)
		st.Lines[i].Cost = append([]SideVersion(nil), st.Lines[i].Cost...)
	}
	st.Notes = slices.Clone(st.Notes)
	for i := range st.Notes {
		st.Notes[i].Lines = append([]NoteLine(nil), st.Notes[i].Lines...)
	}
	return st
}

// AddedNotes returns the notes issued since the job was loaded and the stored debit notes cancelled since, in their
// new state.
func (j *FreightJob) AddedNotes() (notes []Note, cancelled []Note) {
	st := j.State()
	for _, id := range j.cancelled {
		for _, n := range st.Notes[:j.stored.notes] {
			if n.ID == id {
				cancelled = append(cancelled, n)
			}
		}
	}
	return st.Notes[j.stored.notes:], cancelled
}

// Added returns what was written since the job was loaded: bases, new lines and new side versions (of any line).
func (j *FreightJob) Added() (bases []Basis, lines []ChargeLine, sides []SideVersion, lineOf []int) {
	st := j.State()
	bases = st.Bases[j.stored.bases:]
	lines = st.Lines[j.stored.lines:]
	for _, l := range st.Lines {
		for _, v := range l.Sell[min(j.stored.sell[l.No], len(l.Sell)):] {
			sides, lineOf = append(sides, v), append(lineOf, l.No)
		}
		for _, v := range l.Cost[min(j.stored.cost[l.No], len(l.Cost)):] {
			sides, lineOf = append(sides, v), append(lineOf, l.No)
		}
	}
	return bases, lines, sides, lineOf
}

func (j *FreightJob) Pending() []Event { return append([]Event(nil), j.pending...) }

// Acknowledge is permitted only after the transaction holding the job and its Outbox events commits.
func (j *FreightJob) Acknowledge() {
	j.pending, j.cancelled = nil, nil
	j.stored = stored{bases: len(j.state.Bases), lines: len(j.state.Lines), notes: len(j.state.Notes), sell: map[int]int{}, cost: map[int]int{}}
	for _, l := range j.state.Lines {
		j.stored.sell[l.No], j.stored.cost[l.No] = len(l.Sell), len(l.Cost)
	}
}
