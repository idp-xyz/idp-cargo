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
// creditor in a creditor role first (T3). A creditor whose roles have ended is still paid what it is owed, and only
// that: ORG still knows the party as ACTIVE and the legal entity as VERIFIED, and in the payment's currency the
// creditor's matched invoices (disputed or not) have open more than the payments on account; the payment is recorded at
// the revision the latest such invoice names and is not more than what is owed less what is on account.
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
	if noRole != nil {
		if err = d.formerPartner(ctx, c.Scope.TenantID, in.CreditorPartyID, in.LegalEntityID, noRole); err != nil {
			return Receipt{}, err
		}
	}
	return d.write(ctx, c, hash, "CreditorPayment", func(txCtx context.Context) (Receipt, aggregate, error) {
		if _, err := d.policy(txCtx, c.Scope.TenantID, in.LegalEntityID); err != nil {
			return Receipt{}, nil, err
		}
		if noRole != nil {
			items, err := d.Payments.OpenItems(txCtx, c.Scope.TenantID, in.LegalEntityID, in.CreditorPartyID)
			if err != nil {
				return Receipt{}, nil, err
			}
			onAccount, err := d.Payments.OnAccount(txCtx, c.Scope.TenantID, in.LegalEntityID, in.CreditorPartyID)
			if err != nil {
				return Receipt{}, nil, err
			}
			var owed int64
			in.CreditorRevision = 0
			for _, n := range items {
				if n.Matched && n.Currency == in.Amount.Currency {
					owed += n.Open()
					in.CreditorRevision = max(in.CreditorRevision, n.CreditorRevision)
				}
			}
			for _, m := range onAccount {
				if m.Currency == in.Amount.Currency {
					owed -= m.Minor
				}
			}
			if owed <= 0 || in.Amount.Minor > owed || in.CreditorRevision == 0 {
				return Receipt{}, nil, fmt.Errorf("%w; it is owed %d %s beyond what is on account", noRole, max(owed, 0), in.Amount.Currency)
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

// formerPartner checks, for a party ORG no longer confirms in a role, that ORG still knows it as ACTIVE and the legal
// entity as VERIFIED: a party suspended or unknown, or a legal entity not verified, takes nothing (refused).
func (d Deps) formerPartner(ctx context.Context, tenant fin.TenantID, party, legalEntity string, noRole error) error {
	id, err := d.Directory.PartyIdentity(ctx, tenant, party)
	if errors.Is(err, ErrUnknownSource) {
		return noRole
	}
	if err != nil {
		return err
	}
	if id.Status != "ACTIVE" {
		return fmt.Errorf("%w; ORG reports it %s", noRole, id.Status)
	}
	return d.verified(ctx, tenant, legalEntity)
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
