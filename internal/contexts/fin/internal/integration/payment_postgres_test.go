//go:build integration

package integration

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (a ap) payment(id string) api.CreditorPayment {
	a.t.Helper()
	p, err := a.module.GetCreditorPayment(a.ctx, api.GetCreditorPayment{TenantID: a.tenant, PaymentID: id})
	if err != nil {
		a.t.Fatal(err)
	}
	return p
}

func (a ap) pay(id, creditor, ref string, h int, amount kernel.Money) api.RecordCreditorPayment {
	return api.RecordCreditorPayment{CommandID: "pay-" + id, TenantID: a.tenant, ActorID: "ap-1", PaymentID: id, LegalEntityID: "LE", CreditorPartyID: creditor,
		Amount: amount, PaidOn: a.day(h), EvidenceSource: "BANK", EvidenceRef: ref, OccurredAt: a.hours(h)}
}

func (a ap) allocate(id string, h int, items ...api.PaymentAllocation) api.AllocateCreditorPayment {
	return api.AllocateCreditorPayment{CommandID: "allocate-" + id + "-" + a.day(h) + "-" + items[0].InvoiceID, TenantID: a.tenant, ActorID: "ap-1",
		PaymentID: id, ExpectedRevision: a.payment(id).Revision, Allocations: items, Date: a.day(h), OccurredAt: a.hours(h)}
}

func paying(invoice string, amount kernel.Money) api.PaymentAllocation {
	return api.PaymentAllocation{InvoiceID: invoice, Amount: amount}
}

// payableWorld is creditorWorld with the trucker's invoices: CI-1 (pickup 1,500.00 CNY, matched final on JOB-A), CI-2
// (CFS handling 500.00, matched final on P-1), CI-R (300.00, recorded only) and CI-D (300.00, matched on JOB-B and
// disputed).
func payableWorld(t *testing.T) ap {
	a := creditorWorld(t)
	a.must(a.module.RecordCreditorInvoice(a.ctx, a.record("CI-1", "trucker", "TR-1", 3, "CNY", line("PICKUP", 150_000))))
	a.must(a.module.MatchCreditorInvoice(a.ctx, a.match("CI-1", "ap-1", 3, onJob("JOB-A", 1, 1, 150_000, true))))
	a.must(a.module.RecordCreditorInvoice(a.ctx, a.record("CI-2", "trucker", "TR-2", 3, "CNY", line("CFS_HANDLING", 50_000))))
	a.must(a.module.MatchCreditorInvoice(a.ctx, a.match("CI-2", "ap-1", 3, onPool("P-1", 2, 1, 50_000, true))))
	a.must(a.module.RecordCreditorInvoice(a.ctx, a.record("CI-R", "trucker", "TR-R", 3, "CNY", line("DELIVERY", 10_000))))
	a.must(a.module.RecordCreditorInvoice(a.ctx, a.record("CI-D", "trucker", "TR-D", 3, "CNY", line("DELIVERY", 30_000))))
	a.must(a.module.MatchCreditorInvoice(a.ctx, a.match("CI-D", "ap-1", 3, onJob("JOB-B", 1, 1, 30_000, true))))
	a.must(a.module.DisputeCreditorInvoice(a.ctx, api.DisputeCreditorInvoice{CommandID: "dispute-d", TenantID: a.tenant, ActorID: "ap-1", InvoiceID: "CI-D",
		ExpectedRevision: a.invoice("CI-D").Revision, Reason: "delivery not done", OccurredAt: a.hours(3)}))
	return a
}

