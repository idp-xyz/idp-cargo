package fin

import (
	"context"
	"fmt"
	"slices"
	"strings"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.4 (FB3): consol cost pools and apportionment.

func (m *Module) OpenConsolCostPool(ctx context.Context, cmd api.OpenConsolCostPool) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).OpenCostPool(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionOpenConsolCostPool,
		cmd.PoolID, 0, cmd.OccurredAt, cmd), cmd.ConsolidationID)
	return receipt(r, err)
}

func (m *Module) RecordConsolCost(ctx context.Context, cmd api.RecordConsolCost) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RecordPoolCost(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionRecordConsolCost,
		cmd.PoolID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), application.PoolCostAsk{ChargeCode: cmd.ChargeCode, Description: cmd.Description,
		SupplierRef: cmd.SupplierRef, Method: cmd.Method, Cost: domain.SideInput{PartyID: cmd.Cost.PartyID, Amount: cmd.Cost.Amount}, Reason: cmd.Reason})
	return receipt(r, err)
}

func (m *Module) ReviseConsolCost(ctx context.Context, cmd api.ReviseConsolCost) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).RevisePoolCost(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID, api.ActionReviseConsolCost,
		cmd.PoolID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), application.RevisePoolCostAsk{LineNo: cmd.LineNo, Expected: cmd.ExpectedVersion,
		Value: domain.SideInput{PartyID: cmd.Value.PartyID, Amount: cmd.Value.Amount}, Reason: cmd.Reason})
	return receipt(r, err)
}

func (m *Module) PreviewConsolCostApportionment(ctx context.Context, cmd api.PreviewConsolCostApportionment) (api.Receipt, error) {
	manual := make([]domain.ManualShare, 0, len(cmd.Manual))
	for _, s := range cmd.Manual {
		manual = append(manual, domain.ManualShare{ShipmentID: s.ShipmentID, Amount: s.Amount})
	}
	attributions := make([]domain.AttributionInput, 0, len(cmd.Attributions))
	for _, a := range cmd.Attributions {
		attributions = append(attributions, domain.AttributionInput{LineNo: a.LineNo, ShipmentID: a.ShipmentID, Loss: a.Loss, Exception: a.Exception,
			Party: a.Party, PartyReference: a.PartyReference, Voucher: a.Voucher, Reason: a.Reason})
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).PreviewPoolApportionment(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionPreviewConsolCostApportionment, cmd.PoolID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		application.ApportionAsk{LineNos: append([]int(nil), cmd.LineNos...), Method: cmd.Method, Manual: manual,
			NotUsed: append([]string(nil), cmd.NotUsed...), AllHouses: cmd.AllHouses, Reason: cmd.Reason, Attributions: attributions})
	return receipt(r, err)
}

func (m *Module) SubmitConsolCostApportionment(ctx context.Context, cmd api.SubmitConsolCostApportionment) (api.Receipt, error) {
	r, err := m.deps(cmd.TenantID, cmd.CommandID).SubmitPoolApportionment(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionSubmitConsolCostApportionment, cmd.PoolID, cmd.ExpectedRevision, cmd.OccurredAt, cmd), cmd.PreviewNo)
	return receipt(r, err)
}

