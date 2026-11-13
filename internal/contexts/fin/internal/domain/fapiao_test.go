package domain

import (
	"errors"
	"strings"
	"testing"
)

func fapiaoProfile() ProfileVersion {
	return ProfileVersion{No: 2, BuyerKind: BuyerCNEnterprise, Title: "上海甲贸易有限公司", TaxpayerNumber: "91310000MA1FL8TQ2X", PartyRevision: 4,
		ProfileInput: ProfileInput{BuyerKind: BuyerCNEnterprise, RegisteredAddress: "上海市浦东新区世纪大道100号", RegisteredPhone: "021-5888 0000",
			BankName: "中国银行上海分行", BankAccount: "4567 8901 2345"}}
}

func fapiaoInput() FapiaoInput {
	return FapiaoInput{ID: "FP-1", LegalEntityID: "LE", DebtorPartyID: "party-a", DebtorRevision: 4, Type: FapiaoOrdinary, Currency: "CNY",
		Profile: fapiaoProfile()}
}

// fapiaoNotes: DN-1 bills exempt freight of 8,640.00, a document fee of 300.00 at 6% and a disbursement of 150.00;
// DN-2 a document fee of 100.00 at 6% and handling of 50.00 at 9%, dated a day later.
func fapiaoNotes() []FapiaoNote {
	note := func(id, number string, on string, amounts ...TaxAmount) FapiaoNote {
		return FapiaoNote{InvoiceID: id, Kind: KindDebitNote, State: NoteIssued, Number: number, LegalEntityID: "LE", DebtorPartyID: "party-a",
			InvoiceDate: day(on), Amounts: amounts}
	}
	return []FapiaoNote{
		note("DN-2", "DN000002", "2026-11-12", TaxAmount{TaxTaxable, 600, 10_000}, TaxAmount{TaxTaxable, 900, 5_000}),
		note("DN-1", "DN000001", "2026-11-11", TaxAmount{TaxExemptIntlFreight, 0, 864_000}, TaxAmount{TaxTaxable, 600, 30_000},
			TaxAmount{TaxPassThrough, 0, 15_000}),
	}
}

func issuedEvidence() PlatformEvidence {
	return PlatformEvidence{Number: "26312000000012345678", On: day("2026-11-12"), EvidenceRef: "sha256:9f2c"}
}

