package application

import (
	"context"
	"fmt"
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
			x, err = phy.OpenStuffing(c.Scope.TenantID, c.ExecutionID, consol, c.LoadUnitNo, c.Facility, taken, c.Scope.ActorID, c.OccurredAt)
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
		return Receipt{ExecutionID: c.ExecutionID, ConsolidationID: c.ConsolidationID, NewRevision: revision}, x, err
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
	hash, entered, consolidation, stored, replayed, err := d.executionCommand(ctx, e.Scope, e.CommandID, e.OccurredAt, command, action, e.ExecutionID)
	if err != nil || replayed {
		return stored, err
	}
	return d.changeExecution(ctx, e.Scope, e.CommandID, hash, entered, consolidation, e.ExecutionID, e.ExpectedRevision, apply)
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

// RecordCargoLoaded records packs of a lot loaded or tendered. After the execution, the house's membership
// ledger is locked FOR NO KEY UPDATE and the lot FOR UPDATE; the lot's and the house's live loads are read under
// those locks, so loads of one house from two lots never pass its plan together.
type RecordCargoLoaded struct{ Deps }

func (h RecordCargoLoaded) Handle(ctx context.Context, c RecordCargoLoadedCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionRecordCargoLoaded, func(txCtx context.Context, consol *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		tenant := c.Scope.TenantID
		seen, _, err := h.Lots.Read(txCtx, tenant, c.LotID)
		if err != nil {
			return 0, err
		}
		m, _, err := h.Memberships.LoadForCargo(txCtx, tenant, seen.Shipment())
		if err != nil {
			return 0, err
		}
		live, ok := m.Live()
		if !ok || live.ConsolidationID != consol.ID() {
			return 0, phy.ErrNotAttached
		}
		lot, _, err := h.Lots.Load(txCtx, tenant, c.LotID)
		if err != nil {
			return 0, err
		}
		var unit kernel.UnitCode
		if m, ok := lot.Measured(); ok {
			unit = m.Volume.Unit
		}
		lotLoaded, err := h.Lots.Loaded(txCtx, tenant, c.LotID, unit)
		if err != nil {
			return 0, err
		}
		houseLoaded, err := h.Executions.HouseLoaded(txCtx, tenant, consol.ID(), lot.Shipment())
		if err != nil {
			return 0, err
		}
		line := phy.LoadLine{Packs: c.Packs, GrossWeight: c.GrossWeight, Volume: c.Volume, Evidence: c.Evidence}
		if err = x.Load(line, consol, lot, live, lotLoaded, houseLoaded, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(x.Lines()), nil
	})
}

type RecordCargoUnloadedCommand struct {
	ExecutionCommand
	LineNo   int
	Packs    int64
	Reason   phy.ReversalReason
	Evidence phy.Evidence
}

// RecordCargoUnloaded reverses packs of a load line. After the execution, the house's membership ledger is
// locked FOR NO KEY UPDATE and the line's lot FOR UPDATE, so that a lot does not close while its packs come out.
type RecordCargoUnloaded struct{ Deps }

func (h RecordCargoUnloaded) Handle(ctx context.Context, c RecordCargoUnloadedCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionRecordCargoUnloaded, func(txCtx context.Context, _ *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		lines := x.Lines()
		if c.LineNo < 1 || c.LineNo > len(lines) {
			return 0, phy.ErrInvalidLoad
		}
		line := lines[c.LineNo-1]
		if _, _, err := h.Memberships.LoadForCargo(txCtx, c.Scope.TenantID, line.ShipmentID); err != nil {
			return 0, err
		}
		lot, _, err := h.Lots.Load(txCtx, c.Scope.TenantID, line.LotID)
		if err != nil {
			return 0, err
		}
		if err = x.Unload(c.LineNo, c.Packs, c.Reason, c.Evidence, lot, c.Scope.ActorID, c.OccurredAt); err != nil {
			return 0, err
		}
		return len(x.State().Reversals), nil
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

// CloseLoadExecution closes a tender with the receipt of the airline terminal or the co-loader's CFS.
type CloseLoadExecution struct{ Deps }

func (h CloseLoadExecution) Handle(ctx context.Context, c CloseLoadExecutionCommand) (Receipt, error) {
	return h.execute(ctx, c.ExecutionCommand, c, ActionCloseLoadExecution, func(_ context.Context, _ *phy.Consolidation, x *phy.LoadExecution) (int, error) {
		return 0, x.Close(c.Evidence, c.Scope.ActorID, c.OccurredAt)
	})
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