func poolView(p *domain.ConsolCostPool, revision int64) api.ConsolCostPool {
	st := p.State()
	local := func(n int64) kernel.Money { return kernel.Money{Currency: st.FunctionalCurrency, Minor: n} }
	out := api.ConsolCostPool{TenantID: string(st.TenantID), PoolID: st.PoolID, Revision: revision, ConsolidationID: st.ConsolidationID,
		LegalEntityID: st.LegalEntityID, Mode: st.Mode, FunctionalCurrency: st.FunctionalCurrency, BookCode: st.BookCode, State: st.State,
		OpenedBy: st.OpenedBy, OpenedAt: st.OpenedAt}
	for _, c := range st.Closures {
		out.Closures = append(out.Closures, api.PoolClosure{ClosureNo: c.No, ClosedBy: c.By, ClosedAt: c.At, ReopenedBy: c.ReopenedBy,
			ReopenReason: c.ReopenReason, ReopenedAt: c.ReopenedAt, SettingsVersion: c.SettingsVersion})
	}
	for _, l := range st.Lines {
		currency := l.Versions[len(l.Versions)-1].Amount.Currency
		c := p.Consumed(l.No)
		cost, costLocal := p.LineCost(l.No)
		source := l.Source
		if source == "" {
			source = domain.SourceManual
		}
		line := api.PoolCostLine{LineNo: l.No, ChargeCode: l.ChargeCode, Description: l.Description, SupplierRef: l.SupplierRef, Method: l.Method,
			Source: source, BasisID: l.BasisID, BasisLineNo: l.BasisLineNo,
			RecordedAt: l.At, EnteredAt: l.EnteredAt, Status: p.LineStatus(l.No), Matched: kernel.Money{Currency: currency, Minor: c.Amount},
			MatchedLocal: local(c.Local), Cost: kernel.Money{Currency: currency, Minor: cost}, CostLocal: local(costLocal)}
		for _, v := range l.Versions {
			line.Versions = append(line.Versions, api.PoolCostVersion{Version: v.No, CreditorPartyID: v.CreditorPartyID,
				CreditorRevision: v.CreditorRevision, Amount: v.Amount, Local: local(v.Local), Status: v.Status, Reason: v.Reason, RecordedAt: v.At,
				EnteredAt: v.EnteredAt})
		}
		out.Lines = append(out.Lines, line)
	}
	linesOf := func(lines []domain.LineApportionment) (out []api.ApportionedLine) {
		for _, la := range lines {
			currency := st.Lines[la.LineNo-1].Versions[la.LineVersion-1].Amount.Currency
			lv := api.ApportionedLine{LineNo: la.LineNo, LineVersion: la.LineVersion, Method: la.Method, Reason: la.Reason,
				Amount: kernel.Money{Currency: currency, Minor: la.Amount}, Local: local(la.Local),
				ToAllocate: kernel.Money{Currency: currency, Minor: la.ToAllocate}, ToAllocateLocal: local(la.ToAllocateLocal)}
			for _, x := range la.Attributions {
				lv.Attributions = append(lv.Attributions, api.PoolAttribution{ShipmentID: x.ShipmentID, JobID: x.JobID,
					Loss: kernel.Money{Currency: currency, Minor: x.Loss}, LossLocal: local(x.LossLocal),
					Exception: kernel.Money{Currency: currency, Minor: x.Exception}, ExceptionLocal: local(x.ExceptionLocal), Party: x.Party,
					PartyReference: x.PartyReference, Voucher: x.Voucher, Reason: x.Reason, DepartedAt: x.DepartedAt, Carried: x.Carried})
			}
			for _, s := range la.Shares {
				lv.Shares = append(lv.Shares, api.PoolShare{ShareNo: s.No, ShipmentID: s.ShipmentID, JobID: s.JobID, NotUsed: !s.Used, Quantity: s.Quantity, Unit: s.Unit,
					Amount: kernel.Money{Currency: currency, Minor: s.Amount}, Local: local(s.Local)})
			}
			out = append(out, lv)
		}
		return out
	}
	submitted := map[int]int{}
	for _, a := range st.Apportionments {
		submitted[a.PreviewNo] = a.No
		out.Apportionments = append(out.Apportionments, api.PoolApportionment{ApportionmentNo: a.No, BasisRevision: a.BasisRevision, Stage: a.Stage, By: a.By,
			At: a.At, EnteredAt: a.EnteredAt, Lines: linesOf(a.Lines), PreviewNo: a.PreviewNo, SettingsVersion: a.SettingsVersion})
	}
	for _, v := range st.Previews {
		out.Previews = append(out.Previews, api.PoolPreview{PreviewNo: v.No, BasisRevision: v.BasisRevision, Stage: v.Stage, By: v.By, At: v.At, EnteredAt: v.EnteredAt,
			Lines: linesOf(v.Lines), FingerprintSHA256: v.FingerprintSHA256, SubmittedAs: submitted[v.No], SettingsVersion: v.SettingsVersion})
	}
	return out
}