// A fapiao record is requested on issued debit notes of one legal entity to one Chinese enterprise debtor, in CNY; it
// states a line per tax treatment and rate (pass-through disbursements left off) with the VAT a taxable amount
// includes, rounded half up once per rate.
func TestRequestFapiao(t *testing.T) {
	r, err := RequestFapiao("T", fapiaoInput(), fapiaoNotes(), "bill-1", hour(30))
	if err != nil {
		t.Fatal(err)
	}
	st := r.State()
	if st.State != FapiaoRequested || len(st.Notes) != 2 || st.Notes[0].InvoiceID != "DN-1" || len(st.Notes[0].Amounts) != 2 || st.Buyer.Title != "上海甲贸易有限公司" ||
		st.Buyer.TaxpayerNumber != "91310000MA1FL8TQ2X" || st.Buyer.ProfileVersion != 2 || st.Buyer.BankAccount != "4567 8901 2345" || !st.Live() {
		t.Fatalf("requested: %+v", st)
	}
	lines := st.Lines()
	// the disbursement left off; 400.00 at 6%: VAT 22.64 (40,000 × 600 / 10,600 = 2,264.15); 50.00 at 9%: VAT 4.13 (412.84)
	want := []FapiaoLine{{TaxExemptIntlFreight, 0, 864_000, 864_000, 0}, {TaxTaxable, 600, 40_000, 37_736, 2_264}, {TaxTaxable, 900, 5_000, 4_587, 413}}
	if len(lines) != len(want) {
		t.Fatalf("lines %+v", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d: %+v, want %+v", i, lines[i], want[i])
		}
	}
	if gross, vat := st.Total(); gross != 909_000 || vat != 2_677 {
		t.Fatalf("total %d, VAT %d", gross, vat)
	}
	// half up: 10.60 at 6% includes 0.60 exactly; 0.53 includes 0.03 (3.0)
	for gross, vat := range map[int64]int64{1_060: 60, 53: 3, 1: 0, 9: 1} {
		s := FapiaoState{Notes: []CoveredNote{{Amounts: []TaxAmount{{TaxTaxable, 600, gross}}}}}
		if lines := s.Lines(); lines[0].VAT != vat {
			t.Errorf("%d at 6%%: VAT %d, want %d", gross, lines[0].VAT, vat)
		}
	}

	refused := func(name string, in FapiaoInput, notes []FapiaoNote, want error) {
		t.Helper()
		if _, err := RequestFapiao("T", in, notes, "bill-1", hour(30)); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	in := fapiaoInput()
	in.Type = FapiaoSpecial
	refused("a special fapiao with exempt freight", in, fapiaoNotes(), ErrNotFapiaoable)
	if r, err := RequestFapiao("T", in, fapiaoNotes()[:1], "bill-1", hour(30)); err != nil || r.State().Type != FapiaoSpecial {
		t.Errorf("a special fapiao on taxable lines: %v", err)
	}
	in = fapiaoInput()
	in.Type = "VAT"
	refused("an unknown type", in, fapiaoNotes(), ErrInvalid)
	in = fapiaoInput()
	in.Currency = "USD"
	refused("a legal entity keeping its books in USD", in, fapiaoNotes(), ErrNotFapiaoable)
	in = fapiaoInput()
	in.Profile.BuyerKind = BuyerForeign
	refused("a foreign buyer", in, fapiaoNotes(), ErrNotFapiaoable)
	in = fapiaoInput()
	in.DebtorRevision = 0
	refused("no debtor revision", in, fapiaoNotes(), ErrInvalid)
	refused("no notes", fapiaoInput(), nil, ErrInvalid)
	refused("too many notes", fapiaoInput(), make([]FapiaoNote, MaxFapiaoNotes+1), ErrInvalid)
	for name, change := range map[string]func(*FapiaoNote){
		"a credit note":            func(n *FapiaoNote) { n.Kind = KindCreditNote },
		"a cancelled note":         func(n *FapiaoNote) { n.State = NoteCancelled },
		"another legal entity's":   func(n *FapiaoNote) { n.LegalEntityID = "LE-2" },
		"another debtor's":         func(n *FapiaoNote) { n.DebtorPartyID = "party-b" },
		"a negative taxable total": func(n *FapiaoNote) { n.Amounts = []TaxAmount{{TaxTaxable, 600, -20_000}} },
		"a disbursement only":      func(n *FapiaoNote) { n.Amounts = []TaxAmount{{TaxPassThrough, 0, 15_000}} },
		"nothing billed":           func(n *FapiaoNote) { n.Amounts = []TaxAmount{{TaxTaxable, 600, 0}} },
		"over the largest amount":  func(n *FapiaoNote) { n.Amounts = []TaxAmount{{TaxTaxable, 600, MaxAmount + 1}} },
	} {
		notes := fapiaoNotes()[:1]
		change(&notes[0])
		refused(name, fapiaoInput(), notes, ErrNotFapiaoable)
	}
	// the notes' amounts together over the largest amount
	big := fapiaoNotes()
	big[0].Amounts, big[1].Amounts = []TaxAmount{{TaxTaxable, 3000, MaxAmount}}, []TaxAmount{{TaxTaxable, 3000, 1}}
	refused("the notes over the largest amount", fapiaoInput(), big, ErrNotFapiaoable)
	twice := append(fapiaoNotes(), fapiaoNotes()[0])
	refused("a note twice", fapiaoInput(), twice, ErrInvalid)
	// a note with a disbursement only puts nothing on the fapiao, beside another note too
	only := fapiaoNotes()
	only[0].Amounts = []TaxAmount{{TaxPassThrough, 0, 15_000}}
	refused("a disbursement only beside another note", fapiaoInput(), only, ErrNotFapiaoable)
}

// A requested record is issued (number, date, evidence) or fails; an issued one is red-lettered with its own number,
// dated not before it; a failed or red-lettered record releases its notes.
func TestFapiaoLifecycle(t *testing.T) {
	request := func(t *testing.T) *FapiaoRecord {
		t.Helper()
		r, err := RequestFapiao("T", fapiaoInput(), fapiaoNotes(), "bill-1", hour(30))
		if err != nil {
			t.Fatal(err)
		}
		r.Stamp(hour(30))
		r.Acknowledge()
		return r
	}
	r := request(t)
	for name, change := range map[string]func(*PlatformEvidence){
		"a short number":            func(e *PlatformEvidence) { e.Number = "1234567" },
		"an 8-digit number":         func(e *PlatformEvidence) { e.Number = "12345678" },
		"a 21-digit number":         func(e *PlatformEvidence) { e.Number = "263120000000123456789" },
		"letters in the number":     func(e *PlatformEvidence) { e.Number = "2631200000001234567A" },
		"dated before DN-2":         func(e *PlatformEvidence) { e.On = day("2026-11-11") },
		"dated after it is entered": func(e *PlatformEvidence) { e.On = day("2026-11-14") },
		"free text as evidence":     func(e *PlatformEvidence) { e.EvidenceRef = "see the email!" },
		"a confirmation form":       func(e *PlatformEvidence) { e.ConfirmationRef = "RC-1" },
	} {
		e := issuedEvidence()
		change(&e)
		if err := r.RecordIssued(e, "bill-2", hour(32)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := r.RecordRedLetter(issuedEvidence(), "wrong buyer", "bill-2", hour(32)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("red letter of a requested record: %v", err)
	}
	if err := r.RecordIssued(issuedEvidence(), "bill-2", hour(32)); err != nil {
		t.Fatal(err)
	}
	r.Stamp(hour(33))
	if st := r.State(); st.State != FapiaoIssued || st.Issued.By != "bill-2" || !st.Issued.At.Equal(hour(32)) || !st.Issued.EnteredAt.Equal(hour(33)) ||
		len(r.Pending()) != 1 || r.Pending()[0].Type != eventFapiaoIssued || r.Pending()[0].DebtorPartyID != "party-a" || !st.Live() {
		t.Fatalf("issued: %+v %+v", st, r.Pending())
	}
	if err := r.RecordFailed("platform refused", "bill-2", hour(33)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an issued record failing: %v", err)
	}
	red := PlatformEvidence{Number: "26312000000012349999", On: day("2026-11-13"), EvidenceRef: "sha256:aa01", ConfirmationRef: "HZ-2026-0001"}
	same := red
	same.Number = issuedEvidence().Number
	earlier := red
	earlier.On = day("2026-11-11")
	for name, e := range map[string]PlatformEvidence{"its own number": same, "dated before the fapiao": earlier} {
		if err := r.RecordRedLetter(e, "wrong buyer", "bill-2", hour(34)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := r.RecordRedLetter(red, "", "bill-2", hour(34)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no reason: %v", err)
	}
	unconfirmed := red
	unconfirmed.ConfirmationRef = ""
	if err := r.RecordRedLetter(unconfirmed, "wrong buyer title", "bill-2", hour(34)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no confirmation form: %v", err)
	}
	if err := r.RecordRedLetter(red, "wrong buyer title", "bill-2", hour(34)); err != nil {
		t.Fatal(err)
	}
	r.Stamp(hour(34))
	st := r.State()
	if st.State != FapiaoRedLettered || st.RedLetter.ConfirmationRef != "HZ-2026-0001" || st.RedReason != "wrong buyer title" || st.Live() ||
		!st.Notes[0].Released || !st.Notes[1].Released || r.Pending()[len(r.Pending())-1].Type != eventFapiaoRedLettered {
		t.Fatalf("red-lettered: %+v", st)
	}
	if _, err := RestoreFapiao(st); err != nil {
		t.Fatal(err)
	}

	failed := request(t)
	if err := failed.RecordFailed(strings.Repeat("x", 501), "bill-2", hour(31)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a long reason: %v", err)
	}
	if err := failed.RecordFailed("buyer title rejected by the platform", "bill-2", hour(31)); err != nil {
		t.Fatal(err)
	}
	failed.Stamp(hour(31))
	if st := failed.State(); st.State != FapiaoFailed || st.Live() || !st.Notes[0].Released || len(failed.Pending()) != 0 || st.FailEnteredAt.IsZero() {
		t.Fatalf("failed: %+v", st)
	}
	if err := failed.RecordIssued(issuedEvidence(), "bill-2", hour(32)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a failed record issued: %v", err)
	}

	// a stored record that does not hold together is refused
	bad := request(t).State()
	bad.Notes[0].Released = true
	if _, err := RestoreFapiao(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a requested record with a note released: %v", err)
	}
	bad = request(t).State()
	bad.State = FapiaoIssued
	if _, err := RestoreFapiao(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an issued record without its fapiao: %v", err)
	}
}
