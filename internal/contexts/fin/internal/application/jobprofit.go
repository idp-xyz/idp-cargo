package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// JobPoolCosts is what a job bears of the consol cost pools (FIN V2.93), in the functional currency: the shares it
// bears and the re-routing losses it bears, which make up Cost, and its former shares, pending finance's confirmation,
// which are in no cost and are listed apart with their sum.
type JobPoolCosts struct {
	Borne       []PoolShare
	Losses      []PoolLoss
	Former      []PoolShare
	Cost        int64
	LossTotal   int64
	FormerTotal int64
}

// SplitFormerShares divides a job's pool shares into those it bears and its former shares: a share is a former share
// when PHY's basis of its pool's consolidation lists the job's house among the houses that left it. A former share is
// pending finance's confirmation: it is not part of the job's cost, and is listed apart. A re-routing loss the job
// bears is part of it. bases are PHY's bases by consolidation; a share whose consolidation has none is borne (the
// caller reads a basis for each).
func SplitFormerShares(shares []PoolShare, losses []PoolLoss, shipment string, bases map[string]fin.CostBasis) JobPoolCosts {
	var out JobPoolCosts
	for _, s := range shares {
		if b, read := bases[s.ConsolidationID]; read {
			if _, former := b.Former(shipment); former {
				out.Former, out.FormerTotal = append(out.Former, s), out.FormerTotal+s.Local
				continue
			}
		}
		out.Borne, out.Cost = append(out.Borne, s), out.Cost+s.Local
	}
	for _, l := range losses {
		out.Losses, out.LossTotal, out.Cost = append(out.Losses, l), out.LossTotal+l.Local, out.Cost+l.Local
	}
	return out
}

// FormerBases reads PHY's basis of the consolidation of each pool the shares are in, outside FIN's transactions. A job
// with a share and no PHY wired is answered ErrMissingDependency: a former share could not be told from a share. This
// is a defensive check, the module itself refuses to start without PHY. With no share nothing is asked.
func (d Deps) FormerBases(ctx context.Context, tenant fin.TenantID, shares []PoolShare) (map[string]fin.CostBasis, error) {
	bases := map[string]fin.CostBasis{}
	if len(shares) == 0 {
		return bases, nil
	}
	if d.Physical == nil {
		return nil, ErrMissingDependency
	}
	for _, s := range shares {
		if _, read := bases[s.ConsolidationID]; read {
			continue
		}
		b, err := d.Physical.CostBasis(ctx, tenant, s.ConsolidationID)
		if err != nil {
			return nil, err
		}
		bases[s.ConsolidationID] = b
	}
	return bases, nil
}

// JobProfitFigures are the figures of a job's profit that the profit version covers (FIN V2.99), in the functional
// currency: PoolCost is what the job bears of the pools, with a former share pending in it under F6 INCLUDED_MARKED.
type JobProfitFigures struct {
	Currency        string
	Revenue         int64
	Invoiced        int64
	Cost            int64
	Matched         int64
	Accrued         int64
	PoolCost        int64
	Profit          int64
	PendingInProfit int64
	FormerPending   int64
	ReroutingLoss   int64
	ByCode          []fin.CodeProfit
	Basis           string
	SettingsVersion int
	// Version is the profit version: see ProfitVersion.
	Version string
}

// ProfitOf computes a job's profit as GetJobProfit tells it, from the job, its revision, the settings in force for its
// legal entity, what it bears of the pools (costs, with PHY's former houses told apart) and the revision of each pool
// it has a share in, and its profit version.
func ProfitOf(j *fin.FreightJob, revision int64, rules fin.RuleSettings, costs JobPoolCosts, poolRevisions map[string]int64) JobProfitFigures {
	p := j.Profit()
	var pendingIn int64 // under F6 INCLUDED_MARKED the former shares pending are counted in the profit, marked
	basis := "SEPARATE"
	if rules.F6 == fin.F6IncludedMarked {
		pendingIn, basis = costs.FormerTotal, fin.F6IncludedMarked
	}
	f := JobProfitFigures{Currency: string(p.Currency), Revenue: p.Revenue, Invoiced: p.Invoiced, Cost: p.Cost, Matched: p.Matched, Accrued: p.Accrued,
		PoolCost: costs.Cost + pendingIn, Profit: p.Profit - costs.Cost - pendingIn, PendingInProfit: pendingIn, FormerPending: costs.FormerTotal,
		ReroutingLoss: costs.LossTotal, ByCode: p.Lines, Basis: basis, SettingsVersion: rules.Version}
	f.Version = ProfitVersion(j.State().JobID, revision, poolRevisions, f)
	return f
}

// ProfitVersion is the profit version of a job: the SHA-256, in hex, of a text of one fact to a line, sorted: what the
// profit is computed from (the job's revision, the revision of each pool the job has a share in, the settings version)
// and the figures. Equal figures from the same sources give the same version, and a change of any of them another.
// Clients compare versions for equality only.
func ProfitVersion(job string, revision int64, poolRevisions map[string]int64, f JobProfitFigures) string {
	lines := []string{
		"job=" + job, fmt.Sprintf("job.revision=%d", revision), fmt.Sprintf("settings.version=%d", f.SettingsVersion), "currency=" + f.Currency,
		"basis=" + f.Basis, fmt.Sprintf("revenue=%d", f.Revenue), fmt.Sprintf("invoiced=%d", f.Invoiced), fmt.Sprintf("cost=%d", f.Cost),
		fmt.Sprintf("cost.matched=%d", f.Matched), fmt.Sprintf("cost.accrued=%d", f.Accrued), fmt.Sprintf("pool.cost=%d", f.PoolCost),
		fmt.Sprintf("profit=%d", f.Profit), fmt.Sprintf("pending.in.profit=%d", f.PendingInProfit), fmt.Sprintf("former.pending=%d", f.FormerPending),
		fmt.Sprintf("rerouting.loss=%d", f.ReroutingLoss),
	}
	for pool, rev := range poolRevisions {
		lines = append(lines, fmt.Sprintf("pool.revision.%q=%d", pool, rev))
	}
	for _, c := range f.ByCode {
		lines = append(lines, fmt.Sprintf("code.%q=%d/%d/%d", c.ChargeCode, c.Revenue, c.Cost, c.Profit))
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
