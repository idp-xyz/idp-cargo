//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (b *billing) receipt(id, debtor string, h int, amount, charges kernel.Money, source, ref string) api.RecordDebtorReceipt {
	return api.RecordDebtorReceipt{CommandID: "receive-" + id, TenantID: b.tenant, ActorID: "cash-1", ReceiptID: id, LegalEntityID: "LE",
		DebtorPartyID: debtor, Amount: amount, BankCharges: charges, ReceivedOn: b.date(h), EvidenceSource: source, EvidenceRef: ref, OccurredAt: b.hours(h)}
}

// received records a receipt and returns its revision.
func (b *billing) received(t *testing.T, cmd api.RecordDebtorReceipt) int64 {
	t.Helper()
	r, err := b.module.RecordDebtorReceipt(context.Background(), cmd)
	if err != nil || r.SubjectID != cmd.ReceiptID || r.NewRevision != 1 {
		t.Fatalf("%s: %+v %v", cmd.ReceiptID, r, err)
	}
	return r.NewRevision
}

func to(invoice string, amount kernel.Money) api.ReceiptAllocation {
	return api.ReceiptAllocation{InvoiceID: invoice, Amount: amount}
}

func (b *billing) allocation(command, id string, revision int64, h int, items ...api.ReceiptAllocation) api.AllocateDebtorReceipt {
	return api.AllocateDebtorReceipt{CommandID: command, TenantID: b.tenant, ActorID: "cash-1", ReceiptID: id, ExpectedRevision: revision, Allocations: items,
		Date: b.date(h), OccurredAt: b.hours(h)}
}

// billedTwice gives the job of newBilling its debit notes: DN-1 to party-a in CNY (USD 1,200.00 at 7.2000 and CNY
// 300.00 = CNY 8,940.00) and DN-2 to agent-ny in USD (200.00, CNY 1,420.00 at 7.1000), both debtors with a credit
// account.
func billedTwice(t *testing.T) *billing {
	t.Helper()
	b := newBilling(t)
	b.ready(t)
	b.w.mu.Lock()
	b.w.documents["SH-A"] = houseDocument("SH-A", "SHA0000001", b.hours(3))
	b.w.mu.Unlock()
	for _, debtor := range []string{"party-a", "agent-ny"} {
		if _, err := b.module.SetCreditTerms(context.Background(), api.SetCreditTerms{CommandID: "terms-" + debtor, TenantID: b.tenant, ActorID: "credit-1",
			LegalEntityID: "LE", DebtorPartyID: debtor, Limit: cny(100_000_000), PaymentDays: 30, PaymentBasis: api.TermsFromInvoiceDate,
			OccurredAt: b.hours(0)}); err != nil {
			t.Fatal(err)
		}
	}
	b.issued(t, b.debit("DN-1", "JOB-A", 4, "party-a", "CNY", 1, 2))
	b.issued(t, b.debit("DN-2", "JOB-A", 4, "agent-ny", "USD", 3))
	return b
}

