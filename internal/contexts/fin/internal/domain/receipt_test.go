package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func receiptInput() ReceiptInput {
	return ReceiptInput{ID: "R-1", LegalEntityID: "LE", DebtorPartyID: "party-a", DebtorRevision: 4, Amount: cny(999_000), BankCharges: cny(1_000),
		ReceivedOn: day("2026-11-10"), EvidenceSource: EvidenceBank, EvidenceRef: "BOC 2026-11-10 #0042"}
}

func settlementNotes() []SettlementNote {
	note := func(id, number, debtor, currency string, remaining int64) SettlementNote {
		return SettlementNote{InvoiceID: id, Kind: KindDebitNote, State: NoteIssued, Number: number, LegalEntityID: "LE", DebtorPartyID: debtor,
			Currency: kernelCurrency(currency), InvoiceDate: day("2026-11-11"), Remaining: remaining}
	}
	credit := note("CN-1", "CN000001", "party-a", "CNY", -30_000)
	credit.Kind = KindCreditNote
	cancelled := note("DN-C", "DN000009", "party-a", "CNY", 0)
	cancelled.State = NoteCancelled
	other := note("DN-L", "DN000008", "party-a", "CNY", 10_000)
	other.LegalEntityID = "LE-2"
	partly := note("DN-2", "DN000002", "party-a", "CNY", 600_000)
	partly.Settled = 450_000
	later := note("DN-3", "DN000003", "party-a", "CNY", 10_000)
	later.InvoiceDate = day("2026-11-12")
	return []SettlementNote{note("DN-1", "DN000001", "party-a", "CNY", 884_000), partly, later, note("DN-U", "DN000004", "party-a", "USD", 20_000),
		note("DN-B", "DN000005", "party-b", "CNY", 10_000), credit, cancelled, other}
}

