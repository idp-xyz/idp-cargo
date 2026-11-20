package application

import (
	"context"
	"fmt"
	"slices"
	"time"

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type OpenLoadExecutionCommand struct {
	CommandID       string
	Scope           Scope
	ExecutionID     phy.LoadExecutionID
	ConsolidationID phy.ConsolidationID
	Kind            phy.ExecutionKind
	Facility        phy.Facility
	LoadUnitNo      int `json:",omitzero"` // the unit a STUFFING fills; left out of the hash of a tender, as before
	OccurredAt      time.Time
}

// OpenLoadExecution opens a tender of a consolidation, or the stuffing of one of its planned containers, under the
// consolidation's FOR SHARE lock. Two stuffings of one unit opened at once meet the database's one-per-unit index.
type OpenLoadExecution struct{ Deps }

func (h OpenLoadExecution) Handle(ctx context.Context, c OpenLoadExecutionCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionOpenLoadExecution, c.ConsolidationID); err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		consol, _, err := h.Consolidations.LoadShared(txCtx, c.Scope.TenantID, c.ConsolidationID)
		if err != nil {
			return Receipt{}, nil, err
		}
		var x *phy.LoadExecution
		if c.Kind == phy.ExecutionStuffing {
			uses, err := h.Executions.UnitUses(txCtx, c.Scope.TenantID, c.ConsolidationID)
			if err != nil {
				return Receipt{}, nil, err
			}
			_, taken := uses[c.LoadUnitNo]
			cancelled, err := h.Executions.LastCancellations(txCtx, c.Scope.TenantID, c.ConsolidationID)
			if err != nil {
				return Receipt{}, nil, err
			}
			x, err = phy.OpenStuffingAfter(c.Scope.TenantID, c.ExecutionID, consol, c.LoadUnitNo, c.Facility, taken, cancelled[c.LoadUnitNo],
				c.Scope.ActorID, c.OccurredAt)
			if err != nil {
				return Receipt{}, nil, err
			}
		} else {
			if c.LoadUnitNo != 0 {
				return Receipt{}, nil, fmt.Errorf("%w: a tender names no load unit", phy.ErrInvalidExecution)
			}
			if x, err = phy.OpenLoadExecution(c.Scope.TenantID, c.ExecutionID, consol, c.Kind, c.Facility, c.Scope.ActorID, c.OccurredAt); err != nil {
				return Receipt{}, nil, err
			}
		}
		x.Stamp(entered)
		revision, err := h.Executions.Insert(txCtx, x)
		return Receipt{ExecutionID: c.ExecutionID, ConsolidationID: c.ConsolidationID, NewRevision: revision, ItemNo: x.State().LoadUnitNo}, x, err
	})
}

// ExecutionCommand is the part every command on a stored load execution carries.
type ExecutionCommand struct {
	CommandID        string
	Scope            Scope
	ExecutionID      phy.LoadExecutionID
	ExpectedRevision int64
	OccurredAt       time.Time
}

// execute runs a command on a stored execution: replay check, authorization on its consolidation, and one
// transaction under the consolidation's FOR SHARE and the execution's FOR UPDATE locks.
func (d Deps) execute(ctx context.Context, e ExecutionCommand, command any, action Action,
	apply func(context.Context, *phy.Consolidation, *phy.LoadExecution) (int, error)) (Receipt, error) {
	return d.executeAlso(ctx, e, command, action, func(txCtx context.Context, c *phy.Consolidation, x *phy.LoadExecution) (int, []ExecutionRead, error) {
		item, err := apply(txCtx, c, x)
		return item, nil, err
	})
}

// executeAlso is execute for a command that may place a hold on other executions (changeExecutionAlso).
func (d Deps) executeAlso(ctx context.Context, e ExecutionCommand, command any, action Action,
	apply func(context.Context, *phy.Consolidation, *phy.LoadExecution) (int, []ExecutionRead, error)) (Receipt, error) {
	hash, entered, consolidation, stored, replayed, err := d.executionCommand(ctx, e.Scope, e.CommandID, e.OccurredAt, command, action, e.ExecutionID)
	if err != nil || replayed {
		return stored, err
	}
	return d.changeExecutionAlso(ctx, e.Scope, e.CommandID, hash, entered, consolidation, e.ExecutionID, e.ExpectedRevision, apply)
}

