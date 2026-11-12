package fin

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.5 (FB4): creditor invoices, cost variance approval, the release of accruals and the closing of pools.

func (m *Module) SetCostVarianceThreshold(ctx context.Context, cmd api.SetCostVarianceThreshold) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).SetVarianceThreshold(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionSetCostVarianceThreshold, cmd.LegalEntityID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Amount, cmd.RateBP)
	return receipt(r, err)
}

func (m *Module) RecordCreditorInvoice(ctx context.Context, cmd api.RecordCreditorInvoice) (api.Receipt, error) {
	dated, err := domain.ParseDate(cmd.InvoiceDate)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	in := domain.CreditorInvoiceInput{ID: cmd.InvoiceID, LegalEntityID: cmd.LegalEntityID, CreditorPartyID: cmd.CreditorPartyID,
		SupplierInvoiceNo: cmd.SupplierInvoiceNo, InvoiceDate: dated, Currency: cmd.Currency, TaxDocumentKind: cmd.TaxDocumentKind,
		TaxDocumentNumber: cmd.TaxDocumentNumber}
	for _, l := range cmd.Lines {
		in.Lines = append(in.Lines, domain.CreditorLineInput{ChargeCode: l.ChargeCode, Description: l.Description, SupplierRef: l.SupplierRef, Amount: l.Amount})
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordCreditorInvoice(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionRecordCreditorInvoice, cmd.InvoiceID, 0, cmd.OccurredAt, cmd), in)
	return receipt(r, err)
}

func (m *Module) MatchCreditorInvoice(ctx context.Context, cmd api.MatchCreditorInvoice) (api.Receipt, error) {
	items := make([]domain.MatchItem, 0, len(cmd.Matches))
	for _, x := range cmd.Matches {
		items = append(items, domain.MatchItem{LineNo: x.LineNo, Target: domain.TargetKey{Kind: x.Target.Kind, ID: x.Target.ID, LineNo: x.Target.LineNo},
			Amount: x.Amount, Final: x.Final, Reason: x.Reason})
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).MatchCreditorInvoice(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionMatchCreditorInvoice, cmd.InvoiceID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), items)
	return receipt(r, err)
}

func (m *Module) ApproveCostVariance(ctx context.Context, cmd api.ApproveCostVariance) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ApproveCostVariance(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionApproveCostVariance, cmd.InvoiceID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) DisputeCreditorInvoice(ctx context.Context, cmd api.DisputeCreditorInvoice) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).DisputeCreditorInvoice(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionDisputeCreditorInvoice, cmd.InvoiceID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) ResolveCreditorInvoiceDispute(ctx context.Context, cmd api.ResolveCreditorInvoiceDispute) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ResolveCreditorInvoiceDispute(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionResolveCreditorInvoiceDispute, cmd.InvoiceID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) CancelCreditorInvoice(ctx context.Context, cmd api.CancelCreditorInvoice) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).CancelCreditorInvoice(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionCancelCreditorInvoice, cmd.InvoiceID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func (m *Module) ReleaseJobAccrual(ctx context.Context, cmd api.ReleaseJobAccrual) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ReleaseJobAccrual(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionReleaseJobAccrual,
		cmd.JobID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.LineNo, cmd.ExpectedSideVersion, cmd.Reason)
	return receipt(r, err)
}

func (m *Module) ReleasePoolAccrual(ctx context.Context, cmd api.ReleasePoolAccrual) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ReleasePoolAccrual(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionReleasePoolAccrual,
		cmd.PoolID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.LineNo, cmd.ExpectedVersion, cmd.Reason)
	return receipt(r, err)
}

func (m *Module) CloseConsolCostPool(ctx context.Context, cmd api.CloseConsolCostPool) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ClosePool(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionCloseConsolCostPool,
		cmd.PoolID, cmd.ExpectedRevision, cmd.OccurredAt, cmd))
	return receipt(r, err)
}

