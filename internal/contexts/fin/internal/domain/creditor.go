package domain

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.5 (FB4): a creditor invoice is what a carrier, co-loader or agent invoices a legal entity: recorded from the
// supplier's document, then matched in full to the accruals it actually costs (job cost sides and pool cost lines), a
// final match closing its accrual with the variance booked, a match not final leaving the rest accrued for a later
// invoice. A final variance beyond the legal entity's threshold needs approval (OD-9 B). A disputed invoice blocks
// what depends on it until the dispute is resolved; a cancelled one consumes nothing.

// Creditor invoice states.
const (
	CreditorRecorded  = "RECORDED"
	CreditorMatched   = "MATCHED"
	CreditorDisputed  = "DISPUTED"
	CreditorCancelled = "CANCELLED"
)

// Incoming tax documents of a creditor invoice: a domestic special or ordinary VAT fapiao, or a foreign creditor's
// own invoice or debit note. FIN keeps their kind and number for the ERP; input VAT stays with the ERP and tax filing.
const (
	TaxDocSpecialFapiao  = "SPECIAL_FAPIAO"
	TaxDocOrdinaryFapiao = "ORDINARY_FAPIAO"
	TaxDocForeignInvoice = "FOREIGN_INVOICE"
)

// MaxCreditorLines bounds an invoice's lines, MaxMatches the matches of one command.
const (
	MaxCreditorLines = 200
	MaxMatches       = 200
)

// documentNumber is the form of a supplier's invoice number or a tax document number: an identifier of letters,
// digits, space and . _ : / # -, up to 64 characters, not starting or ending with a space ("ABC " would pass as
// another invoice than "ABC").
var documentNumber = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9 ._:/#-]{0,62}[A-Za-z0-9._:/#-])?$`)

const maxInvoiceAge = 366 * 24 * time.Hour // an invoice date further back is taken for a typing error

// CreditorLine is a line of the supplier's invoice: a charge code, the supplier's reference (a MAWB, container or
// booking number) and a positive amount in the invoice's currency.
type CreditorLine struct {
	No          int
	ChargeCode  string
	Description string
	SupplierRef string
	Amount      int64
}

// Match is part of an invoice line matched to one accrual, at the accrual's version it consumed: the amount in the
// invoice's currency (the accrual's), whether it is final, its local amount at the ACCOUNTING rate in force when
// matched, the accrual's figures then and what earlier invoices had consumed of it; a final match carries the variance
// (what all live matches consume less the accrual), in the currency and in the functional currency.
type Match struct {
	No            int
	LineNo        int
	Target        TargetKey
	TargetVersion int
	Amount        int64
	Final         bool
	Reason        string // why an accrual of another charge code is matched
	Local         int64
	FX            *FxSnapshot
	AccrualAmount int64
	AccrualLocal  int64
	PriorAmount   int64
	PriorLocal    int64
	Variance      int64
	VarianceLocal int64
}

// Dispute is a dispute of the invoice with the supplier, from the state it interrupted, resolved at most once.
type Dispute struct {
	No               int
	FromState        string
	Reason           string
	By               string
	At               time.Time
	EnteredAt        time.Time
	ResolvedBy       string
	ResolveReason    string
	ResolvedAt       time.Time
	ResolveEnteredAt time.Time
}

// Open reports whether the dispute is not resolved.
func (d Dispute) Open() bool { return d.ResolvedAt.IsZero() }

// CreditorInvoiceState is a stored creditor invoice, for persistence.
type CreditorInvoiceState struct {
	TenantID          TenantID
	InvoiceID         string
	LegalEntityID     string
	CreditorPartyID   string
	CreditorRevision  int64
	SupplierInvoiceNo string
	InvoiceDate       time.Time
	Currency          kernel.CurrencyCode
	TaxDocumentKind   string
	TaxDocumentNumber string
	State             string
	RecordedBy        string
	RecordedAt        time.Time
	EnteredAt         time.Time
	Lines             []CreditorLine
	Matches           []Match
	MatchedBy         string
	MatchedAt         time.Time
	MatchEnteredAt    time.Time
	ApprovalRequired  bool
	ThresholdNo       int // the threshold version the variances were weighed against; 0 when none was set
	ApprovedBy        string
	ApprovalReason    string
	SelfApproved      bool
	ApprovedAt        time.Time
	ApprovalEnteredAt time.Time
	Disputes          []Dispute
	CancelReason      string
	CancelledBy       string
	CancelledAt       time.Time
	CancelEnteredAt   time.Time
}

