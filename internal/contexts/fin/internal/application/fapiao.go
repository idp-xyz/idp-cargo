package application

import (
	"context"
	"fmt"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// RequestFapiao asks for a fapiao on issued debit notes of one legal entity and one debtor (FB6). Before FIN's
// transaction (T3) ORG confirms the debtor in a debtor role for the legal entity and answers its identity; in the
// transaction FIN takes the legal entity's functional currency, the debtor's billing profile version (which must still
// name it as ORG does), and, with the notes' jobs locked, what each note bills per tax treatment and rate.
func (d Deps) RequestFapiao(ctx context.Context, c Command, in fin.FapiaoInput, invoiceIDs []string) (Receipt, error) {
	if d.Directory == nil || d.Fapiao == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	if len(invoiceIDs) == 0 || len(invoiceIDs) > fin.MaxFapiaoNotes { // after begin: an actor not allowed hears unauthorized, not rejected
		return Receipt{}, fmt.Errorf("%w: a fapiao record covers 1 to %d debit notes", fin.ErrInvalid, fin.MaxFapiaoNotes)
	}
	named := map[string]bool{}
	for _, id := range invoiceIDs {
		if named[id] {
			return Receipt{}, fmt.Errorf("%w: debit note %s named twice", fin.ErrInvalid, id)
		}
		named[id] = true
	}
	if in.DebtorRevision, err = d.Directory.PartyInRole(ctx, c.Scope.TenantID, in.DebtorPartyID, in.LegalEntityID, DebtorRoles, c.OccurredAt); err != nil {
		return Receipt{}, err
	}
	identity, err := d.Directory.PartyIdentity(ctx, c.Scope.TenantID, in.DebtorPartyID)
	if err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "FapiaoRecord", func(txCtx context.Context) (Receipt, aggregate, error) {
		p, err := d.policy(txCtx, c.Scope.TenantID, in.LegalEntityID)
		if err != nil {
			return Receipt{}, nil, err
		}
		in.Currency = p.State().FunctionalCurrency
		profile, found, err := d.Profiles.Load(txCtx, c.Scope.TenantID, in.DebtorPartyID)
		if err != nil {
			return Receipt{}, nil, err
		}
		if !found {
			return Receipt{}, nil, fmt.Errorf("%w: %s", fin.ErrNoBillingProfile, in.DebtorPartyID)
		}
		if in.Profile, _ = profile.Current(); !in.Profile.Matches(identity) {
			return Receipt{}, nil, fmt.Errorf("%w: %s, version %d", fin.ErrProfileStale, in.DebtorPartyID, in.Profile.No)
		}
		notes, err := d.Fapiao.Notes(txCtx, c.Scope.TenantID, invoiceIDs)
		if err != nil {
			return Receipt{}, nil, err
		}
		known := map[string]bool{}
		for _, n := range notes {
			known[n.InvoiceID] = true
		}
		for _, id := range invoiceIDs {
			if !known[id] {
				return Receipt{}, nil, fmt.Errorf("%w: debit note %s", bentorepo.ErrNotFound, id)
			}
			if other, covered, err := d.Fapiao.Covering(txCtx, c.Scope.TenantID, id); err != nil || covered {
				if err == nil {
					err = fmt.Errorf("%w: debit note %s, by fapiao record %s", fin.ErrFapiaoCovers, id, other)
				}
				return Receipt{}, nil, err
			}
		}
		r, err := fin.RequestFapiao(c.Scope.TenantID, in, notes, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		r.Stamp(entered)
		revision, err := d.Fapiao.Insert(txCtx, r)
		return Receipt{SubjectID: in.ID, NewRevision: revision}, r, err
	})
}

// changeFapiao applies a move to the record c.Subject names, read under its lock at the expected revision.
func (d Deps) changeFapiao(ctx context.Context, c Command, apply func(*fin.FapiaoRecord) error) (Receipt, error) {
	if d.Fapiao == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "FapiaoRecord", func(txCtx context.Context) (Receipt, aggregate, error) {
		r, revision, err := d.Fapiao.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, fin.ErrConflict
		}
		if err = apply(r); err != nil {
			return Receipt{}, nil, err
		}
		r.Stamp(entered)
		if revision, err = d.Fapiao.Save(txCtx, r, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, r, nil
	})
}

// RecordFapiaoIssued records the fapiao the platform issued on the record's request.
func (d Deps) RecordFapiaoIssued(ctx context.Context, c Command, e fin.PlatformEvidence) (Receipt, error) {
	return d.changeFapiao(ctx, c, func(r *fin.FapiaoRecord) error { return r.RecordIssued(e, c.Scope.ActorID, c.OccurredAt) })
}

// RecordFapiaoFailed records that the request failed or was abandoned; its debit notes are covered no longer.
func (d Deps) RecordFapiaoFailed(ctx context.Context, c Command, reason string) (Receipt, error) {
	return d.changeFapiao(ctx, c, func(r *fin.FapiaoRecord) error { return r.RecordFailed(reason, c.Scope.ActorID, c.OccurredAt) })
}

// RecordRedLetter records the red-letter fapiao that reversed the issued one; credit notes may then reverse its notes.
func (d Deps) RecordRedLetter(ctx context.Context, c Command, e fin.PlatformEvidence, reason string) (Receipt, error) {
	return d.changeFapiao(ctx, c, func(r *fin.FapiaoRecord) error { return r.RecordRedLetter(e, reason, c.Scope.ActorID, c.OccurredAt) })
}