// workUnits asks PHY, outside FIN's transactions, which containers of the consolidation are in rework: the pool is in
// work while there is one. It answers PHY's basis too, which tells the houses that left (the former shares).
func (m *Module) workUnits(ctx context.Context, d application.Deps, tenant, consolidation string) (basis domain.CostBasis, units []api.WorkUnit, err error) {
	if d.Physical == nil {
		return domain.CostBasis{}, nil, nil
	}
	basis, err = d.Physical.CostBasis(ctx, domain.TenantID(tenant), consolidation)
	if err != nil {
		return domain.CostBasis{}, nil, err
	}
	for _, no := range basis.UnitsInRework() {
		unit := api.WorkUnit{UnitNo: no}
		for _, l := range basis.PackingLists {
			if l.UnitNo == no {
				unit.ExecutionID = l.ExecutionID
			}
		}
		units = append(units, unit)
	}
	return basis, units, nil
}

// lineAttributions says, for each line of the pool apportioned, what its latest apportionment leaves out of the shares:
// the losses, the exception costs by party (those whose party is PENDING apart), the amount to be allocated and the
// former shares pending finance's confirmation, which PHY's basis tells.
func lineAttributions(p *domain.ConsolCostPool, basis domain.CostBasis) []api.LineAttributions {
	st := p.State()
	local := func(n int64) kernel.Money { return kernel.Money{Currency: st.FunctionalCurrency, Minor: n} }
	var out []api.LineAttributions
	for _, l := range st.Lines {
		_, la, ok := p.SharesOfLine(l.No)
		if !ok {
			continue
		}
		currency := l.Versions[la.LineVersion-1].Amount.Currency
		money := func(n int64) kernel.Money { return kernel.Money{Currency: currency, Minor: n} }
		v := api.LineAttributions{LineNo: l.No, ToAllocate: money(la.ToAllocate), ToAllocateLocal: local(la.ToAllocateLocal), ExceptionPending: money(0),
			ExceptionPendingLocal: local(0)}
		byParty := map[string][2]int64{}
		for _, x := range la.Attributions {
			if x.Loss > 0 {
				v.Losses = append(v.Losses, api.HouseLoss{ShipmentID: x.ShipmentID, JobID: x.JobID, Amount: money(x.Loss), Local: local(x.LossLocal)})
			}
			if x.Exception > 0 {
				sum := byParty[x.Party]
				byParty[x.Party] = [2]int64{sum[0] + x.Exception, sum[1] + x.ExceptionLocal}
			}
		}
		for _, party := range []string{domain.PartyCarrier, domain.PartyWarehouse, domain.PartyOwnOperations} {
			if sum, ok := byParty[party]; ok {
				v.Exceptions = append(v.Exceptions, api.ExceptionByParty{Party: party, Amount: money(sum[0]), Local: local(sum[1])})
			}
		}
		pending := byParty[domain.PartyPending]
		v.ExceptionPending, v.ExceptionPendingLocal = money(pending[0]), local(pending[1])
		for _, f := range p.FormerShares(basis) {
			if f.LineNo == l.No {
				v.FormerShares = append(v.FormerShares, api.FormerShare{ShipmentID: f.Share.ShipmentID, JobID: f.Share.JobID, Amount: money(f.Share.Amount),
					Local: local(f.Share.Local), End: f.House.End, EndedAt: f.House.EndedAt})
			}
		}
		out = append(out, v)
	}
	return out
}