type StartLoadingCommand struct{ ExecutionCommand }

// StartLoading starts loading.
type StartLoading struct{ Deps }

func (h StartLoading) Handle(ctx context.Context, c StartLoadingCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionStartLoading, func(_ context.Context, _ *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		return 0, x.Start(c.Scope.ActorID, c.OccurredAt)
	})
}

type RecordCargoLoadedCommand struct {
	ExecutionCommand
	LotID       phy.CargoLotID
	Packs       int64
	GrossWeight kernel.Quantity `json:",omitzero"`
	Volume      kernel.Quantity `json:",omitzero"`
	Evidence    phy.Evidence
}

// RecordCargoLoaded records packs of a lot loaded or tendered. After the execution, the house's membership ledger is
// locked FOR NO KEY UPDATE and the lot FOR UPDATE; the lot's and the house's live loads are read under those locks, so
// loads of one house from two lots never pass its plan together. A load into a stuffing checks its container's limits
// and those of the other stuffings that hold packs of the lot (see limits.go), which are locked, after the lot, without
// waiting: a busy one makes the attempt a retryable conflict, with nothing written, and the command path runs the
// command again (Deps.retrying); the conflict reaches the caller only when the stuffing stayed busy through every
// attempt, and the caller then tries again.
type RecordCargoLoaded struct{ Deps }

func (h RecordCargoLoaded) Handle(ctx context.Context, c RecordCargoLoadedCommand) (Receipt, error) {
	return h.executeAlso(ctx, c.ExecutionCommand, c, ActionRecordCargoLoaded, func(txCtx context.Context, consol *phy.Consolidation,
		x *phy.LoadExecution) (int, []ExecutionRead, error) {
		tenant := c.Scope.TenantID
		seen, _, err := h.Lots.Read(txCtx, tenant, c.LotID)
		if err != nil {
			return 0, nil, err
		}
		m, _, err := h.Memberships.LoadForCargo(txCtx, tenant, seen.Shipment())
		if err != nil {
			return 0, nil, err
		}
		live, ok := m.Live()
		if !ok || live.ConsolidationID != consol.ID() {
			return 0, nil, phy.ErrNotAttached
		}
		lot, _, err := h.Lots.Load(txCtx, tenant, c.LotID)
		if err != nil {
			return 0, nil, err
		}
		var unit kernel.UnitCode
		if m, ok := lot.Measured(); ok {
			unit = m.Volume.Unit
		}
		lotLoaded, err := h.Lots.Loaded(txCtx, tenant, c.LotID, unit)
		if err != nil {
			return 0, nil, err
		}
		houseLoaded, err := h.Executions.HouseLoaded(txCtx, tenant, consol.ID(), lot.Shipment())
		if err != nil {
			return 0, nil, err
		}
		line := phy.LoadLine{Packs: c.Packs, GrossWeight: c.GrossWeight, Volume: c.Volume, Evidence: c.Evidence}
		var watch *limitsWatch
		if x.State().Kind == phy.ExecutionStuffing {
			if watch, err = h.watchLimits(txCtx, tenant, consol, lot, x, true); err != nil {
				return 0, nil, err
			}
		}
		if err = x.Load(line, consol, lot, live, lotLoaded, houseLoaded, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, nil, err
		}
		if watch == nil {
			return len(x.Lines()), nil, nil
		}
		others, err := watch.hold(txCtx, c.Evidence, c.Scope.ActorID, c.OccurredAt)
		return len(x.Lines()), others, err
	})
}

// stuffedLots reads the lots of a stuffing's lines for its limits check; locked is a lot the command holds already.
// A lot measured meanwhile re-checks the stuffing itself, under the stuffing's lock (RecordCargoMeasurement).
func (d Deps) stuffedLots(ctx context.Context, tenant phy.TenantID, x *phy.LoadExecution, locked *phy.CargoLot) (map[phy.CargoLotID]*phy.CargoLot, error) {
	lots := map[phy.CargoLotID]*phy.CargoLot{}
	if locked != nil {
		lots[locked.ID()] = locked
	}
	for _, line := range x.Lines() {
		if _, ok := lots[line.LotID]; ok {
			continue
		}
		l, _, err := d.Lots.Read(ctx, tenant, line.LotID)
		if err != nil {
			return nil, err
		}
		lots[line.LotID] = l
	}
	return lots, nil
}

