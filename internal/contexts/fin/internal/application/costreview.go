package application

import (
	"context"
	"fmt"
	"slices"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// FIN V2.99: the cost review. A request locks the job's row (the lock an apportionment and a job change take), then
// takes the settings lock in shared mode (DecisionSettings), computes the profit and, if the job is a loss that the
// requester saw, writes the request under the tenant's numbering lock. PHY's basis of each pool the job has a share in
// is read before the transaction (T3); a pool the job had no share in then is refused as stale, and the figures are
// the job's as of the lock. A resolution locks the same job row, so that a request and the resolution of the job's
// previous request take turns.

// CostReviewFilter is what ListCostReviewRequests asks: the job (empty for every job), Open or Resolved (empty for
// both), the request number to start after and the page size.
type CostReviewFilter struct {
	JobID    string
	Resolved *bool
	After    int
	Limit    int
}

// CostReviewStore keeps cost review requests and their resolutions; rows are only inserted.
type CostReviewStore interface {
	// Next takes the tenant's numbering lock until the transaction ends and answers the next request number.
	Next(ctx context.Context, tenant fin.TenantID) (int, error)
	// OpenOf answers the id of the job's request that is not resolved, if there is one.
	OpenOf(ctx context.Context, tenant fin.TenantID, job string) (string, bool, error)
	Insert(ctx context.Context, r *fin.CostReview) error
	InsertResolution(ctx context.Context, r *fin.CostReview) error
	// Read answers the request with its resolution; bentorepo.ErrNotFound when the tenant has none of the id.
	Read(ctx context.Context, tenant fin.TenantID, id string) (*fin.CostReview, error)
	List(ctx context.Context, tenant fin.TenantID, f CostReviewFilter) ([]*fin.CostReview, error)
}

// ReviewAsk is what a requester asks: the job, the profit version they saw and the reason.
type ReviewAsk struct {
	JobID         string
	ProfitVersion string
	Reason        string
}

// ProfitRead is a job's profit read in one transaction with what it was computed from.
type ProfitRead struct {
	Job      *fin.FreightJob
	Revision int64
	Rules    fin.RuleSettings
	Shares   []PoolShare
	Losses   []PoolLoss
	PoolRevs map[string]int64
}

// ReadProfit reads, in the caller's transaction, what a job's profit is computed from. The caller says how the job is
// read (a query reads it, a command has locked it).
func (d Deps) ReadProfit(ctx context.Context, tenant fin.TenantID, j *fin.FreightJob, revision int64, rules fin.RuleSettings) (ProfitRead, error) {
	out := ProfitRead{Job: j, Revision: revision, Rules: rules}
	var err error
	if out.Shares, err = d.Pools.SharesOfJob(ctx, tenant, j.State().JobID); err != nil {
		return out, err
	}
	if out.Losses, err = d.Pools.LossesOfJob(ctx, tenant, j.State().JobID); err != nil {
		return out, err
	}
	pools := PoolsOf(out.Shares, out.Losses)
	if len(pools) > 0 {
		out.PoolRevs, err = d.Pools.RevisionsOf(ctx, tenant, pools)
	}
	return out, err
}

// PoolsOf lists the pools the shares and the losses are in, once each, in id order.
func PoolsOf(shares []PoolShare, losses []PoolLoss) []string {
	var pools []string
	for _, s := range shares {
		pools = append(pools, s.PoolID)
	}
	for _, l := range losses {
		pools = append(pools, l.PoolID)
	}
	slices.Sort(pools)
	return slices.Compact(pools)
}

// Figures computes the profit of what was read, with PHY's bases (one for the consolidation of each share).
func (p ProfitRead) Figures(bases map[string]fin.CostBasis) JobProfitFigures {
	costs := SplitFormerShares(p.Shares, p.Losses, p.Job.State().ShipmentID, bases)
	return ProfitOf(p.Job, p.Revision, p.Rules, costs, p.PoolRevs)
}

// RequestCostReview asks for the review of a loss-making job; Subject is the job. The receipt's subject is the request
// (CRR-<n>), its revision 1 and its Version the request's number.
func (d Deps) RequestCostReview(ctx context.Context, c Command, in ReviewAsk) (Receipt, error) {
	if d.CostReviews == nil || d.Pools == nil || d.Physical == nil || d.Settings == nil {
		return Receipt{}, ErrMissingDependency
	}
	if err := d.authorizeFirst(ctx, c); err != nil {
		return Receipt{}, err
	}
	hash, _, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	// PHY's basis of each consolidation the job has a share in, before the transaction (T3)
	var shares []PoolShare
	if err = d.Reader(ctx, func(txCtx context.Context) (err error) {
		shares, err = d.Pools.SharesOfJob(txCtx, c.Scope.TenantID, in.JobID)
		return err
	}); err != nil {
		return Receipt{}, err
	}
	bases, err := d.FormerBases(ctx, c.Scope.TenantID, shares)
	if err != nil {
		return Receipt{}, err
	}
	var j *fin.FreightJob
	var revision int64
	return d.writeAfter(ctx, c, hash, "CostReviewRequest", func(txCtx context.Context) (err error) {
		j, revision, err = d.Jobs.Load(txCtx, c.Scope.TenantID, in.JobID) // the job's lock first: a second copy of the command waits here
		return err
	}, func(txCtx context.Context) (Receipt, aggregate, error) {
		rules, at, err := d.DecisionSettings(txCtx, c.Scope.TenantID, j.State().LegalEntityID)
		if err != nil {
			return Receipt{}, nil, err
		}
		read, err := d.ReadProfit(txCtx, c.Scope.TenantID, j, revision, rules)
		if err != nil {
			return Receipt{}, nil, err
		}
		for _, pool := range PoolsOf(read.Shares, nil) {
			if !poolBasisRead(read.Shares, pool, bases) {
				return Receipt{}, nil, fmt.Errorf("%w (job %s gained a share in pool %s while the request was read): request again", fin.ErrReviewStale, in.JobID, pool)
			}
		}
		f := read.Figures(bases)
		open, found, err := d.CostReviews.OpenOf(txCtx, c.Scope.TenantID, in.JobID)
		if err != nil {
			return Receipt{}, nil, err
		}
		if !found {
			open = ""
		}
		no, err := d.CostReviews.Next(txCtx, c.Scope.TenantID)
		if err != nil {
			return Receipt{}, nil, err
		}
		figures := fin.ReviewFigures{Currency: j.State().FunctionalCurrency, Revenue: f.Revenue, Cost: f.Revenue - f.Profit, Profit: f.Profit, ProfitBasis: f.Basis,
			SettingsVersion: f.SettingsVersion, ProfitVersion: f.Version}
		r, err := fin.RequestCostReview(c.Scope.TenantID, no, j.State(), figures, in.ProfitVersion, open, in.Reason, c.Scope.ActorID, at)
		if err != nil {
			return Receipt{}, nil, err
		}
		if err = d.CostReviews.Insert(txCtx, r); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: r.State().RequestID, NewRevision: 1, Version: no}, r, nil
	})
}

