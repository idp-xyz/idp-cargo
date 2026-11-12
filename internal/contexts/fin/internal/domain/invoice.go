package domain

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Note states: a debit note is ISSUED until credit notes reverse all its lines; a credit note stays ISSUED. An
// issued note never changes otherwise (corrections are credit notes; CargoWise INV and CRD).
const (
	NoteIssued    = "ISSUED"
	NoteCancelled = "CANCELLED_BY_CREDIT_NOTE"
)

// DocumentRef is the issued house document a debit note bills on, from DOC's TransportDocumentSnapshot v1, with the
// time that version was issued.
type DocumentRef struct {
	DocumentID   string
	Version      int
	Number       string
	MasterNumber string
	IssuedAt     time.Time
}

// TaxRef is the tax treatment version a note line was billed under.
type TaxRef struct {
	No        int
	Treatment string
	RateBP    int
}

// NoteLine is a line of a note. A debit note line invoices a sell side: the side's amount in its currency, the
// amount in the note's currency and in the functional currency. A note in the functional currency converts a foreign
// side at the INVOICE rate (FX of that type); a note in a foreign currency bills sides of that currency and converts
// them into the functional currency at the ACCOUNTING rate (OD-5 B). A credit note line reverses a debit note line:
// every amount negated, the same conversion and tax.
type NoteLine struct {
	No            int
	JobLineNo     int
	SideVersion   int // a debit note line: the sell side version it wrote, INVOICED
	CreditsLineNo int // a credit note line: the debit note line it reverses
	ChargeCode    string
	Amount        kernel.Money
	Invoice       int64
	Local         int64
	FX            *FxSnapshot
	Tax           TaxRef
}

// Note is a debit note or a credit note of the job, to one debtor, numbered from the legal entity's series of its
// kind at issue. A debit note freezes the debtor's billing profile version, the payment terms of its credit account
// with the due date (due on the invoice date without an account) and the house document it bills on; a credit note
// names the debit note it reverses lines of and the reason.
type Note struct {
	ID             string
	Kind           string
	Number         string
	Seq            int64
	DebtorPartyID  string
	DebtorRevision int64
	ProfileVersion int
	Currency       kernel.CurrencyCode
	InvoiceDate    time.Time
	DueDate        time.Time
	PaymentDays    int
	PaymentBasis   string
	TermsVersion   int
	Document       DocumentRef
	CreditsID      string
	Reason         string
	WriteOff       bool
	State          string
	Total          int64
	LocalTotal     int64
	Lines          []NoteLine
	By             string
	At             time.Time
	EnteredAt      time.Time
}

const (
	eventDebitNoteIssued    = "DebtorInvoiceIssued.v1"
	eventDebitNoteCancelled = "DebtorInvoiceCancelled.v1"
	eventCreditNoteIssued   = "CreditNoteIssued.v1"
)

// Draw takes the next number of the legal entity's series of a note kind, under the series' lock.
type Draw func(kind string) (number string, seq int64, err error)

// DebitNoteInput is a debit note as asked, with what FIN read for it: the debtor's revision ORG confirmed in a debtor
// role, its billing profile version, the terms of its credit account (nil without one) and the issued house
// document.
type DebitNoteInput struct {
	ID             string
	DebtorPartyID  string
	DebtorRevision int64
	LineNos        []int
	Currency       kernel.CurrencyCode
	InvoiceDate    time.Time
	Profile        ProfileVersion
	Terms          *TermsVersion
	Document       DocumentRef
}

func (j *FreightJob) note(id string) *Note {
	for i := range j.state.Notes {
		if j.state.Notes[i].ID == id {
			return &j.state.Notes[i]
		}
	}
	return nil
}

func addAmount(sum, n int64) (int64, error) {
	sum += n
	if sum > MaxAmount || sum < -MaxAmount {
		return 0, fmt.Errorf("%w: a note total beyond %d minor units", ErrInvalid, MaxAmount)
	}
	return sum, nil
}

// dueDate applies payment terms to an invoice date: days from the invoice date, or from the end of its month.
func dueDate(invoice time.Time, t *TermsVersion) (due time.Time, days int, basis string, version int) {
	if t == nil {
		return invoice, 0, TermsFromInvoiceDate, 0
	}
	base := invoice
	if t.PaymentBasis == TermsFromMonthEnd {
		base = time.Date(invoice.Year(), invoice.Month()+1, 0, 0, 0, 0, 0, time.UTC)
	}
	return base.AddDate(0, 0, t.PaymentDays), t.PaymentDays, t.PaymentBasis, t.No
}

