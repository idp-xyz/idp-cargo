package fin

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (m *Module) RecordDebtorReceipt(ctx context.Context, cmd api.RecordDebtorReceipt) (api.Receipt, error) {
	received, err := domain.ParseDate(cmd.ReceivedOn)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordReceipt(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordDebtorReceipt,
		cmd.ReceiptID, 0, cmd.OccurredAt, cmd), domain.ReceiptInput{ID: cmd.ReceiptID, LegalEntityID: cmd.LegalEntityID, DebtorPartyID: cmd.DebtorPartyID,
		Amount: cmd.Amount, BankCharges: cmd.BankCharges, ReceivedOn: received, EvidenceSource: cmd.EvidenceSource, EvidenceRef: cmd.EvidenceRef})
	return receipt(r, err)
}

func (m *Module) AllocateDebtorReceipt(ctx context.Context, cmd api.AllocateDebtorReceipt) (api.Receipt, error) {
	on, err := domain.ParseDate(cmd.Date)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	items := make([]domain.AllocationItem, 0, len(cmd.Allocations))
	for _, a := range cmd.Allocations {
		items = append(items, domain.AllocationItem{InvoiceID: a.InvoiceID, Amount: a.Amount})
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).AllocateReceipt(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionAllocateDebtorReceipt,
		cmd.ReceiptID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), items, on)
	return receipt(r, err)
}

func (m *Module) ReverseDebtorReceiptAllocation(ctx context.Context, cmd api.ReverseDebtorReceiptAllocation) (api.Receipt, error) {
	on, err := domain.ParseDate(cmd.Date)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ReverseAllocation(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionReverseDebtorReceiptAllocation, cmd.ReceiptID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.AllocationNo, cmd.Reason, on)
	return receipt(r, err)
}

func (m *Module) CancelDebtorReceipt(ctx context.Context, cmd api.CancelDebtorReceipt) (api.Receipt, error) {
	on, err := domain.ParseDate(cmd.Date)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).CancelReceipt(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionCancelDebtorReceipt,
		cmd.ReceiptID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason, on)
	return receipt(r, err)
}

func receiptView(r *domain.DebtorReceipt, revision int64) api.DebtorReceipt {
	st := r.State()
	money := func(v int64) kernel.Money { return kernel.Money{Currency: st.Currency, Minor: v} }
	out := api.DebtorReceipt{TenantID: string(st.TenantID), ReceiptID: st.ReceiptID, Revision: revision, LegalEntityID: st.LegalEntityID,
		DebtorPartyID: st.DebtorPartyID, DebtorRevision: st.DebtorRevision, Amount: money(st.Amount), BankCharges: money(st.BankCharges),
		Unallocated: money(st.Unallocated()), ReceivedOn: dateView(st.ReceivedOn), EvidenceSource: st.EvidenceSource, EvidenceRef: st.EvidenceRef,
		State: st.State, RecordedBy: st.RecordedBy, RecordedAt: st.RecordedAt, CancelReason: st.CancelReason, CancelledBy: st.CancelledBy,
		CancelledOn: dateView(st.CancelledOn), CancelledAt: st.CancelledAt}
	if st.State == domain.ReceiptCancelled {
		out.Unallocated = money(0)
	}
	for _, a := range st.Allocations {
		out.Allocations = append(out.Allocations, api.DebtorReceiptAllocation{AllocationNo: a.No, InvoiceID: a.InvoiceID, Amount: money(a.Amount),
			Date: dateView(a.Date), AllocatedBy: a.By, AllocatedAt: a.At, ReversedBy: a.ReversedBy, ReverseReason: a.ReverseReason,
			ReversedOn: dateView(a.ReversedOn), ReversedAt: a.ReversedAt})
	}
	return out
}

