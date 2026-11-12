package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// FIN V0.6: creditor payments. ORG confirms the creditor before FIN's transaction (T3); an allocation locks the payment,
// then the invoices it pays (in id order, as invoice commands lock them), so allocations, invoice cancellations and
// disputes take turns.

// RecordCreditorPayment records money paid to a creditor of a legal entity with a finance policy: ORG confirms the
// creditor in a creditor role first (T3). A creditor whose roles have ended is still paid what it is owed: with a
// matched invoice of the legal entity open, the payment is recorded at the revision the latest such invoice names.
func (d Deps) RecordCreditorPayment(ctx context.Context, c Command, in fin.PaymentInput) (Receipt, error) {
	if d.Directory == nil || d.Payments == nil || d.Creditors == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	in.CreditorRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.CreditorPartyID, in.LegalEntityID, CreditorRoles, c.OccurredAt)
	if err != nil && !errors.Is(err, fin.ErrPartyRole) {
		return Receipt{}, err
	}
	noRole := err
	return d.write(ctx, c, hash, "CreditorPayment", func(txCtx context.Context) (Receipt, aggregate, error) {
		if _, err := d.policy(txCtx, c.Scope.TenantID, in.LegalEntityID); err != nil {
			return Receipt{}, nil, err
		}
		if noRole != nil {
			invoices, _, err := d.Creditors.List(txCtx, c.Scope.TenantID, in.LegalEntityID, in.CreditorPartyID)
			if err != nil {
				return Receipt{}, nil, err
			}
			in.CreditorRevision = 0
			for _, inv := range invoices {
				st := inv.State()
				if st.State != fin.CreditorMatched || st.CreditorRevision <= in.CreditorRevision {
					continue
				}
				paid, err := d.Payments.Paid(txCtx, c.Scope.TenantID, st.InvoiceID)
				if err != nil {
					return Receipt{}, nil, err
				}
				var total int64
				for _, a := range paid {
					total += a.Amount
				}
				if total < st.Total() {
					in.CreditorRevision = st.CreditorRevision
				}
			}
			if in.CreditorRevision == 0 {
				return Receipt{}, nil, noRole
			}
		}
		p, err := fin.RecordPayment(c.Scope.TenantID, in, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		p.Stamp(entered)
		revision, err := d.Payments.Insert(txCtx, p)
		return Receipt{SubjectID: in.ID, NewRevision: revision}, p, err
	})
}

// changePayment applies a change to the payment c.Subject names, read under its lock at the expected revision; apply
// answers the version the command's receipt reports.
func (d Deps) changePayment(ctx context.Context, c Command, apply func(context.Context, *fin.CreditorPayment) (int, error)) (Receipt, error) {
	if d.Payments == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "CreditorPayment", func(txCtx context.Context) (Receipt, aggregate, error) {
		p, revision, err := d.Payments.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		version, err := apply(txCtx, p)
		if err != nil {
			return Receipt{}, nil, err
		}
		p.Stamp(entered)
		if revision, err = d.Payments.Save(txCtx, p, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: version}, p, nil
	})
}

// AllocateCreditorPayment pays creditor invoices from the payment. With the payment locked, it locks the invoices and
// reads what is open of each, so that another payment's allocation, a dispute or a cancellation of the same invoice
// waits its turn.
func (d Deps) AllocateCreditorPayment(ctx context.Context, c Command, items []fin.AllocationItem, on time.Time) (Receipt, error) {
	return d.changePayment(ctx, c, func(txCtx context.Context, p *fin.CreditorPayment) (int, error) {
		if len(items) == 0 || len(items) > fin.MaxAllocations { // after begin: an actor not allowed hears unauthorized, not rejected
			return 0, fmt.Errorf("%w: an allocation names 1 to %d creditor invoices", fin.ErrInvalid, fin.MaxAllocations)
		}
		ids := make([]string, 0, len(items))
		for _, it := range items {
			ids = append(ids, it.InvoiceID)
		}
		invoices, err := d.Payments.PayableInvoices(txCtx, c.Scope.TenantID, ids)
		if err != nil {
			return 0, err
		}
		if err = p.Allocate(items, invoices, on, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(p.State().Allocations), nil
	})
}

// ReverseCreditorPaymentAllocation reverses one allocation of the payment.
func (d Deps) ReverseCreditorPaymentAllocation(ctx context.Context, c Command, no int, reason string, on time.Time) (Receipt, error) {
	return d.changePayment(ctx, c, func(_ context.Context, p *fin.CreditorPayment) (int, error) {
		return no, p.ReverseAllocation(no, reason, on, c.Scope.ActorID, c.OccurredAt)
	})
}

// CancelCreditorPayment cancels a payment with no live allocation.
func (d Deps) CancelCreditorPayment(ctx context.Context, c Command, reason string, on time.Time) (Receipt, error) {
	return d.changePayment(ctx, c, func(_ context.Context, p *fin.CreditorPayment) (int, error) {
		return 0, p.Cancel(reason, on, c.Scope.ActorID, c.OccurredAt)
	})
}