// Creditor payments on PostgreSQL (FIN V0.6): recorded once per piece of evidence, allocated to the creditor's matched
// invoices only, within what each has open and the payment has unallocated, the rest on account; a creditor whose
// roles ended is paid what it is owed; reversals, a cancellation, a paid invoice not cancelled, the creditor's account;
// and what the database refuses past FIN.
func TestCreditorPaymentsOnPostgres(t *testing.T) {
	a := payableWorld(t)
	ctx, f := a.ctx, a.fixture

	pay1 := a.pay("PAY-1", "trucker", "BOC-0001", 4, cny(180_000))
	customer := pay1
	customer.CommandID, customer.PaymentID, customer.CreditorPartyID = "pay-customer", "PAY-X", "party-a"
	_, err := f.module.RecordCreditorPayment(ctx, customer)
	a.rejected("a payee without a creditor role", err, "FIN_PARTY_ROLE")
	ahead := pay1
	ahead.CommandID, ahead.PaymentID, ahead.PaidOn = "pay-ahead", "PAY-Y", a.day(60)
	_, err = f.module.RecordCreditorPayment(ctx, ahead)
	a.rejected("paid ahead", err, "date paid")
	r := a.must(f.module.RecordCreditorPayment(ctx, pay1))
	if r.SubjectID != "PAY-1" || r.NewRevision != 1 {
		t.Fatalf("recorded: %+v", r)
	}
	if again, err := f.module.RecordCreditorPayment(ctx, pay1); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	twice := pay1
	twice.CommandID, twice.PaymentID = "pay-twice", "PAY-1B"
	_, err = f.module.RecordCreditorPayment(ctx, twice)
	a.rejected("a piece of evidence twice", err, "FIN_EVIDENCE_USED")

	// held from payment: an invoice recorded only, one disputed; more than an invoice has open
	_, err = f.module.AllocateCreditorPayment(ctx, a.allocate("PAY-1", 5, paying("CI-R", cny(1))))
	a.rejected("a recorded invoice", err, "FIN_NOT_PAYABLE")
	_, err = f.module.AllocateCreditorPayment(ctx, a.allocate("PAY-1", 5, paying("CI-D", cny(1))))
	a.rejected("a disputed invoice", err, "FIN_NOT_PAYABLE")
	_, err = f.module.AllocateCreditorPayment(ctx, a.allocate("PAY-1", 5, paying("CI-1", cny(150_001))))
	a.rejected("more than the invoice has open", err, "FIN_OVER_ALLOCATED")
	if r = a.must(f.module.AllocateCreditorPayment(ctx, a.allocate("PAY-1", 5, paying("CI-1", cny(150_000)), paying("CI-2", cny(30_000))))); r.Version != 2 {
		t.Fatalf("allocated: %+v", r)
	}
	if p := a.payment("PAY-1"); p.Unallocated != cny(0) || len(p.Allocations) != 2 {
		t.Fatalf("PAY-1: %+v", p)
	}
	if c := a.invoice("CI-2"); c.Paid != cny(30_000) || c.Open != cny(20_000) || len(c.Payments) != 1 || c.Payments[0].PaymentID != "PAY-1" {
		t.Fatalf("CI-2 in part: %+v", c)
	}

	// the trucker's roles end: it is still paid what it is owed, CI-2's 200.00 and CI-D's 300.00 (matched, then disputed),
	// not CI-R's (never matched); in that currency, not beyond, and not while ORG reports it suspended. A creditor owed
	// nothing is not paid.
	a.w.mu.Lock()
	roles := a.w.roles["trucker"]
	delete(a.w.roles, "trucker")
	delete(a.w.roles, "airline-mu")
	a.w.mu.Unlock()
	_, err = f.module.RecordCreditorPayment(ctx, a.pay("PAY-2X", "trucker", "V-87", 6, cny(50_001)))
	a.rejected("a former creditor paid beyond what it is owed", err, "owed 50000 CNY")
	usd := a.pay("PAY-2U", "trucker", "V-86", 6, cny(1))
	usd.Amount.Currency = "USD"
	_, err = f.module.RecordCreditorPayment(ctx, usd)
	a.rejected("a former creditor paid in a currency it is not owed", err, "owed 0 USD")
	a.w.mu.Lock()
	suspended := a.w.identities["trucker"]
	suspended.Status = "SUSPENDED"
	a.w.identities["trucker"] = suspended
	a.w.mu.Unlock()
	_, err = f.module.RecordCreditorPayment(ctx, a.pay("PAY-2S", "trucker", "V-85", 6, cny(1)))
	a.rejected("a former creditor ORG suspended", err, "ORG reports it SUSPENDED")
	a.w.mu.Lock()
	suspended.Status = "ACTIVE"
	a.w.identities["trucker"] = suspended
	a.w.mu.Unlock()
	a.must(f.module.RecordCreditorPayment(ctx, a.pay("PAY-2", "trucker", "V-88", 6, cny(50_000))))
	_, err = f.module.RecordCreditorPayment(ctx, a.pay("PAY-2Y", "trucker", "V-84", 6, cny(1)))
	a.rejected("a former creditor paid again once on account", err, "owed 0 CNY")
	_, err = f.module.RecordCreditorPayment(ctx, a.pay("PAY-MU", "airline-mu", "V-89", 6, cny(1)))
	a.rejected("a former creditor owed nothing", err, "FIN_PARTY_ROLE")
	a.w.mu.Lock()
	a.w.roles["trucker"], a.w.roles["airline-mu"] = roles, []string{"CARRIER"}
	a.w.mu.Unlock()
	if p := a.payment("PAY-2"); p.CreditorRevision != 4 {
		t.Fatalf("PAY-2 at the revision CI-2 named: %+v", p)
	}
	a.must(f.module.AllocateCreditorPayment(ctx, a.allocate("PAY-2", 6, paying("CI-2", cny(20_000)))))

	_, err = f.module.GetCreditorAccount(ctx, api.GetCreditorAccount{TenantID: f.tenant, LegalEntityID: "LE"})
	a.rejected("an account of no creditor", err, "invalid query")
	_, err = f.module.ListCreditorPayments(ctx, api.ListCreditorPayments{TenantID: f.tenant, CreditorPartyID: "trucker"})
	a.rejected("payments by no legal entity", err, "invalid query")
	account, err := f.module.GetCreditorAccount(ctx, api.GetCreditorAccount{TenantID: f.tenant, LegalEntityID: "LE", CreditorPartyID: "trucker"})
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err != nil || account.StatedOn != today.Format(time.DateOnly) || len(account.OpenItems) != 2 || len(account.Open) != 1 || account.Open[0] != cny(40_000) || len(account.OnAccount) != 1 ||
		account.OnAccount[0] != cny(30_000) {
		t.Fatalf("the trucker's account: %+v %v", account, err)
	}
	for _, it := range account.OpenItems {
		dated, _ := time.Parse(time.DateOnly, it.InvoiceDate)
		if it.Payable || (it.InvoiceID != "CI-R" && it.InvoiceID != "CI-D") || it.DaysSinceInvoice != int(today.Sub(dated).Hours()/24) {
			t.Fatalf("open item: %+v", it)
		}
	}

	// a paid invoice is not cancelled until its allocations are reversed
	a.must(f.module.DisputeCreditorInvoice(ctx, api.DisputeCreditorInvoice{CommandID: "dispute-2", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-2",
		ExpectedRevision: a.invoice("CI-2").Revision, Reason: "the CFS billed twice", OccurredAt: a.hours(7)}))
	cancel2 := api.CancelCreditorInvoice{CommandID: "cancel-2", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-2",
		ExpectedRevision: a.invoice("CI-2").Revision, Reason: "billed twice", OccurredAt: a.hours(7)}
	_, err = f.module.CancelCreditorInvoice(ctx, cancel2)
	a.rejected("a paid invoice cancelled", err, "FIN_INVOICE_PAID")
	reverse := func(id string, no int) {
		a.must(f.module.ReverseCreditorPaymentAllocation(ctx, api.ReverseCreditorPaymentAllocation{CommandID: "reverse-" + id, TenantID: f.tenant,
			ActorID: "ap-1", PaymentID: id, ExpectedRevision: a.payment(id).Revision, AllocationNo: no, Reason: "CI-2 billed twice", Date: a.day(7),
			OccurredAt: a.hours(7)}))
	}
	reverse("PAY-1", 2)
	reverse("PAY-2", 1)
	cancel2.CommandID = "cancel-2-again"
	a.must(f.module.CancelCreditorInvoice(ctx, cancel2))
	if p := a.payment("PAY-1"); p.Unallocated != cny(30_000) || p.Allocations[1].ReverseReason == "" {
		t.Fatalf("PAY-1 after the reversal: %+v", p)
	}

	// the bank returns PAY-2; its evidence is entered again
	a.must(f.module.CancelCreditorPayment(ctx, api.CancelCreditorPayment{CommandID: "cancel-pay-2", TenantID: f.tenant, ActorID: "ap-1", PaymentID: "PAY-2",
		ExpectedRevision: a.payment("PAY-2").Revision, Reason: "PAYMENT_RETURNED", Date: a.day(8), OccurredAt: a.hours(8)}))
	a.must(f.module.RecordCreditorPayment(ctx, a.pay("PAY-3", "trucker", "V-88", 8, cny(50_000))))
	if payments, err := f.module.ListCreditorPayments(ctx, api.ListCreditorPayments{TenantID: f.tenant, LegalEntityID: "LE", CreditorPartyID: "trucker"}); err != nil ||
		len(payments) != 3 || payments[1].State != api.PaymentCancelled || payments[1].Unallocated != cny(0) {
		t.Fatalf("payments: %+v %v", payments, err)
	}

	counts := map[string]int{}
	for _, kind := range f.outboxTypes(t) {
		counts[kind]++
	}
	for kind, want := range map[string]int{"CreditorPaymentRecorded": 3, "CreditorPaymentAllocated": 3, "CreditorPaymentAllocationReversed": 2,
		"CreditorPaymentCancelled": 1, "CreditorInvoiceCancelled": 1} {
		if counts[kind] != want {
			t.Errorf("outbox: %d %s, want %d", counts[kind], kind, want)
		}
	}

	allocation := `INSERT INTO cargo_fin.creditor_payment_allocation (tenant_id,payment_id,allocation_no,invoice_id,amount_minor,allocated_on,allocated_by,
        allocated_at,entered_at) VALUES ($1,'PAY-3',1,'%s',%d,current_date,'x',now(),now())`
	withPayment := `UPDATE cargo_fin.creditor_payment SET revision=revision+1 WHERE tenant_id=$1 AND payment_id='PAY-3';
`
	for name, tc := range map[string]struct {
		sql, state, check string
	}{
		"an allocation rewritten": {`UPDATE cargo_fin.creditor_payment_allocation SET amount_minor=1 WHERE tenant_id=$1`, "42501|23001", ""},
		"an allocation deleted":   {`DELETE FROM cargo_fin.creditor_payment_allocation WHERE tenant_id=$1`, "42501|23001", ""},
		"an allocation behind its payment": {strings.Replace(strings.Replace(allocation, "'%s'", "'CI-1'", 1), "%d", "1", 1), "23514",
			"fin_records_with_parent"},
		"an invoice paid beyond its total": {withPayment + strings.Replace(strings.Replace(allocation, "'%s'", "'CI-1'", 1), "%d", "1", 1), "23514",
			"creditor_invoice_paid_within"},
		"a recorded invoice paid": {withPayment + strings.Replace(strings.Replace(allocation, "'%s'", "'CI-R'", 1), "%d", "1", 1), "23514",
			"creditor_payment_consistent"},
		"a disputed invoice paid": {withPayment + strings.Replace(strings.Replace(allocation, "'%s'", "'CI-D'", 1), "%d", "1", 1), "23514",
			"creditor_payment_consistent"},
		"a payment paying invoices cancelled": {`UPDATE cargo_fin.creditor_payment SET state='CANCELLED',cancel_reason='ENTERED_IN_ERROR',cancelled_by='x',
            cancelled_on=current_date,cancelled_at=now(),cancel_entered_at=now(),revision=revision+1 WHERE tenant_id=$1 AND payment_id='PAY-1'`,
			"23514", "creditor_payment_consistent"},
		"a paid invoice cancelled": {`UPDATE cargo_fin.creditor_invoice SET state='DISPUTED',revision=revision+1 WHERE tenant_id=$1 AND invoice_id='CI-1';
INSERT INTO cargo_fin.creditor_invoice_dispute (tenant_id,invoice_id,dispute_no,from_state,reason,disputed_by,disputed_at,entered_at)
            VALUES ($1,'CI-1',1,'MATCHED','x','x',now(),now());
UPDATE cargo_fin.creditor_invoice SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),cancel_entered_at=now(),revision=revision+1
            WHERE tenant_id=$1 AND invoice_id='CI-1'`, "23514", "creditor_invoice_paid_within"},
		"a piece of evidence twice": {`INSERT INTO cargo_fin.creditor_payment (tenant_id,payment_id,legal_entity_id,creditor_party_id,creditor_revision,currency,
            amount_minor,paid_on,evidence_source,evidence_ref,state,recorded_by,recorded_at,entered_at) VALUES ($1,'PAY-9','LE','trucker',4,'CNY',1,current_date,
            'BANK','BOC-0001','RECORDED','x',now(),now())`, "23505", "creditor_payment_evidence_once"},
		"a row committed once the transaction moved to another tenant": {`INSERT INTO cargo_fin.creditor_payment (tenant_id,payment_id,legal_entity_id,
            creditor_party_id,creditor_revision,currency,amount_minor,paid_on,evidence_source,evidence_ref,state,recorded_by,recorded_at,entered_at)
            VALUES ($1,'PAY-8','LE','trucker',4,'CNY',1,current_date,'BANK','SCOPE-1','RECORDED','x',now(),now());
SELECT set_config('app.tenant_id', 'another-tenant', true)`, "23514", "fin_tenant_scope"},
	} {
		statements := strings.Split(tc.sql, ";\n")
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range statements {
				var args []any
				if strings.Contains(s, "$1") {
					args = append(args, f.tenant)
				}
				if _, err := e.Exec(ctx, s, args...); err != nil {
					return err
				}
			}
			return nil
		})
		state, constraint := sqlState(err)
		if state == "" || !strings.Contains(tc.state, state) || tc.check != "" && constraint != tc.check {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, tc.state, tc.check)
		}
	}
}