// Total is the sum of the invoice's lines.
func (s CreditorInvoiceState) Total() int64 {
	var total int64
	for _, l := range s.Lines {
		total += l.Amount
	}
	return total
}

// AwaitingApproval reports whether a variance of the invoice needs approval not yet given.
func (s CreditorInvoiceState) AwaitingApproval() bool {
	return s.ApprovalRequired && s.ApprovedAt.IsZero()
}

// CreditorInvoice is the aggregate root of a supplier's invoice to a legal entity and its matches to accruals.
type CreditorInvoice struct {
	state    CreditorInvoiceState
	matches  int   // matches stored
	disputes int   // disputes stored
	resolved []int // stored disputes resolved since the invoice was loaded
	paid     int64 // what live allocations of creditor payments pay of it, set under the invoice's lock (FIN V0.6)
	pending  []Event
}

// Paid sets what the live allocations of creditor payments pay of the invoice, read under its lock.
func (c *CreditorInvoice) Paid(amount int64) { c.paid = amount }

const (
	eventCreditorRecorded  = "CreditorInvoiceRecorded.v1"
	eventCreditorMatched   = "CreditorInvoiceMatched.v1"
	eventVarianceApproved  = "CostVarianceApproved.v1"
	eventCreditorDisputed  = "CreditorInvoiceDisputed.v1"
	eventDisputeResolved   = "CreditorInvoiceDisputeResolved.v1"
	eventCreditorCancelled = "CreditorInvoiceCancelled.v1"
)

// CreditorLineInput is a line of an invoice as asked.
type CreditorLineInput struct {
	ChargeCode  string
	Description string
	SupplierRef string
	Amount      int64
}

// CreditorInvoiceInput is an invoice as asked, with the creditor's revision ORG confirmed in a creditor role.
type CreditorInvoiceInput struct {
	ID                string
	LegalEntityID     string
	CreditorPartyID   string
	CreditorRevision  int64
	SupplierInvoiceNo string
	InvoiceDate       time.Time
	Currency          kernel.CurrencyCode
	TaxDocumentKind   string
	TaxDocumentNumber string
	Lines             []CreditorLineInput
}

// RecordCreditorInvoice records a supplier's invoice as received: the supplier's number (once per legal entity and
// creditor among invoices not cancelled; the database keeps it), the civil invoice date (not after the day recorded,
// in any time zone, and not more than 366 days before), one currency, the incoming tax document's kind and number
// when there is one, and 1 to 200 lines with positive amounts.
func RecordCreditorInvoice(tenant TenantID, in CreditorInvoiceInput, by string, at time.Time) (*CreditorInvoice, error) {
	_, known := kernel.MinorDigits(in.Currency)
	switch {
	case tenant == "" || !identifier(in.ID) || !identifier(in.LegalEntityID) || !identifier(in.CreditorPartyID) || in.CreditorRevision < 1 || blank(by) ||
		at.IsZero():
		return nil, fmt.Errorf("%w: a creditor invoice names its id, legal entity, creditor, author and time", ErrInvalid)
	case !documentNumber.MatchString(in.SupplierInvoiceNo) || !known:
		return nil, fmt.Errorf("%w: a creditor invoice has the supplier's number (letters, digits, space and . _ : / # -, 64 characters, not "+
			"starting or ending with a space) and a currency with known minor digits", ErrInvalid)
	case !date(in.InvoiceDate) || at.Before(in.InvoiceDate.Add(-14*time.Hour)) || !at.Before(in.InvoiceDate.Add(maxInvoiceAge+36*time.Hour)):
		return nil, fmt.Errorf("%w: the invoice date is not after the day the invoice is recorded and not more than 366 days before", ErrInvalid)
	case (in.TaxDocumentKind == "") != (in.TaxDocumentNumber == ""),
		in.TaxDocumentKind != "" && (in.TaxDocumentKind != TaxDocSpecialFapiao && in.TaxDocumentKind != TaxDocOrdinaryFapiao &&
			in.TaxDocumentKind != TaxDocForeignInvoice || !documentNumber.MatchString(in.TaxDocumentNumber)):
		return nil, fmt.Errorf("%w: a tax document has its kind (SPECIAL_FAPIAO, ORDINARY_FAPIAO or FOREIGN_INVOICE) and number, or neither", ErrInvalid)
	case len(in.Lines) == 0 || len(in.Lines) > MaxCreditorLines:
		return nil, fmt.Errorf("%w: a creditor invoice has 1 to %d lines", ErrInvalid, MaxCreditorLines)
	}
	st := CreditorInvoiceState{TenantID: tenant, InvoiceID: in.ID, LegalEntityID: in.LegalEntityID, CreditorPartyID: in.CreditorPartyID,
		CreditorRevision: in.CreditorRevision, SupplierInvoiceNo: in.SupplierInvoiceNo, InvoiceDate: in.InvoiceDate.UTC(), Currency: in.Currency,
		TaxDocumentKind: in.TaxDocumentKind, TaxDocumentNumber: in.TaxDocumentNumber, State: CreditorRecorded, RecordedBy: by, RecordedAt: at.UTC()}
	var total int64
	for i, l := range in.Lines {
		if !ValidChargeCode(l.ChargeCode) || (l.Description != "" && !text(l.Description)) || (l.SupplierRef != "" && !text(l.SupplierRef)) ||
			l.Amount <= 0 || l.Amount > MaxAmount {
			return nil, fmt.Errorf("%w: line %d has a charge code and an amount of 1 to %d minor units", ErrInvalid, i+1, MaxAmount)
		}
		if total += l.Amount; total > MaxAmount {
			return nil, fmt.Errorf("%w: the lines total more than %d minor units", ErrInvalid, MaxAmount)
		}
		st.Lines = append(st.Lines, CreditorLine{No: i + 1, ChargeCode: l.ChargeCode, Description: l.Description, SupplierRef: l.SupplierRef,
			Amount: l.Amount})
	}
	c := &CreditorInvoice{state: st}
	c.record(eventCreditorRecorded, at, 0)
	return c, nil
}