// IssueDebitNote bills sell sides of the job to one debtor: lines whose latest sell side is ESTIMATED, non-zero and of
// that debtor, each under the tax treatment of its charge code in force at the invoice date, the invoice date being the
// issue's date. The total is positive. The note takes the next number of the debit note series, and every billed
// side a new version, INVOICED, at the note's local amount (ISSUE_DEBTOR_INVOICE; one job, one debtor; several notes
// per job and debtor, as supplementary charges arise).
func (j *FreightJob) IssueDebitNote(in DebitNoteInput, p *FinancePolicy, draw Draw, by string, at time.Time) error {
	if err := j.open(by, at); err != nil {
		return err
	}
	st := &j.state
	_, known := kernel.MinorDigits(in.Currency)
	switch {
	case !identifier(in.ID) || !identifier(in.DebtorPartyID) || in.DebtorRevision < 1 || len(in.LineNos) == 0 || !known:
		return fmt.Errorf("%w: a debit note names its id, debtor, lines and a currency with known minor digits", ErrInvalid)
	case !dateOf(in.InvoiceDate, at):
		return fmt.Errorf("%w: the invoice date is the date of the issue", ErrInvalid)
	case in.Profile.No < 1:
		return fmt.Errorf("%w: %s", ErrNoBillingProfile, in.DebtorPartyID)
	case !identifier(in.Document.DocumentID) || in.Document.Version < 1 || !identifier(in.Document.Number) || in.Document.IssuedAt.IsZero():
		return fmt.Errorf("%w: shipment %s", ErrNoIssuedDocument, st.ShipmentID)
	case at.Before(in.Document.IssuedAt):
		return fmt.Errorf("%w: house document %s was issued at %s, after the note's business time", ErrNoIssuedDocument, in.Document.Number,
			in.Document.IssuedAt.UTC().Format(time.RFC3339))
	case j.note(in.ID) != nil:
		return fmt.Errorf("%w: note %s exists", ErrInvalidTransition, in.ID)
	case p.State().LegalEntityID != st.LegalEntityID:
		return fmt.Errorf("%w: the policy is another legal entity's", ErrInvalid)
	}
	n := Note{ID: in.ID, Kind: KindDebitNote, DebtorPartyID: in.DebtorPartyID, DebtorRevision: in.DebtorRevision, ProfileVersion: in.Profile.No,
		Currency: in.Currency, InvoiceDate: in.InvoiceDate.UTC(), Document: in.Document, State: NoteIssued, By: by, At: at.UTC()}
	n.DueDate, n.PaymentDays, n.PaymentBasis, n.TermsVersion = dueDate(n.InvoiceDate, in.Terms)
	seen := map[int]bool{}
	var err error
	for _, no := range in.LineNos {
		if no < 1 || no > len(st.Lines) || seen[no] {
			return fmt.Errorf("%w: line %d is not a line of the job, or twice on the note", ErrInvalid, no)
		}
		seen[no] = true
		l := st.Lines[no-1]
		if len(l.Sell) == 0 {
			return fmt.Errorf("%w: line %d has no sell side", ErrInvalid, no)
		}
		v := l.Sell[len(l.Sell)-1]
		switch {
		case v.Status == StatusInvoiced:
			return fmt.Errorf("%w: line %d is on debit note %s", ErrInvoiced, no, v.InvoiceID)
		case v.PartyID != in.DebtorPartyID:
			return fmt.Errorf("%w: line %d bills %s", ErrInvalid, no, v.PartyID)
		case v.Amount.Minor == 0:
			return fmt.Errorf("%w: line %d charges nothing; a zero charge is not billed", ErrInvalid, no)
		case in.Currency != st.FunctionalCurrency && v.Amount.Currency != in.Currency:
			return fmt.Errorf("%w: line %d is in %s; a note in %s bills lines in %s only, a note in %s converts", ErrInvalid, no, v.Amount.Currency,
				in.Currency, in.Currency, st.FunctionalCurrency)
		}
		tax, ok := p.TaxOn(l.ChargeCode, n.InvoiceDate)
		if !ok {
			return fmt.Errorf("%w: %s on %s", ErrTaxMissing, l.ChargeCode, n.InvoiceDate.Format(time.DateOnly))
		}
		line := NoteLine{No: len(n.Lines) + 1, JobLineNo: no, SideVersion: v.No + 1, ChargeCode: l.ChargeCode, Amount: v.Amount,
			Tax: TaxRef{No: tax.No, Treatment: tax.Treatment, RateBP: tax.RateBP}}
		switch {
		case v.Amount.Currency == st.FunctionalCurrency:
			line.Invoice, line.Local = v.Amount.Minor, v.Amount.Minor
		case v.Amount.Currency == in.Currency:
			line.Invoice = v.Amount.Minor
			line.Local, line.FX, err = p.Local(v.Amount, RateAccounting, at)
		default:
			line.Invoice, line.FX, err = p.Local(v.Amount, RateInvoice, at)
			line.Local = line.Invoice
		}
		if err != nil {
			return err
		}
		if line.Local > MaxAmount || line.Local < -MaxAmount || line.Invoice > MaxAmount || line.Invoice < -MaxAmount {
			return fmt.Errorf("%w: line %d beyond %d minor units", ErrInvalid, no, MaxAmount)
		}
		if n.Total, err = addAmount(n.Total, line.Invoice); err != nil {
			return err
		}
		if n.LocalTotal, err = addAmount(n.LocalTotal, line.Local); err != nil {
			return err
		}
		n.Lines = append(n.Lines, line)
	}
	if n.Total <= 0 {
		return fmt.Errorf("%w: a debit note's total is positive; a reduction is a credit note", ErrInvalid)
	}
	if n.Number, n.Seq, err = draw(KindDebitNote); err != nil {
		return err
	}
	st.Notes = append(st.Notes, n)
	j.recordNote(eventDebitNoteIssued, at, n)
	for _, line := range n.Lines {
		l := &st.Lines[line.JobLineNo-1]
		prev := l.Sell[len(l.Sell)-1]
		l.Sell = append(l.Sell, SideVersion{Side: SideSell, No: line.SideVersion, PartyID: prev.PartyID, PartyRevision: in.DebtorRevision,
			Amount: prev.Amount, Local: line.Local, FX: line.FX, Status: StatusInvoiced, Reason: "debit note " + n.Number, InvoiceID: n.ID,
			InvoiceLineNo: line.No, By: by, At: at.UTC()})
		j.recordLine(eventSideRevised, at, l.No, SideSell, line.SideVersion)
	}
	return nil
}