func (m *Module) GetConsolCostPool(ctx context.Context, q api.GetConsolCostPool) (api.ConsolCostPool, error) {
	d := m.deps(q.TenantID, "")
	var p *domain.ConsolCostPool
	var revision int64
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		p, revision, err = readPool(txCtx, d, q.TenantID, q.PoolID)
		return err
	})
	if err != nil {
		return api.ConsolCostPool{}, classify(err)
	}
	out := poolView(p, revision)
	var basis domain.CostBasis
	if basis, out.WorkUnits, err = m.workUnits(ctx, d, q.TenantID, out.ConsolidationID); err != nil {
		return api.ConsolCostPool{}, classify(err)
	}
	out.Attributions = lineAttributions(p, basis)
	out.InWork = len(out.WorkUnits) > 0
	return out, nil
}

// GetConsolProfit reads a consolidation's economics through its pool: the pooled cost, its lines' current costs (what
// creditor invoices matched plus what is still accrued), the part the latest apportionments cover, and the house jobs
// of those apportionments with their own economics and their pool shares. Since V2.93 the houses' losses, the exception
// costs, what is still to be allocated and the former shares pending finance's confirmation are told apart, and the
// costs are conserved: the former shares are in no house's cost, a loss is in its house's. PHY's basis tells which
// houses left; the query fails when PHY does not answer.
func (m *Module) GetConsolProfit(ctx context.Context, q api.GetConsolProfit) (api.ConsolProfit, error) {
	d := m.deps(q.TenantID, "")
	var pool *domain.ConsolCostPool
	var rules domain.RuleSettings
	err := d.Reader(ctx, func(txCtx context.Context) error {
		pools, _, err := d.Pools.ForConsolidation(txCtx, domain.TenantID(q.TenantID), q.ConsolidationID)
		if err != nil {
			return err
		}
		if len(pools) == 0 {
			return fmt.Errorf("%w: consolidation %s has no cost pool", bentorepo.ErrNotFound, q.ConsolidationID)
		}
		pool = pools[0]
		consumed, err := d.Creditors.Consumed(txCtx, domain.TenantID(q.TenantID), domain.TargetPoolCost, pool.State().PoolID)
		if err != nil {
			return err
		}
		pool.Matched(consumed)
		// the settings are read in the snapshot of the pool, so that the profit is told on one state of both
		rules, err = d.RuleSettingsOf(txCtx, domain.TenantID(q.TenantID), pool.State().LegalEntityID)
		return err
	})
	if err != nil {
		return api.ConsolProfit{}, classify(err)
	}
	basis, workUnits, err := m.workUnits(ctx, d, q.TenantID, q.ConsolidationID)
	if err != nil {
		return api.ConsolProfit{}, classify(err)
	}
	var out api.ConsolProfit
	err = d.Reader(ctx, func(txCtx context.Context) error {
		st := pool.State()
		local := func(n int64) kernel.Money { return kernel.Money{Currency: st.FunctionalCurrency, Minor: n} }
		out = api.ConsolProfit{ConsolidationID: q.ConsolidationID, PoolID: st.PoolID, LegalEntityID: st.LegalEntityID, State: st.State,
			ProfitBasis: profitBasis(rules), SettingsVersion: rules.Version}
		var pooled, matched, apportioned, retained, losses, exceptions, pending, toAllocated, former int64
		type house struct {
			jobID   string
			pool    int64
			pending int64 // a former share pending, counted in pool under F6 INCLUDED_MARKED
		}
		byShipment := map[string]*house{}
		for _, l := range st.Lines {
			_, cost := pool.LineCost(l.No)
			pooled += cost
			matched += pool.Consumed(l.No).Local
			_, la, ok := pool.SharesOfLine(l.No)
			if !ok {
				continue
			}
			taken := int64(0)
			for _, x := range la.Attributions {
				if byShipment[x.ShipmentID] == nil {
					byShipment[x.ShipmentID] = &house{jobID: x.JobID}
				}
				byShipment[x.ShipmentID].pool += x.LossLocal // a loss is in its house's cost
				losses += x.LossLocal
				exceptions += x.ExceptionLocal
				if x.Party == domain.PartyPending {
					pending += x.ExceptionLocal
				}
				taken += x.LossLocal + x.ExceptionLocal
			}
			toAllocated += la.ToAllocateLocal
			if la.Method == domain.MethodRetained {
				retained += la.Local - taken
				continue
			}
			for _, s := range la.Shares {
				if byShipment[s.ShipmentID] == nil {
					byShipment[s.ShipmentID] = &house{jobID: s.JobID} // a house the lines name is listed, bearing cost or not
				}
				if _, left := basis.Former(s.ShipmentID); left && s.Used && s.Amount > 0 {
					former += s.Local // pending finance's confirmation: in no house's cost, unless F6 is INCLUDED_MARKED
					if rules.F6 == domain.F6IncludedMarked {
						byShipment[s.ShipmentID].pool += s.Local
						byShipment[s.ShipmentID].pending += s.Local
					}
					continue
				}
				if s.Used { // a house left out carries nothing of the line
					apportioned += s.Local
					byShipment[s.ShipmentID].pool += s.Local
				}
			}
		}
		out.Pooled, out.Matched, out.Apportioned, out.Retained = local(pooled), local(matched), local(apportioned), local(retained)
		out.Losses, out.Exceptions, out.ExceptionsPending, out.ToAllocated, out.FormerSharesPending = local(losses), local(exceptions), local(pending),
			local(toAllocated), local(former)
		out.Unapportioned = local(pooled - apportioned - retained - losses - exceptions - toAllocated - former)
		out.Lines = lineAttributions(pool, basis)
		shipments := make([]string, 0, len(byShipment))
		for s := range byShipment {
			shipments = append(shipments, s)
		}
		slices.Sort(shipments)
		for _, shipment := range shipments {
			h := byShipment[shipment]
			j, _, err := readJob(txCtx, d, q.TenantID, h.jobID)
			if err != nil {
				return err
			}
			p := j.Profit()
			out.Houses = append(out.Houses, api.HouseProfit{ShipmentID: shipment, JobID: h.jobID, Revenue: local(p.Revenue), Invoiced: local(p.Invoiced),
				Cost: local(p.Cost), PoolCost: local(h.pool), Profit: local(p.Revenue - p.Cost - h.pool), PendingInProfit: local(h.pending)})
		}
		return nil
	})
	if err != nil {
		return api.ConsolProfit{}, classify(err)
	}
	out.WorkUnits = workUnits
	out.InWork = len(out.WorkUnits) > 0
	return out, nil
}

