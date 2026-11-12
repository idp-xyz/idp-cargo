package domain

import (
	"errors"
	"testing"
	"time"
)

func paymentInput() PaymentInput {
	return PaymentInput{ID: "PAY-1", LegalEntityID: "LE", CreditorPartyID: "CARRIER-1", CreditorRevision: 3, Amount: usd(100_000),
		PaidOn: t0.Truncate(24 * time.Hour), EvidenceSource: EvidenceBank, EvidenceRef: "BOC 2026-11-11 #77"}
}

func payable(id string, total, paid int64) PayableInvoice {
	return PayableInvoice{InvoiceID: id, SupplierInvoiceNo: "MU-" + id, LegalEntityID: "LE", CreditorPartyID: "CARRIER-1", Currency: "USD",
		InvoiceDate: t0.Truncate(24*time.Hour).AddDate(0, 0, -3), State: CreditorMatched, Total: total, Paid: paid}
}

func TestRecordPaymentChecks(t *testing.T) {
	for name, change := range map[string]func(*PaymentInput){
		"nothing paid":            func(in *PaymentInput) { in.Amount = usd(0) },
		"a negative amount":       func(in *PaymentInput) { in.Amount = usd(-1) },
		"paid tomorrow":           func(in *PaymentInput) { in.PaidOn = in.PaidOn.AddDate(0, 0, 2) },
		"paid over a year ago":    func(in *PaymentInput) { in.PaidOn = in.PaidOn.AddDate(0, 0, -368) },
		"an unknown source":       func(in *PaymentInput) { in.EvidenceSource = "CASH" },
		"a reference with space":  func(in *PaymentInput) { in.EvidenceRef = "BOC 77 " },
		"no creditor revision":    func(in *PaymentInput) { in.CreditorRevision = 0 },
		"beyond the amount bound": func(in *PaymentInput) { in.Amount = usd(MaxAmount + 1) },
	} {
		in := paymentInput()
		change(&in)
		if _, err := RecordPayment("T", in, "ap-1", hour(4)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	p, err := RecordPayment("T", paymentInput(), "ap-1", hour(4))
	if err != nil {
		t.Fatal(err)
	}
	if st := p.State(); st.State != PaymentRecorded || st.Unallocated() != 100_000 || p.Pending()[0].Type != "CreditorPaymentRecorded.v1" ||
		p.Pending()[0].CreditorPartyID != "CARRIER-1" {
		t.Fatalf("recorded: %+v %+v", st, p.Pending())
	}
}

// A payment pays matched invoices of its creditor, legal entity and currency, not disputed nor awaiting approval,
// within what it has unallocated and each invoice has open; the rest stays on account. Reversals reopen what they
// paid; a payment paying nothing is cancelled.
func TestAllocateCreditorPayment(t *testing.T) {
	on := t0.Truncate(24 * time.Hour)
	pay := func() *CreditorPayment {
		p, err := RecordPayment("T", paymentInput(), "ap-1", hour(4))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	item := func(id string, amount int64) AllocationItem {
		return AllocationItem{InvoiceID: id, Amount: usd(amount)}
	}
	invoices := []PayableInvoice{payable("CI-1", 60_000, 0), payable("CI-2", 80_000, 30_000)}
	for name, tc := range map[string]struct {
		items    []AllocationItem
		invoices []PayableInvoice
		want     error
	}{
		"an invoice FIN does not know": {[]AllocationItem{item("CI-9", 1)}, invoices, ErrNotPayable},
		"a recorded invoice":           {[]AllocationItem{item("CI-1", 1)}, withInvoice(invoices, 0, func(n *PayableInvoice) { n.State = CreditorRecorded }), ErrNotPayable},
		"a disputed invoice":           {[]AllocationItem{item("CI-1", 1)}, withInvoice(invoices, 0, func(n *PayableInvoice) { n.State = CreditorDisputed }), ErrNotPayable},
		"a variance awaiting approval": {[]AllocationItem{item("CI-1", 1)}, withInvoice(invoices, 0, func(n *PayableInvoice) { n.AwaitingApproval = true }), ErrNotPayable},
		"another creditor's invoice":   {[]AllocationItem{item("CI-1", 1)}, withInvoice(invoices, 0, func(n *PayableInvoice) { n.CreditorPartyID = "CARRIER-2" }), ErrNotPayable},
		"another legal entity's":       {[]AllocationItem{item("CI-1", 1)}, withInvoice(invoices, 0, func(n *PayableInvoice) { n.LegalEntityID = "LE2" }), ErrNotPayable},
		"an invoice in CNY":            {[]AllocationItem{item("CI-1", 1)}, withInvoice(invoices, 0, func(n *PayableInvoice) { n.Currency = "CNY" }), ErrNotPayable},
		"more than the invoice's open": {[]AllocationItem{item("CI-2", 50_001)}, invoices, ErrOverAllocated},
		"more than the payment has":    {[]AllocationItem{item("CI-1", 60_000), item("CI-2", 40_001)}, invoices, ErrOverAllocated},
		"an invoice twice":             {[]AllocationItem{item("CI-1", 1), item("CI-1", 1)}, invoices, ErrInvalid},
		"an invoice dated after":       {[]AllocationItem{item("CI-1", 1)}, withInvoice(invoices, 0, func(n *PayableInvoice) { n.InvoiceDate = on.AddDate(0, 0, 1) }), ErrInvalid},
		"another currency asked":       {[]AllocationItem{{InvoiceID: "CI-1", Amount: cny(1)}}, invoices, ErrInvalid},
	} {
		if err := pay().Allocate(tc.items, tc.invoices, on, "ap-1", hour(5)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	p := pay()
	if err := p.Allocate([]AllocationItem{item("CI-1", 60_000), item("CI-2", 30_000)}, invoices, on, "ap-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if st := p.State(); len(st.Allocations) != 2 || st.Unallocated() != 10_000 || st.Allocations[1].InvoiceID != "CI-2" {
		t.Fatalf("allocated: %+v", st)
	}
	if err := p.Cancel(ReceiptEnteredInError, on, "ap-1", hour(6)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a payment paying invoices cancelled: %v", err)
	}
	if err := p.ReverseAllocation(2, "", on, "ap-1", hour(6)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a reversal without a reason: %v", err)
	}
	restored, err := RestorePayment(p.State())
	if err != nil {
		t.Fatal(err)
	}
	for _, no := range []int{1, 2} {
		if err = restored.ReverseAllocation(no, "paid to the wrong invoice", on, "ap-1", hour(6)); err != nil {
			t.Fatal(err)
		}
	}
	if err = restored.ReverseAllocation(1, "again", on, "ap-1", hour(6)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reversed twice: %v", err)
	}
	if added, reversed := restored.Added(); len(added) != 0 || len(reversed) != 2 {
		t.Fatalf("stored allocations reversed: %+v %+v", added, reversed)
	}
	if err = restored.Cancel("WRONG", on, "ap-1", hour(7)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown reason: %v", err)
	}
	if err = restored.Cancel(ReceiptReturned, on, "ap-1", hour(7)); err != nil {
		t.Fatal(err)
	}
	if err = restored.Allocate([]AllocationItem{item("CI-1", 1)}, invoices, on, "ap-1", hour(8)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a cancelled payment allocated: %v", err)
	}
	var kinds []string
	for _, e := range restored.Pending() {
		kinds = append(kinds, e.Type)
	}
	if len(kinds) != 3 || kinds[2] != "CreditorPaymentCancelled.v1" || kinds[0] != "CreditorPaymentAllocationReversed.v1" {
		t.Fatalf("events: %v", kinds)
	}
}

func withInvoice(ns []PayableInvoice, i int, change func(*PayableInvoice)) []PayableInvoice {
	out := append([]PayableInvoice(nil), ns...)
	change(&out[i])
	return out
}

// An invoice payments pay is not cancelled; once their allocations are reversed it is.
func TestPaidInvoiceIsNotCancelled(t *testing.T) {
	job := jobAccrual()
	c := recorded(t, CreditorLineInput{ChargeCode: "AIR_FREIGHT", Amount: 60_000})
	if err := c.Dispute("rate disputed", []AccrualTarget{job}, "ap-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	c.SetPaid(10_000)
	if err := c.Cancel("withdrawn", []AccrualTarget{job}, "ap-1", hour(6)); !errors.Is(err, ErrInvoicePaid) {
		t.Fatalf("a paid invoice cancelled: %v", err)
	}
	c.SetPaid(0)
	if err := c.Cancel("withdrawn", []AccrualTarget{job}, "ap-1", hour(6)); err != nil {
		t.Fatal(err)
	}
}