// CreditNoteInput is a credit note as asked: the debit note and its lines to reverse, the reason and the invoice date.
// WriteOff returns the reversed sides at zero (a waived charge, not billed again) instead of at their amount.
type CreditNoteInput struct {
	ID          string
	DebitNoteID string
	LineNos     []int
	Reason      string
	InvoiceDate time.Time
	WriteOff    bool
}

// IssueCreditNote reverses whole lines of an ISSUED debit note of the job with a reason (ISSUE_CREDIT_NOTE): each line
// once, every amount negated under the debit note's conversion and tax. A credit note reduces what the debtor owes (its
// total is negative), and the lines it leaves on the debit note still total more than zero, so a discount line is
// reversed with, or after, the lines it discounts. The note takes the next number of the credit note series; each
// reversed sell side returns to ESTIMATED as a new version, at its amount converted at the ESTIMATE rate in force
// (FIN_RATE_MISSING without one), to be revised or billed again, or at zero for a write-off; a debit note with every line
// reversed is CANCELLED_BY_CREDIT_NOTE. Fapiao and their red-letter records come later (FB6), so none is asked for yet.
func (j *FreightJob) IssueCreditNote(in CreditNoteInput, p *FinancePolicy, draw Draw, by string, at time.Time) error {
	if err := j.open(by, at); err != nil {
		return err
	}
	st := &j.state
	switch {
	case !identifier(in.ID) || !text(in.Reason) || len(in.LineNos) == 0:
		return fmt.Errorf("%w: a credit note names its id, the debit note lines it reverses and a reason", ErrInvalid)
	case !dateOf(in.InvoiceDate, at):
		return fmt.Errorf("%w: the invoice date is the date of the issue", ErrInvalid)
	case j.note(in.ID) != nil:
		return fmt.Errorf("%w: note %s exists", ErrInvalidTransition, in.ID)
	}
	d := j.note(in.DebitNoteID)
	switch {
	case d == nil || d.Kind != KindDebitNote:
		return fmt.Errorf("%w: the job has no debit note %s", ErrInvalid, in.DebitNoteID)
	case d.State != NoteIssued:
		return fmt.Errorf("%w: debit note %s is %s", ErrInvalidTransition, d.Number, d.State)
	case in.InvoiceDate.Before(d.InvoiceDate):
		return fmt.Errorf("%w: a credit note is dated on or after debit note %s", ErrInvalid, d.Number)
	}
	credited := map[int]bool{}
	remaining := d.Total
	for _, o := range st.Notes {
		if o.CreditsID == d.ID {
			remaining += o.Total
			for _, l := range o.Lines {
				credited[l.CreditsLineNo] = true
			}
		}
	}
	n := Note{ID: in.ID, Kind: KindCreditNote, DebtorPartyID: d.DebtorPartyID, DebtorRevision: d.DebtorRevision, ProfileVersion: d.ProfileVersion,
		Currency: d.Currency, InvoiceDate: in.InvoiceDate.UTC(), CreditsID: d.ID, Reason: in.Reason, WriteOff: in.WriteOff, State: NoteIssued, By: by,
		At: at.UTC()}
	var returned []SideVersion
	var err error
	for _, no := range in.LineNos {
		if no < 1 || no > len(d.Lines) || credited[no] {
			return fmt.Errorf("%w: line %d of debit note %s is not there or reversed already", ErrInvalidTransition, no, d.Number)
		}
		credited[no] = true
		dl := d.Lines[no-1]
		sell := st.Lines[dl.JobLineNo-1].Sell
		v := sell[len(sell)-1]
		if v.Status != StatusInvoiced || v.InvoiceID != d.ID || v.InvoiceLineNo != dl.No {
			return fmt.Errorf("%w: the sell side of line %d is not the one debit note %s invoiced", ErrInvalid, dl.JobLineNo, d.Number)
		}
		amount := v.Amount
		if in.WriteOff {
			amount.Minor = 0
		}
		back, err := j.side(SideSell, v.PartyID, v.PartyRevision, amount, StatusEstimated, "credit note", p, v.No+1, by, at)
		if err != nil {
			return err
		}
		returned = append(returned, back)
		line := NoteLine{No: len(n.Lines) + 1, JobLineNo: dl.JobLineNo, CreditsLineNo: dl.No, ChargeCode: dl.ChargeCode,
			Amount: kernel.Money{Currency: dl.Amount.Currency, Minor: -dl.Amount.Minor}, Invoice: -dl.Invoice, Local: -dl.Local, FX: dl.FX, Tax: dl.Tax}
		if n.Total, err = addAmount(n.Total, line.Invoice); err != nil {
			return err
		}
		if n.LocalTotal, err = addAmount(n.LocalTotal, line.Local); err != nil {
			return err
		}
		n.Lines = append(n.Lines, line)
	}
	switch remaining += n.Total; {
	case n.Total >= 0:
		return fmt.Errorf("%w: a credit note reduces what the debtor owes; reverse a discount line with the lines it discounts", ErrInvalid)
	case len(credited) < len(d.Lines) && remaining <= 0:
		return fmt.Errorf("%w: the lines left on debit note %s would total %d; reverse its discount lines with them", ErrInvalid, d.Number, remaining)
	}
	if n.Number, n.Seq, err = draw(KindCreditNote); err != nil {
		return err
	}
	cancelled := len(credited) == len(d.Lines)
	if cancelled {
		d.State = NoteCancelled // before the append below, which may move the notes d points into
		j.cancelled = append(j.cancelled, d.ID)
	}
	debit := *d
	st.Notes = append(st.Notes, n)
	j.recordNote(eventCreditNoteIssued, at, n)
	if cancelled {
		j.recordNote(eventDebitNoteCancelled, at, debit)
	}
	for i, back := range returned {
		back.Reason = "credit note " + n.Number
		if in.WriteOff {
			back.Reason = "credit note " + n.Number + ", written off"
		}
		l := &st.Lines[n.Lines[i].JobLineNo-1]
		l.Sell = append(l.Sell, back)
		j.recordLine(eventSideRevised, at, l.No, SideSell, back.No)
	}
	return nil
}