func (m *Module) GetDebtorReceipt(ctx context.Context, q api.GetDebtorReceipt) (api.DebtorReceipt, error) {
	d := m.deps(q.TenantID, "")
	var out api.DebtorReceipt
	err := d.Reader(ctx, func(txCtx context.Context) error {
		r, revision, err := d.DebtorReceipts.Read(txCtx, domain.TenantID(q.TenantID), q.ReceiptID)
		if err == nil {
			out = receiptView(r, revision)
		}
		return err
	})
	if err != nil {
		return api.DebtorReceipt{}, classify(err)
	}
	return out, nil
}

func (m *Module) ListDebtorReceipts(ctx context.Context, q api.ListDebtorReceipts) ([]api.DebtorReceipt, error) {
	d := m.deps(q.TenantID, "")
	out := []api.DebtorReceipt{}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		receipts, revisions, err := d.DebtorReceipts.OfDebtor(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID)
		for i, r := range receipts {
			out = append(out, receiptView(r, revisions[i]))
		}
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// GetDebtorStatement reads the debtor's notes and receipts in one snapshot and states the account from them.
func (m *Module) GetDebtorStatement(ctx context.Context, q api.GetDebtorStatement) (api.DebtorStatement, error) {
	from, err := domain.ParseDate(q.From)
	if err != nil {
		return api.DebtorStatement{}, classify(err)
	}
	to, err := domain.ParseDate(q.To)
	if err != nil {
		return api.DebtorStatement{}, classify(err)
	}
	d := m.deps(q.TenantID, "")
	var notes []domain.Note
	var receipts []domain.ReceiptState
	err = d.Reader(ctx, func(txCtx context.Context) error {
		jobs, err := d.Jobs.BilledTo(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			for _, n := range j.State().Notes {
				if n.DebtorPartyID == q.DebtorPartyID {
					notes = append(notes, n)
				}
			}
		}
		rs, _, err := d.DebtorReceipts.OfDebtor(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.DebtorPartyID)
		for _, r := range rs {
			receipts = append(receipts, r.State())
		}
		return err
	})
	if err != nil {
		return api.DebtorStatement{}, classify(err)
	}
	stated, err := domain.Statement(notes, receipts, from, to)
	if err != nil {
		return api.DebtorStatement{}, classify(err)
	}
	out := api.DebtorStatement{LegalEntityID: q.LegalEntityID, DebtorPartyID: q.DebtorPartyID, From: dateView(from), To: dateView(to),
		Currencies: []api.CurrencyStatement{}}
	for _, s := range stated {
		money := func(v int64) kernel.Money { return kernel.Money{Currency: s.Currency, Minor: v} }
		c := api.CurrencyStatement{Currency: s.Currency, Opening: money(s.Opening), Closing: money(s.Closing), Ageing: api.StatementAgeing{
			NotDue: money(s.Ageing.NotDue), Days1To30: money(s.Ageing.Days1To30), Days31To60: money(s.Ageing.Days31To60),
			Days61To90: money(s.Ageing.Days61To90), Over90: money(s.Ageing.Over90)}}
		for _, e := range s.Entries {
			c.Entries = append(c.Entries, api.StatementEntry{Date: dateView(e.Date), Kind: e.Kind, ID: e.ID, Number: e.Number, DocumentNumber: e.DocumentNumber,
				Amount: money(e.Amount), Balance: money(e.Balance)})
		}
		for _, i := range s.OpenItems {
			c.OpenItems = append(c.OpenItems, api.StatementItem{InvoiceID: i.InvoiceID, Number: i.Number, DocumentNumber: i.DocumentNumber,
				InvoiceDate: dateView(i.InvoiceDate), DueDate: dateView(i.DueDate), Total: money(i.Total), Credited: money(i.Credited),
				Settled: money(i.Settled), Open: money(i.Open), DaysOverdue: i.DaysOverdue})
		}
		for _, r := range s.OnAccount {
			c.OnAccount = append(c.OnAccount, api.StatementReceipt{ReceiptID: r.ReceiptID, ReceivedOn: dateView(r.ReceivedOn), EvidenceSource: r.EvidenceSource,
				EvidenceRef: r.EvidenceRef, Credited: money(r.Credited), Unallocated: money(r.Unallocated)})
		}
		out.Currencies = append(out.Currencies, c)
	}
	return out, nil
}
