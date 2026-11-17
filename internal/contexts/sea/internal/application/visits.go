package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
)

var (
	ErrContainerInUse     = errors.New("sea application: the container has a live visit in the tenant")
	ErrUnitHasContainer   = errors.New("sea application: the movement's load unit has a container assigned")
	ErrContainersAssigned = errors.New("sea application: the movement has containers assigned; release them first")
)

// VisitStore loads and writes container visits; Load locks the visit row.
type VisitStore interface {
	Load(context.Context, sea.TenantID, sea.VisitID) (*sea.Visit, int64, error)
	Read(context.Context, sea.TenantID, sea.VisitID) (*sea.Visit, int64, error)
	Insert(context.Context, *sea.Visit) (int64, error)
	Save(context.Context, *sea.Visit, int64) (int64, error)
	// OfMovement lists a movement's visits in the order they were assigned.
	OfMovement(context.Context, sea.TenantID, sea.MovementID) ([]sea.VisitID, error)
	// LiveByNumber finds the live visit of a container number.
	LiveByNumber(ctx context.Context, tenant sea.TenantID, number string) (sea.VisitID, bool, error)
	// LiveByUnit finds the live visit assigned to a load unit of a movement.
	LiveByUnit(ctx context.Context, tenant sea.TenantID, movement sea.MovementID, unit int) (sea.VisitID, bool, error)
}

// AssignContainer assigns a container to a load unit of a movement. It locks the movement, as a cancellation does:
// a container is never assigned to a movement that is being cancelled, and a movement with containers is not
// cancelled (VisitsHeld). The movement itself does not change.
func (d Deps) AssignContainer(ctx context.Context, c Command, movement sea.MovementID, unit int, number, sizeType string, plate sea.Plate) (Receipt, error) {
	if d.Visits == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "ContainerVisit", func(txCtx context.Context) (Receipt, []aggregate, error) {
		m, _, err := d.Movements.Load(txCtx, c.Scope.TenantID, movement)
		if err != nil {
			return Receipt{}, nil, err
		}
		// Looked up rather than inserted and caught: a unique violation aborts the transaction, and with it any
		// receipt. The indexes remain the backstop when two commands pass this look at once.
		if id, found, err := d.Visits.LiveByNumber(txCtx, c.Scope.TenantID, number); err != nil {
			return Receipt{}, nil, err
		} else if found {
			return Receipt{}, nil, fmt.Errorf("%w: %s on %s", ErrContainerInUse, number, id)
		}
		if id, found, err := d.Visits.LiveByUnit(txCtx, c.Scope.TenantID, movement, unit); err != nil {
			return Receipt{}, nil, err
		} else if found {
			return Receipt{}, nil, fmt.Errorf("%w: unit %d on %s", ErrUnitHasContainer, unit, id)
		}
		v, err := sea.AssignContainer(sea.VisitID(c.Subject), m, unit, number, sizeType, plate, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		v.Stamp(entered)
		revision, err := d.Visits.Insert(txCtx, v)
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, []aggregate{v}, err
	})
}

// ChangeVisit runs a command on a stored visit locked at the expected revision. apply may read the visit's
// movement in the transaction (Movement).
func (d Deps) ChangeVisit(ctx context.Context, c Command, apply func(context.Context, *sea.Visit) (seal int, err error)) (Receipt, error) {
	if d.Visits == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "ContainerVisit", func(txCtx context.Context) (Receipt, []aggregate, error) {
		v, revision, err := d.Visits.Load(txCtx, c.Scope.TenantID, sea.VisitID(c.Subject))
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, sea.ErrConflict
		}
		seal, err := apply(txCtx, v)
		if err != nil {
			return Receipt{}, nil, err
		}
		v.Stamp(entered)
		if revision, err = d.Visits.Save(txCtx, v, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, SealNo: seal}, []aggregate{v}, nil
	})
}

// Movement reads a visit's movement in the command's transaction, without locking it: the houses' entries it reads
// are immutable.
func (d Deps) Movement(ctx context.Context, tenant sea.TenantID, id sea.MovementID) (*sea.Movement, error) {
	m, _, err := d.Movements.Read(ctx, tenant, id)
	return m, err
}

// VisitsHeld counts a movement's live container visits and states the latest business time recorded on any of its
// visits, in the transaction that holds the movement's lock: a movement is cancelled without containers, and not
// before the last of them was released.
func (d Deps) VisitsHeld(ctx context.Context, tenant sea.TenantID, movement sea.MovementID) (live int, last time.Time, err error) {
	if d.Visits == nil {
		return 0, time.Time{}, ErrMissingDependency
	}
	ids, err := d.Visits.OfMovement(ctx, tenant, movement)
	if err != nil {
		return 0, time.Time{}, err
	}
	for _, id := range ids {
		v, _, err := d.Visits.Read(ctx, tenant, id)
		if err != nil {
			return 0, time.Time{}, err
		}
		if v.Live() {
			live++
		}
		if t := v.LastChange(); t.After(last) {
			last = t
		}
	}
	return live, last, nil
}

// Visit reads a visit with its revision.
func (r Reads) Visit(ctx context.Context, tenant sea.TenantID, id sea.VisitID) (*sea.Visit, int64, error) {
	if tenant == "" || id == "" || r.Visits == nil {
		return nil, 0, ErrInvalidQuery
	}
	var v *sea.Visit
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		v, revision, err = r.Visits.Read(txCtx, tenant, id)
		return err
	})
	return v, revision, err
}

// VisitRead is a visit as read, with its revision.
type VisitRead struct {
	Visit    *sea.Visit
	Revision int64
}

// VisitsOf reads a movement's visits in the order they were assigned, in one transaction. A movement SEA does not
// know has none.
func (r Reads) VisitsOf(ctx context.Context, tenant sea.TenantID, movement sea.MovementID) ([]VisitRead, error) {
	if tenant == "" || movement == "" || r.Visits == nil {
		return nil, ErrInvalidQuery
	}
	var out []VisitRead
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		out, err = r.visitsOf(txCtx, tenant, movement)
		return err
	})
	return out, err
}

func (r Reads) visitsOf(txCtx context.Context, tenant sea.TenantID, movement sea.MovementID) ([]VisitRead, error) {
	ids, err := r.Visits.OfMovement(txCtx, tenant, movement)
	if err != nil {
		return nil, err
	}
	var out []VisitRead
	for _, id := range ids {
		v, revision, err := r.Visits.Read(txCtx, tenant, id)
		if err != nil {
			return nil, err
		}
		out = append(out, VisitRead{Visit: v, Revision: revision})
	}
	return out, nil
}

// LiveVisitByNumber reads the live visit of a container number.
func (r Reads) LiveVisitByNumber(ctx context.Context, tenant sea.TenantID, number string) (*sea.Visit, int64, error) {
	if tenant == "" || !sea.ValidContainerNumber(number) || r.Visits == nil {
		return nil, 0, ErrInvalidQuery
	}
	var v *sea.Visit
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		id, found, err := r.Visits.LiveByNumber(txCtx, tenant, number)
		if err != nil {
			return err
		}
		if !found {
			return bentorepo.ErrNotFound
		}
		v, revision, err = r.Visits.Read(txCtx, tenant, id)
		return err
	})
	return v, revision, err
}