func (c *CreditorInvoice) lastChange() time.Time {
	st := c.state
	last := later(later(later(st.RecordedAt, st.MatchedAt), st.ApprovedAt), st.CancelledAt)
	for _, d := range st.Disputes {
		last = later(later(last, d.At), d.ResolvedAt)
	}
	return last
}

func (c *CreditorInvoice) change(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(c.lastChange()) {
		return fmt.Errorf("%w: a change names its author and a time not before the invoice's last change", ErrInvalid)
	}
	return nil
}

// MatchItem is part of an invoice line for one accrual, as asked.
type MatchItem struct {
	LineNo int
	Target TargetKey
	Amount int64
	Final  bool
	Reason string
}

// MatchPolicy is what matching needs of the legal entity's policy: the ACCOUNTING rate and the variance threshold
// in force.
type MatchPolicy interface {
	Converter
	ThresholdAt(at time.Time) *VarianceThreshold
}

// Match matches every line of a RECORDED invoice in full to accruals of open jobs and pools of its legal entity (each
// accrual once): of the invoice's currency, naming no creditor or the invoice's, of the line's charge code or with a
// reason, neither matched final nor released. A match not final consumes no more than the accrual has open; a final
// one closes the accrual and carries the variance. Each match converts at the ACCOUNTING rate in force (OD-5 B). A
// final variance beyond the threshold in force needs approval.
func (c *CreditorInvoice) Match(items []MatchItem, targets []AccrualTarget, policy MatchPolicy, by string, at time.Time) error {
	if err := c.change(by, at); err != nil {
		return err
	}
	st := &c.state
	switch {
	case st.State != CreditorRecorded:
		return fmt.Errorf("%w: creditor invoice %s is %s", ErrInvalidTransition, st.InvoiceID, st.State)
	case len(items) == 0 || len(items) > MaxMatches:
		return fmt.Errorf("%w: a matching names 1 to %d matches", ErrInvalid, MaxMatches)
	}
	byKey := map[TargetKey]AccrualTarget{}
	for _, t := range targets {
		byKey[t.Key()] = t
	}
	seen := map[TargetKey]bool{}
	perLine := make([]int64, len(st.Lines))
	threshold := policy.ThresholdAt(at)
	var matches []Match
	required := false
	for _, it := range items {
		k := it.Target
		switch {
		case it.LineNo < 1 || it.LineNo > len(st.Lines) || it.Amount <= 0 || it.Amount > MaxAmount || (k.Kind != TargetJobCost && k.Kind != TargetPoolCost) ||
			!identifier(k.ID) || k.LineNo < 1 || (it.Reason != "" && !text(it.Reason)):
			return fmt.Errorf("%w: a match names a line of the invoice, a job cost side or pool cost line and a positive amount", ErrInvalid)
		case seen[k]:
			return fmt.Errorf("%w: %s is named twice", ErrMatchIncomplete, k)
		}
		seen[k] = true
		t, ok := byKey[k]
		line := st.Lines[it.LineNo-1]
		switch {
		case !ok || t.LegalEntityID != st.LegalEntityID || !t.Open:
			return fmt.Errorf("%w: %s", ErrMatchTarget, k)
		case t.Status == StatusReleased || t.Consumed.Final:
			return fmt.Errorf("%w: %s is %s", ErrAccrualClosed, k, AccrualStatus(t.Status, t.Consumed))
		case t.Amount.Currency != st.Currency:
			return fmt.Errorf("%w: %s is in %s, the invoice in %s", ErrMatchCurrency, k, t.Amount.Currency, st.Currency)
		case t.CreditorPartyID != "" && t.CreditorPartyID != st.CreditorPartyID:
			return fmt.Errorf("%w: %s names %s", ErrMatchCreditor, k, t.CreditorPartyID)
		case t.ChargeCode != line.ChargeCode && it.Reason == "":
			return fmt.Errorf("%w: line %d is %s, %s is %s", ErrMatchChargeCode, it.LineNo, line.ChargeCode, k, t.ChargeCode)
		case !it.Final && t.Consumed.Amount+it.Amount > t.Amount.Minor:
			return fmt.Errorf("%w: %s has %d open", ErrMatchExceeds, k, Open(t.Amount.Minor, t.Status, t.Consumed))
		}
		perLine[it.LineNo-1] += it.Amount
		local, fx, err := policy.Local(kernel.Money{Currency: st.Currency, Minor: it.Amount}, RateAccounting, at)
		if err != nil {
			return err
		}
		if local > MaxAmount || local < -MaxAmount {
			return fmt.Errorf("%w: a local amount beyond %d minor units", ErrInvalid, MaxAmount)
		}
		m := Match{LineNo: it.LineNo, Target: k, TargetVersion: t.Version, Amount: it.Amount, Final: it.Final, Reason: it.Reason, Local: local, FX: fx,
			AccrualAmount: t.Amount.Minor, AccrualLocal: t.Local, PriorAmount: t.Consumed.Amount, PriorLocal: t.Consumed.Local}
		if it.Final {
			m.Variance = t.Consumed.Amount + it.Amount - t.Amount.Minor
			m.VarianceLocal = t.Consumed.Local + local - t.Local
			required = required || Exceeded(threshold, m.VarianceLocal, t.Local)
		}
		matches = append(matches, m)
	}
	for i, l := range st.Lines {
		if perLine[i] != l.Amount {
			return fmt.Errorf("%w: line %d is %d, its matches %d", ErrMatchIncomplete, l.No, l.Amount, perLine[i])
		}
	}
	for i := range matches {
		matches[i].No = i + 1
	}
	st.Matches, st.State, st.MatchedBy, st.MatchedAt, st.ApprovalRequired = matches, CreditorMatched, by, at.UTC(), required
	st.ThresholdNo = 0
	if threshold != nil {
		st.ThresholdNo = threshold.No
	}
	c.record(eventCreditorMatched, at, len(matches))
	return nil
}