// TestPaymentsRaceForOneInvoice races two payments of 1,000.00 each allocated in full to CI-1's 1,500.00: together
// they would pay more than it totals, so exactly one commits and the other hears FIN_OVER_ALLOCATED.
func TestPaymentsRaceForOneInvoice(t *testing.T) {
	setup := func(t *testing.T) ap {
		a := payableWorld(t)
		for _, id := range []string{"PAY-A", "PAY-B"} {
			a.must(a.module.RecordCreditorPayment(a.ctx, a.pay(id, "trucker", "REF-"+id, 4, cny(100_000))))
		}
		return a
	}
	allocating := func(id string) func(a ap) error {
		return func(a ap) error {
			_, err := a.module.AllocateCreditorPayment(a.ctx, api.AllocateCreditorPayment{CommandID: "allocate-" + id, TenantID: a.tenant, ActorID: "ap-1",
				PaymentID: id, ExpectedRevision: 1, Allocations: []api.PaymentAllocation{paying("CI-1", cny(100_000))}, Date: a.day(5), OccurredAt: a.hours(5)})
			return err
		}
	}
	race(t, setup, allocating("PAY-A"), allocating("PAY-B"), func(round int, _ bool, err error) {
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_OVER_ALLOCATED") {
			t.Fatalf("round %d: refused for another reason: %v", round, err)
		}
	})
}