type RecordCargoUnloadedCommand struct {
	ExecutionCommand
	LineNo   int
	Packs    int64
	Reason   phy.ReversalReason
	Evidence phy.Evidence
}

// RecordCargoUnloaded reverses packs of a load line. After the execution, the house's membership ledger is
// locked FOR NO KEY UPDATE and the line's lot FOR UPDATE, so that a lot does not close while its packs come out. A
// stuffing unloaded from checks the limits of every stuffing that holds packs of the lot, the others locked, after the
// lot, without waiting (see limits.go).
type RecordCargoUnloaded struct{ Deps }

func (h RecordCargoUnloaded) Handle(ctx context.Context, c RecordCargoUnloadedCommand) (Receipt, error) {
	return h.executeAlso(ctx, c.ExecutionCommand, c, ActionRecordCargoUnloaded, func(txCtx context.Context, consol *phy.Consolidation,
		x *phy.LoadExecution) (int, []ExecutionRead, error) {
		lines := x.Lines()
		if c.LineNo < 1 || c.LineNo > len(lines) {
			return 0, nil, phy.ErrInvalidLoad
		}
		line := lines[c.LineNo-1]
		if _, _, err := h.Memberships.LoadForCargo(txCtx, c.Scope.TenantID, line.ShipmentID); err != nil {
			return 0, nil, err
		}
		lot, _, err := h.Lots.Load(txCtx, c.Scope.TenantID, line.LotID)
		if err != nil {
			return 0, nil, err
		}
		var watch *limitsWatch
		if x.State().Kind == phy.ExecutionStuffing {
			if watch, err = h.watchLimits(txCtx, c.Scope.TenantID, consol, lot, x, false); err != nil {
				return 0, nil, err
			}
		}
		if err = x.Unload(c.LineNo, c.Packs, c.Reason, c.Evidence, lot, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, nil, err
		}
		if watch == nil {
			return len(x.State().Reversals), nil, nil
		}
		others, err := watch.hold(txCtx, c.Evidence, c.Scope.ActorID, c.OccurredAt)
		return len(x.State().Reversals), others, err
	})
}

type PlaceLoadExecutionOnHoldCommand struct {
	ExecutionCommand
	Reason   phy.HoldReason
	Text     string
	Evidence phy.Evidence
}

// PlaceLoadExecutionOnHold holds an execution for a reason an operator gives.
type PlaceLoadExecutionOnHold struct{ Deps }

func (h PlaceLoadExecutionOnHold) Handle(ctx context.Context, c PlaceLoadExecutionOnHoldCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionPlaceLoadExecutionOnHold, func(_ context.Context, _ *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		if err := x.PlaceHold(c.Reason, c.Text, c.Evidence, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(x.State().Holds), nil
	})
}

type ReleaseLoadExecutionHoldCommand struct {
	ExecutionCommand
	HoldNo   int
	Evidence phy.Evidence
}

// ReleaseLoadExecutionHold releases a hold; a screening hold is released against the lot's latest screening,
// read under the lot's lock.
type ReleaseLoadExecutionHold struct{ Deps }

func (h ReleaseLoadExecutionHold) Handle(ctx context.Context, c ReleaseLoadExecutionHoldCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionReleaseLoadExecutionHold, func(txCtx context.Context, _ *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		var outcome phy.Outcome
		if holds := x.State().Holds; c.HoldNo >= 1 && c.HoldNo <= len(holds) && holds[c.HoldNo-1].LotID != "" {
			lot, _, err := h.Lots.Load(txCtx, c.Scope.TenantID, holds[c.HoldNo-1].LotID)
			if err != nil {
				return 0, err
			}
			outcome = lot.Screened()
		}
		return c.HoldNo, x.ReleaseHold(c.HoldNo, c.Evidence, outcome, c.Scope.ActorID, c.OccurredAt)
	})
}

type CloseLoadExecutionCommand struct {
	ExecutionCommand
	Evidence phy.Evidence
}