// ApproveVariance approves the variance of a MATCHED invoice that needs approval, with a reason: by another person
// than the one who matched it, unless the approver may approve their own (the flag stays on the approval).
func (c *CreditorInvoice) ApproveVariance(reason string, mayApproveOwn bool, by string, at time.Time) error {
	if err := c.change(by, at); err != nil {
		return err
	}
	st := &c.state
	switch {
	case st.State != CreditorMatched || !st.AwaitingApproval():
		return fmt.Errorf("%w: creditor invoice %s is %s and awaits no approval", ErrInvalidTransition, st.InvoiceID, st.State)
	case !text(reason):
		return fmt.Errorf("%w: an approval needs a reason", ErrInvalid)
	case by == st.MatchedBy && !mayApproveOwn:
		return fmt.Errorf("%w: %s matched the invoice", ErrVarianceApproval, by)
	}
	st.ApprovedBy, st.ApprovalReason, st.SelfApproved, st.ApprovedAt = by, reason, by == st.MatchedBy, at.UTC()
	c.record(eventVarianceApproved, at, 0)
	return nil
}

// targetsOpen checks that every accrual the invoice matched belongs to an open job or pool, as read under their locks.
func (c *CreditorInvoice) targetsOpen(targets []AccrualTarget) error {
	for _, m := range c.state.Matches {
		i := slices.IndexFunc(targets, func(t AccrualTarget) bool { return t.Key() == m.Target })
		if i < 0 || !targets[i].Open {
			return fmt.Errorf("%w: %s", ErrTargetClosed, m.Target)
		}
	}
	return nil
}