// Receipts recorded from evidence settle debit notes in part or across several; open amounts, exposure and money on
// account follow; a credit note is refused beyond what is unsettled; allocations are reversed and receipts cancelled;
// the statement balances; and the database refuses changes past FIN.
func TestDebtorReceiptsSettleDebitNotes(t *testing.T) {
	ctx := context.Background()
	b := billedTwice(t)
	rejected := func(name string, err error, code string) {
		t.Helper()
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), code) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	invoice := func(id string) api.DebtorInvoice {
		t.Helper()
		n, err := b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: id})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	account := func(debtor string) api.CreditAccount {
		t.Helper()
		a, err := b.module.GetCreditAccount(ctx, api.GetCreditAccount{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: debtor})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	allocated := func(cmd api.AllocateDebtorReceipt) int64 {
		t.Helper()
		r, err := b.module.AllocateDebtorReceipt(ctx, cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd.CommandID, err)
		}
		return r.NewRevision
	}

	// recording
	r1 := b.receipt("R-1", "party-a", 5, cny(500_000), kernel.Money{}, api.EvidenceBank, "BOC-20261111-0042")
	for name, change := range map[string]func(*api.RecordDebtorReceipt){
		"a debtor without a debtor role": func(c *api.RecordDebtorReceipt) { c.DebtorPartyID = "airline-mu" },
		"a date received in the future":  func(c *api.RecordDebtorReceipt) { c.ReceivedOn = b.date(72) },
		"not a date":                     func(c *api.RecordDebtorReceipt) { c.ReceivedOn = "11/11/2026" },
		"bank charges above the amount":  func(c *api.RecordDebtorReceipt) { c.BankCharges = cny(500_001) },
		"free text as evidence":          func(c *api.RecordDebtorReceipt) { c.EvidenceRef = "paid by Mr. Wang, see the chat" },
		"an unknown evidence source":     func(c *api.RecordDebtorReceipt) { c.EvidenceSource = "CASH" },
		"no amount":                      func(c *api.RecordDebtorReceipt) { c.Amount = cny(0) },
	} {
		c := r1
		change(&c)
		_, err := b.module.RecordDebtorReceipt(ctx, c)
		rejected(name, err, "")
	}
	intruder := r1
	intruder.ActorID = "intruder"
	if _, err := b.module.RecordDebtorReceipt(ctx, intruder); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("an actor not allowed: %v", err)
	}
	revision := b.received(t, r1)
	if again, err := b.module.RecordDebtorReceipt(ctx, r1); err != nil || again.NewRevision != 1 || again.SubjectID != "R-1" {
		t.Fatalf("replay: %+v %v", again, err)
	}
	sameEvidence := b.receipt("R-1b", "party-a", 5, cny(1), kernel.Money{}, api.EvidenceBank, "BOC-20261111-0042")
	_, err := b.module.RecordDebtorReceipt(ctx, sameEvidence)
	rejected("the evidence recorded twice", err, "FIN_EVIDENCE_USED")
	sameID := b.receipt("R-1", "party-a", 5, cny(1), kernel.Money{}, api.EvidenceBank, "BOC-20261111-0043")
	sameID.CommandID = "receive-R-1-again"
	if _, err = b.module.RecordDebtorReceipt(ctx, sameID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a receipt id used twice: %v", err)
	}
	if got, err := b.module.GetDebtorReceipt(ctx, api.GetDebtorReceipt{TenantID: b.tenant, ReceiptID: "R-1"}); err != nil || got.State != api.ReceiptRecorded ||
		got.Amount != cny(500_000) || got.BankCharges != cny(0) || got.Unallocated != cny(500_000) || got.ReceivedOn != b.date(5) ||
		got.EvidenceRef != "BOC-20261111-0042" || got.DebtorRevision != 4 || got.Revision != 1 || !got.RecordedAt.Equal(b.hours(5)) {
		t.Fatalf("receipt read: %+v %v", got, err)
	}
	if _, err = b.module.GetDebtorReceipt(ctx, api.GetDebtorReceipt{TenantID: b.tenant, ReceiptID: "R-9"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown receipt: %v", err)
	}

	// allocating part of DN-1
	job := b.job
	_, err = b.module.AllocateDebtorReceipt(ctx, b.allocation("allocate-other", "R-1", revision, 6, to("DN-2", cny(1))))
	rejected("an amount in another currency than the note", err, "FIN_NOT_ALLOCATABLE")
	_, err = b.module.AllocateDebtorReceipt(ctx, b.allocation("allocate-unknown", "R-1", revision, 6, to("DN-9", cny(1))))
	rejected("an unknown note", err, "FIN_NOT_ALLOCATABLE")
	_, err = b.module.AllocateDebtorReceipt(ctx, b.allocation("allocate-over", "R-1", revision, 6, to("DN-1", cny(500_001))))
	rejected("more than the receipt has", err, "FIN_OVER_ALLOCATED")
	if _, err = b.module.AllocateDebtorReceipt(ctx, b.allocation("allocate-stale", "R-1", revision+1, 6, to("DN-1", cny(1)))); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale receipt revision: %v", err)
	}
	first := b.allocation("allocate-R-1", "R-1", revision, 6, to("DN-1", cny(500_000)))
	r, err := b.module.AllocateDebtorReceipt(ctx, first)
	if err != nil || r.NewRevision != 2 || r.Version != 1 || r.SubjectID != "R-1" {
		t.Fatalf("allocated: %+v %v", r, err)
	}
	if again, err := b.module.AllocateDebtorReceipt(ctx, first); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if n := invoice("DN-1"); n.Settled != cny(500_000) || n.Open != cny(394_000) || n.State != api.NoteIssued || len(n.Settlements) != 1 ||
		n.Settlements[0] != (api.InvoiceSettlement{ReceiptID: "R-1", AllocationNo: 1, Amount: cny(500_000), Date: b.date(6)}) {
		t.Fatalf("DN-1 partly settled: %+v", n)
	}
	if a := account("party-a"); a.Billed != cny(394_000) || a.Unbilled != cny(0) || a.Exposure != cny(394_000) || len(a.OnAccount) != 0 {
		t.Fatalf("party-a's exposure: %+v", a)
	}
	if j, err := b.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: b.tenant, JobID: "JOB-A"}); err != nil || j.Revision != job {
		t.Fatalf("an allocation changed the job's revision: %d, was %d, %v", j.Revision, job, err)
	}

	// the agent pays USD 200.00 less USD 5.00 of bank charges: credited with 200.00, allocated in two steps
	r2 := b.received(t, b.receipt("R-2", "agent-ny", 5, usd(19_500), usd(500), api.EvidenceERP, "ERP-REC-0007"))
	r2 = allocated(b.allocation("allocate-R-2a", "R-2", r2, 6, to("DN-2", usd(15_000))))
	if a := account("agent-ny"); a.Billed != cny(35_500) || len(a.OnAccount) != 1 || a.OnAccount[0] != usd(5_000) { // 1,420.00 x 50/200
		t.Fatalf("the agent's exposure: %+v", a)
	}
	allocated(b.allocation("allocate-R-2b", "R-2", r2, 7, to("DN-2", usd(5_000))))
	if n := invoice("DN-2"); n.Settled != usd(20_000) || n.Open != usd(0) || len(n.Settlements) != 2 {
		t.Fatalf("DN-2 settled: %+v", n)
	}
	if a := account("agent-ny"); a.Billed != cny(0) || a.Exposure != cny(0) || len(a.OnAccount) != 0 {
		t.Fatalf("the agent's exposure once settled: %+v", a)
	}
	if items, err := b.module.ListOpenDebtorItems(ctx, api.ListOpenDebtorItems{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "agent-ny"}); err != nil ||
		len(items) != 0 {
		t.Fatalf("the agent's open items: %+v %v", items, err)
	}
	if items, err := b.module.ListOpenDebtorItems(ctx, api.ListOpenDebtorItems{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a"}); err != nil ||
		len(items) != 1 || items[0].InvoiceID != "DN-1" || items[0].Open != cny(394_000) {
		t.Fatalf("party-a's open items: %+v %v", items, err)
	}
	r3 := b.received(t, b.receipt("R-3", "agent-ny", 7, usd(100), kernel.Money{}, api.EvidenceERP, "ERP-REC-0009"))
	_, err = b.module.AllocateDebtorReceipt(ctx, b.allocation("allocate-R-3", "R-3", r3, 7, to("DN-2", usd(100))))
	rejected("more than the note has open", err, "FIN_OVER_ALLOCATED")
	_, err = b.module.AllocateDebtorReceipt(ctx, b.allocation("allocate-R-3-other", "R-3", r3, 7, to("DN-1", usd(100))))
	rejected("another debtor's note", err, "FIN_NOT_ALLOCATABLE")

	// DN-1 settled in full: a credit note waits for the allocation to be reversed
	r4 := b.received(t, b.receipt("R-4", "party-a", 7, cny(394_000), kernel.Money{}, api.EvidenceBank, "BOC-20261112-0003"))
	r4 = allocated(b.allocation("allocate-R-4", "R-4", r4, 7, to("DN-1", cny(394_000))))
	_, err = b.module.IssueCreditNote(ctx, b.credit("CN-1", "DN-1", 8, 2))
	rejected("a credit note on a settled debit note", err, "FIN_SETTLED")
	_, err = b.module.CancelDebtorReceipt(ctx, api.CancelDebtorReceipt{CommandID: "cancel-R-4", TenantID: b.tenant, ActorID: "cash-1", ReceiptID: "R-4",
		ExpectedRevision: r4, Reason: api.ReceiptEnteredInError, Date: b.date(8), OccurredAt: b.hours(8)})
	rejected("cancelling a receipt that settles a note", err, "reverse its allocations first")
	reverse := api.ReverseDebtorReceiptAllocation{CommandID: "reverse-R-4", TenantID: b.tenant, ActorID: "cash-2", ReceiptID: "R-4", ExpectedRevision: r4,
		AllocationNo: 1, Reason: "the document fee is to be credited", Date: b.date(8), OccurredAt: b.hours(8)}
	noReason := reverse
	noReason.Reason = " "
	_, err = b.module.ReverseDebtorReceiptAllocation(ctx, noReason)
	rejected("a reversal without a reason", err, "")
	if r, err = b.module.ReverseDebtorReceiptAllocation(ctx, reverse); err != nil || r.Version != 1 || r.NewRevision != r4+1 {
		t.Fatalf("reversed: %+v %v", r, err)
	}
	r4 = r.NewRevision
	again := reverse
	again.CommandID, again.ExpectedRevision = "reverse-R-4-again", r4
	_, err = b.module.ReverseDebtorReceiptAllocation(ctx, again)
	rejected("an allocation reversed twice", err, "reversed already")
	if r, err = b.module.IssueCreditNote(ctx, b.credit("CN-1", "DN-1", 8, 2)); err != nil {
		t.Fatalf("the credit note once the allocation is reversed: %v", err)
	}
	b.job = r.NewRevision
	r4 = allocated(b.allocation("allocate-R-4-again", "R-4", r4, 9, to("DN-1", cny(364_000))))
	if n := invoice("DN-1"); n.Settled != cny(864_000) || n.Open != cny(0) || len(n.Settlements) != 2 || len(n.CreditNotes) != 1 {
		t.Fatalf("DN-1 settled after the credit note: %+v", n)
	}
	// the document fee is an estimate again; CNY 300.00 of the second receipt stays on account and lowers nothing
	if a := account("party-a"); a.Billed != cny(0) || a.Unbilled != cny(30_000) || a.Exposure != cny(30_000) || len(a.OnAccount) != 1 || a.OnAccount[0] != cny(30_000) {
		t.Fatalf("party-a's exposure with money on account: %+v", a)
	}
	exposure, err := b.module.ListCreditExposure(ctx, api.ListCreditExposure{TenantID: b.tenant, LegalEntityID: "LE"})
	if err != nil || len(exposure) != 2 {
		t.Fatalf("exposure list: %+v %v", exposure, err)
	}
	onAccount := map[string][]kernel.Money{}
	for _, a := range exposure {
		onAccount[a.DebtorPartyID] = a.OnAccount
	}
	if !slices.Equal(onAccount["party-a"], []kernel.Money{cny(30_000)}) || !slices.Equal(onAccount["agent-ny"], []kernel.Money{usd(100)}) {
		t.Fatalf("money on account in the exposure list: %+v", exposure)
	}
	if got, err := b.module.GetDebtorReceipt(ctx, api.GetDebtorReceipt{TenantID: b.tenant, ReceiptID: "R-4"}); err != nil || got.Revision != r4 ||
		got.Unallocated != cny(30_000) || len(got.Allocations) != 2 || got.Allocations[0].ReversedBy != "cash-2" || got.Allocations[0].ReversedOn != b.date(8) ||
		got.Allocations[0].ReverseReason != "the document fee is to be credited" || got.Allocations[1].ReversedBy != "" ||
		got.Allocations[1].Amount != cny(364_000) || got.Allocations[1].Date != b.date(9) {
		t.Fatalf("R-4 read: %+v %v", got, err)
	}

	// a receipt entered in error is cancelled and its evidence entered again
	cancel := api.CancelDebtorReceipt{CommandID: "cancel-R-3", TenantID: b.tenant, ActorID: "cash-1", ReceiptID: "R-3", ExpectedRevision: r3,
		Reason: "LOST", Date: b.date(9), OccurredAt: b.hours(9)}
	_, err = b.module.CancelDebtorReceipt(ctx, cancel)
	rejected("an unknown cancellation reason", err, "")
	cancel.Reason = api.ReceiptEnteredInError
	if r, err = b.module.CancelDebtorReceipt(ctx, cancel); err != nil || r.NewRevision != r3+1 {
		t.Fatalf("cancelled: %+v %v", r, err)
	}
	_, err = b.module.AllocateDebtorReceipt(ctx, b.allocation("allocate-cancelled", "R-3", r.NewRevision, 9, to("DN-1", usd(1))))
	rejected("allocating a cancelled receipt", err, "CANCELLED")
	b.received(t, b.receipt("R-3b", "agent-ny", 9, usd(100), kernel.Money{}, api.EvidenceERP, "ERP-REC-0009"))
	receipts, err := b.module.ListDebtorReceipts(ctx, api.ListDebtorReceipts{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "agent-ny"})
	if err != nil || len(receipts) != 3 || receipts[0].ReceiptID != "R-2" || receipts[1].State != api.ReceiptCancelled || receipts[1].Unallocated != usd(0) ||
		receipts[1].CancelReason != api.ReceiptEnteredInError || receipts[1].CancelledOn != b.date(9) || receipts[2].Unallocated != usd(100) {
		t.Fatalf("the agent's receipts: %+v %v", receipts, err)
	}

	// statements: opening and entries make the closing balance, which is the open items less the money on account
	for debtor, want := range map[string]struct {
		currency kernel.CurrencyCode
		entries  int
		closing  int64
		open     int
		account  int
	}{"party-a": {"CNY", 4, -30_000, 0, 1}, "agent-ny": {"USD", 5, -100, 0, 1}} {
		s, err := b.module.GetDebtorStatement(ctx, api.GetDebtorStatement{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: debtor, From: b.date(-48),
			To: b.date(24)})
		if err != nil || len(s.Currencies) != 1 {
			t.Fatalf("%s's statement: %+v %v", debtor, s, err)
		}
		c := s.Currencies[0]
		balance := c.Opening.Minor
		for _, e := range c.Entries {
			if balance += e.Amount.Minor; e.Balance.Minor != balance {
				t.Fatalf("%s's statement: balance after %s is %v", debtor, e.ID, e.Balance)
			}
		}
		var open int64
		for _, i := range c.OpenItems {
			open += i.Open.Minor
		}
		for _, r := range c.OnAccount {
			open -= r.Unallocated.Minor
		}
		if c.Currency != want.currency || c.Opening.Minor != 0 || len(c.Entries) != want.entries || c.Closing.Minor != want.closing || balance != want.closing ||
			open != want.closing || len(c.OpenItems) != want.open || len(c.OnAccount) != want.account {
			t.Fatalf("%s's statement: %+v", debtor, c)
		}
	}
	s, err := b.module.GetDebtorStatement(ctx, api.GetDebtorStatement{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a", From: b.date(-48),
		To: b.date(24)})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range s.Currencies[0].Entries {
		kinds[e.Kind]++
		if e.Kind == api.EntryDebitNote && (e.Number != "DN000001" || e.DocumentNumber != "SHA0000001" || e.Amount != cny(894_000)) {
			t.Fatalf("the debit note's entry: %+v", e)
		}
	}
	if kinds[api.EntryDebitNote] != 1 || kinds[api.EntryCreditNote] != 1 || kinds[api.EntryReceipt] != 2 {
		t.Fatalf("party-a's entries: %+v", s.Currencies[0].Entries)
	}
	_, err = b.module.GetDebtorStatement(ctx, api.GetDebtorStatement{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a", From: b.date(24),
		To: b.date(0)})
	rejected("a statement that ends before it starts", err, "")
	if s, err = b.module.GetDebtorStatement(ctx, api.GetDebtorStatement{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "airline-mu", From: b.date(0),
		To: b.date(24)}); err != nil || len(s.Currencies) != 0 {
		t.Fatalf("a statement of a party with nothing: %+v %v", s, err)
	}

	types := b.outboxTypes(t)
	for _, want := range []string{"DebtorReceiptRecorded", "DebtorReceiptAllocated", "DebtorReceiptAllocationReversed", "DebtorReceiptCancelled"} {
		if !slices.Contains(types, want) {
			t.Errorf("no %s event in %v", want, types)
		}
	}

	// past FIN on the application role: rows only change the way a receipt and an allocation may
	for name, sql := range map[string]string{
		"delete a receipt":          `DELETE FROM cargo_fin.debtor_receipt WHERE receipt_id='R-1'`,
		"change a receipt's amount": `UPDATE cargo_fin.debtor_receipt SET amount_minor=amount_minor+1 WHERE receipt_id='R-1'`,
		"bring a receipt back": `UPDATE cargo_fin.debtor_receipt SET state='RECORDED',cancel_reason=NULL,cancelled_by=NULL,cancelled_on=NULL,cancelled_at=NULL,
            cancel_entered_at=NULL,revision=revision+1 WHERE receipt_id='R-3'`,
		"delete an allocation":          `DELETE FROM cargo_fin.debtor_receipt_allocation WHERE receipt_id='R-1'`,
		"change an allocation's amount": `UPDATE cargo_fin.debtor_receipt_allocation SET amount_minor=1 WHERE receipt_id='R-1'`,
		"bring an allocation back": `UPDATE cargo_fin.debtor_receipt_allocation SET reversed_by=NULL,reverse_reason=NULL,reversed_on=NULL,reversed_at=NULL,
            reverse_entered_at=NULL WHERE receipt_id='R-4' AND allocation_no=1`,
		"another tenant's receipt": `INSERT INTO cargo_fin.debtor_receipt (tenant_id,receipt_id,legal_entity_id,debtor_party_id,debtor_revision,currency,
            amount_minor,bank_charges_minor,received_on,evidence_source,evidence_ref,state,recorded_by,recorded_at,entered_at)
            SELECT 'another-tenant','R-X',legal_entity_id,debtor_party_id,debtor_revision,currency,amount_minor,bank_charges_minor,received_on,evidence_source,
            'X-1',state,recorded_by,recorded_at,entered_at FROM cargo_fin.debtor_receipt WHERE receipt_id='R-1'`,
	} {
		err := b.within(t, func(ctx context.Context, e bentopg.Executor) error {
			tag, err := e.Exec(ctx, sql)
			if err == nil && tag.RowsAffected() == 0 {
				return errors.New("no row")
			}
			return err
		})
		if code, _ := sqlState(err); code != "23001" && code != "42501" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// at commit: an allocation comes with its receipt, to a note of the receipt's debtor and currency, within what the
	// note bills and the receipt holds; a cancelled receipt settles nothing
	const insert = `INSERT INTO cargo_fin.debtor_receipt_allocation (tenant_id,receipt_id,allocation_no,invoice_id,amount_minor,allocated_on,allocated_by,
        allocated_at,entered_at) SELECT tenant_id,receipt_id,(SELECT count(*)+1 FROM cargo_fin.debtor_receipt_allocation a WHERE a.receipt_id=r.receipt_id),`
	const from = `,current_date,'x',now(),now() FROM cargo_fin.debtor_receipt r WHERE receipt_id=`
	const touch = `UPDATE cargo_fin.debtor_receipt SET revision=revision+1 WHERE receipt_id=`
	for name, c := range map[string]struct {
		sql        []string
		constraint string
	}{
		"an allocation behind its receipt":     {[]string{insert + `'DN-2',1` + from + `'R-3b'`}, "fin_records_with_parent"},
		"more than the note bills":             {[]string{insert + `'DN-2',1` + from + `'R-3b'`, touch + `'R-3b'`}, "debtor_invoice_settlement_consistent"},
		"another debtor's note":                {[]string{insert + `'DN-1',1` + from + `'R-3b'`, touch + `'R-3b'`}, "debtor_receipt_consistent"},
		"a credit note":                        {[]string{insert + `'CN-1',1` + from + `'R-4'`, touch + `'R-4'`}, "debtor_receipt_consistent"},
		"more than the receipt holds":          {[]string{touch + `'R-1'`, insert + `'DN-1',1` + from + `'R-1'`}, "debtor_receipt_consistent"},
		"an allocation numbered out of order":  {[]string{touch + `'R-4'`, strings.Replace(insert, "count(*)+1", "count(*)+2", 1) + `'DN-1',1` + from + `'R-4'`}, "debtor_receipt_consistent"},
		"a receipt cancelled while it settles": {[]string{`UPDATE cargo_fin.debtor_receipt SET state='CANCELLED',cancel_reason='ENTERED_IN_ERROR',cancelled_by='x',cancelled_on=current_date,cancelled_at=now(),cancel_entered_at=now(),revision=revision+1 WHERE receipt_id='R-1'`}, "debtor_receipt_consistent"},
	} {
		err := b.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, sql := range c.sql {
				if tag, err := e.Exec(ctx, sql); err != nil {
					return err
				} else if tag.RowsAffected() != 1 {
					return fmt.Errorf("%d rows", tag.RowsAffected())
				}
			}
			return nil
		})
		if _, constraint := sqlState(err); constraint != c.constraint {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a stored allocation made larger than its note (as the migrator role): the job's and the receipt's next commit refuse
	check := func(sql string) error {
		err := b.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range []string{sql, `SET CONSTRAINTS ALL IMMEDIATE`} {
				if _, err := e.Exec(ctx, s); err != nil {
					return err
				}
			}
			return errRolledBack
		})
		if errors.Is(err, errRolledBack) {
			return nil
		}
		return err
	}
	const jobWrite = `UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE job_id='JOB-A'`
	if err = errors.Join(check(jobWrite), check(touch+`'R-2'`)); err != nil {
		t.Fatalf("the job and the receipt as FIN wrote them: %v", err)
	}
	b.rewrite(t, `UPDATE cargo_fin.debtor_receipt_allocation SET amount_minor=amount_minor+1 WHERE tenant_id=$1 AND receipt_id='R-2' AND allocation_no=2`)
	if _, constraint := sqlState(check(jobWrite)); constraint != "debtor_invoice_settlement_consistent" {
		t.Fatalf("a job with a note settled beyond what it bills: %v", check(jobWrite))
	}
	if _, constraint := sqlState(check(touch + `'R-2'`)); constraint != "debtor_receipt_consistent" {
		t.Fatalf("a receipt allocated beyond what it holds: %v", check(touch+`'R-2'`))
	}
	b.rewrite(t, `UPDATE cargo_fin.debtor_receipt_allocation SET amount_minor=amount_minor-1 WHERE tenant_id=$1 AND receipt_id='R-2' AND allocation_no=2`)
	if err = errors.Join(check(jobWrite), check(touch+`'R-2'`)); err != nil {
		t.Fatalf("put back: %v", err)
	}
	// a stored allocation dated before its receipt: the receipt's next commit refuses
	b.rewrite(t, `UPDATE cargo_fin.debtor_receipt_allocation SET allocated_on=allocated_on-21, allocated_at=allocated_at-interval '21 days'
        WHERE tenant_id=$1 AND receipt_id='R-2' AND allocation_no=2`)
	if _, constraint := sqlState(check(touch + `'R-2'`)); constraint != "debtor_receipt_consistent" {
		t.Fatalf("an allocation dated before its receipt: %v", check(touch+`'R-2'`))
	}
	b.rewrite(t, `UPDATE cargo_fin.debtor_receipt_allocation SET allocated_on=allocated_on+21, allocated_at=allocated_at+interval '21 days'
        WHERE tenant_id=$1 AND receipt_id='R-2' AND allocation_no=2`)
	if err = check(touch + `'R-2'`); err != nil {
		t.Fatalf("put back: %v", err)
	}
}