// CloseLoadExecution closes a tender with the receipt of the airline terminal or the co-loader's CFS, or seals a
// stuffing while its consolidation, locked FOR SHARE, is CLOSED.
type CloseLoadExecution struct{ Deps }

func (h CloseLoadExecution) Handle(ctx context.Context, c CloseLoadExecutionCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionCloseLoadExecution, func(_ context.Context, consol *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		if x.State().Kind == phy.ExecutionStuffing {
			return 0, x.Seal(consol, c.Evidence, c.Scope.ActorID, c.OccurredAt)
		}
		return 0, x.Close(c.Evidence, c.Scope.ActorID, c.OccurredAt)
	})
}

type CancelLoadExecutionCommand struct {
	ExecutionCommand
	Reason string
}

// CancelLoadExecution gives up a stuffing that holds no live packs, under the consolidation's FOR SHARE lock and the
// execution's FOR UPDATE lock: a load into the stuffing and its cancellation wait for each other, so a cancelled
// stuffing never holds a live pack, and an opening of a second stuffing on the unit reads the stuffing as it is once
// this commits.
type CancelLoadExecution struct{ Deps }

func (h CancelLoadExecution) Handle(ctx context.Context, c CancelLoadExecutionCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionCancelLoadExecution, func(_ context.Context, _ *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		return 0, x.Cancel(c.Reason, c.Scope.ActorID, c.OccurredAt)
	})
}

type OpenStuffingForReworkCommand struct {
	ExecutionCommand
	Reason string
}

// OpenStuffingForRework opens a sealed stuffing for rework, under the consolidation's FOR SHARE lock and the
// execution's FOR UPDATE lock, so that it and a carrier evidence recorded on the same execution wait for each other:
// whichever comes second reads what the first committed (a container with carrier evidence is not opened, and one in
// rework takes none). The execution, its carrier evidence and its consolidation are read inside the transaction. The
// receipt's item is the number of the rework.
type OpenStuffingForRework struct{ Deps }

func (h OpenStuffingForRework) Handle(ctx context.Context, c OpenStuffingForReworkCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionOpenStuffingForRework, func(_ context.Context, consol *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		if err := x.OpenForRework(consol, c.Reason, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(x.State().Reworks), nil
	})
}

type UnloadHouseForReworkCommand struct {
	ExecutionCommand
	ShipmentID phy.ShipmentID
	Evidence   phy.Evidence
}

// UnloadHouseForRework unloads one whole house from a container in rework. It locks, in the order of the package, the
// consolidation FOR SHARE, the execution FOR UPDATE, the house's membership ledger FOR NO KEY UPDATE and the house's
// lots FOR UPDATE in lot id order, and reads inside the transaction, under those locks, which lines of the house are
// live in the execution, the house's live packs in the consolidation (a house with packs in another container is
// refused) and the lots. A load of the house in another container, which takes the same ledger lock, and this command
// wait for each other: the one that gets the locks first is seen by the other. A lot of the house that is CLOSED is
// opened again first, saved before the execution's reversals, and its events come before the execution's; a lot is
// refused its reopening while the shipment has another lot in force at its facility (one lot in force per shipment and
// facility). The container's other stuffings are not locked or held: the house's lots hold packs in this container
// only, so no other stuffing's allotment moves. The receipt's item is the number of lines it reversed.
type UnloadHouseForRework struct{ Deps }

