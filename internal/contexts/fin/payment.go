package fin

import (
	"context"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.6: creditor payments.

func (m *Module) RecordCreditorPayment(ctx context.Context, cmd api.RecordCreditorPayment) (api.Receipt, error) {
	paid, err := domain.ParseDate(cmd.PaidOn)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordCreditorPayment(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionRecordCreditorPayment, cmd.PaymentID, 0, cmd.OccurredAt, cmd), domain.PaymentInput{ID: cmd.PaymentID, LegalEntityID: cmd.LegalEntityID,
		CreditorPartyID: cmd.CreditorPartyID, Amount: cmd.Amount, PaidOn: paid, EvidenceSource: cmd.EvidenceSource, EvidenceRef: cmd.EvidenceRef})
	return receipt(r, err)
}

func (m *Module) AllocateCreditorPayment(ctx context.Context, cmd api.AllocateCreditorPayment) (api.Receipt, error) {
	on, err := domain.ParseDate(cmd.Date)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	items := make([]domain.AllocationItem, 0, len(cmd.Allocations))
	for _, a := range cmd.Allocations {
		items = append(items, domain.AllocationItem{InvoiceID: a.InvoiceID, Amount: a.Amount})
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).AllocateCreditorPayment(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionAllocateCreditorPayment, cmd.PaymentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), items, on)
	return receipt(r, err)
}

func (m *Module) ReverseCreditorPaymentAllocation(ctx context.Context, cmd api.ReverseCreditorPaymentAllocation) (api.Receipt, error) {
	on, err := domain.ParseDate(cmd.Date)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ReverseCreditorPaymentAllocation(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionReverseCreditorPaymentAllocation, cmd.PaymentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.AllocationNo, cmd.Reason, on)
	return receipt(r, err)
}

func (m *Module) CancelCreditorPayment(ctx context.Context, cmd api.CancelCreditorPayment) (api.Receipt, error) {
	on, err := domain.ParseDate(cmd.Date)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).CancelCreditorPayment(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionCancelCreditorPayment, cmd.PaymentID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason, on)
	return receipt(r, err)
}

func paymentView(p *domain.CreditorPayment, revision int64) api.CreditorPayment {
	st := p.State()
	money := func(v int64) kernel.Money { return kernel.Money{Currency: st.Currency, Minor: v} }
	out := api.CreditorPayment{TenantID: string(st.TenantID), PaymentID: st.PaymentID, Revision: revision, LegalEntityID: st.LegalEntityID,
		CreditorPartyID: st.CreditorPartyID, CreditorRevision: st.CreditorRevision, Amount: money(st.Amount), Unallocated: money(st.Unallocated()),
		PaidOn: dateView(st.PaidOn), EvidenceSource: st.EvidenceSource, EvidenceRef: st.EvidenceRef, State: st.State, RecordedBy: st.RecordedBy,
		RecordedAt: st.RecordedAt, CancelReason: st.CancelReason, CancelledBy: st.CancelledBy, CancelledOn: dateView(st.CancelledOn), CancelledAt: st.CancelledAt}
	if st.State == domain.PaymentCancelled {
		out.Unallocated = money(0)
	}
	for _, a := range st.Allocations {
		out.Allocations = append(out.Allocations, api.CreditorPaymentAllocation{AllocationNo: a.No, InvoiceID: a.InvoiceID, Amount: money(a.Amount),
			Date: dateView(a.Date), AllocatedBy: a.By, AllocatedAt: a.At, ReversedBy: a.ReversedBy, ReverseReason: a.ReverseReason,
			ReversedOn: dateView(a.ReversedOn), ReversedAt: a.ReversedAt})
	}
	return out
}

func (m *Module) GetCreditorPayment(ctx context.Context, q api.GetCreditorPayment) (api.CreditorPayment, error) {
	d := m.deps(q.TenantID, "")
	var out api.CreditorPayment
	err := d.Reader(ctx, func(txCtx context.Context) error {
		p, revision, err := d.Payments.Read(txCtx, domain.TenantID(q.TenantID), q.PaymentID)
		if err == nil {
			out = paymentView(p, revision)
		}
		return err
	})
	if err != nil {
		return api.CreditorPayment{}, classify(err)
	}
	return out, nil
}

func (m *Module) ListCreditorPayments(ctx context.Context, q api.ListCreditorPayments) ([]api.CreditorPayment, error) {
	d := m.deps(q.TenantID, "")
	out := []api.CreditorPayment{}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		payments, revisions, err := d.Payments.OfCreditor(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.CreditorPartyID)
		for i, p := range payments {
			out = append(out, paymentView(p, revisions[i]))
		}
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// paidOf reads the live allocations paying a creditor invoice.
func paidOf(ctx context.Context, d application.Deps, tenant, invoice string) ([]application.PaidAllocation, error) {
	return d.Payments.Paid(ctx, domain.TenantID(tenant), invoice)
}

// GetCreditorAccount states, in one snapshot, what a legal entity owes a creditor: its invoices not cancelled with
// something open, what is open per currency, and what payments have on account.
func (m *Module) GetCreditorAccount(ctx context.Context, q api.GetCreditorAccount) (api.CreditorAccount, error) {
	asOf := m.clock.Now().UTC().Truncate(24 * time.Hour)
	if q.AsOf != "" {
		var err error
		if asOf, err = domain.ParseDate(q.AsOf); err != nil {
			return api.CreditorAccount{}, classify(err)
		}
	}
	d := m.deps(q.TenantID, "")
	out := api.CreditorAccount{LegalEntityID: q.LegalEntityID, CreditorPartyID: q.CreditorPartyID, AsOf: dateView(asOf), OpenItems: []api.CreditorOpenItem{},
		Open: []kernel.Money{}, OnAccount: []kernel.Money{}}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		invoices, _, err := d.Creditors.List(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.CreditorPartyID)
		if err != nil {
			return err
		}
		open := map[kernel.CurrencyCode]int64{}
		for _, inv := range invoices {
			st := inv.State()
			if st.State == domain.CreditorCancelled {
				continue
			}
			paid, err := paidOf(txCtx, d, q.TenantID, st.InvoiceID)
			if err != nil {
				return err
			}
			var total int64
			for _, a := range paid {
				total += a.Amount
			}
			if st.Total()-total <= 0 {
				continue
			}
			money := func(v int64) kernel.Money { return kernel.Money{Currency: st.Currency, Minor: v} }
			out.OpenItems = append(out.OpenItems, api.CreditorOpenItem{InvoiceID: st.InvoiceID, SupplierInvoiceNo: st.SupplierInvoiceNo,
				InvoiceDate: dateView(st.InvoiceDate), State: st.State, AwaitingApproval: st.AwaitingApproval(),
				Payable: st.State == domain.CreditorMatched && !st.AwaitingApproval(), Total: money(st.Total()), Paid: money(total),
				Open: money(st.Total() - total), DaysSinceInvoice: int(asOf.Sub(st.InvoiceDate).Hours() / 24)})
			open[st.Currency] += st.Total() - total
		}
		for currency, v := range open {
			out.Open = append(out.Open, kernel.Money{Currency: currency, Minor: v})
		}
		slices.SortFunc(out.Open, func(a, b kernel.Money) int {
			if a.Currency < b.Currency {
				return -1
			}
			if a.Currency > b.Currency {
				return 1
			}
			return 0
		})
		onAccount, err := d.Payments.OnAccount(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.CreditorPartyID)
		out.OnAccount = append(out.OnAccount, onAccount...)
		return err
	})
	if err != nil {
		return api.CreditorAccount{}, classify(err)
	}
	return out, nil
}
