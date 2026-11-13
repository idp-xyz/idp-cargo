package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FB6: fapiao records (发票). A fapiao record covers issued debit notes of one legal entity and one Chinese debtor,
// often a month of them, and freezes the buyer data of the debtor's billing profile. FIN does not issue the fapiao: the
// national e-invoice service platform does (fully digital e-invoices, 数电票, since 2024-12-01), and an authorized
// operator records its number, date and evidence, never inferred from a call succeeding. Corrections are red-letter
// fapiao, recorded before a credit note reverses a covered debit note (CargoWise: INV and CRD with RongJin's portal).
// Amounts are what the debtor pays, in CNY, the functional currency; the record splits out the VAT of taxable lines.
// The tax adviser confirms the treatments (exemption of international freight forwarding, disbursements, fapiao for
// overseas debtors); until then a pass-through disbursement is left off the fapiao and a foreign debtor takes none.

// Fapiao record states and types.
const (
	FapiaoRequested   = "REQUESTED"
	FapiaoIssued      = "ISSUED"
	FapiaoFailed      = "FAILED"
	FapiaoRedLettered = "RED_LETTERED"

	FapiaoOrdinary = "ORDINARY" // 普通发票: exempt international freight forwarding takes these only
	FapiaoSpecial  = "SPECIAL"  // 专用发票: taxable lines only, as the tax adviser confirms
)

// MaxFapiaoNotes is how many debit notes one fapiao record covers.
const MaxFapiaoNotes = 200

var (
	ErrFapiaoCovers   = errors.New("fin: FIN_FAPIAO_COVERS: a fapiao record requested or issued covers the debit note; record the fapiao failed, or its red-letter fapiao, first")
	ErrFapiaoNumber   = errors.New("fin: FIN_FAPIAO_NUMBER_USED: this fapiao number was recorded already (the platform's numbers are unique nationwide)")
	ErrNotFapiaoable  = errors.New("fin: FIN_NOT_FAPIAOABLE: not issued debit notes of the legal entity and debtor with an amount to put on a fapiao")
	fapiaoNumberRegex = regexp.MustCompile(`^[0-9]{20}$`)
)

const (
	eventFapiaoIssued      = "FapiaoIssued.v1"
	eventFapiaoRedLettered = "FapiaoRedLettered.v1"
)

// TaxAmount is an amount of a tax treatment and rate in the functional currency.
type TaxAmount struct {
	Treatment string
	RateBP    int // TAXABLE only; 0 otherwise
	Local     int64
}

// FapiaoNote is what a fapiao request needs of a debit note, read once the note's job is locked: its identity and
// state, and per tax treatment and rate what its lines not reversed by credit notes bill.
type FapiaoNote struct {
	InvoiceID     string
	Kind          string
	State         string
	Number        string
	LegalEntityID string
	DebtorPartyID string
	InvoiceDate   time.Time
	Amounts       []TaxAmount
}

// CoveredNote is a debit note a record covers, with the amounts it puts on the fapiao (its pass-through disbursements
// stay off it, and credit notes may reverse them while the note is covered). Released is set once the record failed or
// was red-lettered: the note may then be covered again or credited.
type CoveredNote struct {
	InvoiceID   string
	Number      string
	InvoiceDate time.Time
	Amounts     []TaxAmount
	Released    bool
}

// FapiaoLine is what the fapiao states for a tax treatment and rate: the amount with tax, without it, and the VAT.
type FapiaoLine struct {
	Treatment string
	RateBP    int
	Gross     int64
	Net       int64
	VAT       int64
}

// FapiaoBuyer is the buyer data frozen from the debtor's billing profile version.
type FapiaoBuyer struct {
	ProfileVersion int
	Title          string
	TaxpayerNumber string
	Address        string
	Phone          string
	BankName       string
	BankAccount    string
}

// PlatformEvidence is a fapiao the national platform issued, as an authorized operator recorded it: its number and
// date, the evidence reference (the file's hash or the platform's reference) and, for a red-letter fapiao, the
// confirmation form where the rules require one.
type PlatformEvidence struct {
	Number          string
	On              time.Time
	EvidenceRef     string
	ConfirmationRef string
	By              string
	At              time.Time
	EnteredAt       time.Time
}