// A receipt is recorded from evidence with a positive amount, bank charges within it and a date received not after the
// day it is recorded; it is allocated to issued debit notes of its legal entity, debtor and currency within what it has
// unallocated and each note has open; an allocation is reversed once; a receipt that settles nothing is cancelled once.
func TestDebtorReceipt(t *testing.T) {
	for name, change := range map[string]func(*ReceiptInput){
		"no id":                    func(in *ReceiptInput) { in.ID = " " },
		"no debtor revision":       func(in *ReceiptInput) { in.DebtorRevision = 0 },
		"no amount":                func(in *ReceiptInput) { in.Amount = cny(0) },
		"an amount beyond bounds":  func(in *ReceiptInput) { in.Amount = cny(MaxAmount + 1) },
		"charges in another money": func(in *ReceiptInput) { in.BankCharges = usd(100) },
		"charges above the amount": func(in *ReceiptInput) { in.BankCharges = cny(999_001) },
		"negative charges":         func(in *ReceiptInput) { in.BankCharges = cny(-1) },
		"received tomorrow":        func(in *ReceiptInput) { in.ReceivedOn = day("2026-11-12") },
		"received not on a date":   func(in *ReceiptInput) { in.ReceivedOn = hour(-3) },
		"received long ago":        func(in *ReceiptInput) { in.ReceivedOn = day("2025-11-08") },
		"an unknown source":        func(in *ReceiptInput) { in.EvidenceSource = "CASH" },
		"free text as evidence":    func(in *ReceiptInput) { in.EvidenceRef = "paid by Mr. Wang, see the chat" },
		"evidence ending in space": func(in *ReceiptInput) { in.EvidenceRef = "BOC 2026-11-10 #0042 " },
	} {
		in := receiptInput()
		change(&in)
		if _, err := RecordReceipt("T", in, "cash-1", hour(0)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := RecordReceipt("T", receiptInput(), " ", hour(0)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no author: %v", err)
	}
	today := receiptInput()
	today.ReceivedOn, today.BankCharges = day("2026-11-12"), kernel.Money{}
	if r, err := RecordReceipt("T", today, "cash-1", hour(2)); err != nil || r.State().BankCharges != 0 { // 10:00 UTC is the 12th at UTC+14
		t.Fatalf("received on the day it is recorded, east of UTC: %v", err)
	}
	r, err := RecordReceipt("T", receiptInput(), "cash-1", hour(0))
	if err != nil {
		t.Fatal(err)
	}
	if st := r.State(); st.State != ReceiptRecorded || st.Credited() != 1_000_000 || st.Unallocated() != 1_000_000 || !st.ReceivedOn.Equal(day("2026-11-10")) {
		t.Fatalf("recorded: %+v", st)
	}
	on := day("2026-11-11")
	item := func(id string, amount kernel.Money) AllocationItem {
		return AllocationItem{InvoiceID: id, Amount: amount}
	}
	for name, c := range map[string]struct {
		items []AllocationItem
		on    time.Time
		want  error
	}{
		"nothing":                     {nil, on, ErrInvalid},
		"a note twice":                {[]AllocationItem{item("DN-1", cny(1)), item("DN-1", cny(1))}, on, ErrInvalid},
		"no amount":                   {[]AllocationItem{item("DN-1", cny(0))}, on, ErrInvalid},
		"an amount in another money":  {[]AllocationItem{item("DN-1", usd(1))}, on, ErrInvalid},
		"a date that is not the day":  {[]AllocationItem{item("DN-1", cny(1))}, day("2026-11-13"), ErrInvalid},
		"before the date received":    {[]AllocationItem{item("DN-1", cny(1))}, day("2026-11-09"), ErrInvalid},
		"before the note's date":      {[]AllocationItem{item("DN-3", cny(1))}, on, ErrInvalid},
		"an unknown note":             {[]AllocationItem{item("DN-9", cny(1))}, on, ErrNotAllocatable},
		"a note in another currency":  {[]AllocationItem{item("DN-U", cny(1))}, on, ErrNotAllocatable},
		"another debtor's note":       {[]AllocationItem{item("DN-B", cny(1))}, on, ErrNotAllocatable},
		"a credit note":               {[]AllocationItem{item("CN-1", cny(1))}, on, ErrNotAllocatable},
		"a cancelled debit note":      {[]AllocationItem{item("DN-C", cny(1))}, on, ErrNotAllocatable},
		"another legal entity's note": {[]AllocationItem{item("DN-L", cny(1))}, on, ErrNotAllocatable},
		"more than the note has open": {[]AllocationItem{item("DN-2", cny(150_001))}, on, ErrOverAllocated},
		"more than the receipt has":   {[]AllocationItem{item("DN-1", cny(884_000)), item("DN-2", cny(116_001))}, on, ErrOverAllocated},
	} {
		if err := r.Allocate(c.items, settlementNotes(), c.on, "cash-1", hour(1)); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(r.State().Allocations) != 0 {
		t.Fatalf("a refused allocation wrote: %+v", r.State().Allocations)
	}
	if err = r.Allocate([]AllocationItem{item("DN-1", cny(884_000)), item("DN-2", cny(100_000))}, settlementNotes(), on, "cash-1", hour(1)); err != nil {
		t.Fatal(err)
	}
	st := r.State()
	if len(st.Allocations) != 2 || st.Allocations[1].No != 2 || st.Allocations[1].InvoiceID != "DN-2" || st.Unallocated() != 16_000 ||
		!st.Allocations[0].Date.Equal(on) {
		t.Fatalf("allocated: %+v", st)
	}
	if events := r.Pending(); len(events) != 3 || events[0].Type != eventReceiptRecorded || events[2].Type != eventReceiptAllocated || events[2].Version != 2 ||
		events[2].InvoiceID != "DN-2" || events[2].SubjectID != "R-1" || events[2].DebtorPartyID != "party-a" {
		t.Fatalf("events: %+v", events)
	}
	if err = r.Allocate([]AllocationItem{item("DN-2", cny(1))}, settlementNotes(), on, "cash-1", hour(0)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a time before the last change: %v", err)
	}
	r.Stamp(hour(2))
	r.Acknowledge()
	if added, reversed := r.Added(); len(added)+len(reversed) != 0 || r.State().Allocations[0].EnteredAt.IsZero() || r.State().EnteredAt.IsZero() {
		t.Fatalf("stored: %v %v", added, reversed)
	}

	if err = r.Cancel(ReceiptEnteredInError, on, "cash-1", hour(2)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancelling a receipt that settles notes: %v", err)
	}
	for name, c := range map[string]struct {
		no     int
		reason string
		on     time.Time
	}{"no such allocation": {3, "x", on}, "no reason": {1, " ", on}, "before the allocation": {1, "x", day("2026-11-10")}} {
		if err = r.ReverseAllocation(c.no, c.reason, c.on, "cash-1", hour(2)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err = r.ReverseAllocation(1, "allocated to the wrong note", on, "cash-2", hour(2)); err != nil {
		t.Fatal(err)
	}
	if err = r.ReverseAllocation(1, "again", on, "cash-2", hour(2)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reversed twice: %v", err)
	}
	if st = r.State(); st.Unallocated() != 900_000 || st.Allocations[0].Live() || st.Allocations[0].ReversedBy != "cash-2" || !st.Allocations[1].Live() {
		t.Fatalf("reversed: %+v", st)
	}
	// a second allocation to the note counts what the first settles, as the notes read then say
	if err = r.Allocate([]AllocationItem{item("DN-2", cny(50_000))}, settlementNotes()[1:2], on, "cash-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	if added, reversed := r.Added(); len(added) != 1 || added[0].No != 3 || len(reversed) != 1 || reversed[0].No != 1 {
		t.Fatalf("to write: %+v %+v", added, reversed)
	}
	for _, no := range []int{2, 3} {
		if err = r.ReverseAllocation(no, "the payment was returned", day("2026-11-12"), "cash-2", hour(20)); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct {
		reason string
		on     time.Time
	}{"an unknown reason": {"LOST", day("2026-11-12")}, "before a reversal": {ReceiptReturned, day("2026-11-11")}} {
		if err = r.Cancel(c.reason, c.on, "cash-2", hour(20)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err = r.Cancel(ReceiptReturned, day("2026-11-12"), "cash-2", hour(20)); err != nil {
		t.Fatal(err)
	}
	r.Stamp(hour(21))
	if st = r.State(); st.State != ReceiptCancelled || st.CancelReason != ReceiptReturned || st.CancelEnteredAt.IsZero() || st.Allocations[2].ReverseEnteredAt.IsZero() {
		t.Fatalf("cancelled: %+v", st)
	}
	if err = r.Cancel(ReceiptReturned, day("2026-11-12"), "cash-2", hour(21)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancelled twice: %v", err)
	}
	if err = r.Allocate([]AllocationItem{item("DN-1", cny(1))}, settlementNotes(), day("2026-11-12"), "cash-1", hour(21)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("allocating a cancelled receipt: %v", err)
	}
	back, err := RestoreReceipt(st)
	if err != nil || back.State().Unallocated() != 1_000_000 {
		t.Fatalf("restored: %v", err)
	}
	st.Allocations[0].ReversedAt = time.Time{}
	st.Allocations[1].ReversedAt = time.Time{}
	st.Allocations[1].Amount = 200_000
	if _, err = RestoreReceipt(st); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a stored receipt allocated beyond its amount: %v", err)
	}
}

// What is open of a debit note counts as exposure in proportion to its local amount; a credit note is refused when it
// would leave less on the debit note than receipts settle.
func TestSettledNotes(t *testing.T) {
	for _, c := range []struct{ local, part, whole, want int64 }{
		{142_000, 15_000, 20_000, 106_500}, {100, 1, 3, 33}, {100, 2, 3, 67}, {1, 1, 2, 1}, {-1, 1, 2, -1}, {7, 0, 9, 0}, {7, 9, 9, 7}, {5, 3, 0, 0},
		{MaxAmount, MaxAmount - 1, MaxAmount, MaxAmount - 1},
	} {
		if got := proportion(c.local, c.part, c.whole); got != c.want {
			t.Errorf("proportion(%d, %d, %d) = %d, want %d", c.local, c.part, c.whole, got, c.want)
		}
	}
	p := billingPolicy(t)
	j := billedJob(t, p)
	numbers := counter{}
	for _, in := range []DebitNoteInput{debitNote("DN-1", "party-a", "CNY", 1, 2, 3), debitNote("DN-2", "agent-ny", "USD", 4)} {
		if err := j.IssueDebitNote(in, p, numbers.draw(p), "bill-1", hour(2)); err != nil {
			t.Fatal(err)
		}
	}
	// USD 1,200.00 at 7.2000 + CNY 300.00 - CNY 100.00 = CNY 8,840.00; USD 200.00 at 7.1000 = CNY 1,420.00
	if _, billed := j.Exposure("party-a"); billed != 884_000 {
		t.Fatalf("billed to party-a: %d", billed)
	}
	settled := func(dn1, dn2 int64) *FreightJob {
		t.Helper()
		st := j.State()
		st.Notes[0].Settled, st.Notes[1].Settled = dn1, dn2
		back, err := RestoreJob(st)
		if err != nil {
			t.Fatal(err)
		}
		return back
	}
	part := settled(221_000, 5_000)
	if _, billed := part.Exposure("party-a"); billed != 663_000 {
		t.Fatalf("party-a with a quarter settled: %d", billed)
	}
	if _, billed := part.Exposure("agent-ny"); billed != 106_500 {
		t.Fatalf("agent-ny with USD 50.00 of 200.00 settled: %d", billed)
	}
	if unbilled, billed := settled(884_000, 20_000).Exposure("agent-ny"); unbilled != 0 || billed != 0 {
		t.Fatalf("all settled: %d %d", unbilled, billed)
	}
	credit := CreditNoteInput{ID: "CN-1", DebitNoteID: "DN-1", LineNos: []int{2}, Reason: "billed in error", InvoiceDate: day("2026-11-11")}
	if err := settled(854_001, 0).IssueCreditNote(credit, p, numbers.draw(p), "bill-1", hour(3)); !errors.Is(err, ErrSettled) {
		t.Fatalf("a credit note leaving less than is settled: %v", err)
	}
	within := settled(854_000, 0)
	if err := within.IssueCreditNote(credit, p, numbers.draw(p), "bill-1", hour(3)); err != nil {
		t.Fatalf("a credit note leaving what is settled: %v", err)
	}
	if amount, local := within.Remaining("DN-1"); amount != 854_000 || local != 854_000 {
		t.Fatalf("remaining: %d %d", amount, local)
	}
	if _, billed := within.Exposure("party-a"); billed != 0 {
		t.Fatalf("settled in full after the credit note: %d", billed)
	}
	whole := credit
	whole.LineNos = []int{1, 2, 3}
	if err := settled(1, 0).IssueCreditNote(whole, p, numbers.draw(p), "bill-1", hour(3)); !errors.Is(err, ErrSettled) {
		t.Fatalf("reversing a note receipts settle part of: %v", err)
	}
}

// A statement balances at every end date: opening and entries make the closing balance, which is also the open items
// less the money on account, with allocations and reversals counted from their dates.
func TestStatement(t *testing.T) {
	note := func(id, kind, number, currency, invoiced, due string, total int64) Note {
		n := Note{ID: id, Kind: kind, Number: number, Currency: kernelCurrency(currency), InvoiceDate: day(invoiced), Total: total, State: NoteIssued,
			DebtorPartyID: "party-a"}
		if kind == KindDebitNote {
			n.DueDate, n.Document = day(due), DocumentRef{Number: "SHA" + id}
		}
		return n
	}
	credit := note("CN-1", KindCreditNote, "CN000001", "CNY", "2026-11-05", "", -20_000)
	credit.CreditsID = "DN-1"
	notes := []Note{note("DN-1", KindDebitNote, "DN000001", "CNY", "2026-10-20", "2026-11-19", 100_000), credit,
		note("DN-2", KindDebitNote, "DN000002", "CNY", "2026-11-10", "2026-11-10", 50_000),
		note("DN-3", KindDebitNote, "DN000003", "USD", "2026-11-12", "2026-12-12", 30_000)}
	receipt := func(id, currency, received string, amount, charges int64, allocations ...Allocation) ReceiptState {
		for i := range allocations {
			allocations[i].No = i + 1
		}
		return ReceiptState{ReceiptID: id, Currency: kernelCurrency(currency), Amount: amount, BankCharges: charges, ReceivedOn: day(received),
			EvidenceSource: EvidenceBank, EvidenceRef: id, State: ReceiptRecorded, Allocations: allocations}
	}
	reversed := Allocation{InvoiceID: "DN-1", Amount: 50_000, Date: day("2026-11-03"), ReversedOn: day("2026-11-06"), ReversedAt: hour(1)}
	returned := receipt("R-3", "CNY", "2026-11-04", 5_000, 0)
	returned.State, returned.CancelledOn, returned.CancelledAt = ReceiptCancelled, day("2026-11-08"), hour(1)
	receipts := []ReceiptState{
		receipt("R-1", "CNY", "2026-11-03", 59_000, 1_000, reversed, Allocation{InvoiceID: "DN-1", Amount: 60_000, Date: day("2026-11-06")}),
		receipt("R-2", "CNY", "2026-11-11", 70_000, 0, Allocation{InvoiceID: "DN-2", Amount: 50_000, Date: day("2026-11-11")}),
		returned,
		receipt("R-4", "USD", "2026-11-13", 10_000, 0, Allocation{InvoiceID: "DN-3", Amount: 10_000, Date: day("2026-11-13")}),
	}
	for name, c := range map[string][2]time.Time{"an end before the start": {day("2026-11-02"), day("2026-11-01")}, "more than 366 days": {day("2025-11-01"),
		day("2026-11-30")}, "not dates": {hour(0), hour(5)}} {
		if _, err := Statement(notes, receipts, c[0], c[1]); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, err := Statement(notes, receipts, day("2026-11-01"), day("2026-11-30"))
	if err != nil || len(got) != 2 || got[0].Currency != "CNY" || got[1].Currency != "USD" {
		t.Fatalf("statement: %+v %v", got, err)
	}
	c := got[0]
	want := []StatementEntry{
		{Date: day("2026-11-03"), Kind: EntryReceipt, ID: "R-1", Number: "BANK R-1", Amount: -60_000, Balance: 40_000},
		{Date: day("2026-11-04"), Kind: EntryReceipt, ID: "R-3", Number: "BANK R-3", Amount: -5_000, Balance: 35_000},
		{Date: day("2026-11-05"), Kind: EntryCreditNote, ID: "CN-1", Number: "CN000001", Amount: -20_000, Balance: 15_000},
		{Date: day("2026-11-08"), Kind: EntryReceiptCancelled, ID: "R-3", Number: "BANK R-3", Amount: 5_000, Balance: 20_000},
		{Date: day("2026-11-10"), Kind: EntryDebitNote, ID: "DN-2", Number: "DN000002", DocumentNumber: "SHADN-2", Amount: 50_000, Balance: 70_000},
		{Date: day("2026-11-11"), Kind: EntryReceipt, ID: "R-2", Number: "BANK R-2", Amount: -70_000, Balance: 0},
	}
	if c.Opening != 100_000 || c.Closing != 0 || len(c.Entries) != len(want) {
		t.Fatalf("CNY: %+v", c)
	}
	for i, e := range c.Entries {
		if e != want[i] {
			t.Errorf("entry %d: %+v, want %+v", i, e, want[i])
		}
	}
	if len(c.OpenItems) != 1 || c.OpenItems[0] != (StatementItem{InvoiceID: "DN-1", Number: "DN000001", DocumentNumber: "SHADN-1",
		InvoiceDate: day("2026-10-20"), DueDate: day("2026-11-19"), Total: 100_000, Credited: 20_000, Settled: 60_000, Open: 20_000, DaysOverdue: 11}) ||
		len(c.OnAccount) != 1 || c.OnAccount[0].ReceiptID != "R-2" || c.OnAccount[0].Unallocated != 20_000 || c.OnAccount[0].Credited != 70_000 ||
		c.Ageing != (Ageing{Days1To30: 20_000}) {
		t.Fatalf("CNY at the end: %+v %+v %+v", c.OpenItems, c.OnAccount, c.Ageing)
	}
	if u := got[1]; u.Opening != 0 || u.Closing != 20_000 || len(u.Entries) != 2 || len(u.OpenItems) != 1 || u.OpenItems[0].DaysOverdue != 0 ||
		u.Ageing != (Ageing{NotDue: 20_000}) || len(u.OnAccount) != 0 {
		t.Fatalf("USD: %+v", u)
	}
	// on 5 November the first allocation still counts and the returned receipt is not cancelled yet
	early, err := Statement(notes, receipts, day("2026-11-01"), day("2026-11-05"))
	if err != nil || early[0].Closing != 15_000 || early[0].OpenItems[0].Settled != 50_000 || early[0].OpenItems[0].Open != 30_000 ||
		len(early[0].OnAccount) != 2 || early[0].OnAccount[0].Unallocated != 10_000 || early[0].OnAccount[1].Unallocated != 5_000 || len(early) != 1 {
		t.Fatalf("as of 5 November: %+v %v", early, err)
	}
	old, err := Statement(notes, receipts, day("2026-12-01"), day("2027-03-01"))
	if err != nil || old[0].Opening != 0 || len(old[0].Entries) != 0 || old[0].Ageing != (Ageing{Over90: 20_000}) || old[1].Ageing != (Ageing{Days61To90: 20_000}) {
		t.Fatalf("months later: %+v %v", old, err)
	}
	for to := day("2026-10-15"); to.Before(day("2026-12-20")); to = to.AddDate(0, 0, 1) {
		stated, err := Statement(notes, receipts, to.AddDate(0, 0, -6), to)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stated {
			balance := s.Opening
			for _, e := range s.Entries {
				if balance += e.Amount; e.Balance != balance {
					t.Fatalf("%s %s: the balance after %s is %d, not %d", to.Format(time.DateOnly), s.Currency, e.ID, e.Balance, balance)
				}
			}
			var open, aged int64
			for _, i := range s.OpenItems {
				open += i.Open
			}
			for _, r := range s.OnAccount {
				open -= r.Unallocated
			}
			aged = s.Ageing.NotDue + s.Ageing.Days1To30 + s.Ageing.Days31To60 + s.Ageing.Days61To90 + s.Ageing.Over90
			if balance != s.Closing || open != s.Closing || aged != s.Closing+sumOnAccount(s) {
				t.Fatalf("%s %s: closing %d, entries make %d, open items less money on account %d, aged %d", to.Format(time.DateOnly), s.Currency, s.Closing,
					balance, open, aged)
			}
		}
	}
}

func sumOnAccount(s CurrencyStatement) (sum int64) {
	for _, r := range s.OnAccount {
		sum += r.Unallocated
	}
	return sum
}