func (m *Module) ReopenConsolCostPool(ctx context.Context, cmd api.ReopenConsolCostPool) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ReopenPool(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionReopenConsolCostPool,
		cmd.PoolID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.Reason)
	return receipt(r, err)
}

func accrualRef(k domain.TargetKey) api.AccrualRef {
	return api.AccrualRef{Kind: k.Kind, ID: k.ID, LineNo: k.LineNo}
}

func creditorView(c *domain.CreditorInvoice, revision int64, functional kernel.CurrencyCode) api.CreditorInvoice {
	st := c.State()
	money := func(n int64) kernel.Money { return kernel.Money{Currency: st.Currency, Minor: n} }
	local := func(n int64) kernel.Money { return kernel.Money{Currency: functional, Minor: n} }
	out := api.CreditorInvoice{TenantID: string(st.TenantID), InvoiceID: st.InvoiceID, Revision: revision, LegalEntityID: st.LegalEntityID,
		CreditorPartyID: st.CreditorPartyID, CreditorRevision: st.CreditorRevision, SupplierInvoiceNo: st.SupplierInvoiceNo,
		InvoiceDate: st.InvoiceDate.Format(time.DateOnly), Currency: st.Currency, TaxDocumentKind: st.TaxDocumentKind, TaxDocumentNumber: st.TaxDocumentNumber,
		Total: money(st.Total()), State: st.State, RecordedBy: st.RecordedBy, RecordedAt: st.RecordedAt, MatchedBy: st.MatchedBy, MatchedAt: st.MatchedAt,
		ApprovalRequired: st.ApprovalRequired, ThresholdVersion: st.ThresholdNo, AwaitingApproval: st.AwaitingApproval(), ApprovedBy: st.ApprovedBy,
		ApprovalReason: st.ApprovalReason, SelfApproved: st.SelfApproved, ApprovedAt: st.ApprovedAt, CancelReason: st.CancelReason,
		CancelledBy: st.CancelledBy, CancelledAt: st.CancelledAt}
	for _, l := range st.Lines {
		out.Lines = append(out.Lines, api.CreditorInvoiceLine{ChargeCode: l.ChargeCode, Description: l.Description, SupplierRef: l.SupplierRef, Amount: l.Amount})
	}
	for _, x := range st.Matches {
		out.Matches = append(out.Matches, api.CreditorInvoiceMatch{MatchNo: x.No, LineNo: x.LineNo, Target: accrualRef(x.Target), TargetVersion: x.TargetVersion,
			Amount: money(x.Amount), Final: x.Final, Reason: x.Reason, Local: local(x.Local), FX: fxView(x.FX), Accrual: money(x.AccrualAmount),
			AccrualLocal: local(x.AccrualLocal), Prior: money(x.PriorAmount), PriorLocal: local(x.PriorLocal), Variance: money(x.Variance),
			VarianceLocal: local(x.VarianceLocal)})
	}
	for _, d := range st.Disputes {
		out.Disputes = append(out.Disputes, api.CreditorInvoiceDispute{DisputeNo: d.No, FromState: d.FromState, Reason: d.Reason, By: d.By, At: d.At,
			ResolvedBy: d.ResolvedBy, ResolveReason: d.ResolveReason, ResolvedAt: d.ResolvedAt})
	}
	return out
}

// functional reads a legal entity's functional currency.
func (m *Module) functional(ctx context.Context, d application.Deps, tenant, legalEntity string) (kernel.CurrencyCode, error) {
	p, _, err := d.Policies.Read(ctx, domain.TenantID(tenant), legalEntity)
	if err != nil {
		return "", err
	}
	return p.State().FunctionalCurrency, nil
}

func (m *Module) GetCreditorInvoice(ctx context.Context, q api.GetCreditorInvoice) (api.CreditorInvoice, error) {
	d := m.deps(q.TenantID, "")
	var out api.CreditorInvoice
	err := d.Reader(ctx, func(txCtx context.Context) error {
		c, revision, err := d.Creditors.Read(txCtx, domain.TenantID(q.TenantID), q.InvoiceID)
		if err != nil {
			return err
		}
		functional, err := m.functional(txCtx, d, q.TenantID, c.State().LegalEntityID)
		out = creditorView(c, revision, functional)
		return err
	})
	if err != nil {
		return api.CreditorInvoice{}, classify(err)
	}
	return out, nil
}

