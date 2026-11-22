package application

import (
	"context"

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