// TestAllocationRacesADispute races paying CI-1 against disputing it: a dispute first holds the invoice from payment
// (FIN_NOT_PAYABLE); a payment first is followed by the dispute, which a paid invoice may still take. The allocation
// locks the invoice row as the dispute does, so one sees the other's commit.
func TestAllocationRacesADispute(t *testing.T) {
	paidFirst := 0
	for round := range 6 {
		a := payableWorld(t)
		a.must(a.module.RecordCreditorPayment(a.ctx, a.pay("PAY-A", "trucker", "REF-A", 4, cny(100_000))))
		errs := make(chan error, 2)
		start := make(chan struct{})
		go func() {
			<-start
			_, err := a.module.AllocateCreditorPayment(a.ctx, api.AllocateCreditorPayment{CommandID: "allocate", TenantID: a.tenant, ActorID: "ap-1",
				PaymentID: "PAY-A", ExpectedRevision: 1, Allocations: []api.PaymentAllocation{paying("CI-1", cny(100_000))}, Date: a.day(5), OccurredAt: a.hours(5)})
			errs <- err
		}()
		go func() {
			<-start
			if round%2 == 1 { // the allocation's head start, so that both orders occur
				time.Sleep(time.Duration(2000+rand.IntN(3000)) * time.Microsecond)
			}
			_, err := a.module.DisputeCreditorInvoice(a.ctx, api.DisputeCreditorInvoice{CommandID: "dispute", TenantID: a.tenant, ActorID: "ap-1", InvoiceID: "CI-1",
				ExpectedRevision: 2, Reason: "weight", OccurredAt: a.hours(5)})
			errs <- err
		}()
		close(start)
		var failed []error
		for range 2 {
			if err := <-errs; err != nil {
				failed = append(failed, err)
			}
		}
		c := a.invoice("CI-1")
		switch {
		case len(failed) > 1, len(failed) == 1 && !strings.Contains(failed[0].Error(), "FIN_NOT_PAYABLE"):
			t.Fatalf("round %d: %v", round, failed)
		case c.State != api.CreditorDisputed:
			t.Fatalf("round %d: CI-1 is %s", round, c.State)
		case len(failed) == 0 && (len(c.Payments) != 1 || c.Paid != cny(100_000)):
			t.Fatalf("round %d: paid and disputed: %+v", round, c)
		case len(failed) == 1 && len(c.Payments) != 0:
			t.Fatalf("round %d: refused yet paid: %+v", round, c)
		}
		if len(failed) == 0 {
			paidFirst++
		}
	}
	t.Logf("the payment came first in %d of 6 rounds, the dispute in %d", paidFirst, 6-paidFirst)
}