// profitBasis names the basis a profit view is told on, from F6: SEPARATE or INCLUDED_MARKED.
func profitBasis(rules domain.RuleSettings) string {
	if rules.F6 == domain.F6IncludedMarked {
		return domain.F6IncludedMarked
	}
	return "SEPARATE"
}

// poolShareViews shows a job's pool shares, by pool and line; pending marks former shares awaiting finance's
// confirmation.
func poolShareViews(shares []application.PoolShare, currency kernel.CurrencyCode, pending bool) ([]api.JobPoolShare, int64) {
	var out []api.JobPoolShare
	var total int64
	for _, s := range shares {
		out = append(out, api.JobPoolShare{PoolID: s.PoolID, LineNo: s.LineNo, LineVersion: s.LineVersion, ChargeCode: s.ChargeCode, Method: s.Method,
			Stage: s.Stage, Quantity: s.Quantity, Unit: s.Unit, Amount: s.Amount, Local: kernel.Money{Currency: currency, Minor: s.Local}, Pending: pending})
		total += s.Local
	}
	slices.SortStableFunc(out, func(a, b api.JobPoolShare) int {
		if c := strings.Compare(a.PoolID, b.PoolID); c != 0 {
			return c
		}
		return a.LineNo - b.LineNo
	})
	return out, total
}
