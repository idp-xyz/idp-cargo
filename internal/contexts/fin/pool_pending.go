package fin

import (
	"context"
	"fmt"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.94: what a cost pool still needs, and what a preview would change before it is submitted. Both are read-only:
// the pool is read in one snapshot, PHY outside it (as the other pool queries do since V2.91) and, for the preview's
// effect, the houses' open jobs in a second snapshot, without the locks a submission takes.

// shipmentsToRead are the shipments whose open jobs a submission's fingerprint reads: the live houses.
func shipmentsToRead(basis domain.CostBasis) []string {
	var out []string
	for _, h := range basis.Houses {
		out = append(out, h.ShipmentID)
	}
	return out
}

// GetConsolCostPoolPendingItems lists what the pool needs (domain.PendingItems): a container in rework, a former share
// not attributed, an amount to be allocated, an exception cost whose party is pending, a closed pool that holds a
// former share, and the lines the close would refuse as not apportioned or not on the current basis. It reads PHY's
// basis as GetConsolCostPool does, and fails when PHY does not answer: the list cannot be told, and an empty one would
// say that nothing is pending.
func (m *Module) GetConsolCostPoolPendingItems(ctx context.Context, q api.GetConsolCostPoolPendingItems) ([]api.PoolPendingItem, error) {
	d := m.deps(q.TenantID, "")
	if d.Physical == nil {
		return nil, classify(application.ErrMissingDependency)
	}
	var p *domain.ConsolCostPool
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		p, _, err = readPool(txCtx, d, q.TenantID, q.PoolID)
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	st := p.State()
	basis, err := d.Physical.CostBasis(ctx, domain.TenantID(q.TenantID), st.ConsolidationID)
	if err != nil {
		return nil, classify(err)
	}
	if st.State != domain.PoolClosed && p.ByRevenue() {
		var revenue map[string]map[string]int64
		err = d.Reader(ctx, func(txCtx context.Context) (err error) {
			_, revenue, err = d.ReadOpenJobs(txCtx, domain.TenantID(q.TenantID), st.LegalEntityID, shipmentsToRead(basis))
			return err
		})
		if err != nil {
			return nil, classify(err)
		}
		p.HouseRevenue(revenue)
	}
	out := []api.PoolPendingItem{}
	for _, it := range p.PendingItems(basis) {
		item := api.PoolPendingItem{Kind: it.Kind, LineNo: it.LineNo, ShipmentID: it.ShipmentID, UnitNo: it.UnitNo, Message: it.Message, Actor: it.Actor}
		if it.Currency != "" {
			item.Amount = kernel.Money{Currency: it.Currency, Minor: it.Amount}
			item.AmountLocal = kernel.Money{Currency: st.FunctionalCurrency, Minor: it.Local}
		}
		out = append(out, item)
	}
	return out, nil
}

// GetConsolCostPreviewEffect tells what submitting a stored preview would change (domain.PreviewEffect): per line the
// cost now and in the preview and whether the parts sum to it, per house and line what it bears now and what the
// preview gives it with the change to its job profit, the exception costs and the amount to be allocated before and
// after, the reasons, and whether the preview is still current. It needs no particular person and changes nothing.
func (m *Module) GetConsolCostPreviewEffect(ctx context.Context, q api.GetConsolCostPreviewEffect) (api.PreviewEffect, error) {
	d := m.deps(q.TenantID, "")
	if d.Physical == nil {
		return api.PreviewEffect{}, classify(application.ErrMissingDependency)
	}
	var p *domain.ConsolCostPool
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		p, _, err = readPool(txCtx, d, q.TenantID, q.PoolID)
		return err
	})
	if err != nil {
		return api.PreviewEffect{}, classify(err)
	}
	st := p.State()
	if q.PreviewNo < 1 || q.PreviewNo > len(st.Previews) {
		return api.PreviewEffect{}, classify(fmt.Errorf("%w: pool %s has no preview %d", bentorepo.ErrNotFound, q.PoolID, q.PreviewNo))
	}
	basis, err := d.Physical.CostBasis(ctx, domain.TenantID(q.TenantID), st.ConsolidationID)
	if err != nil {
		return api.PreviewEffect{}, classify(err)
	}
	in := domain.SubmitInput{Basis: basis}
	err = d.Reader(ctx, func(txCtx context.Context) (err error) {
		in.Jobs, in.Revenue, err = d.ReadOpenJobs(txCtx, domain.TenantID(q.TenantID), st.LegalEntityID, shipmentsToRead(basis))
		return err
	})
	if err != nil {
		return api.PreviewEffect{}, classify(err)
	}
	e, err := p.PreviewEffect(q.PreviewNo, in)
	if err != nil {
		return api.PreviewEffect{}, classify(err)
	}
	local := func(n int64) kernel.Money { return kernel.Money{Currency: st.FunctionalCurrency, Minor: n} }
	out := api.PreviewEffect{PreviewNo: e.PreviewNo, Stage: e.Stage, PreviewedBy: e.PreviewedBy, PreviewedAt: e.PreviewedAt, SubmittedAs: e.SubmittedAs,
		Current: e.Current, Changes: e.Changes, InWork: e.InWork, PoolClosed: e.PoolClosed, WayOut: e.WayOut}
	currency := map[int]kernel.CurrencyCode{}
	for _, l := range e.Lines {
		currency[l.LineNo] = l.Currency
		money := func(n int64) kernel.Money { return kernel.Money{Currency: l.Currency, Minor: n} }
		out.Lines = append(out.Lines, api.PreviewLineEffect{LineNo: l.LineNo, Method: l.Method, Reason: l.Reason, CostNow: money(l.CostNow), Cost: money(l.Cost),
			CostChanged: l.CostChanged, Shares: money(l.Shares), Losses: money(l.Losses), Exceptions: money(l.Exceptions), ToAllocate: money(l.ToAllocate),
			Retained: money(l.Retained), Conserved: l.Conserved, ToAllocateNow: money(l.ToAllocateNow)})
	}
	for _, h := range e.Houses {
		money := func(n int64) kernel.Money { return kernel.Money{Currency: currency[h.LineNo], Minor: n} }
		out.Houses = append(out.Houses, api.PreviewHouseEffect{LineNo: h.LineNo, ShipmentID: h.ShipmentID, JobID: h.JobID, Live: h.Live, Former: h.Former,
			NowKind: h.NowKind, Now: money(h.Now), NowLocal: local(h.NowLocal), PreviewKind: h.PreviewKind, Preview: money(h.Preview), PreviewLocal: local(h.PreviewLocal),
			Difference: money(h.Difference), DifferenceLocal: local(h.DifferenceLocal), ProfitChange: local(h.ProfitChange)})
	}
	for _, t := range e.Totals {
		out.HouseTotals = append(out.HouseTotals, api.PreviewHouseTotal{ShipmentID: t.ShipmentID, JobID: t.JobID, ProfitChange: local(t.ProfitChange)})
	}
	for _, x := range e.Exceptions {
		money := func(n int64) kernel.Money { return kernel.Money{Currency: x.Currency, Minor: n} }
		out.Exceptions = append(out.Exceptions, api.PreviewExceptionEffect{LineNo: x.LineNo, Party: x.Party, Before: money(x.Before), After: money(x.After),
			BeforeLocal: local(x.BeforeLocal), AfterLocal: local(x.AfterLocal)})
	}
	for _, r := range e.Reasons {
		out.Reasons = append(out.Reasons, api.PreviewReason{LineNo: r.LineNo, ShipmentID: r.ShipmentID, Former: r.Former, End: r.End, EndReason: r.EndReason,
			EndedBy: r.EndedBy, EndedAt: r.EndedAt, TransferredTo: r.TransferredTo, Attributed: r.Attributed, Party: r.Party, PartyReference: r.PartyReference,
			Voucher: r.Voucher, AttributionNote: r.AttributionNote, Carried: r.Carried})
	}
	return out, nil
}
