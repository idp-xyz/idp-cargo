package application

import (
	"context"
	"fmt"
	"slices"
	"strings"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// FIN V0.4 (FB3): consol cost pool commands. Each reads PHY's PhysicalCostBasis v1 and ORG outside FIN's
// transactions (T3), then changes the pool under its legal entity's policy in one transaction (T2).

// OpenCostPool opens the pool of a consolidation: PHY answers the consolidation and its operating legal entity,
// which must be VERIFIED in ORG and becomes the pool's. A second pool of the consolidation and legal entity is a
// conflict (UL-08).
func (d Deps) OpenCostPool(ctx context.Context, c Command, consolidation string) (Receipt, error) {
	if d.Pools == nil || d.Physical == nil || d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	basis, err := d.Physical.CostBasis(ctx, c.Scope.TenantID, consolidation)
	if err != nil {
		return Receipt{}, err
	}
	if err = d.verified(ctx, c.Scope.TenantID, basis.OperatingLegalEntityID); err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "ConsolCostPool", func(txCtx context.Context) (Receipt, aggregate, error) {
		p, err := d.policy(txCtx, c.Scope.TenantID, basis.OperatingLegalEntityID)
		if err != nil {
			return Receipt{}, nil, err
		}
		pool, err := fin.OpenPool(fin.PoolState{TenantID: c.Scope.TenantID, PoolID: c.Subject, ConsolidationID: consolidation,
			LegalEntityID: basis.OperatingLegalEntityID}, p, basis, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		pool.Stamp(entered)
		revision, err := d.Pools.Insert(txCtx, pool)
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, pool, err
	})
}

// changePool reads the pool before FIN's transaction for the T3 questions, then applies the change to the locked
// pool under its legal entity's policy (held FOR SHARE).
func (d Deps) changePool(ctx context.Context, c Command, ask func(context.Context, *fin.ConsolCostPool) error,
	apply func(context.Context, *fin.ConsolCostPool, *fin.FinancePolicy) (Receipt, error)) (Receipt, error) {
	return d.changePoolLocking(ctx, c, ask, nil, apply)
}

// changePoolLocking is changePool with lock run on the locked pool before the policy is taken: every FIN command
// takes the policy last (after its invoice, pools and jobs), so that a rate or threshold change waiting for the
// policy never sits between two commands that wait for each other's rows.
func (d Deps) changePoolLocking(ctx context.Context, c Command, ask func(context.Context, *fin.ConsolCostPool) error,
	lock func(context.Context, *fin.ConsolCostPool) error, apply func(context.Context, *fin.ConsolCostPool, *fin.FinancePolicy) (Receipt, error)) (Receipt, error) {
	if d.Pools == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if ask != nil {
		var current *fin.ConsolCostPool
		if err = d.read(ctx, func(txCtx context.Context) error {
			var revision int64
			current, revision, err = d.Pools.Read(txCtx, c.Scope.TenantID, c.Subject)
			if err == nil && revision != c.ExpectedRevision {
				return fin.ErrConflict
			}
			return err
		}); err != nil {
			return Receipt{}, err
		}
		if err = ask(ctx, current); err != nil {
			return Receipt{}, err
		}
	}
	return d.write(ctx, c, hash, "ConsolCostPool", func(txCtx context.Context) (Receipt, aggregate, error) {
		pool, revision, err := d.Pools.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		if err = d.matched(txCtx, c.Scope.TenantID, fin.TargetPoolCost, c.Subject, pool.Matched); err != nil {
			return Receipt{}, nil, err
		}
		if lock != nil {
			if err = lock(txCtx, pool); err != nil {
				return Receipt{}, nil, err
			}
		}
		p, err := d.policy(txCtx, c.Scope.TenantID, pool.State().LegalEntityID)
		if err != nil {
			return Receipt{}, nil, err
		}
		r, err := apply(txCtx, pool, p)
		if err != nil {
			return Receipt{}, nil, err
		}
		pool.Stamp(entered)
		if revision, err = d.Pools.Save(txCtx, pool, revision); err != nil {
			return Receipt{}, nil, err
		}
		r.SubjectID, r.NewRevision = c.Subject, revision
		return r, pool, nil
	})
}

// PoolCostAsk is a pool cost line as asked; the creditor may not be known yet.
type PoolCostAsk struct {
	ChargeCode  string
	Description string
	SupplierRef string
	Method      string
	Cost        fin.SideInput
	Reason      string
}

// RecordPoolCost adds a cost line of the whole consolidation: ORG confirms a creditor it names first (T3).
func (d Deps) RecordPoolCost(ctx context.Context, c Command, in PoolCostAsk) (Receipt, error) {
	if d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	return d.changePool(ctx, c, func(ctx context.Context, pool *fin.ConsolCostPool) (err error) {
		if in.Cost.PartyID == "" {
			return nil
		}
		in.Cost.PartyRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.Cost.PartyID, pool.State().LegalEntityID, CreditorRoles,
			c.OccurredAt)
		return err
	}, func(_ context.Context, pool *fin.ConsolCostPool, p *fin.FinancePolicy) (Receipt, error) {
		err := pool.RecordCost(in.ChargeCode, in.Description, in.SupplierRef, in.Method, in.Cost, in.Reason, p, c.Scope.ActorID, c.OccurredAt)
		return Receipt{Version: len(pool.State().Lines)}, err
	})
}

// RevisePoolCostAsk is a cost revision as asked; an empty party keeps the line's creditor.
type RevisePoolCostAsk struct {
	LineNo   int
	Expected int
	Value    fin.SideInput
	Reason   string
}