// FapiaoState is a stored fapiao record, for persistence.
type FapiaoState struct {
	TenantID       TenantID
	FapiaoID       string
	LegalEntityID  string
	DebtorPartyID  string
	DebtorRevision int64
	Type           string
	Currency       kernel.CurrencyCode
	Buyer          FapiaoBuyer
	Notes          []CoveredNote
	State          string
	RequestedBy    string
	RequestedAt    time.Time
	EnteredAt      time.Time
	Issued         *PlatformEvidence
	FailReason     string
	FailedBy       string
	FailedAt       time.Time
	FailEnteredAt  time.Time
	RedLetter      *PlatformEvidence
	RedReason      string
}

// Lines groups the covered amounts by tax treatment and rate, in that order, with the VAT a taxable amount includes
// (rounded half up once per rate).
func (s FapiaoState) Lines() (lines []FapiaoLine) {
	type key struct {
		treatment string
		rate      int
	}
	sums := map[key]int64{}
	for _, n := range s.Notes {
		for _, a := range n.Amounts {
			sums[key{a.Treatment, a.RateBP}] += a.Local
		}
	}
	keys := make([]key, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int {
		if c := strings.Compare(a.treatment, b.treatment); c != 0 {
			return c
		}
		return a.rate - b.rate
	})
	for _, k := range keys {
		gross := sums[k]
		if gross == 0 {
			continue
		}
		var vat int64
		if k.treatment == TaxTaxable && k.rate > 0 && gross > 0 {
			den := int64(10_000 + k.rate)
			vat = (gross*int64(k.rate) + den/2) / den
		}
		lines = append(lines, FapiaoLine{Treatment: k.treatment, RateBP: k.rate, Gross: gross, Net: gross - vat, VAT: vat})
	}
	return lines
}

// Total is what the fapiao states in all: the amount with tax and the VAT it includes.
func (s FapiaoState) Total() (gross, vat int64) {
	for _, l := range s.Lines() {
		gross, vat = gross+l.Gross, vat+l.VAT
	}
	return gross, vat
}

// Live reports whether the record covers its notes: requested or issued.
func (s FapiaoState) Live() bool { return s.State == FapiaoRequested || s.State == FapiaoIssued }

// FapiaoRecord is the aggregate root of a fapiao the legal entity asks the platform to issue on debit notes.
type FapiaoRecord struct {
	state   FapiaoState
	changed bool
	pending []Event
}

// FapiaoInput is a fapiao as asked, with what FIN read for it: the debtor's revision ORG confirmed in a debtor role,
// its billing profile version (which still names it as ORG does) and the legal entity's functional currency.
type FapiaoInput struct {
	ID             string
	LegalEntityID  string
	DebtorPartyID  string
	DebtorRevision int64
	Type           string
	Currency       kernel.CurrencyCode
	Profile        ProfileVersion
}

