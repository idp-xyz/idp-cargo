package application

import (
	"context"
	"fmt"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// FIN V0.5 (FB4): creditor invoices, their matches to accruals, variance approval, the release of accruals and the
// closing of consol cost pools. ORG confirms the creditor before FIN's transaction (T3); a matching locks the invoice,
// then the pools, then the jobs of the accruals it names (each in id order), then holds the policy FOR SHARE, so
// matches, the pools' and jobs' own commands and rate changes take turns.

// ActionApproveOwnCostVariance is what an Authorizer allows for an approver to approve a variance on an invoice they
// matched themselves (OD-9: the approver is not the matcher unless authorized).
const ActionApproveOwnCostVariance Action = "ApproveOwnCostVariance"

// matched reads, under the lock of the job or pool the command changes, what creditor invoices consume of its
// accruals, and hands it to the aggregate.
func (d Deps) matched(ctx context.Context, tenant fin.TenantID, kind, id string, set func(map[int]fin.Consumption)) error {
	if d.Creditors == nil {
		return ErrMissingDependency
	}
	consumed, err := d.Creditors.Consumed(ctx, tenant, kind, id)
	if err == nil {
		set(consumed)
	}
	return err
}

// SetVarianceThreshold records the next version of a legal entity's cost variance threshold (OD-9 B), in force after
// the legal entity's latest matching: an invoice keeps the threshold in force when it was matched. With the policy
// locked, a matching (which holds it FOR SHARE) waits or has committed.
func (d Deps) SetVarianceThreshold(ctx context.Context, c Command, amount int64, rateBP int) (Receipt, error) {
	if d.Creditors == nil {
		return Receipt{}, ErrMissingDependency
	}
	return d.changePolicy(ctx, c, func(txCtx context.Context, p *fin.FinancePolicy) (int, error) {
		latest, err := d.Creditors.LatestMatch(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return 0, err
		}
		if !c.OccurredAt.After(latest) {
			return 0, fmt.Errorf("%w: an invoice of the legal entity was matched at %s; a threshold is set after it", fin.ErrInvalid,
				latest.Format(time.RFC3339))
		}
		// FIN's clock, read under the policy's lock, stamps the version and is when it is in force from (since V2.100);
		// never before the latest match, so that a lagging server does not date a version before a match already made
		now, err := d.recordTime(txCtx, c, p)
		if err != nil {
			return 0, err
		}
		if err := p.ProposeVarianceThreshold(amount, rateBP, c.Scope.ActorID, c.OccurredAt, now); err != nil {
			return 0, err
		}
		return len(p.State().Thresholds), nil
	})
}

// recordTime is the system time a threshold set or approved is stamped with: FIN's clock, read under the policy's lock
// (which a match holds in share mode), and never before the system time of the latest match of the legal entity, so
// that a clock behind a match that has committed does not put a version in force before it.
func (d Deps) recordTime(ctx context.Context, c Command, _ *fin.FinancePolicy) (time.Time, error) {
	if d.Creditors == nil {
		return time.Time{}, ErrMissingDependency
	}
	now := d.Clock.Now().UTC().Truncate(time.Microsecond)
	latest, err := d.Creditors.LatestMatchEntered(ctx, c.Scope.TenantID, c.Subject)
	if err == nil && latest.After(now) {
		now = latest
	}
	return now, err
}

// ApproveVarianceThreshold approves the version of the legal entity's cost variance threshold that waits for approval
// (the one named), by a person other than its setter; it is in force from FIN's clock, read under the policy's lock.
// The permission is ApproveSettings, asked by the caller.
func (d Deps) ApproveVarianceThreshold(ctx context.Context, c Command, version int) (Receipt, error) {
	return d.changePolicy(ctx, c, func(txCtx context.Context, p *fin.FinancePolicy) (int, error) {
		now, err := d.recordTime(txCtx, c, p)
		if err != nil {
			return 0, err
		}
		return version, p.ApproveVarianceThreshold(version, c.Scope.ActorID, now)
	})
}

// RefuseVarianceThreshold refuses the version that waits for approval, with a reason, by a person other
// than its setter.
func (d Deps) RefuseVarianceThreshold(ctx context.Context, c Command, version int, reason string) (Receipt, error) {
	return d.changePolicy(ctx, c, func(_ context.Context, p *fin.FinancePolicy) (int, error) {
		return version, p.RefuseVarianceThreshold(version, reason, c.Scope.ActorID, d.Clock.Now().UTC().Truncate(time.Microsecond))
	})
}

// matchTime is the system time a match is read at: FIN's clock, and never before the latest approval of a threshold in
// force, so that a clock behind a decision that has committed does not make the match read an older threshold.
func matchTime(now time.Time, p *fin.FinancePolicy) time.Time {
	now = now.UTC().Truncate(time.Microsecond)
	if latest := p.LatestApprovalAt(); latest.After(now) {
		return latest
	}
	return now
}

// RecordCreditorInvoice records a supplier's invoice to a legal entity with a finance policy: ORG confirms the creditor
// in a creditor role first (T3).
func (d Deps) RecordCreditorInvoice(ctx context.Context, c Command, in fin.CreditorInvoiceInput) (Receipt, error) {
	if d.Directory == nil || d.Creditors == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if in.CreditorRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.CreditorPartyID, in.LegalEntityID, CreditorRoles,
		c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "CreditorInvoice", func(txCtx context.Context) (Receipt, aggregate, error) {
		if _, err := d.policy(txCtx, c.Scope.TenantID, in.LegalEntityID); err != nil {
			return Receipt{}, nil, err
		}
		inv, err := fin.RecordCreditorInvoice(c.Scope.TenantID, in, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		inv.Stamp(entered)
		revision, err := d.Creditors.Insert(txCtx, inv)
		return Receipt{SubjectID: in.ID, NewRevision: revision}, inv, err
	})
}

// changeCreditor applies a change to the invoice c.Subject names, read under its lock at the expected revision; ask,
// when given, reads the invoice before FIN's transaction; apply answers the version the command's receipt reports.
func (d Deps) changeCreditor(ctx context.Context, c Command, ask func(context.Context, *fin.CreditorInvoice) error,
	apply func(context.Context, *fin.CreditorInvoice) (int, error)) (Receipt, error) {
	if d.Creditors == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if ask != nil {
		var current *fin.CreditorInvoice
		if err = d.read(ctx, func(txCtx context.Context) error {
			var revision int64
			current, revision, err = d.Creditors.Read(txCtx, c.Scope.TenantID, c.Subject)
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
	return d.write(ctx, c, hash, "CreditorInvoice", func(txCtx context.Context) (Receipt, aggregate, error) {
		inv, revision, err := d.Creditors.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		version, err := apply(txCtx, inv)
		if err != nil {
			return Receipt{}, nil, err
		}
		inv.Stamp(entered)
		if revision, err = d.Creditors.Save(txCtx, inv, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: version}, inv, nil
	})
}

// MatchCreditorInvoice matches every line of a recorded invoice to accruals: with the invoice locked, it locks the
// pools and jobs of the accruals named and reads them, then converts at the policy's ACCOUNTING rate in force.
func (d Deps) MatchCreditorInvoice(ctx context.Context, c Command, items []fin.MatchItem) (Receipt, error) {
	return d.changeCreditor(ctx, c, nil, func(txCtx context.Context, inv *fin.CreditorInvoice) (int, error) {
		if len(items) == 0 || len(items) > fin.MaxMatches { // after begin: an actor not allowed hears unauthorized, not rejected
			return 0, fmt.Errorf("%w: a matching names 1 to %d matches", fin.ErrInvalid, fin.MaxMatches)
		}
		keys := make([]fin.TargetKey, 0, len(items))
		for _, it := range items {
			keys = append(keys, it.Target)
		}
		targets, err := d.Creditors.Targets(txCtx, c.Scope.TenantID, keys)
		if err != nil {
			return 0, err
		}
		p, err := d.policy(txCtx, c.Scope.TenantID, inv.State().LegalEntityID)
		if err != nil {
			return 0, err
		}
		// FIN's clock, read after the policy's lock: a threshold approved at the same moment is in force for this match
		// or
		// not, and the invoice records the version it used (since V2.100)
		if err = inv.MatchAt(items, targets, p, c.Scope.ActorID, c.OccurredAt, matchTime(d.Clock.Now(), p)); err != nil {
			return 0, err
		}
		return len(inv.State().Matches), nil
	})
}

// ApproveCostVariance approves the variance of a matched invoice. When the approver matched it, the Authorizer must
// allow ApproveOwnCostVariance as well, asked before FIN's transaction (the matcher never changes once matched).
func (d Deps) ApproveCostVariance(ctx context.Context, c Command, reason string) (Receipt, error) {
	own := false
	return d.changeCreditor(ctx, c, func(ctx context.Context, inv *fin.CreditorInvoice) error {
		if inv.State().MatchedBy != c.Scope.ActorID {
			return nil
		}
		if err := d.Auth.RequireFinAction(ctx, c.Scope, ActionApproveOwnCostVariance, c.Subject); err != nil {
			return fmt.Errorf("%w: %v", fin.ErrVarianceApproval, err)
		}
		own = true
		return nil
	}, func(_ context.Context, inv *fin.CreditorInvoice) (int, error) {
		return 0, inv.ApproveVariance(reason, own, c.Scope.ActorID, c.OccurredAt)
	})
}

// DisputeCreditorInvoice disputes an invoice; the pools and jobs of its matches are locked and must be open.
func (d Deps) DisputeCreditorInvoice(ctx context.Context, c Command, reason string) (Receipt, error) {
	return d.changeCreditor(ctx, c, nil, func(txCtx context.Context, inv *fin.CreditorInvoice) (int, error) {
		targets, err := d.Creditors.Targets(txCtx, c.Scope.TenantID, inv.Targets())
		if err != nil {
			return 0, err
		}
		if err = inv.Dispute(reason, targets, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(inv.State().Disputes), nil
	})
}

// ResolveCreditorInvoiceDispute resolves the open dispute of an invoice.
func (d Deps) ResolveCreditorInvoiceDispute(ctx context.Context, c Command, reason string) (Receipt, error) {
	return d.changeCreditor(ctx, c, nil, func(_ context.Context, inv *fin.CreditorInvoice) (int, error) {
		if err := inv.ResolveDispute(reason, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(inv.State().Disputes), nil
	})
}

// CancelCreditorInvoice cancels an invoice that nothing pays (read under its lock, which allocations take too); the
// pools and jobs of its matches are locked and must be open.
func (d Deps) CancelCreditorInvoice(ctx context.Context, c Command, reason string) (Receipt, error) {
	if d.Payments == nil {
		return Receipt{}, ErrMissingDependency
	}
	return d.changeCreditor(ctx, c, nil, func(txCtx context.Context, inv *fin.CreditorInvoice) (int, error) {
		paid, err := d.Payments.Paid(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return 0, err
		}
		var total int64
		for _, a := range paid {
			total += a.Amount
		}
		inv.SetPaid(total)
		targets, err := d.Creditors.Targets(txCtx, c.Scope.TenantID, inv.Targets())
		if err != nil {
			return 0, err
		}
		return 0, inv.Cancel(reason, targets, c.Scope.ActorID, c.OccurredAt)
	})
}

// ReleaseJobAccrual releases what no creditor invoice will consume of a job's cost side.
func (d Deps) ReleaseJobAccrual(ctx context.Context, c Command, lineNo, expected int, reason string) (Receipt, error) {
	return d.change(ctx, c, nil, func(_ context.Context, j *fin.FreightJob, _ *fin.FinancePolicy) (Receipt, error) {
		return Receipt{Version: expected + 1}, j.ReleaseCost(lineNo, expected, reason, c.Scope.ActorID, c.OccurredAt)
	})
}

// ReleasePoolAccrual releases what no creditor invoice will consume of a pool cost line.
func (d Deps) ReleasePoolAccrual(ctx context.Context, c Command, lineNo, expected int, reason string) (Receipt, error) {
	return d.changePool(ctx, c, nil, func(_ context.Context, pool *fin.ConsolCostPool, _ *fin.FinancePolicy) (Receipt, error) {
		return Receipt{Version: expected + 1}, pool.ReleaseCost(lineNo, expected, reason, c.Scope.ActorID, c.OccurredAt)
	})
}

// ClosePool closes a pool whose costs are final: PHY answers the consolidation's current basis first (T3). A pool with
// a line whose latest apportionment is by REVENUE also locks the houses' jobs, in job id order after the pool and
// before the policy as PreviewPoolApportionment does, and reads their revenue for the comparison; any other pool takes
// the locks and reads what it always did. The jobs are also locked and read when the REVENUE line's final cost is zero
// or the consolidation is gone, where Close compares nothing: more locks than needed, never a wrong result. Since V2.97
// it reads the settings in force for the pool's legal entity inside the transaction: F7 decides whether a pending cause
// holds the close back, and the closure records the version.
func (d Deps) ClosePool(ctx context.Context, c Command) (Receipt, error) {
	if d.Physical == nil {
		return Receipt{}, ErrMissingDependency
	}
	var basis fin.CostBasis
	return d.changePoolLocking(ctx, c, func(ctx context.Context, pool *fin.ConsolCostPool) (err error) {
		basis, err = d.Physical.CostBasis(ctx, c.Scope.TenantID, pool.State().ConsolidationID)
		return err
	}, func(txCtx context.Context, pool *fin.ConsolCostPool) error {
		if !pool.ByRevenue() {
			return nil
		}
		shipments := make([]string, 0, len(basis.Houses))
		for _, h := range basis.Houses {
			shipments = append(shipments, h.ShipmentID)
		}
		jobs, err := d.Jobs.OpenJobsFor(txCtx, c.Scope.TenantID, pool.State().LegalEntityID, shipments)
		if err != nil {
			return err
		}
		revenue, err := d.houseRevenue(txCtx, c.Scope.TenantID, jobs)
		pool.HouseRevenue(revenue)
		return err
	}, func(txCtx context.Context, pool *fin.ConsolCostPool, _ *fin.FinancePolicy) (Receipt, error) {
		rules, at, err := d.DecisionSettings(txCtx, c.Scope.TenantID, pool.State().LegalEntityID)
		if err != nil {
			return Receipt{}, err
		}
		if err = pool.CloseUnder(rules, basis, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, err
		}
		pool.Stamp(at) // the closure is stamped with the time its settings were read at
		return Receipt{Version: len(pool.State().Closures)}, nil
	})
}

// ReopenPool reopens a closed pool with a reason.
func (d Deps) ReopenPool(ctx context.Context, c Command, reason string) (Receipt, error) {
	return d.changePool(ctx, c, nil, func(_ context.Context, pool *fin.ConsolCostPool, _ *fin.FinancePolicy) (Receipt, error) {
		if err := pool.Reopen(reason, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, err
		}
		return Receipt{Version: len(pool.State().Closures)}, nil
	})
}
