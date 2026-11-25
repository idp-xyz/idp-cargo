package application

import (
	"context"
	"fmt"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// FIN V2.102 (batch B): the after-the-fact review list and the mark of a reviewed week. The list is a query over the
// apportionments of the legal entity whose system time is in the week; a review is the one command that writes: it takes
// the week's lock first (as a settings command takes the tenant's), so that a second copy of it, or a second review of
// the week, waits for the first and then answers its receipt or is refused as done; reads the list under that lock, in
// one statement, and writes the mark with the list's size and digest.

// AfterTheFactStore keeps the marks of the weeks reviewed (rows are only inserted) and reads the list.
type AfterTheFactStore interface {
	// Lock takes the week's lock until the transaction ends.
	Lock(ctx context.Context, tenant fin.TenantID, legalEntity, week string) error
	// Lines lists the attributions of the legal entity confirmed in [from, to) by the apportionments' system time that are
	// listed for a reason, in the order of fin.ReviewKey, those after the key; limit 0 lists them all.
	Lines(ctx context.Context, tenant fin.TenantID, legalEntity string, from, to time.Time, after fin.ReviewKey, limit int) ([]fin.ReviewLine, error)
	// Find answers the week's mark, if it has one.
	Find(ctx context.Context, tenant fin.TenantID, legalEntity, week string) (*fin.AfterTheFactReview, bool, error)
	Insert(ctx context.Context, r *fin.AfterTheFactReview) error
}

// ReviewPage is a page of the list with the week's mark and, when it has one, the whole list's size and digest now.
type ReviewPage struct {
	Lines   []fin.ReviewLine
	More    bool
	Review  *fin.AfterTheFactReviewState
	Size    int
	Digest  string
	Matches bool
}

// AfterTheFactPage reads, in one snapshot, a page of a week's list (limit lines after the key) and the week's mark.
// When the week is reviewed it reads the whole list too, for its size and digest, to say whether it still matches.
func (d Deps) AfterTheFactPage(ctx context.Context, tenant fin.TenantID, legalEntity, week string, after fin.ReviewKey, limit int) (ReviewPage, error) {
	from, to, err := fin.ParseWeek(week)
	if err != nil {
		return ReviewPage{}, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	if d.AfterTheFact == nil || d.Policies == nil {
		return ReviewPage{}, ErrMissingDependency
	}
	var out ReviewPage
	err = d.Reader(ctx, func(txCtx context.Context) error {
		if _, found, err := d.Policies.Find(txCtx, tenant, legalEntity); err != nil || !found {
			if err == nil {
				err = fmt.Errorf("%w (legal entity %s)", ErrNoPolicy, legalEntity)
			}
			return err
		}
		lines, err := d.AfterTheFact.Lines(txCtx, tenant, legalEntity, from, to, after, limit+1)
		if err != nil {
			return err
		}
		if out.More = len(lines) > limit; out.More {
			lines = lines[:limit]
		}
		out.Lines = lines
		r, found, err := d.AfterTheFact.Find(txCtx, tenant, legalEntity, week)
		if err != nil || !found {
			return err
		}
		st := r.State()
		out.Review = &st
		all, err := d.AfterTheFact.Lines(txCtx, tenant, legalEntity, from, to, fin.ReviewKey{}, 0)
		if err != nil {
			return err
		}
		out.Size, out.Digest = len(all), fin.ListDigest(all)
		out.Matches = out.Size == st.ListSize && out.Digest == st.ListDigest
		return nil
	})
	return out, err
}

// MarkAfterTheFactReviewed marks a legal entity's week reviewed; Subject is "<legal entity>/<week>". The receipt's
// subject is the same, its revision 1.
func (d Deps) MarkAfterTheFactReviewed(ctx context.Context, c Command, legalEntity, week, note string) (Receipt, error) {
	if d.AfterTheFact == nil || d.Policies == nil {
		return Receipt{}, ErrMissingDependency
	}
	if err := d.authorizeFirst(ctx, c); err != nil {
		return Receipt{}, err
	}
	from, to, err := fin.ParseWeek(week)
	if err != nil {
		return Receipt{}, err
	}
	hash, _, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.writeAfter(ctx, c, hash, "AfterTheFactReview", func(txCtx context.Context) error {
		return d.AfterTheFact.Lock(txCtx, c.Scope.TenantID, legalEntity, week)
	}, func(txCtx context.Context) (Receipt, aggregate, error) {
		if _, found, err := d.Policies.Find(txCtx, c.Scope.TenantID, legalEntity); err != nil || !found {
			if err == nil {
				err = fmt.Errorf("%w (legal entity %s)", ErrNoPolicy, legalEntity)
			}
			return Receipt{}, nil, err
		}
		var done *fin.AfterTheFactReviewState
		if r, found, err := d.AfterTheFact.Find(txCtx, c.Scope.TenantID, legalEntity, week); err != nil {
			return Receipt{}, nil, err
		} else if found {
			st := r.State()
			done = &st
		}
		lines, err := d.AfterTheFact.Lines(txCtx, c.Scope.TenantID, legalEntity, from, to, fin.ReviewKey{}, 0)
		if err != nil {
			return Receipt{}, nil, err
		}
		at := d.Clock.Now().UTC().Truncate(time.Microsecond)
		r, err := fin.ReviewAfterTheFact(c.Scope.TenantID, legalEntity, week, lines, done, c.Scope.ActorID, note, at)
		if err != nil {
			return Receipt{}, nil, err
		}
		if err = d.AfterTheFact.Insert(txCtx, r); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: fin.ReviewSubject(legalEntity, week), NewRevision: 1, Version: len(lines)}, r, nil
	})
}