func (h UnloadHouseForRework) Handle(ctx context.Context, c UnloadHouseForReworkCommand) (Receipt, error) {
	hash, entered, consolidation, stored, replayed, err := h.executionCommand(ctx, c.Scope, c.CommandID, c.OccurredAt, c, ActionUnloadHouseForRework, c.ExecutionID)
	if err != nil || replayed {
		return stored, err
	}
	tenant := c.Scope.TenantID
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		consol, _, err := h.Consolidations.LoadShared(txCtx, tenant, consolidation)
		if err != nil {
			return Receipt{}, nil, err
		}
		x, revision, err := h.Executions.Load(txCtx, tenant, c.ExecutionID)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, phy.ErrConflict
		}
		if _, _, err = h.Memberships.LoadForCargo(txCtx, tenant, c.ShipmentID); err != nil {
			return Receipt{}, nil, err
		}
		var ids []phy.CargoLotID
		for _, line := range x.Lines() {
			if live, _, _ := x.LineLive(line.No); live > 0 && line.ShipmentID == c.ShipmentID && !slices.Contains(ids, line.LotID) {
				ids = append(ids, line.LotID)
			}
		}
		slices.Sort(ids)
		lots := map[phy.CargoLotID]*phy.CargoLot{}
		revisions := map[phy.CargoLotID]int64{}
		for _, id := range ids {
			if lots[id], revisions[id], err = h.Lots.Load(txCtx, tenant, id); err != nil {
				return Receipt{}, nil, err
			}
		}
		houseLoaded, err := h.Executions.HouseLoaded(txCtx, tenant, consol.ID(), c.ShipmentID)
		if err != nil {
			return Receipt{}, nil, err
		}
		for _, id := range ids {
			if err = h.lotCanOpen(txCtx, tenant, lots[id]); err != nil {
				return Receipt{}, nil, err
			}
		}
		n, err := x.UnloadHouse(c.ShipmentID, lots, houseLoaded-x.LiveFor(c.ShipmentID), c.Evidence, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		changed := group{}
		for _, id := range ids {
			if len(lots[id].AddedReopens()) == 0 {
				continue
			}
			lots[id].Stamp(entered)
			if _, err = h.Lots.Save(txCtx, lots[id], revisions[id]); err != nil {
				return Receipt{}, nil, err
			}
			changed = append(changed, lots[id])
		}
		x.Stamp(entered)
		if revision, err = h.Executions.Save(txCtx, x, revision); err != nil {
			return Receipt{}, nil, err
		}
		changed = append(changed, x)
		return Receipt{ExecutionID: c.ExecutionID, ConsolidationID: consolidation, NewRevision: revision, ItemNo: n}, changed, nil
	})
}

// lotCanOpen refuses the reopening of a closed lot while the shipment has another lot in force at the lot's facility: a
// shipment has one lot in force per facility (the database's cargo_lot_one_live). The other lots are read as committed;
// a lot received after this read meets the database's index, which refuses the later of the two.
func (d Deps) lotCanOpen(ctx context.Context, tenant phy.TenantID, lot *phy.CargoLot) error {
	if lot.Current() != phy.LotClosed {
		return nil
	}
	others, err := d.Lots.ByShipment(ctx, tenant, lot.Shipment())
	if err != nil {
		return err
	}
	for _, o := range others {
		if o.Lot.ID() != lot.ID() && o.Lot.Current() != phy.LotClosed && o.Lot.State().Facility == lot.State().Facility {
			return fmt.Errorf("%w: lot %s of %s is in force at %s %s, so lot %s is not opened again", phy.ErrLotInForce, o.Lot.ID(), lot.Shipment(),
				lot.State().Facility.Location.Code, lot.State().Facility.Code, lot.ID())
		}
	}
	return nil
}

type RecordCarrierLoadEvidenceCommand struct {
	ExecutionCommand
	Evidence phy.CarrierEvidence
}

// RecordCarrierLoadEvidence records the carrier's status of a closed tender (OD-4 C: entered by hand in N1).
type RecordCarrierLoadEvidence struct{ Deps }

func (h RecordCarrierLoadEvidence) Handle(ctx context.Context, c RecordCarrierLoadEvidenceCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionRecordCarrierLoadEvidence, func(_ context.Context, _ *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		no, _, err := x.RecordCarrierEvidence(c.Evidence, c.Scope.ActorID, c.OccurredAt)
		return no, err
	})
}

type ResolveLoadingDiscrepancyCommand struct {
	ExecutionCommand
	DiscrepancyNo int
	Resolution    phy.Resolution
	Reference     string
	Reason        string
}

// ResolveLoadingDiscrepancy resolves an open loading discrepancy.
type ResolveLoadingDiscrepancy struct{ Deps }

func (h ResolveLoadingDiscrepancy) Handle(ctx context.Context, c ResolveLoadingDiscrepancyCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionResolveLoadingDiscrepancy, func(_ context.Context, _ *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		return c.DiscrepancyNo, x.ResolveDiscrepancy(c.DiscrepancyNo, c.Resolution, c.Reference, c.Reason, c.Scope.ActorID, c.OccurredAt)
	})
}
