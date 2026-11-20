package application

import (
	"context"
	"slices"
	"time"

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// The container limit check reads the figure PHY publishes for a container: its part of each lot's measure, which is
// divided over every execution that holds live packs of the lot (phy.AllotLots). A command that checks the limits of
// the stuffings it holds therefore also reads the other executions that hold live packs of the same lots.
//
// When it runs. Every command that can move a lot's allotted figures is followed by the check of the stuffings of the
// lot that are LOADING or ON_HOLD: a load, an unload, a receipt of more packs of the lot and a measurement. A seal
// changes no line and a cancellation is of a stuffing without live packs, and the allotment reads the live lines only,
// so neither moves a figure (reasoned, not run). A receipt, which only lowers the apportioned figures by exact
// arithmetic, can raise a container's by one unit of the plan's measure through the rounding of the largest-remainder
// division (a lot of 7 packs weighed at 4 kg with 3, 3 and 1 packs apportioned in three containers: 2, 2 and 0 kg are
// allotted, and 1, 1 and 1 kg once an eighth pack is received); with a volume in cubic metres the unit is a cubic
// metre. A container at its limit can be pushed over by it.
//
// Who is held. The stuffing a load goes into is held whenever it is over, as before this version, and so is every
// stuffing after a measurement: the measurement addresses the lot of each of them, and that has been so since V2.58, so
// it is the one place where a hold a person released can come back without a rise of the figure. In every other case,
// the other stuffings after a load, every stuffing after an unload (the one unloaded from included) and the stuffings
// after a receipt, a stuffing is held only for a dimension the command raised into excess (phy.LimitsCheck.RaisedOver,
// the figures of before worked out before the command's change is applied, under the same locks): it is over the
// maximum gross and its grams are greater than before, or over the capacity and its cubic centimetres are greater than
// before, or not checked now and checked before. So a hold a person released does not come back because of a command on
// another container that left the figure where it was, or lowered the figure it is over in while rounding raised the
// other. The search of TestAnUnloadNeverRaisesTheContainerItComesOutOf, and one of the reviewer of V2.83 over 1.5
// million cases, found no unload that raises the figure of the container it comes out of.
//
// Locks. The other stuffings are locked, after the lot and without waiting, because they are written: a command that
// holds a stuffing may wait for the lot, so waiting for it while holding the lot could be a deadlock. Reading them
// without locking them would let two commands on two lots of one container each conclude that the container is not
// over. A busy stuffing makes the attempt answer phy.ErrRetryable, nothing written, even if no hold would have been
// placed. This is so for a receipt as well as for a load and an unload: a receipt of a lot conflicts whenever another
// command holds one of the open stuffings that hold the lot, and under a steady stress of loaders, unloaders and a
// measurement on those stuffings the reviewer's receipt loop got 969 conflicts and no success in ten seconds, without a
// deadlock or a database error. Since V2.84 the command path retries it (Deps.retrying): the whole transaction is run
// again after a short wait, holding nothing while it waits, six attempts in all by default, the waits starting at 5 ms
// and doubling with up to their own length added at random (155 to 310 ms in all). What reaches the caller is the
// conflict of the last attempt, as before, when the stuffing stayed busy through all of them, and it is a plain
// phy.ErrConflict, never retried, when the caller's expected revision is stale. A retry makes starvation under a
// sustained stream of commands on the same stuffings rarer and bounded (a caller waits at most six attempts and about a
// third of a second), not impossible. A conflict also comes before a business rejection, because the stuffings are
// locked before the change is applied, so that the figures of before can be read: a load of more packs than were
// received answers the conflict while another stuffing of the lot is busy, and the rejection only when none is. Only
// the stuffings that are LOADING or ON_HOLD are locked; a stuffing that is CLOSED is never written, so it is read for
// the allotment and not locked (one that is sealed between the reading of the set and the lock is locked for nothing,
// which does no harm). The stuffings are read only for the allotment of the lot's other stuffings; for the lot that is
// changed, whose lock the command holds, every load, unload, receipt and measurement of it waits for the command, so
// the packs of the lot in them are as the command reads them. The figures of the container's other lots are as
// committed, and a command on those lots runs its own check, which needs the container's lock as well.

// limitsWatch is the check of the limits that follows a command on a lot: it locks, reads and measures what the
// command will move, before the command changes anything, and holds afterwards.
type limitsWatch struct {
	d       Deps
	tenant  phy.TenantID
	consols map[phy.ConsolidationID]*phy.Consolidation
	lot     *phy.CargoLot
	self    *phy.LoadExecution // the stuffing the command addresses; nil for a receipt
	always  bool               // a load into self: it is held whenever it is over
	others  []ExecutionRead
	before  map[phy.LoadExecutionID]phy.LimitsCheck
}

// watchLimits starts the check around a command on the lot, whose lock the caller holds, before the command's change is
// applied. self is the stuffing the command addresses (nil for a receipt); always says that the command is a load into
// it, which holds it whenever it is over. consol is self's consolidation, locked by the caller; for a receipt it is nil
// and the consolidations of the stuffings are read without a lock, which is enough for their units' limits (frozen when
// the unit is planned).
func (d Deps) watchLimits(ctx context.Context, tenant phy.TenantID, consol *phy.Consolidation, lot *phy.CargoLot, self *phy.LoadExecution,
	always bool) (*limitsWatch, error) {
	w := &limitsWatch{d: d, tenant: tenant, consols: map[phy.ConsolidationID]*phy.Consolidation{}, lot: lot, self: self, always: always,
		before: map[phy.LoadExecutionID]phy.LimitsCheck{}}
	if consol != nil {
		w.consols[consol.ID()] = consol
	}
	var selfID phy.LoadExecutionID
	if self != nil {
		selfID = self.ID()
	}
	var err error
	if w.others, err = d.otherStuffings(ctx, tenant, lot.ID(), selfID); err != nil {
		return nil, err
	}
	for _, o := range w.others {
		id := o.Execution.Consolidation()
		if w.consols[id] != nil {
			continue
		}
		if w.consols[id], _, err = d.Consolidations.Read(ctx, tenant, id); err != nil {
			return nil, err
		}
	}
	targets := w.targets(false)
	if len(targets) == 0 {
		return w, nil
	}
	lots, allotment, err := d.limitsOf(ctx, tenant, w.all(), lot)
	if err != nil {
		return nil, err
	}
	for _, x := range targets {
		if c, ok := x.CheckLimits(w.consols[x.Consolidation()], lots, allotment); ok {
			w.before[x.ID()] = c
		}
	}
	return w, nil
}

// all is the executions the command holds or has locked: the stuffing it addresses and the other stuffings of the lot.
func (w *limitsWatch) all() []*phy.LoadExecution {
	var all []*phy.LoadExecution
	if w.self != nil {
		all = append(all, w.self)
	}
	for _, o := range w.others {
		all = append(all, o.Execution)
	}
	return all
}

// targets are the stuffings whose figure the command may raise and so are measured before it: the other stuffings and,
// unless the command is a load into it (held whenever over, so measured after only), the stuffing it addresses.
func (w *limitsWatch) targets(afterToo bool) []*phy.LoadExecution {
	var out []*phy.LoadExecution
	if w.self != nil && (!w.always || afterToo) {
		out = append(out, w.self)
	}
	for _, o := range w.others {
		out = append(out, o.Execution)
	}
	return out
}

// hold checks the stuffings after the command's change, with the evidence, actor and time of the command, and returns
// the other stuffings it held, to be saved with the command's. The stuffing the command addresses is held on its own
// object, which the command saves.
func (w *limitsWatch) hold(ctx context.Context, evidence phy.Evidence, by string, at time.Time) ([]ExecutionRead, error) {
	targets := w.targets(true)
	if len(targets) == 0 {
		return nil, nil
	}
	lots, allotment, err := w.d.limitsOf(ctx, w.tenant, w.all(), w.lot)
	if err != nil {
		return nil, err
	}
	for _, x := range targets {
		consol := w.consols[x.Consolidation()]
		if x == w.self && w.always {
			x.HoldIfOverLimitsAllotted(consol, lots, allotment, evidence, by, at)
		} else {
			x.HoldIfRaisedOverLimits(consol, lots, allotment, w.before[x.ID()], evidence, by, at)
		}
	}
	var held []ExecutionRead
	for _, o := range w.others {
		if len(o.Execution.Pending()) > 0 {
			held = append(held, o)
		}
	}
	return held, nil
}

// otherStuffings locks the other LOADING or ON_HOLD stuffings with live packs of the lot, without waiting, and reads
// them, under the lot's lock the caller holds. A CLOSED stuffing is not among them: it is never written.
func (d Deps) otherStuffings(ctx context.Context, tenant phy.TenantID, lot phy.CargoLotID, self phy.LoadExecutionID) ([]ExecutionRead, error) {
	ids, err := d.Executions.StuffingsWithLiveLot(ctx, tenant, lot)
	if err != nil {
		return nil, err
	}
	ids = slices.DeleteFunc(ids, func(id phy.LoadExecutionID) bool { return id == self })
	if len(ids) == 0 {
		return nil, nil
	}
	if err = d.Executions.LockWithoutWaiting(ctx, tenant, ids); err != nil {
		return nil, err
	}
	out := make([]ExecutionRead, len(ids))
	for i, id := range ids {
		x, revision, err := d.Executions.Load(ctx, tenant, id)
		if err != nil {
			return nil, err
		}
		out[i] = ExecutionRead{Execution: x, Revision: revision}
	}
	return out, nil
}

// limitsOf reads what the limit check of the held executions needs: the lots of their lines (locked is a lot the
// command holds, as it stands) and the allotment of those lots over the held executions and every other execution
// that holds live packs of them.
func (d Deps) limitsOf(ctx context.Context, tenant phy.TenantID, held []*phy.LoadExecution, locked *phy.CargoLot) (map[phy.CargoLotID]*phy.CargoLot,
	phy.Allotment, error) {
	lots := map[phy.CargoLotID]*phy.CargoLot{}
	var ids []phy.CargoLotID
	for _, x := range held {
		l, err := d.stuffedLots(ctx, tenant, x, locked)
		if err != nil {
			return nil, phy.Allotment{}, err
		}
		for id, lot := range l {
			lots[id] = lot
			if x.LiveFromLot(id) > 0 && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	others, err := d.Executions.WithLiveLots(ctx, tenant, ids)
	if err != nil {
		return nil, phy.Allotment{}, err
	}
	all := slices.Clone(held)
	for _, id := range others {
		if slices.ContainsFunc(held, func(x *phy.LoadExecution) bool { return x.ID() == id }) {
			continue
		}
		x, _, err := d.Executions.Read(ctx, tenant, id)
		if err != nil {
			return nil, phy.Allotment{}, err
		}
		all = append(all, x)
	}
	return lots, phy.AllotLots(all, lots), nil
}

// holdOverLimits checks the limits of the executions by the allotted figures and holds those that are over, with the
// evidence, actor and time of the command; it returns the ones it held, as read, to be saved.
func (d Deps) holdOverLimits(ctx context.Context, tenant phy.TenantID, consols map[phy.ConsolidationID]*phy.Consolidation, check []ExecutionRead, all []*phy.LoadExecution,
	locked *phy.CargoLot, evidence phy.Evidence, by string, at time.Time) ([]ExecutionRead, error) {
	lots, allotment, err := d.limitsOf(ctx, tenant, all, locked)
	if err != nil {
		return nil, err
	}
	var held []ExecutionRead
	for _, r := range check {
		if r.Execution.HoldIfOverLimitsAllotted(consols[r.Execution.Consolidation()], lots, allotment, evidence, by, at) {
			held = append(held, r)
		}
	}
	return held, nil
}
