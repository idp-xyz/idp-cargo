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

func (m *Module) ApportionConsolCosts(ctx context.Context, cmd api.ApportionConsolCosts) (api.Receipt, error) {
	manual := make([]domain.ManualShare, 0, len(cmd.Manual))
	for _, s := range cmd.Manual {
		manual = append(manual, domain.ManualShare{ShipmentID: s.ShipmentID, Amount: s.Amount})
	}
	r, err := m.deps(cmd.TenantID, cmd.CommandID).ApportionPoolCosts(ctx, m.command(cmd.CommandID, cmd.TenantID, cmd.ActorID,
		api.ActionApportionConsolCosts, cmd.PoolID, cmd.ExpectedRevision, cmd.OccurredAt, cmd),
		application.ApportionAsk{LineNos: append([]int(nil), cmd.LineNos...), Method: cmd.Method, Manual: manual, Reason: cmd.Reason})
	return receipt(r, err)
}

func poolView(p *domain.ConsolCostPool, revision int64) api.ConsolCostPool {
	st := p.State()
	local := func(n int64) kernel.Money { return kernel.Money{Currency: st.FunctionalCurrency, Minor: n} }
	out := api.ConsolCostPool{TenantID: string(st.TenantID), PoolID: st.PoolID, Revision: revision, ConsolidationID: st.ConsolidationID,
		LegalEntityID: st.LegalEntityID, Mode: st.Mode, FunctionalCurrency: st.FunctionalCurrency, BookCode: st.BookCode, State: st.State,
		OpenedBy: st.OpenedBy, OpenedAt: st.OpenedAt}
	for _, l := range st.Lines {
		line := api.PoolCostLine{LineNo: l.No, ChargeCode: l.ChargeCode, Description: l.Description, SupplierRef: l.SupplierRef, Method: l.Method,
			RecordedAt: l.At, EnteredAt: l.EnteredAt}
		for _, v := range l.Versions {
			line.Versions = append(line.Versions, api.PoolCostVersion{Version: v.No, CreditorPartyID: v.CreditorPartyID,
				CreditorRevision: v.CreditorRevision, Amount: v.Amount, Local: local(v.Local), Status: v.Status, Reason: v.Reason, RecordedAt: v.At,
				EnteredAt: v.EnteredAt})
		}
		out.Lines = append(out.Lines, line)
	}
	for _, a := range st.Apportionments {
		av := api.PoolApportionment{ApportionmentNo: a.No, BasisRevision: a.BasisRevision, Stage: a.Stage, By: a.By, At: a.At, EnteredAt: a.EnteredAt}
		for _, la := range a.Lines {
			lv := api.ApportionedLine{LineNo: la.LineNo, LineVersion: la.LineVersion, Method: la.Method, Reason: la.Reason}
			currency := st.Lines[la.LineNo-1].Versions[la.LineVersion-1].Amount.Currency
			for _, s := range la.Shares {
				lv.Shares = append(lv.Shares, api.PoolShare{ShareNo: s.No, ShipmentID: s.ShipmentID, JobID: s.JobID, Quantity: s.Quantity, Unit: s.Unit,
					Amount: kernel.Money{Currency: currency, Minor: s.Amount}, Local: local(s.Local)})
			}
			av.Lines = append(av.Lines, lv)
		}
		out.Apportionments = append(out.Apportionments, av)
	}
	return out
}

func (m *Module) GetConsolCostPool(ctx context.Context, q api.GetConsolCostPool) (api.ConsolCostPool, error) {
	d := m.deps(q.TenantID, "")
	var p *domain.ConsolCostPool
	var revision int64
	err := d.Reader(ctx, func(txCtx context.Context) (err error) {
		p, revision, err = d.Pools.Read(txCtx, domain.TenantID(q.TenantID), q.PoolID)
		return err
	})
	if err != nil {
		return api.ConsolCostPool{}, classify(err)
	}
	return poolView(p, revision), nil
}

// GetConsolProfit reads a consolidation's economics through its pool: the pooled cost at the latest versions, the
// part the latest apportionments cover, and the house jobs of those apportionments with their own economics and their
// pool shares.
func (m *Module) GetConsolProfit(ctx context.Context, q api.GetConsolProfit) (api.ConsolProfit, error) {
	d := m.deps(q.TenantID, "")
	var out api.ConsolProfit
	err := d.Reader(ctx, func(txCtx context.Context) error {
		pools, _, err := d.Pools.ForConsolidation(txCtx, domain.TenantID(q.TenantID), q.ConsolidationID)
		if err != nil {
			return err
		}
		if len(pools) == 0 {
			return fmt.Errorf("%w: consolidation %s has no cost pool", bentorepo.ErrNotFound, q.ConsolidationID)
		}
		st := pools[0].State()
		local := func(n int64) kernel.Money { return kernel.Money{Currency: st.FunctionalCurrency, Minor: n} }
		out = api.ConsolProfit{ConsolidationID: q.ConsolidationID, PoolID: st.PoolID, LegalEntityID: st.LegalEntityID}
		var pooled, apportioned int64
		type house struct {
			jobID string
			pool  int64
		}
		byShipment := map[string]*house{}
		for _, l := range st.Lines {
			pooled += l.Versions[len(l.Versions)-1].Local
			_, la, ok := pools[0].SharesOfLine(l.No)
			if !ok {
				continue
			}
			for _, s := range la.Shares {
				apportioned += s.Local
				if byShipment[s.ShipmentID] == nil {
					byShipment[s.ShipmentID] = &house{jobID: s.JobID}
				}
				byShipment[s.ShipmentID].pool += s.Local
			}
		}
		out.Pooled, out.Apportioned, out.Unapportioned = local(pooled), local(apportioned), local(pooled-apportioned)
		shipments := make([]string, 0, len(byShipment))
		for s := range byShipment {
			shipments = append(shipments, s)
		}
		slices.Sort(shipments)
		for _, shipment := range shipments {
			h := byShipment[shipment]
			j, _, err := d.Jobs.Read(txCtx, domain.TenantID(q.TenantID), h.jobID)
			if err != nil {
				return err
			}
			p := j.Profit()
			out.Houses = append(out.Houses, api.HouseProfit{ShipmentID: shipment, JobID: h.jobID, Revenue: local(p.Revenue), Invoiced: local(p.Invoiced),
				Cost: local(p.Cost), PoolCost: local(h.pool), Profit: local(p.Revenue - p.Cost - h.pool)})
		}
		return nil
	})
	if err != nil {
		return api.ConsolProfit{}, classify(err)
	}
	return out, nil
}

// poolShareViews shows a job's pool shares, by pool and line.
func poolShareViews(shares []application.PoolShare, currency kernel.CurrencyCode) ([]api.JobPoolShare, int64) {
	var out []api.JobPoolShare
	var total int64
	for _, s := range shares {
		out = append(out, api.JobPoolShare{PoolID: s.PoolID, LineNo: s.LineNo, LineVersion: s.LineVersion, ChargeCode: s.ChargeCode, Method: s.Method,
			Stage: s.Stage, Quantity: s.Quantity, Unit: s.Unit, Amount: s.Amount, Local: kernel.Money{Currency: currency, Minor: s.Local}})
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