// RequestFapiao asks for a fapiao on issued debit notes of one legal entity and one Chinese enterprise debtor, none
// covered by another live record (REQUEST_FAPIAO): in CNY; each note with an amount to put on it (its pass-through
// disbursements stay off), none of the fapiao's treatments below zero; a SPECIAL fapiao only without exempt amounts.
func RequestFapiao(tenant TenantID, in FapiaoInput, notes []FapiaoNote, by string, at time.Time) (*FapiaoRecord, error) {
	p := in.Profile
	switch {
	case tenant == "" || !identifier(in.ID) || !identifier(in.LegalEntityID) || !identifier(in.DebtorPartyID) || in.DebtorRevision < 1 || blank(by) || at.IsZero():
		return nil, fmt.Errorf("%w: a fapiao record names its id, legal entity, debtor, author and time", ErrInvalid)
	case in.Type != FapiaoOrdinary && in.Type != FapiaoSpecial:
		return nil, fmt.Errorf("%w: a fapiao is ORDINARY or SPECIAL", ErrInvalid)
	case in.Currency != "CNY":
		return nil, fmt.Errorf("%w: a fapiao is in CNY; the legal entity's functional currency is %s", ErrNotFapiaoable, in.Currency)
	case p.BuyerKind != BuyerCNEnterprise || p.TaxpayerNumber == "" || p.No < 1:
		return nil, fmt.Errorf("%w: a fapiao goes to a Chinese enterprise buyer with a billing profile; fapiao for overseas debtors wait for the tax adviser",
			ErrNotFapiaoable)
	case len(notes) == 0 || len(notes) > MaxFapiaoNotes:
		return nil, fmt.Errorf("%w: a fapiao record covers 1 to %d debit notes", ErrInvalid, MaxFapiaoNotes)
	}
	r := &FapiaoRecord{state: FapiaoState{TenantID: tenant, FapiaoID: in.ID, LegalEntityID: in.LegalEntityID, DebtorPartyID: in.DebtorPartyID,
		DebtorRevision: in.DebtorRevision, Type: in.Type, Currency: in.Currency, State: FapiaoRequested, RequestedBy: by, RequestedAt: at.UTC(),
		Buyer: FapiaoBuyer{ProfileVersion: p.No, Title: p.Title, TaxpayerNumber: p.TaxpayerNumber, Address: p.RegisteredAddress, Phone: p.RegisteredPhone,
			BankName: p.BankName, BankAccount: p.BankAccount}}, changed: true}
	seen := map[string]bool{}
	for _, n := range notes {
		switch {
		case seen[n.InvoiceID]:
			return nil, fmt.Errorf("%w: debit note %s twice", ErrInvalid, n.InvoiceID)
		case n.Kind != KindDebitNote || n.State != NoteIssued || n.LegalEntityID != in.LegalEntityID || n.DebtorPartyID != in.DebtorPartyID:
			return nil, fmt.Errorf("%w: %s is not an issued debit note of %s to %s", ErrNotFapiaoable, n.InvoiceID, in.LegalEntityID, in.DebtorPartyID)
		}
		seen[n.InvoiceID] = true
		c := CoveredNote{InvoiceID: n.InvoiceID, Number: n.Number, InvoiceDate: n.InvoiceDate}
		for _, a := range n.Amounts {
			if a.Local != 0 && a.Treatment != TaxPassThrough {
				c.Amounts = append(c.Amounts, a)
			}
		}
		if len(c.Amounts) == 0 {
			return nil, fmt.Errorf("%w: debit note %s puts nothing on a fapiao (pass-through disbursements stay off it)", ErrNotFapiaoable, n.Number)
		}
		r.state.Notes = append(r.state.Notes, c)
	}
	slices.SortFunc(r.state.Notes, func(a, b CoveredNote) int { return strings.Compare(a.InvoiceID, b.InvoiceID) })
	// each amount within MaxAmount, and their sum, so that the VAT (amount × rate) cannot overflow
	var sum int64
	for _, n := range r.state.Notes {
		for _, a := range n.Amounts {
			if a.Local > MaxAmount || a.Local < -MaxAmount {
				return nil, fmt.Errorf("%w: debit note %s bills more than %d", ErrNotFapiaoable, n.Number, int64(MaxAmount))
			}
			if a.Local < 0 {
				sum -= a.Local
			} else {
				sum += a.Local
			}
			if sum > MaxAmount {
				return nil, fmt.Errorf("%w: the notes bill more than one fapiao record takes (%d)", ErrNotFapiaoable, int64(MaxAmount))
			}
		}
	}
	gross, _ := r.state.Total()
	for _, l := range r.state.Lines() {
		switch {
		case l.Gross < 0:
			return nil, fmt.Errorf("%w: the %s amounts come to %d, below zero", ErrNotFapiaoable, l.Treatment, l.Gross)
		case in.Type == FapiaoSpecial && l.Treatment == TaxExemptIntlFreight:
			return nil, fmt.Errorf("%w: exempt international freight forwarding takes an ORDINARY fapiao only", ErrNotFapiaoable)
		}
	}
	if gross <= 0 {
		return nil, fmt.Errorf("%w: nothing to put on a fapiao", ErrNotFapiaoable)
	}
	return r, nil
}

