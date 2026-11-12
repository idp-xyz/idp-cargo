package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func kernelCurrency(s string) kernel.CurrencyCode { return kernel.CurrencyCode(s) }

func day(s string) time.Time {
	d, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

// billingPolicy adds INVOICE (7.2000) and ACCOUNTING (7.1000) USD rates, tax treatments from 2026-11-01 and the
// two number series to the test policy.
func billingPolicy(t *testing.T) *FinancePolicy {
	t.Helper()
	p := policy(t)
	for _, e := range []RateEntry{
		{Type: RateInvoice, From: "USD", Rate: 72_000, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: t0.AddDate(0, 0, -1), ValidUntil: t0.AddDate(0, 1, 0),
			Source: "bank selling rate"},
		{Type: RateAccounting, From: "USD", Rate: 71_000, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: t0.AddDate(0, 0, -10), ValidUntil: t0.AddDate(0, 1, 0),
			Source: "monthly rate 2026-11"},
	} {
		if err := p.RecordRate(e, "acct-1", hour(0)); err != nil {
			t.Fatal(err)
		}
	}
	for _, x := range []TaxTreatment{
		{ChargeCode: "AIR_FREIGHT", Treatment: TaxExemptIntlFreight, EffectiveFrom: day("2026-11-01")},
		{ChargeCode: "DOC_FEE", Treatment: TaxTaxable, RateBP: 600, EffectiveFrom: day("2026-11-01")},
		{ChargeCode: "LOYALTY", Treatment: TaxExemptIntlFreight, EffectiveFrom: day("2026-11-01")},
		{ChargeCode: "DEST_FEE", Treatment: TaxPassThrough, EffectiveFrom: day("2026-11-01")},
	} {
		if err := p.SetTaxTreatment(x, "tax-1", hour(0)); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []Series{{Kind: KindDebitNote, Prefix: "DN", Template: "DN{seq:6}", FirstNumber: 1},
		{Kind: KindCreditNote, Prefix: "CN", Template: "CN{seq:6}", FirstNumber: 1}} {
		if err := p.RegisterSeries(s, "acct-1", hour(0)); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// counter stands in for the platform's sequences of continuous <prefix>{seq:6} templates: it draws in order from each
// series' first number.
type counter map[string]int64

func (c counter) draw(p *FinancePolicy) Draw {
	return func(kind string, _ time.Time) (string, int64, string, error) {
		s, ok := p.SeriesOf(kind)
		if !ok {
			return "", 0, "", ErrNoSeries
		}
		seq := s.FirstNumber + c[kind]
		c[kind]++
		return fmt.Sprintf("%s%06d", s.Prefix, seq), seq, "", nil
	}
}

// Tax treatments: one of three kinds, a rate only when taxable, each version of a code later than the last; the one
// in force on a date is the latest started. Series: once per kind, prefixes that never start one another, numbers
// zero-padded until the width runs out.
func TestTaxTreatmentsAndSeries(t *testing.T) {
	p := billingPolicy(t)
	for name, x := range map[string]TaxTreatment{
		"unknown":          {ChargeCode: "X", Treatment: "ZERO_RATED", EffectiveFrom: day("2026-12-01")},
		"taxable no rate":  {ChargeCode: "X", Treatment: TaxTaxable, EffectiveFrom: day("2026-12-01")},
		"exempt with rate": {ChargeCode: "X", Treatment: TaxExemptIntlFreight, RateBP: 600, EffectiveFrom: day("2026-12-01")},
		"rate too high":    {ChargeCode: "X", Treatment: TaxTaxable, RateBP: 3001, EffectiveFrom: day("2026-12-01")},
		"not a date":       {ChargeCode: "X", Treatment: TaxTaxable, RateBP: 600, EffectiveFrom: hour(3)},
		"not later":        {ChargeCode: "DOC_FEE", Treatment: TaxTaxable, RateBP: 900, EffectiveFrom: day("2026-11-01")},
	} {
		if err := p.SetTaxTreatment(x, "tax-1", hour(1)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := p.SetTaxTreatment(TaxTreatment{ChargeCode: "DOC_FEE", Treatment: TaxTaxable, RateBP: 900, EffectiveFrom: day("2026-12-01")}, "tax-1",
		hour(1)); err != nil {
		t.Fatal(err)
	}
	if x, ok := p.TaxOn("DOC_FEE", day("2026-11-30")); !ok || x.RateBP != 600 || x.No != 2 {
		t.Fatalf("in force on 2026-11-30: %+v %v", x, ok)
	}
	if x, ok := p.TaxOn("DOC_FEE", day("2026-12-01")); !ok || x.RateBP != 900 || x.No != 5 {
		t.Fatalf("in force on 2026-12-01: %+v %v", x, ok)
	}
	if _, ok := p.TaxOn("DOC_FEE", day("2026-10-31")); ok {
		t.Fatal("a treatment before its date")
	}
	for name, s := range map[string]Series{
		"kind again":       {Kind: KindDebitNote, Prefix: "INV", Template: "INV{seq:6}", FirstNumber: 1},
		"another kind":     {Kind: "DEBIT", Prefix: "X", Template: "X{seq:6}", FirstNumber: 1},
		"lower case":       {Kind: KindDebitNote, Prefix: "dn", Template: "dn{seq:6}", FirstNumber: 1},
		"prefix not first": {Kind: KindDebitNote, Prefix: "DN", Template: "X{seq:6}", FirstNumber: 1},
		"no first number":  {Kind: KindDebitNote, Prefix: "X", Template: "X{seq:6}"},
	} {
		if err := p.RegisterSeries(s, "acct-1", hour(1)); err == nil {
			t.Errorf("%s registered", name)
		}
	}
	q := policy(t)
	if err := q.RegisterSeries(Series{Kind: KindDebitNote, Prefix: "D", Template: "D{yy}{seq:4}", FirstNumber: 9_999, SeedPeriod: "2026"}, "acct-1",
		hour(1)); err != nil {
		t.Fatal(err)
	}
	if err := q.RegisterSeries(Series{Kind: KindCreditNote, Prefix: "D1", Template: "D1{seq:4}", FirstNumber: 1}, "acct-1", hour(1)); !errors.Is(err,
		ErrInvalid) {
		t.Fatalf("D and D1 start one another: %v", err)
	}
	if s, ok := q.SeriesOf(KindDebitNote); !ok || s.Template != "D{yy}{seq:4}" || s.FirstNumber != 9_999 || s.SeedPeriod != "2026" {
		t.Fatalf("series: %+v", s)
	}
}

// billedJob is the test job with the quote applied for party-a and a manual USD line for agent-ny.
func billedJob(t *testing.T, p *FinancePolicy) *FreightJob {
	t.Helper()
	j := job(t, p)
	if err := j.ApplyBasis(Basis{BasisID: "QUOTE:Q-1@2", QuoteID: "Q-1", QuoteRevision: 2, OrderID: "O-A"}, "party-a", 3, quoteLines, p, "fin-1",
		hour(1)); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordLine("DEST_FEE", "", &SideInput{PartyID: "agent-ny", PartyRevision: 2, Amount: usd(20_000)}, nil, "collect at destination", p,
		"fin-1", hour(1)); err != nil {
		t.Fatal(err)
	}
	return j
}

func debitNote(id, debtor string, currency string, lines ...int) DebitNoteInput {
	return DebitNoteInput{ID: id, DebtorPartyID: debtor, DebtorRevision: 4, LineNos: lines, Currency: kernelCurrency(currency),
		InvoiceDate: day("2026-11-11"), Profile: ProfileVersion{No: 1}, Document: DocumentRef{DocumentID: "DOC-H1", Version: 1, Number: "SHA0000001",
			MasterNumber: "781-12345675", IssuedAt: hour(1)}}
}

// A debit note bills a debtor's estimated sides on the issued house document: in the functional currency it converts
// a foreign side at the INVOICE rate, in a foreign currency it bills that currency's sides and converts them at the
// ACCOUNTING rate; the sides become INVOICED and cannot be revised, billed again or cancelled with the job. A credit
// note reverses whole lines once, negated; the sides return to ESTIMATED, to be revised and billed again; a debit note
// with every line reversed is cancelled.
func TestDebitAndCreditNotes(t *testing.T) {
	p := billingPolicy(t)
	j := billedJob(t, p)
	numbers := counter{}
	issue := func(in DebitNoteInput, h int) error {
		return j.IssueDebitNote(in, p, numbers.draw(p), "bill-1", hour(h))
	}

	refusals := map[string]struct {
		in   DebitNoteInput
		want error
	}{}
	noProfile := debitNote("DN-X", "party-a", "CNY", 1)
	noProfile.Profile = ProfileVersion{}
	refusals["no profile"] = struct {
		in   DebitNoteInput
		want error
	}{noProfile, ErrNoBillingProfile}
	noDocument := debitNote("DN-X", "party-a", "CNY", 1)
	noDocument.Document = DocumentRef{}
	refusals["no document"] = struct {
		in   DebitNoteInput
		want error
	}{noDocument, ErrNoIssuedDocument}
	early := debitNote("DN-X", "party-a", "CNY", 1)
	early.Document.IssuedAt = hour(3)
	refusals["before the document was issued"] = struct {
		in   DebitNoteInput
		want error
	}{early, ErrNoIssuedDocument}
	wrongDate := debitNote("DN-X", "party-a", "CNY", 1)
	wrongDate.InvoiceDate = day("2026-11-13")
	refusals["invoice date not the issue's"] = struct {
		in   DebitNoteInput
		want error
	}{wrongDate, ErrInvalid}
	for name, c := range map[string]DebitNoteInput{
		"another debtor's line":  debitNote("DN-X", "party-a", "CNY", 1, 4),
		"a line twice":           debitNote("DN-X", "party-a", "CNY", 1, 1),
		"no such line":           debitNote("DN-X", "party-a", "CNY", 9),
		"USD note, CNY line":     debitNote("DN-X", "party-a", "USD", 1, 2),
		"a discount alone":       debitNote("DN-X", "party-a", "CNY", 3),
		"currency without rates": debitNote("DN-X", "party-a", "XXX", 1),
	} {
		refusals[name] = struct {
			in   DebitNoteInput
			want error
		}{c, ErrInvalid}
	}
	for name, c := range refusals {
		if err := issue(c.in, 2); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(numbers) != 0 || len(j.State().Notes) != 0 {
		t.Fatalf("a refused note drew %v", numbers)
	}

	dn1 := debitNote("DN-1", "party-a", "CNY", 1, 2, 3)
	dn1.Terms = &TermsVersion{No: 1, PaymentDays: 30, PaymentBasis: TermsFromMonthEnd}
	if err := issue(dn1, 2); err != nil {
		t.Fatal(err)
	}
	n := j.State().Notes[0]
	// USD 1,200.00 at 7.2000 = CNY 8,640.00; CNY 300.00; CNY -100.00
	if n.Number != "DN000001" || n.Total != 884_000 || n.LocalTotal != 884_000 || n.DueDate != day("2026-12-30") || n.TermsVersion != 1 ||
		n.Lines[0].Invoice != 864_000 || n.Lines[0].FX == nil || n.Lines[0].FX.Type != RateInvoice || n.Lines[1].FX != nil || n.Lines[1].Tax.RateBP != 600 {
		t.Fatalf("debit note: %+v", n)
	}
	totals := n.TaxTotals()
	if len(totals) != 2 || totals[0].Treatment != TaxExemptIntlFreight || totals[0].Invoice != 854_000 || totals[1].Invoice != 30_000 {
		t.Fatalf("tax totals: %+v", totals)
	}
	sell := j.State().Lines[0].Sell
	if v := sell[len(sell)-1]; v.No != 2 || v.Status != StatusInvoiced || v.Local != 864_000 || v.InvoiceID != "DN-1" || v.InvoiceLineNo != 1 ||
		v.Reason != "debit note DN000001" {
		t.Fatalf("invoiced side: %+v", v)
	}
	if unbilled, billed := j.Exposure("party-a"); unbilled != 0 || billed != 884_000 {
		t.Fatalf("exposure %d %d", unbilled, billed)
	}
	if err := j.ReviseSide(1, SideSell, 2, SideInput{PartyID: "party-a", PartyRevision: 4, Amount: usd(1)}, "price", p, "fin-1", hour(3)); !errors.Is(err,
		ErrInvoiced) {
		t.Fatalf("revising a billed side: %v", err)
	}
	if err := issue(debitNote("DN-2", "party-a", "CNY", 1), 3); !errors.Is(err, ErrInvoiced) {
		t.Fatalf("billing a side twice: %v", err)
	}
	if err := j.Cancel("shipment cancelled", "fin-1", hour(3)); !errors.Is(err, ErrInvoiced) {
		t.Fatalf("cancelling a billed job: %v", err)
	}
	if err := issue(debitNote("DN-1", "agent-ny", "USD", 4), 3); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a note id twice: %v", err)
	}

	// USD 200.00 to the agent in USD, at the ACCOUNTING rate 7.1000 = CNY 1,420.00; no credit account: due at once
	if err := issue(debitNote("DN-2", "agent-ny", "USD", 4), 3); err != nil {
		t.Fatal(err)
	}
	n2 := j.State().Notes[1]
	if n2.Number != "DN000002" || n2.Total != 20_000 || n2.LocalTotal != 142_000 || n2.Lines[0].FX.Type != RateAccounting || n2.DueDate != day("2026-11-11") ||
		n2.TermsVersion != 0 || n2.PaymentBasis != TermsFromInvoiceDate || n2.Lines[0].Tax.Treatment != TaxPassThrough {
		t.Fatalf("USD note: %+v", n2)
	}
	if p := j.Profit(); p.Invoiced != 884_000+142_000 || p.Revenue != p.Invoiced {
		t.Fatalf("profit: %+v", p)
	}

	j.Acknowledge()
	credit := func(id, debit string, h int, lines ...int) error {
		return j.IssueCreditNote(CreditNoteInput{ID: id, DebitNoteID: debit, LineNos: lines, Reason: "wrong document fee", InvoiceDate: day("2026-11-11")},
			p, numbers.draw(p), "bill-1", hour(h))
	}
	if err := credit("CN-1", "DN-9", 4, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown debit note: %v", err)
	}
	if err := credit("CN-1", "DN-1", 4, 2); err != nil {
		t.Fatal(err)
	}
	c := j.State().Notes[2]
	if c.Number != "CN000001" || c.Kind != KindCreditNote || c.Total != -30_000 || c.Lines[0].CreditsLineNo != 2 || c.Lines[0].Amount.Minor != -30_000 ||
		c.CreditsID != "DN-1" || j.State().Notes[0].State != NoteIssued {
		t.Fatalf("credit note: %+v", c)
	}
	doc := j.State().Lines[1].Sell
	if v := doc[len(doc)-1]; v.No != 3 || v.Status != StatusEstimated || v.Amount.Minor != 30_000 || v.Reason != "credit note CN000001" {
		t.Fatalf("returned side: %+v", v)
	}
	if err := credit("CN-2", "DN-1", 4, 2); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a line reversed twice: %v", err)
	}
	if err := j.ReviseSide(2, SideSell, 3, SideInput{PartyID: "party-a", PartyRevision: 4, Amount: cny(25_000)}, "agreed fee", p, "fin-1",
		hour(5)); err != nil {
		t.Fatal(err)
	}
	if err := issue(debitNote("DN-3", "party-a", "CNY", 2), 5); err != nil || j.State().Notes[3].Number != "DN000003" || j.State().Notes[3].Total != 25_000 {
		t.Fatalf("billed again: %v", err)
	}
	if err := credit("CN-2", "DN-1", 6, 3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a discount line reversed alone raises what the debtor owes: %v", err)
	}
	if err := credit("CN-2", "DN-1", 6, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("leaving the discount line alone on the debit note: %v", err)
	}
	if err := credit("CN-2", "DN-1", 6, 1, 3); err != nil {
		t.Fatal(err)
	}
	var cancelledEvent, sideEvents bool
	for _, e := range j.Pending() {
		cancelledEvent = cancelledEvent || (e.Type == eventDebitNoteCancelled && e.SubjectID == "DN-1" && e.JobID == "JOB-1")
		sideEvents = sideEvents || (e.Type == eventSideRevised && e.LineNo == 1 && e.Version == 3)
	}
	if !cancelledEvent || !sideEvents {
		t.Fatalf("events: %+v", j.Pending())
	}
	if st := j.State(); st.Notes[0].State != NoteCancelled || st.Notes[4].Total != -854_000 || st.Notes[4].Number != "CN000002" {
		t.Fatalf("every line reversed: %+v", st.Notes[0])
	}
	added, cancelled := j.AddedNotes()
	if len(added) != 3 || len(cancelled) != 1 || cancelled[0].ID != "DN-1" || cancelled[0].State != NoteCancelled {
		t.Fatalf("added %d, cancelled %+v", len(added), cancelled)
	}
	if err := credit("CN-3", "DN-1", 7, 1); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("crediting a cancelled note: %v", err)
	}
	dated := CreditNoteInput{ID: "CN-3", DebitNoteID: "DN-2", LineNos: []int{1}, Reason: "x", InvoiceDate: day("2026-11-10")}
	if err := j.IssueCreditNote(dated, p, numbers.draw(p), "bill-1", hour(7)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a credit note dated before its debit note: %v", err)
	}
	// the returned sides are estimates again, at the ESTIMATE rate: USD 1,200.00 at 7.1234 and CNY -100.00
	if unbilled, billed := j.Exposure("party-a"); unbilled != 854_808-10_000 || billed != 25_000 {
		t.Fatalf("exposure after credits %d %d", unbilled, billed)
	}
	// the agreed fee waived: written off, its side returns at zero
	waive := CreditNoteInput{ID: "CN-3", DebitNoteID: "DN-3", LineNos: []int{1}, Reason: "fee waived", InvoiceDate: day("2026-11-11"), WriteOff: true}
	if err := j.IssueCreditNote(waive, p, numbers.draw(p), "bill-1", hour(7)); err != nil {
		t.Fatal(err)
	}
	doc = j.State().Lines[1].Sell
	if v := doc[len(doc)-1]; v.Status != StatusEstimated || v.Amount != cny(0) || v.Local != 0 || v.Reason != "credit note CN000003, written off" {
		t.Fatalf("written off: %+v", v)
	}
	if unbilled, billed := j.Exposure("party-a"); unbilled != 854_808-10_000 || billed != 0 {
		t.Fatalf("exposure after the write-off %d %d", unbilled, billed)
	}

	back, err := RestoreJob(j.State())
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Credits("DN-1")) != 2 {
		t.Fatalf("credits of DN-1: %d", len(back.Credits("DN-1")))
	}
	st := j.State()
	st.Notes = st.Notes[1:]
	if _, err = RestoreJob(st); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an invoiced side without its note: %v", err)
	}

	// a draw that fails leaves the job as it was
	failing := func(string, time.Time) (string, int64, string, error) { return "", 0, "", ErrSeriesExhausted }
	before := len(j.State().Lines[0].Sell)
	if err = j.IssueDebitNote(debitNote("DN-4", "party-a", "CNY", 1), p, failing, "bill-1", hour(8)); !errors.Is(err, ErrSeriesExhausted) ||
		len(j.State().Notes) != 6 || len(j.State().Lines[0].Sell) != before {
		t.Fatalf("a failed draw: %v", err)
	}
	// a code without a treatment on the invoice date
	if err = j.RecordLine("STORAGE", "", &SideInput{PartyID: "party-a", PartyRevision: 4, Amount: cny(5_000)}, nil, "storage", p, "fin-1",
		hour(8)); err != nil {
		t.Fatal(err)
	}
	if err = j.IssueDebitNote(debitNote("DN-4", "party-a", "CNY", 5), p, numbers.draw(p), "bill-1", hour(8)); !errors.Is(err, ErrTaxMissing) {
		t.Fatalf("no tax treatment: %v", err)
	}
}

func soleTraderIdentity() PartyIdentity {
	id := orgIdentity()
	id.LegalName, id.USCC = "上海某五金店", "92310000MA1FL8TQ2X"
	return id
}

func orgIdentity() PartyIdentity {
	return PartyIdentity{PartyID: "party-a", Revision: 4, Status: "ACTIVE", LegalName: "上海甲贸易有限公司", EnglishName: "Shanghai Jia Trading Co., Ltd.",
		CountryCode: "CN", USCC: "91310000MA1FL8TQ2X"}
}

// A billing profile takes the invoice title and taxpayer number from ORG's identity of the debtor, keeps the
// enterprise's invoice data, refuses a version that changes nothing, and knows when ORG names the debtor otherwise.
func TestBillingProfile(t *testing.T) {
	b, err := NewProfile("T", "party-a")
	if err != nil {
		t.Fatal(err)
	}
	in := ProfileInput{BuyerKind: BuyerCNEnterprise, RegisteredAddress: "上海市浦东新区世纪大道100号", RegisteredPhone: "021-5888 0000", BankName: "中国银行上海分行",
		BankAccount: "4567 8901 2345", EvidenceRef: "customer letter 2026-11-01"}
	noUSCC := orgIdentity()
	noUSCC.USCC = ""
	foreign := orgIdentity()
	foreign.CountryCode = "HK"
	for name, c := range map[string]struct {
		in ProfileInput
		id PartyIdentity
	}{
		"no USCC":           {in, noUSCC},
		"enterprise abroad": {in, foreign},
		"foreign in CN":     {ProfileInput{BuyerKind: BuyerForeign, EvidenceRef: "x"}, orgIdentity()},
		"individual":        {ProfileInput{BuyerKind: "CN_INDIVIDUAL", EvidenceRef: "x"}, orgIdentity()},
		"no evidence":       {ProfileInput{BuyerKind: BuyerCNEnterprise}, orgIdentity()},
		"bank alone":        {ProfileInput{BuyerKind: BuyerCNEnterprise, BankName: "中国银行", EvidenceRef: "x"}, orgIdentity()},
		"letters in phone":  {ProfileInput{BuyerKind: BuyerCNEnterprise, RegisteredPhone: "call me", EvidenceRef: "x"}, orgIdentity()},
		"evidence in prose": {ProfileInput{BuyerKind: BuyerCNEnterprise, EvidenceRef: "张三 13800000000 确认"}, orgIdentity()},
		"sole trader phone": {ProfileInput{BuyerKind: BuyerCNEnterprise, RegisteredPhone: "021-5888 0000", EvidenceRef: "x"}, soleTraderIdentity()},
		"sole trader bank": {ProfileInput{BuyerKind: BuyerCNEnterprise, BankName: "中国银行", BankAccount: "4567 8901", EvidenceRef: "x"},
			soleTraderIdentity()},
		"another party": {in, PartyIdentity{PartyID: "party-b", Revision: 1, Status: "ACTIVE", CountryCode: "CN", USCC: "91310000MA1FL8TQ2X"}},
		"archived":      {in, PartyIdentity{PartyID: "party-a", Revision: 4, Status: "ARCHIVED", CountryCode: "CN", USCC: "91310000MA1FL8TQ2X"}},
	} {
		if err = b.Set(c.in, c.id, "bill-1", hour(1)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err = b.Set(in, orgIdentity(), "bill-1", hour(1)); err != nil {
		t.Fatal(err)
	}
	v, _ := b.Current()
	if v.No != 1 || v.Title != "上海甲贸易有限公司" || v.TaxpayerNumber != "91310000MA1FL8TQ2X" || v.PartyRevision != 4 || !v.Matches(orgIdentity()) {
		t.Fatalf("version: %+v", v)
	}
	if err = b.Set(in, orgIdentity(), "bill-1", hour(2)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unchanged version: %v", err)
	}
	renamed := orgIdentity()
	renamed.LegalName, renamed.Revision = "上海甲国际贸易有限公司", 5
	if v.Matches(renamed) {
		t.Fatal("a renamed debtor still matches")
	}
	if err = b.Set(in, renamed, "bill-1", hour(2)); err != nil {
		t.Fatal(err)
	}
	if v, _ = b.Current(); v.No != 2 || v.Title != "上海甲国际贸易有限公司" || len(b.Added()) != 2 || len(b.Pending()) != 2 {
		t.Fatalf("second version: %+v", v)
	}
	agent, _ := NewProfile("T", "agent-ny")
	ny := PartyIdentity{PartyID: "agent-ny", Revision: 2, Status: "ACTIVE", LegalName: "NY Forwarding Inc.", EnglishName: "NY Forwarding", CountryCode: "US"}
	if err = agent.Set(ProfileInput{BuyerKind: BuyerForeign, EvidenceRef: "agency agreement"}, ny, "bill-1", hour(1)); err != nil {
		t.Fatal(err)
	}
	if v, _ = agent.Current(); v.Title != "NY Forwarding" || v.TaxpayerNumber != "" || !strings.HasPrefix(v.EvidenceRef, "agency") {
		t.Fatalf("foreign buyer: %+v", v)
	}
	trader, _ := NewProfile("T", "party-a")
	if err = trader.Set(ProfileInput{BuyerKind: BuyerCNEnterprise, RegisteredAddress: "上海市某区某路1号", EvidenceRef: "licence 2026/11"}, soleTraderIdentity(),
		"bill-1", hour(1)); err != nil {
		t.Fatalf("a sole trader without phone or account: %v", err)
	}
	back, err := RestoreProfile(b.State())
	if err != nil || len(back.Added()) != 0 {
		t.Fatalf("restored: %v", err)
	}
}

// ParseDate reads YYYY-MM-DD only; an invoice date is the date of its issue in some time zone.
func TestDates(t *testing.T) {
	if _, err := ParseDate("2026-11-11T00:00:00Z"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a timestamp: %v", err)
	}
	d := day("2026-11-11")
	for at, want := range map[time.Time]bool{
		d.Add(-14 * time.Hour):                        true,  // 10:00 on the 11th in UTC+14
		d.Add(-14*time.Hour - time.Second):            false, // still the 10th everywhere
		d.Add(36*time.Hour - time.Second):             true,  // 23:59:59 on the 11th in UTC-12
		d.Add(36 * time.Hour):                         false,
		time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC): true,
	} {
		if dateOf(d, at) != want {
			t.Errorf("%s: want %v", at, want)
		}
	}
	if dateOf(hour(0), hour(0)) {
		t.Fatal("an instant is not a date")
	}
}