// Dispute disputes a RECORDED or MATCHED invoice with the supplier, with a reason; while disputed its matches hold the
// pools they consume from closing.
func (c *CreditorInvoice) Dispute(reason string, targets []AccrualTarget, by string, at time.Time) error {
	if err := c.change(by, at); err != nil {
		return err
	}
	st := &c.state
	switch {
	case st.State != CreditorRecorded && st.State != CreditorMatched:
		return fmt.Errorf("%w: creditor invoice %s is %s", ErrInvalidTransition, st.InvoiceID, st.State)
	case !text(reason):
		return fmt.Errorf("%w: a dispute needs a reason", ErrInvalid)
	}
	if err := c.targetsOpen(targets); err != nil {
		return err
	}
	d := Dispute{No: len(st.Disputes) + 1, FromState: st.State, Reason: reason, By: by, At: at.UTC()}
	st.Disputes, st.State = append(st.Disputes, d), CreditorDisputed
	c.record(eventCreditorDisputed, at, d.No)
	return nil
}

// ResolveDispute resolves the open dispute with a reason: the invoice returns to the state the dispute interrupted.
func (c *CreditorInvoice) ResolveDispute(reason string, by string, at time.Time) error {
	if err := c.change(by, at); err != nil {
		return err
	}
	st := &c.state
	switch {
	case st.State != CreditorDisputed:
		return fmt.Errorf("%w: creditor invoice %s is %s", ErrInvalidTransition, st.InvoiceID, st.State)
	case !text(reason):
		return fmt.Errorf("%w: a resolution needs a reason", ErrInvalid)
	}
	d := &st.Disputes[len(st.Disputes)-1]
	d.ResolvedBy, d.ResolveReason, d.ResolvedAt = by, reason, at.UTC()
	if d.No <= c.disputes {
		c.resolved = append(c.resolved, d.No)
	}
	st.State = d.FromState
	c.record(eventDisputeResolved, at, d.No)
	return nil
}

// Cancel cancels a RECORDED or DISPUTED invoice with a reason (an invoice matched is disputed first): its matches stop
// consuming their accruals, which are open again, so their jobs and pools must be open; and no other invoice may hold
// a final match of an accrual this one matched in part, since that match's variance counted this one
// (FIN_FINAL_MATCH_BEHIND). An accrual released since consumes less: its cost is what is still matched. An invoice
// that payments pay is not cancelled (FIN_INVOICE_PAID): their allocations are reversed first.
func (c *CreditorInvoice) Cancel(reason string, targets []AccrualTarget, by string, at time.Time) error {
	if err := c.change(by, at); err != nil {
		return err
	}
	st := &c.state
	switch {
	case st.State != CreditorRecorded && st.State != CreditorDisputed:
		return fmt.Errorf("%w: creditor invoice %s is %s; a matched invoice is disputed before it is cancelled", ErrInvalidTransition, st.InvoiceID, st.State)
	case !text(reason):
		return fmt.Errorf("%w: a cancellation needs a reason", ErrInvalid)
	case c.paid != 0:
		return fmt.Errorf("%w: %d of creditor invoice %s is paid", ErrInvoicePaid, c.paid, st.InvoiceID)
	}
	if err := c.targetsOpen(targets); err != nil {
		return err
	}
	for _, m := range st.Matches {
		i := slices.IndexFunc(targets, func(t AccrualTarget) bool { return t.Key() == m.Target })
		if !m.Final && targets[i].Consumed.Final {
			return fmt.Errorf("%w: %s", ErrFinalBehind, m.Target)
		}
	}
	st.State, st.CancelReason, st.CancelledBy, st.CancelledAt = CreditorCancelled, reason, by, at.UTC()
	c.record(eventCreditorCancelled, at, 0)
	return nil
}

