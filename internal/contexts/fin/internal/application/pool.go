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
	LoadUnitNo  int // since V2.104: one container of the consolidation, 0 the whole consolidation
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
		err := pool.RecordCostInUnit(in.ChargeCode, in.Description, in.SupplierRef, in.Method, in.LoadUnitNo, in.Cost, in.Reason, p, c.Scope.ActorID, c.OccurredAt)
		return Receipt{Version: len(pool.State().Lines)}, err
	})
}

// RevisePoolCostAsk is a cost revision as asked; an empty party keeps the line's creditor.
type RevisePoolCostAsk struct {
	LineNo   int
	Expected int
	Value    fin.SideInput
	Reason   string
	// MoveTo names the container the line belongs to from this version on (since V2.104; 0 the whole consolidation);
	// nil keeps its container.
	MoveTo *int
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
		var err error
		if in.MoveTo != nil {
			err = pool.ReviseCostInUnit(in.LineNo, in.Expected, *in.MoveTo, in.Value, in.Reason, p, c.Scope.ActorID, c.OccurredAt)
		} else {
			err = pool.ReviseCost(in.LineNo, in.Expected, in.Value, in.Reason, p, c.Scope.ActorID, c.OccurredAt)
		}
		return Receipt{Version: in.Expected + 1}, err
	})
}

// ApportionAsk is a preview of an apportionment as asked: the lines (every line when empty) and a method overriding
// each line's default, or MANUAL with one line, shares and a reason, and the houses that take no part in them.
type ApportionAsk struct {
	LineNos   []int
	Method    string
	Manual    []fin.ManualShare
	NotUsed   []string
	AllHouses bool
	Reason    string
	// Attributions state who bears the former share of each house that left (since V2.93).
	Attributions []fin.AttributionInput
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

// PreviewPoolApportionment computes an apportionment of pool cost lines over the house jobs and stores it as a preview:
// PHY answers the consolidation's basis first (T3); in the transaction FIN locks the houses' jobs in job id order,
// before the policy, and requires an OPEN job of the pool's legal entity for every live house. A line by REVENUE also
// reads those jobs' revenue under that lock. Nothing but the preview is written. Since V2.97 the settings in force for
// the pool's legal entity are read inside the transaction (F3 decides which attributions need a voucher) and the
// preview records their version, which its fingerprint holds.
func (d Deps) PreviewPoolApportionment(ctx context.Context, c Command, in ApportionAsk) (Receipt, error) {
	return d.apportionPool(ctx, c, in, false)
}

// ApportionPoolInOneStep makes the apportionment of a preview and its submission in one command (since V2.98, F4
// ONE_STEP_WITHOUT_OPENED_OR_LEFT): the reads are those of a preview and of a submission together, in one transaction,
// with the same locks in the same order (PHY's basis first, then the jobs in job id order, the policy, the revenue of a
// line by REVENUE, and the settings in force), and it writes the apportionment, with no preview. The ask states no
// attribution. The receipt's Version is the apportionment's number.
func (d Deps) ApportionPoolInOneStep(ctx context.Context, c Command, in ApportionAsk) (Receipt, error) {
	in.Attributions = nil
	return d.apportionPool(ctx, c, in, true)
}

func (d Deps) apportionPool(ctx context.Context, c Command, in ApportionAsk, oneStep bool) (Receipt, error) {
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
		if jobs, err = d.openJobs(txCtx, c, pool, basis); err != nil {
			return err
		}
		if apportionsByRevenue(pool, in) {
			revenue, err = d.houseRevenue(txCtx, c.Scope.TenantID, jobs)
		}
		return err
	}, func(txCtx context.Context, pool *fin.ConsolCostPool, _ *fin.FinancePolicy) (Receipt, error) {
		rules, at, err := d.DecisionSettings(txCtx, c.Scope.TenantID, pool.State().LegalEntityID)
		if err != nil {
			return Receipt{}, err
		}
		input := fin.ApportionInput{LineNos: in.LineNos, Method: in.Method, Manual: in.Manual, NotUsed: in.NotUsed, AllHouses: in.AllHouses, Reason: in.Reason, Jobs: jobs,
			Revenue: revenue, Attributions: in.Attributions, Settings: rules}
		if oneStep {
			if err := pool.ApportionInOneStep(input, basis, c.Scope.ActorID, c.OccurredAt); err != nil {
				return Receipt{}, err
			}
			pool.Stamp(at) // the apportionment is stamped with the time its settings were read at
			return Receipt{Version: len(pool.State().Apportionments)}, nil
		}
		if err := pool.Preview(input, basis, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, err
		}
		pool.Stamp(at) // the preview is stamped with the time its settings were read at
		return Receipt{Version: len(pool.State().Previews)}, nil
	})
}