// RevisePoolCost writes the next version of a cost line: ORG confirms a creditor it names (T3).
func (d Deps) RevisePoolCost(ctx context.Context, c Command, in RevisePoolCostAsk) (Receipt, error) {
	if d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	return d.changePool(ctx, c, func(ctx context.Context, pool *fin.ConsolCostPool) (err error) {
		st := pool.State()
		if in.Value.PartyID == "" && in.LineNo >= 1 && in.LineNo <= len(st.Lines) {
			versions := st.Lines[in.LineNo-1].Versions
			last := versions[len(versions)-1]
			in.Value.PartyID, in.Value.PartyRevision = last.CreditorPartyID, last.CreditorRevision
			return nil
		}
		if in.Value.PartyID == "" {
			return nil
		}
		in.Value.PartyRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.Value.PartyID, st.LegalEntityID, CreditorRoles, c.OccurredAt)
		return err
	}, func(_ context.Context, pool *fin.ConsolCostPool, p *fin.FinancePolicy) (Receipt, error) {
		err := pool.ReviseCost(in.LineNo, in.Expected, in.Value, in.Reason, p, c.Scope.ActorID, c.OccurredAt)
		return Receipt{Version: in.Expected + 1}, err
	})
}

// ApportionAsk is an apportionment as asked: the lines (every line when empty) and a method overriding each line's
// default, or MANUAL with one line, shares and a reason, and the houses that take no part in them.
type ApportionAsk struct {
	LineNos   []int
	Method    string
	Manual    []fin.ManualShare
	NotUsed   []string
	AllHouses bool
	Reason    string
}

// houseRevenue reads the revenue by charge code at the estimate rate of the open jobs OpenJobsFor answered, per house
// shipment, in the transaction that holds their locks (job id order, as they were locked).
func (d Deps) houseRevenue(ctx context.Context, tenant fin.TenantID, jobs map[string]string) (map[string]map[string]int64, error) {
	shipments := make([]string, 0, len(jobs))
	for shipment := range jobs {
		shipments = append(shipments, shipment)
	}
	slices.SortFunc(shipments, func(a, b string) int { return strings.Compare(jobs[a], jobs[b]) })
	out := map[string]map[string]int64{}
	for _, shipment := range shipments {
		j, _, err := d.Jobs.Load(ctx, tenant, jobs[shipment])
		if err != nil {
			return nil, err
		}
		out[shipment] = j.EstimatedRevenue()
	}
	return out, nil
}

// apportionsByRevenue tells whether any line an apportionment names is divided by REVENUE: the command's method, or
// the line's own while the command names none.
func apportionsByRevenue(pool *fin.ConsolCostPool, in ApportionAsk) bool {
	if in.Method != "" {
		return in.Method == fin.MethodRevenue
	}
	lines := pool.State().Lines
	if len(in.LineNos) == 0 {
		return slices.ContainsFunc(lines, func(l fin.PoolCostLine) bool { return l.Method == fin.MethodRevenue })
	}
	return slices.ContainsFunc(in.LineNos, func(no int) bool { return no >= 1 && no <= len(lines) && lines[no-1].Method == fin.MethodRevenue })
}

// ApportionPoolCosts divides pool cost lines over the house jobs: PHY answers the consolidation's basis first (T3);
// in the transaction FIN locks the houses' jobs in job id order, before the policy, and requires an OPEN job of the
// pool's legal entity for every live house. A line by REVENUE also reads those jobs' revenue under that lock.
func (d Deps) ApportionPoolCosts(ctx context.Context, c Command, in ApportionAsk) (Receipt, error) {
	if d.Physical == nil {
		return Receipt{}, ErrMissingDependency
	}
	var basis fin.CostBasis
	var jobs map[string]string
	var revenue map[string]map[string]int64
	return d.changePoolLocking(ctx, c, func(ctx context.Context, pool *fin.ConsolCostPool) (err error) {
		st := pool.State()
		basis, err = d.Physical.CostBasis(ctx, c.Scope.TenantID, st.ConsolidationID)
		if err != nil {
			return err
		}
		if basis.ConsolidationID != st.ConsolidationID || (basis.State == "CANCELLED" && in.Method != fin.MethodRetained) {
			return fmt.Errorf("%w: PHY reports consolidation %s %s", fin.ErrPoolNoBasis, st.ConsolidationID, basis.State)
		}
		return nil
	}, func(txCtx context.Context, pool *fin.ConsolCostPool) (err error) {
		shipments := make([]string, 0, len(basis.Houses))
		for _, h := range basis.Houses {
			shipments = append(shipments, h.ShipmentID)
		}
		if jobs, err = d.Jobs.OpenJobsFor(txCtx, c.Scope.TenantID, pool.State().LegalEntityID, shipments); err != nil {
			return err
		}
		if apportionsByRevenue(pool, in) {
			revenue, err = d.houseRevenue(txCtx, c.Scope.TenantID, jobs)
		}
		return err
	}, func(_ context.Context, pool *fin.ConsolCostPool, _ *fin.FinancePolicy) (Receipt, error) {
		input := fin.ApportionInput{LineNos: in.LineNos, Method: in.Method, Manual: in.Manual, NotUsed: in.NotUsed, AllHouses: in.AllHouses, Reason: in.Reason, Jobs: jobs,
			Revenue: revenue}
		if err := pool.Apportion(input, basis, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, err
		}
		return Receipt{Version: len(pool.State().Apportionments)}, nil
	})
}