// Targets lists the accruals the invoice matched.
func (c *CreditorInvoice) Targets() []TargetKey {
	var out []TargetKey
	for _, m := range c.state.Matches {
		out = append(out, m.Target)
	}
	return out
}

func (c *CreditorInvoice) record(kind string, at time.Time, version int) {
	c.pending = append(c.pending, Event{Type: kind, At: at.UTC(), TenantID: c.state.TenantID, SubjectID: c.state.InvoiceID,
		LegalEntityID: c.state.LegalEntityID, Version: version, CreditorPartyID: c.state.CreditorPartyID})
}

// Stamp records when FIN entered what was written since the invoice was loaded (T1).
func (c *CreditorInvoice) Stamp(entered time.Time) {
	e := entered.UTC()
	st := &c.state
	if st.EnteredAt.IsZero() {
		st.EnteredAt = e
	}
	if !st.MatchedAt.IsZero() && st.MatchEnteredAt.IsZero() {
		st.MatchEnteredAt = e
	}
	if !st.ApprovedAt.IsZero() && st.ApprovalEnteredAt.IsZero() {
		st.ApprovalEnteredAt = e
	}
	if !st.CancelledAt.IsZero() && st.CancelEnteredAt.IsZero() {
		st.CancelEnteredAt = e
	}
	for i := range st.Disputes {
		d := &st.Disputes[i]
		if d.EnteredAt.IsZero() {
			d.EnteredAt = e
		}
		if !d.ResolvedAt.IsZero() && d.ResolveEnteredAt.IsZero() {
			d.ResolveEnteredAt = e
		}
	}
}

// RestoreCreditorInvoice rebuilds a stored invoice.
func RestoreCreditorInvoice(st CreditorInvoiceState) (*CreditorInvoice, error) {
	st.Lines, st.Matches, st.Disputes = slices.Clone(st.Lines), slices.Clone(st.Matches), slices.Clone(st.Disputes)
	for i, l := range st.Lines {
		if l.No != i+1 {
			return nil, fmt.Errorf("%w: line %d of creditor invoice %s", ErrInvalid, i+1, st.InvoiceID)
		}
	}
	for i, m := range st.Matches {
		if m.No != i+1 || m.LineNo < 1 || m.LineNo > len(st.Lines) {
			return nil, fmt.Errorf("%w: match %d of creditor invoice %s", ErrInvalid, i+1, st.InvoiceID)
		}
	}
	for i, d := range st.Disputes {
		if d.No != i+1 {
			return nil, fmt.Errorf("%w: dispute %d of creditor invoice %s", ErrInvalid, i+1, st.InvoiceID)
		}
	}
	switch st.State {
	case CreditorRecorded, CreditorMatched, CreditorDisputed, CreditorCancelled:
	default:
		return nil, fmt.Errorf("%w: creditor invoice %s is %q", ErrInvalid, st.InvoiceID, st.State)
	}
	if st.State == CreditorDisputed && (len(st.Disputes) == 0 || !st.Disputes[len(st.Disputes)-1].Open()) {
		return nil, fmt.Errorf("%w: creditor invoice %s is disputed without an open dispute", ErrInvalid, st.InvoiceID)
	}
	return &CreditorInvoice{state: st, matches: len(st.Matches), disputes: len(st.Disputes)}, nil
}

// State returns a copy for persistence and read models.
func (c *CreditorInvoice) State() CreditorInvoiceState {
	st := c.state
	st.Lines, st.Matches, st.Disputes = slices.Clone(st.Lines), slices.Clone(st.Matches), slices.Clone(st.Disputes)
	return st
}

// Added returns the matches and disputes written since the invoice was loaded and the stored disputes resolved since.
func (c *CreditorInvoice) Added() (matches []Match, disputes []Dispute, resolved []Dispute) {
	st := c.State()
	for _, no := range c.resolved {
		resolved = append(resolved, st.Disputes[no-1])
	}
	return st.Matches[c.matches:], st.Disputes[c.disputes:], resolved
}

func (c *CreditorInvoice) Pending() []Event { return append([]Event(nil), c.pending...) }

// Acknowledge is permitted only after the transaction holding the invoice and its Outbox events commits.
func (c *CreditorInvoice) Acknowledge() {
	c.pending, c.matches, c.disputes, c.resolved = nil, len(c.state.Matches), len(c.state.Disputes), nil
}