func (j *FreightJob) recordNote(kind string, at time.Time, n Note) {
	j.pending = append(j.pending, Event{Type: kind, At: at.UTC(), TenantID: j.state.TenantID, SubjectID: n.ID, LegalEntityID: j.state.LegalEntityID,
		ShipmentID: j.state.ShipmentID, DebtorPartyID: n.DebtorPartyID, JobID: j.state.JobID})
}

// Credits returns the credit notes of a debit note of the job.
func (j *FreightJob) Credits(debitNoteID string) []Note {
	var out []Note
	for _, n := range j.state.Notes {
		if n.CreditsID == debitNoteID {
			out = append(out, n)
		}
	}
	return out
}

// TaxTotal is a note's total under one tax treatment.
type TaxTotal struct {
	Treatment string
	RateBP    int
	Invoice   int64
	Local     int64
}

// TaxTotals sums a note's lines per tax treatment and rate, in the order they first appear.
func (n Note) TaxTotals() []TaxTotal {
	var out []TaxTotal
	for _, l := range n.Lines {
		i := 0
		for i < len(out) && (out[i].Treatment != l.Tax.Treatment || out[i].RateBP != l.Tax.RateBP) {
			i++
		}
		if i == len(out) {
			out = append(out, TaxTotal{Treatment: l.Tax.Treatment, RateBP: l.Tax.RateBP})
		}
		out[i].Invoice += l.Invoice
		out[i].Local += l.Local
	}
	return out
}