func (m *Module) ListCreditorInvoices(ctx context.Context, q api.ListCreditorInvoices) ([]api.CreditorInvoice, error) {
	d := m.deps(q.TenantID, "")
	out := []api.CreditorInvoice{}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		invoices, revisions, err := d.Creditors.List(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.CreditorPartyID)
		if err != nil || len(invoices) == 0 {
			return err
		}
		functional, err := m.functional(txCtx, d, q.TenantID, q.LegalEntityID)
		if err != nil {
			return err
		}
		for i, c := range invoices {
			if !q.AwaitingApproval || c.State().AwaitingApproval() {
				out = append(out, creditorView(c, revisions[i], functional))
			}
		}
		return nil
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

func (m *Module) ListOpenAccruals(ctx context.Context, q api.ListOpenAccruals) ([]api.OpenAccrual, error) {
	d := m.deps(q.TenantID, "")
	out := []api.OpenAccrual{}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		accruals, err := d.Creditors.OpenAccruals(txCtx, domain.TenantID(q.TenantID), q.LegalEntityID, q.CreditorPartyID)
		if err != nil || len(accruals) == 0 {
			return err
		}
		functional, err := m.functional(txCtx, d, q.TenantID, q.LegalEntityID)
		if err != nil {
			return err
		}
		local := func(n int64) kernel.Money { return kernel.Money{Currency: functional, Minor: n} }
		for _, a := range accruals {
			money := func(n int64) kernel.Money { return kernel.Money{Currency: a.Amount.Currency, Minor: n} }
			open := domain.Open(a.Amount.Minor, a.Status, a.Consumed)
			_, cost := domain.CurrentCost(a.Amount.Minor, a.Local, a.Status, a.Consumed)
			out = append(out, api.OpenAccrual{Target: accrualRef(a.Key()), Reference: a.Reference, ChargeCode: a.ChargeCode, SupplierRef: a.SupplierRef,
				CreditorPartyID: a.CreditorPartyID, Version: a.Version, Status: domain.AccrualStatus(a.Status, a.Consumed), Amount: a.Amount,
				Local: local(a.Local), Matched: money(a.Consumed.Amount), MatchedLocal: local(a.Consumed.Local), Open: money(open),
				OpenLocal: local(cost - a.Consumed.Local)})
		}
		return nil
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// readJob reads a job with what creditor invoices consume of its cost sides.
func readJob(ctx context.Context, d application.Deps, tenant, id string) (*domain.FreightJob, int64, error) {
	j, revision, err := d.Jobs.Read(ctx, domain.TenantID(tenant), id)
	if err != nil {
		return nil, 0, err
	}
	consumed, err := d.Creditors.Consumed(ctx, domain.TenantID(tenant), domain.TargetJobCost, id)
	j.Matched(consumed)
	return j, revision, err
}

// withMatched sets on jobs what creditor invoices consume of their cost sides.
func withMatched(ctx context.Context, d application.Deps, tenant string, jobs []*domain.FreightJob) error {
	for _, j := range jobs {
		consumed, err := d.Creditors.Consumed(ctx, domain.TenantID(tenant), domain.TargetJobCost, j.State().JobID)
		if err != nil {
			return err
		}
		j.Matched(consumed)
	}
	return nil
}

// readPool reads a pool with what creditor invoices consume of its lines.
func readPool(ctx context.Context, d application.Deps, tenant, id string) (*domain.ConsolCostPool, int64, error) {
	p, revision, err := d.Pools.Read(ctx, domain.TenantID(tenant), id)
	if err != nil {
		return nil, 0, err
	}
	consumed, err := d.Creditors.Consumed(ctx, domain.TenantID(tenant), domain.TargetPoolCost, id)
	p.Matched(consumed)
	return p, revision, err
}