// openJobs locks the open jobs of the pool's legal entity of the basis's live houses and, since V2.93, of the houses
// that left and that hold a former share or an attribution in force on one of the pool's lines (one of them bears the
// loss an attribution states), in job id order. Only the live houses are in the preview's fingerprint; the others' jobs
// are there to bear a loss.
func (d Deps) openJobs(ctx context.Context, c Command, pool *fin.ConsolCostPool, basis fin.CostBasis) (map[string]string, error) {
	shipments := make([]string, 0, len(basis.Houses))
	for _, h := range basis.Houses {
		shipments = append(shipments, h.ShipmentID)
	}
	shipments = append(shipments, pool.HousesToAttribute(basis)...)
	return d.Jobs.OpenJobsFor(ctx, c.Scope.TenantID, pool.State().LegalEntityID, shipments)
}

// SubmitPoolApportionment writes the apportionment of a preview: PHY answers the consolidation's basis again first
// (T3); in the transaction FIN locks the houses' jobs in job id order, before the policy, reads the revenue the preview
// read when it has a line by REVENUE, and compares what the preview was computed on with what is there now. Since V2.97
// it reads the settings in force for the pool's legal entity inside the transaction: the version is part of what the
// preview was computed on, and F1 and F2 decide who may confirm.
func (d Deps) SubmitPoolApportionment(ctx context.Context, c Command, previewNo int) (Receipt, error) {
	if d.Physical == nil {
		return Receipt{}, ErrMissingDependency
	}
	var basis fin.CostBasis
	var jobs map[string]string
	var revenue map[string]map[string]int64
	return d.changePoolLocking(ctx, c, func(ctx context.Context, pool *fin.ConsolCostPool) (err error) {
		basis, err = d.Physical.CostBasis(ctx, c.Scope.TenantID, pool.State().ConsolidationID)
		return err
	}, func(txCtx context.Context, pool *fin.ConsolCostPool) (err error) {
		if jobs, err = d.openJobs(txCtx, c, pool, basis); err != nil {
			return err
		}
		if previews := pool.State().Previews; previewNo >= 1 && previewNo <= len(previews) &&
			slices.ContainsFunc(previews[previewNo-1].Lines, func(l fin.LineApportionment) bool { return l.Method == fin.MethodRevenue }) {
			revenue, err = d.houseRevenue(txCtx, c.Scope.TenantID, jobs)
		}
		return err
	}, func(txCtx context.Context, pool *fin.ConsolCostPool, _ *fin.FinancePolicy) (Receipt, error) {
		rules, at, err := d.DecisionSettings(txCtx, c.Scope.TenantID, pool.State().LegalEntityID)
		if err != nil {
			return Receipt{}, err
		}
		if err = pool.Submit(previewNo, fin.SubmitInput{Basis: basis, Jobs: jobs, Revenue: revenue, Settings: rules}, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, err
		}
		pool.Stamp(at) // the apportionment is stamped with the time its settings were read at
		return Receipt{Version: len(pool.State().Apportionments)}, nil
	})
}

// ReadOpenJobs is OpenJobsFor without its locks, for a query: the OPEN job of the legal entity of each shipment that
// has one, and the revenue by charge code at the estimate rate of each (FreightJob.EstimatedRevenue). It runs in the
// caller's read-only transaction. What it answers is what a submission would read again when nothing changes in
// between.
func (d Deps) ReadOpenJobs(ctx context.Context, tenant fin.TenantID, legalEntity string, shipments []string) (map[string]string, map[string]map[string]int64, error) {
	if d.Jobs == nil {
		return nil, nil, ErrMissingDependency
	}
	jobs, revenue := map[string]string{}, map[string]map[string]int64{}
	for _, shipment := range shipments {
		list, _, err := d.Jobs.ForShipment(ctx, tenant, shipment)
		if err != nil {
			return nil, nil, err
		}
		for _, j := range list {
			if st := j.State(); st.LegalEntityID == legalEntity && st.State == fin.JobOpen {
				jobs[shipment], revenue[shipment] = st.JobID, j.EstimatedRevenue()
			}
		}
	}
	return jobs, revenue, nil
}