// A debtor whose roles have ended still pays what it owes: with a debit note open, a receipt is recorded at the
// revision the latest open note billed; with nothing open, recording stays refused. A debtor with only money on
// account is listed in ListCreditExposure at no exposure.
func TestReceiptsAfterTheDebtorRoleEnds(t *testing.T) {
	ctx := context.Background()
	b := billedTwice(t) // DN-1 to party-a: CNY 8,940.00 open
	b.w.mu.Lock()
	saved := b.w.roles["party-a"]
	b.w.roles["party-a"] = nil
	b.w.mu.Unlock()
	defer func() { b.w.mu.Lock(); b.w.roles["party-a"] = saved; b.w.mu.Unlock() }()
	ra := b.received(t, b.receipt("R-ENDED", "party-a", 5, cny(894_000), kernel.Money{}, api.EvidenceBank, "BOC-ENDED"))
	if _, err := b.module.AllocateDebtorReceipt(ctx, b.allocation("al-ended", "R-ENDED", ra, 5, to("DN-1", cny(894_000)))); err != nil {
		t.Fatalf("allocating the ended debtor's receipt: %v", err)
	}
	_, err := b.module.RecordDebtorReceipt(ctx, b.receipt("R-ENDED-2", "party-a", 5, cny(1_000), kernel.Money{}, api.EvidenceBank, "BOC-ENDED-2"))
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_PARTY_ROLE") {
		t.Fatalf("recording with nothing open and no role: %v", err)
	}
	// a debtor with only money on account, no credit account and no exposure
	b.w.mu.Lock()
	b.w.roles["party-c"] = []string{"CUSTOMER"}
	b.w.mu.Unlock()
	b.received(t, b.receipt("R-ONLY", "party-c", 5, cny(88_800), kernel.Money{}, api.EvidenceBank, "BOC-ONLY"))
	list, err := b.module.ListCreditExposure(ctx, api.ListCreditExposure{TenantID: b.tenant, LegalEntityID: "LE"})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list, func(a api.CreditAccount) bool { return a.DebtorPartyID == "party-c" })
	if i < 0 || list[i].Status != api.CreditNoAccount || list[i].Exposure.Minor != 0 || len(list[i].OnAccount) != 1 || list[i].OnAccount[0].Minor != 88_800 {
		t.Fatalf("money on account without an account or exposure: %+v", list)
	}
	if _, err = b.module.GetCreditAccount(ctx, api.GetCreditAccount{TenantID: b.tenant, LegalEntityID: "LE", DebtorPartyID: "party-c"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a credit account party-c never had: %v", err)
	}
}