// authorizeFirst asks the authorizer before the command's receipt is looked for, so that a person who no longer holds
// the permission is refused on a command id they used before, and a command id another person used is not answered to
// them.
func (d Deps) authorizeFirst(ctx context.Context, c Command) error {
	if d.Auth == nil {
		return ErrMissingDependency
	}
	if err := d.Auth.RequireFinAction(ctx, c.Scope, c.Action, c.Subject); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

// poolBasisRead reports whether PHY's basis was read for the consolidation of the job's shares in the pool.
func poolBasisRead(shares []PoolShare, pool string, bases map[string]fin.CostBasis) bool {
	for _, s := range shares {
		if s.PoolID == pool {
			if _, ok := bases[s.ConsolidationID]; !ok {
				return false
			}
		}
	}
	return true
}

// ResolveCostReview ends a request; Subject is the request. The receipt's revision is 2.
func (d Deps) ResolveCostReview(ctx context.Context, c Command, outcome, note string) (Receipt, error) {
	if d.CostReviews == nil || d.Settings == nil {
		return Receipt{}, ErrMissingDependency
	}
	if err := d.authorizeFirst(ctx, c); err != nil {
		return Receipt{}, err
	}
	hash, _, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	var job string
	if err = d.Reader(ctx, func(txCtx context.Context) error {
		r, err := d.CostReviews.Read(txCtx, c.Scope.TenantID, c.Subject)
		if err == nil {
			job = r.State().JobID
		}
		return err
	}); err != nil {
		return Receipt{}, err
	}
	return d.writeAfter(ctx, c, hash, "CostReviewResolution", func(txCtx context.Context) error {
		_, _, err := d.Jobs.Load(txCtx, c.Scope.TenantID, job) // the job's lock, as a request takes it
		return err
	}, func(txCtx context.Context) (Receipt, aggregate, error) {
		r, err := d.CostReviews.Read(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		at := d.Clock.Now().UTC().Truncate(time.Microsecond)
		if err = r.Resolve(outcome, note, c.Scope.ActorID, at); err != nil {
			return Receipt{}, nil, err
		}
		if err = d.CostReviews.InsertResolution(txCtx, r); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: 2, Version: r.State().No}, r, nil
	})
}