// evidence checks platform evidence recorded at at: the 20-digit number of a fully digital e-invoice (数电票, the
// platform's only kind since 2024-12-01), a civil date not after the day it is recorded and not more than 366 days
// before, an evidence reference and an optional confirmation reference.
func evidence(e PlatformEvidence, by string, at time.Time) error {
	switch {
	case blank(by) || at.IsZero():
		return fmt.Errorf("%w: a record names its author and time", ErrInvalid)
	case !fapiaoNumberRegex.MatchString(e.Number):
		return fmt.Errorf("%w: a fapiao number is the 20 digits of a fully digital e-invoice", ErrInvalid)
	case !date(e.On) || at.Before(e.On.Add(-14*time.Hour)) || !at.Before(e.On.Add(maxReceiptAge+36*time.Hour)):
		return fmt.Errorf("%w: the fapiao date is not after the day it is recorded and not more than 366 days before", ErrInvalid)
	case !receiptEvidencePattern.MatchString(e.EvidenceRef), e.ConfirmationRef != "" && !receiptEvidencePattern.MatchString(e.ConfirmationRef):
		return fmt.Errorf("%w: evidence is a reference (letters, digits, space and . _ : / # -, 200 characters, not ending with a space), such "+
			"as the file's hash", ErrInvalid)
	}
	return nil
}

// RecordIssued records the fapiao the platform issued (RECORD_FAPIAO_ISSUED), dated not before the debit notes it
// covers.
func (r *FapiaoRecord) RecordIssued(e PlatformEvidence, by string, at time.Time) error {
	st := &r.state
	if st.State != FapiaoRequested {
		return fmt.Errorf("%w: fapiao record %s is %s", ErrInvalidTransition, st.FapiaoID, st.State)
	}
	if err := evidence(e, by, at); err != nil {
		return err
	}
	for _, n := range st.Notes {
		if e.On.Before(n.InvoiceDate) {
			return fmt.Errorf("%w: the fapiao is dated before debit note %s", ErrInvalid, n.Number)
		}
	}
	if at.Before(st.RequestedAt) || e.ConfirmationRef != "" {
		return fmt.Errorf("%w: a fapiao is recorded after its request, without a confirmation form", ErrInvalid)
	}
	e.By, e.At, e.EnteredAt = by, at.UTC(), time.Time{}
	st.Issued, st.State, r.changed = &e, FapiaoIssued, true
	r.record(eventFapiaoIssued, at)
	return nil
}

// RecordFailed records that the platform refused the fapiao or the operator abandoned it, with a reason
// (RECORD_FAPIAO_FAILED): the debit notes are covered no longer.
func (r *FapiaoRecord) RecordFailed(reason, by string, at time.Time) error {
	st := &r.state
	switch {
	case st.State != FapiaoRequested:
		return fmt.Errorf("%w: fapiao record %s is %s", ErrInvalidTransition, st.FapiaoID, st.State)
	case !text(reason) || utf8.RuneCountInString(reason) > 500 || blank(by) || at.IsZero() || at.Before(st.RequestedAt):
		return fmt.Errorf("%w: a failure gives its reason (500 characters), author and time", ErrInvalid)
	}
	st.FailReason, st.FailedBy, st.FailedAt, st.State, r.changed = reason, by, at.UTC(), FapiaoFailed, true
	r.release()
	return nil
}