// Six receipts allocate the whole of one debit note at once: one settles it, the others find nothing open.
func TestReceiptsRaceForOneDebitNote(t *testing.T) {
	ctx := context.Background()
	b := billedTwice(t)
	const n = 6
	for i := range n {
		b.received(t, b.receipt(fmt.Sprintf("R-%d", i), "party-a", 5, cny(894_000), kernel.Money{}, api.EvidenceBank, fmt.Sprintf("BOC-%d", i)))
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("R-%d", i)
			_, errs[i] = b.module.AllocateDebtorReceipt(ctx, b.allocation("allocate-"+id, id, 1, 6, to("DN-1", cny(894_000))))
		}()
	}
	close(start)
	wg.Wait()
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_OVER_ALLOCATED"):
			t.Fatalf("receipt %d: %v", i, err)
		}
	}
	n1, err := b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "DN-1"})
	if won != 1 || err != nil || n1.Settled != cny(894_000) || n1.Open != cny(0) || len(n1.Settlements) != 1 {
		t.Fatalf("%d receipts settled the note: %+v %v", won, n1, err)
	}
}

// A receipt allocating the whole of a debit note races a credit note reversing one of its lines: whichever commits
// first, the other is refused on the figures it left, and the note never has more settled than it bills.
func TestAllocationRacesACreditNote(t *testing.T) {
	ctx := context.Background()
	for round := range 4 {
		b := billedTwice(t)
		b.received(t, b.receipt("R-1", "party-a", 5, cny(894_000), kernel.Money{}, api.EvidenceBank, "BOC-1"))
		var allocateErr, creditErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, allocateErr = b.module.AllocateDebtorReceipt(ctx, b.allocation("allocate-R-1", "R-1", 1, 6, to("DN-1", cny(894_000))))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, creditErr = b.module.IssueCreditNote(ctx, b.credit("CN-1", "DN-1", 6, 2))
		}()
		close(start)
		wg.Wait()
		n1, err := b.module.GetDebtorInvoice(ctx, api.GetDebtorInvoice{TenantID: b.tenant, InvoiceID: "DN-1"})
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case allocateErr == nil && errors.Is(creditErr, api.ErrRejected) && strings.Contains(creditErr.Error(), "FIN_SETTLED"):
			if n1.Settled != cny(894_000) || n1.Open != cny(0) || len(n1.CreditNotes) != 0 {
				t.Fatalf("round %d, the allocation first: %+v", round, n1)
			}
		case creditErr == nil && errors.Is(allocateErr, api.ErrRejected) && strings.Contains(allocateErr.Error(), "FIN_OVER_ALLOCATED"):
			if n1.Settled != cny(0) || n1.Open != cny(864_000) || len(n1.CreditNotes) != 1 {
				t.Fatalf("round %d, the credit note first: %+v", round, n1)
			}
		default:
			t.Fatalf("round %d: allocation %v, credit note %v", round, allocateErr, creditErr)
		}
	}
}
