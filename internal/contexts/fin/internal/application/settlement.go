package application

import (
	"context"
	"fmt"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// RecordReceipt records money received from a debtor of a legal entity with a finance policy: ORG confirms the debtor
// in a debtor role first (T3).
func (d Deps) RecordReceipt(ctx context.Context, c Command, in fin.ReceiptInput) (Receipt, error) {
	if d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if in.DebtorRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.DebtorPartyID, in.LegalEntityID, DebtorRoles, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "DebtorReceipt", func(txCtx context.Context) (Receipt, aggregate, error) {
		if _, err := d.policy(txCtx, c.Scope.TenantID, in.LegalEntityID); err != nil {
			return Receipt{}, nil, err
		}
		r, err := fin.RecordReceipt(c.Scope.TenantID, in, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		r.Stamp(entered)
		revision, err := d.DebtorReceipts.Insert(txCtx, r)
		return Receipt{SubjectID: in.ID, NewRevision: revision}, r, err
	})
}

// changeReceipt applies a change to the receipt c.Subject names, read under its lock at the expected revision; apply
// answers the version the command's receipt reports.
func (d Deps) changeReceipt(ctx context.Context, c Command, apply func(context.Context, *fin.DebtorReceipt) (int, error)) (Receipt, error) {
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "DebtorReceipt", func(txCtx context.Context) (Receipt, aggregate, error) {
		r, revision, err := d.DebtorReceipts.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		version, err := apply(txCtx, r)
		if err != nil {
			return Receipt{}, nil, err
		}
		r.Stamp(entered)
		if revision, err = d.DebtorReceipts.Save(txCtx, r, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: version}, r, nil
	})
}

// AllocateReceipt settles debit notes from the receipt. With the receipt locked, it locks the notes' jobs and reads
// what is open of each note, so that a credit note or another receipt's allocation to the same note waits its turn.
func (d Deps) AllocateReceipt(ctx context.Context, c Command, items []fin.AllocationItem, on time.Time) (Receipt, error) {
	if len(items) == 0 || len(items) > fin.MaxAllocations {
		return Receipt{}, fmt.Errorf("%w: an allocation names 1 to %d debit notes", fin.ErrInvalid, fin.MaxAllocations)
	}
	return d.changeReceipt(ctx, c, func(txCtx context.Context, r *fin.DebtorReceipt) (int, error) {
		ids := make([]string, 0, len(items))
		for _, it := range items {
			ids = append(ids, it.InvoiceID)
		}
		notes, err := d.DebtorReceipts.SettlementNotes(txCtx, c.Scope.TenantID, ids)
		if err != nil {
			return 0, err
		}
		if err = r.Allocate(items, notes, on, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(r.State().Allocations), nil
	})
}

// ReverseAllocation reverses one allocation of the receipt.
func (d Deps) ReverseAllocation(ctx context.Context, c Command, no int, reason string, on time.Time) (Receipt, error) {
	return d.changeReceipt(ctx, c, func(_ context.Context, r *fin.DebtorReceipt) (int, error) {
		return no, r.ReverseAllocation(no, reason, on, c.Scope.ActorID, c.OccurredAt)
	})
}

// CancelReceipt cancels a receipt with no live allocation.
func (d Deps) CancelReceipt(ctx context.Context, c Command, reason string, on time.Time) (Receipt, error) {
	return d.changeReceipt(ctx, c, func(_ context.Context, r *fin.DebtorReceipt) (int, error) {
		return 0, r.Cancel(reason, on, c.Scope.ActorID, c.OccurredAt)
	})
}