// RecordRedLetter records the red-letter fapiao that reversed the issued one, with the reference of its red-letter
// information confirmation form (红字发票信息确认单, filled in for every fully digital red-letter fapiao) and a reason
// (RECORD_RED_LETTER): credit notes may then reverse the debit notes, and a new record may cover them.
func (r *FapiaoRecord) RecordRedLetter(e PlatformEvidence, reason, by string, at time.Time) error {
	st := &r.state
	switch {
	case st.State != FapiaoIssued:
		return fmt.Errorf("%w: fapiao record %s is %s", ErrInvalidTransition, st.FapiaoID, st.State)
	case !text(reason) || utf8.RuneCountInString(reason) > 500:
		return fmt.Errorf("%w: a red-letter fapiao gives its reason (500 characters)", ErrInvalid)
	case e.ConfirmationRef == "":
		return fmt.Errorf("%w: a red-letter fapiao names its confirmation form", ErrInvalid)
	}
	if err := evidence(e, by, at); err != nil {
		return err
	}
	if e.Number == st.Issued.Number || e.On.Before(st.Issued.On) || at.Before(st.Issued.At) {
		return fmt.Errorf("%w: a red-letter fapiao has its own number, dated not before the fapiao it reverses", ErrInvalid)
	}
	e.By, e.At, e.EnteredAt = by, at.UTC(), time.Time{}
	st.RedLetter, st.RedReason, st.State, r.changed = &e, reason, FapiaoRedLettered, true
	r.release()
	r.record(eventFapiaoRedLettered, at)
	return nil
}

func (r *FapiaoRecord) release() {
	for i := range r.state.Notes {
		r.state.Notes[i].Released = true
	}
}

func (r *FapiaoRecord) record(kind string, at time.Time) {
	r.pending = append(r.pending, Event{Type: kind, At: at.UTC(), TenantID: r.state.TenantID, SubjectID: r.state.FapiaoID,
		LegalEntityID: r.state.LegalEntityID, DebtorPartyID: r.state.DebtorPartyID})
}

// Stamp records when FIN entered what was written since the record was loaded (T1).
func (r *FapiaoRecord) Stamp(entered time.Time) {
	e, st := entered.UTC(), &r.state
	if st.EnteredAt.IsZero() {
		st.EnteredAt = e
	}
	if st.Issued != nil && st.Issued.EnteredAt.IsZero() {
		st.Issued.EnteredAt = e
	}
	if !st.FailedAt.IsZero() && st.FailEnteredAt.IsZero() {
		st.FailEnteredAt = e
	}
	if st.RedLetter != nil && st.RedLetter.EnteredAt.IsZero() {
		st.RedLetter.EnteredAt = e
	}
}

// RestoreFapiao rebuilds a stored record.
func RestoreFapiao(st FapiaoState) (*FapiaoRecord, error) {
	st.Notes = append([]CoveredNote(nil), st.Notes...)
	released := !st.Live()
	for _, n := range st.Notes {
		if n.Released != released {
			return nil, fmt.Errorf("%w: fapiao record %s releases debit note %s as %s", ErrInvalid, st.FapiaoID, n.InvoiceID, st.State)
		}
	}
	switch {
	case len(st.Notes) == 0, (st.Issued != nil) != (st.State == FapiaoIssued || st.State == FapiaoRedLettered),
		(st.RedLetter != nil) != (st.State == FapiaoRedLettered), (st.State == FapiaoFailed) == st.FailedAt.IsZero(),
		st.State != FapiaoRequested && st.State != FapiaoIssued && st.State != FapiaoFailed && st.State != FapiaoRedLettered:
		return nil, fmt.Errorf("%w: fapiao record %s", ErrInvalid, st.FapiaoID)
	}
	return &FapiaoRecord{state: st}, nil
}

func (r *FapiaoRecord) State() FapiaoState {
	st := r.state
	st.Notes = append([]CoveredNote(nil), st.Notes...)
	for i := range st.Notes {
		st.Notes[i].Amounts = append([]TaxAmount(nil), st.Notes[i].Amounts...)
	}
	if st.Issued != nil {
		e := *st.Issued
		st.Issued = &e
	}
	if st.RedLetter != nil {
		e := *st.RedLetter
		st.RedLetter = &e
	}
	return st
}

func (r *FapiaoRecord) Pending() []Event { return append([]Event(nil), r.pending...) }
func (r *FapiaoRecord) Acknowledge()     { r.pending, r.changed = nil, false }
