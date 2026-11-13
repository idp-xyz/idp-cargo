package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

type ReceiveCargoCommand struct {
	CommandID        string
	Scope            Scope
	LotID            phy.CargoLotID
	ShipmentID       phy.ShipmentID
	Facility         phy.Facility
	ExpectedRevision int64 // 0 opens the lot
	Line             phy.ReceiptLine
	OccurredAt       time.Time
}

// ReceiveCargo opens a cargo lot with its first receipt line, after FUL reported the shipment PLANNED (read
// before PHY's transaction, T3), or adds a receipt line to a receiving lot.
type ReceiveCargo struct {
	Deps
	Intents LoadIntents
}

func (h ReceiveCargo) Handle(ctx context.Context, c ReceiveCargoCommand) (Receipt, error) {
	if h.Intents == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorizeLot(ctx, c.Scope, ActionReceiveCargo, c.LotID); err != nil {
		return Receipt{}, err
	}
	if c.ExpectedRevision > 0 {
		return h.changeLot(ctx, c.Scope, c.CommandID, hash, entered, c.LotID, c.ExpectedRevision, func(_ context.Context, l *phy.CargoLot) (int, error) {
			st := l.State()
			if st.ShipmentID != c.ShipmentID || st.Facility != c.Facility {
				return 0, fmt.Errorf("%w: lot %s holds %s at %s %s", phy.ErrInvalidLot, c.LotID, st.ShipmentID, st.Facility.Location.Code, st.Facility.Code)
			}
			if err := l.Receive(c.Line, c.Scope.ActorID, c.OccurredAt); err != nil {
				return 0, err
			}
			return len(l.State().Lines), nil
		})
	}
	intent, err := h.Intents.LoadIntent(ctx, c.Scope.TenantID, c.ShipmentID)
	if err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		l, err := phy.ReceiveCargo(c.Scope.TenantID, c.LotID, c.Facility, intent, c.Line, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		l.Stamp(entered)
		revision, err := h.Lots.Insert(txCtx, l)
		return Receipt{LotID: c.LotID, NewRevision: revision, ItemNo: 1}, l, err
	})
}

type RecordCargoMeasurementCommand struct {
	CommandID        string
	Scope            Scope
	LotID            phy.CargoLotID
	ExpectedRevision int64
	Measurement      phy.Measurement
	OccurredAt       time.Time
}

// RecordCargoMeasurement adds a measurement version of a lot. A stuffing loading or on hold with live packs of the
// lot is checked again against its container's limits with the new measure (HoldIfOverLimits): their consolidations
// are locked FOR SHARE and the stuffings FOR UPDATE, in id order, before the lot, as RecordScreeningResult does;
// tenders are not locked. If the stuffings with live packs of the lot changed meanwhile, the command is a conflict
// and the caller tries again.
// A lot measured again after its container was sealed changes the measures PHY publishes; the container's verified
// gross mass is SEA's (VGM).
type RecordCargoMeasurement struct{ Deps }

func (h RecordCargoMeasurement) Handle(ctx context.Context, c RecordCargoMeasurementCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorizeLot(ctx, c.Scope, ActionRecordCargoMeasurement, c.LotID); err != nil {
		return Receipt{}, err
	}
	var live []phy.LoadExecutionID
	consolidations := map[phy.LoadExecutionID]phy.ConsolidationID{}
	if err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		if live, err = h.Executions.StuffingsWithLiveLot(txCtx, c.Scope.TenantID, c.LotID); err != nil {
			return err
		}
		for _, id := range live {
			if consolidations[id], err = h.Executions.Consolidation(txCtx, c.Scope.TenantID, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return Receipt{}, err
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		tenant := c.Scope.TenantID
		var ids []phy.ConsolidationID
		for _, id := range live {
			ids = append(ids, consolidations[id])
		}
		slices.Sort(ids)
		consols := map[phy.ConsolidationID]*phy.Consolidation{}
		for _, id := range slices.Compact(ids) {
			consol, _, err := h.Consolidations.LoadShared(txCtx, tenant, id)
			if err != nil {
				return Receipt{}, nil, err
			}
			consols[id] = consol
		}
		var executions []ExecutionRead
		for _, id := range live {
			x, revision, err := h.Executions.Load(txCtx, tenant, id)
			if err != nil {
				return Receipt{}, nil, err
			}
			executions = append(executions, ExecutionRead{Execution: x, Revision: revision})
		}
		l, revision, err := h.Lots.Load(txCtx, tenant, c.LotID)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, phy.ErrConflict
		}
		if err = l.Measure(c.Measurement, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, nil, err
		}
		changed := group{l}
		if len(live) > 0 {
			now, err := h.Executions.StuffingsWithLiveLot(txCtx, tenant, c.LotID)
			if err != nil {
				return Receipt{}, nil, err
			}
			if !slices.Equal(now, live) {
				return Receipt{}, nil, fmt.Errorf("%w: the stuffings loading lot %s changed; try again", phy.ErrConflict, c.LotID)
			}
		}
		for _, r := range executions {
			x := r.Execution
			if x.State().Kind != phy.ExecutionStuffing {
				continue
			}
			lots, err := h.stuffedLots(txCtx, tenant, x, l)
			if err != nil {
				return Receipt{}, nil, err
			}
			if x.HoldIfOverLimits(consols[x.Consolidation()], lots, c.Measurement.Evidence, c.Scope.ActorID, c.OccurredAt) {
				x.Stamp(entered)
				if _, err = h.Executions.Save(txCtx, x, r.Revision); err != nil {
					return Receipt{}, nil, err
				}
				changed = append(changed, x)
			}
		}
		l.Stamp(entered)
		revision, err = h.Lots.Save(txCtx, l, revision)
		return Receipt{LotID: c.LotID, NewRevision: revision, ItemNo: len(l.State().Measurements)}, changed, err
	})
}

type RecordScreeningResultCommand struct {
	CommandID        string
	Scope            Scope
	LotID            phy.CargoLotID
	ExpectedRevision int64
	Screening        phy.Screening
	OccurredAt       time.Time
}

// RecordScreeningResult records a screening result of an AIR lot. A FAILED result holds every loading
// execution with live packs of the lot: their consolidation is locked FOR SHARE and the executions FOR UPDATE,
// in id order, before the lot; if the executions with live packs of the lot changed meanwhile, the command is a
// conflict and the caller tries again.
type RecordScreeningResult struct{ Deps }

func (h RecordScreeningResult) Handle(ctx context.Context, c RecordScreeningResultCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorizeLot(ctx, c.Scope, ActionRecordScreeningResult, c.LotID); err != nil {
		return Receipt{}, err
	}
	var held []phy.LoadExecutionID
	consolidations := map[phy.LoadExecutionID]phy.ConsolidationID{}
	if c.Screening.Outcome == phy.ScreeningFailed {
		if err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
			if held, err = h.Executions.WithLiveLot(txCtx, c.Scope.TenantID, c.LotID); err != nil {
				return err
			}
			for _, id := range held {
				if consolidations[id], err = h.Executions.Consolidation(txCtx, c.Scope.TenantID, id); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return Receipt{}, err
		}
	}
	return h.write(ctx, c.Scope, c.CommandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		var locked []phy.ConsolidationID
		for _, id := range held {
			locked = append(locked, consolidations[id])
		}
		slices.Sort(locked)
		for _, id := range slices.Compact(locked) {
			if _, _, err := h.Consolidations.LoadShared(txCtx, c.Scope.TenantID, id); err != nil {
				return Receipt{}, nil, err
			}
		}
		var executions []ExecutionRead
		for _, id := range held {
			x, revision, err := h.Executions.Load(txCtx, c.Scope.TenantID, id)
			if err != nil {
				return Receipt{}, nil, err
			}
			executions = append(executions, ExecutionRead{Execution: x, Revision: revision})
		}
		l, revision, err := h.Lots.Load(txCtx, c.Scope.TenantID, c.LotID)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, phy.ErrConflict
		}
		if err = l.Screen(c.Screening, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, nil, err
		}
		changed := group{l}
		if c.Screening.Outcome == phy.ScreeningFailed {
			now, err := h.Executions.WithLiveLot(txCtx, c.Scope.TenantID, c.LotID)
			if err != nil {
				return Receipt{}, nil, err
			}
			if !slices.Equal(now, held) {
				return Receipt{}, nil, fmt.Errorf("%w: the executions loading lot %s changed; try again", phy.ErrConflict, c.LotID)
			}
			s := l.State().Screenings
			for _, x := range executions {
				if x.Execution.HoldForScreening(c.LotID, s[len(s)-1].Evidence, c.Scope.ActorID, c.OccurredAt) {
					x.Execution.Stamp(entered)
					if _, err = h.Executions.Save(txCtx, x.Execution, x.Revision); err != nil {
						return Receipt{}, nil, err
					}
					changed = append(changed, x.Execution)
				}
			}
		}
		l.Stamp(entered)
		revision, err = h.Lots.Save(txCtx, l, revision)
		return Receipt{LotID: c.LotID, NewRevision: revision, ItemNo: len(l.State().Screenings)}, changed, err
	})
}

type CompleteReceiptCommand struct {
	CommandID        string
	Scope            Scope
	LotID            phy.CargoLotID
	ExpectedRevision int64
	OccurredAt       time.Time
}

// CompleteReceipt ends receiving against FUL's current LoadIntent v1, read before PHY's transaction (T3).
type CompleteReceipt struct {
	Deps
	Intents LoadIntents
}

func (h CompleteReceipt) Handle(ctx context.Context, c CompleteReceiptCommand) (Receipt, error) {
	if h.Intents == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorizeLot(ctx, c.Scope, ActionCompleteReceipt, c.LotID); err != nil {
		return Receipt{}, err
	}
	intent, err := h.intentOfLot(ctx, h.Intents, c.Scope.TenantID, c.LotID)
	if err != nil {
		return Receipt{}, err
	}
	return h.changeLot(ctx, c.Scope, c.CommandID, hash, entered, c.LotID, c.ExpectedRevision, func(_ context.Context, l *phy.CargoLot) (int, error) {
		return 0, l.Complete(intent, c.Scope.ActorID, c.OccurredAt)
	})
}

// intentOfLot reads FUL's LoadIntent v1 of a lot's shipment, outside PHY's transactions.
func (d Deps) intentOfLot(ctx context.Context, intents LoadIntents, tenant phy.TenantID, id phy.CargoLotID) (phy.LoadIntent, error) {
	var shipment phy.ShipmentID
	if err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		l, _, err := d.Lots.Read(txCtx, tenant, id)
		if err == nil {
			shipment = l.Shipment()
		}
		return err
	}); err != nil {
		return phy.LoadIntent{}, err
	}
	return intents.LoadIntent(ctx, tenant, shipment)
}

type ResolveCargoDiscrepancyCommand struct {
	CommandID        string
	Scope            Scope
	LotID            phy.CargoLotID
	ExpectedRevision int64
	DiscrepancyNo    int
	Resolution       phy.Resolution
	Reference        string
	Reason           string
	OccurredAt       time.Time
}

// ResolveCargoDiscrepancy resolves an open discrepancy of a lot; FUL_REPLANNED reads FUL's LoadIntent v1 first.
type ResolveCargoDiscrepancy struct {
	Deps
	Intents LoadIntents
}

func (h ResolveCargoDiscrepancy) Handle(ctx context.Context, c ResolveCargoDiscrepancyCommand) (Receipt, error) {
	if h.Intents == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorizeLot(ctx, c.Scope, ActionResolveCargoDiscrepancy, c.LotID); err != nil {
		return Receipt{}, err
	}
	var intent phy.LoadIntent
	if c.Resolution == phy.ResolvedByReplan {
		if intent, err = h.intentOfLot(ctx, h.Intents, c.Scope.TenantID, c.LotID); err != nil {
			return Receipt{}, err
		}
	}
	return h.changeLot(ctx, c.Scope, c.CommandID, hash, entered, c.LotID, c.ExpectedRevision, func(_ context.Context, l *phy.CargoLot) (int, error) {
		return c.DiscrepancyNo, l.Resolve(c.DiscrepancyNo, c.Resolution, c.Reference, c.Reason, intent, c.Scope.ActorID, c.OccurredAt)
	})
}

type CloseCargoLotCommand struct {
	CommandID        string
	Scope            Scope
	LotID            phy.CargoLotID
	ExpectedRevision int64
	OccurredAt       time.Time
}

// CloseCargoLot closes a received lot once its packs are loaded or accounted for by FUL's LoadIntent v1, read
// first; the lot's and the house's live loaded packs are read under the house's ledger lock (FOR NO KEY UPDATE)
// and the lot's, which loads and reversals of the house take too.
type CloseCargoLot struct {
	Deps
	Intents LoadIntents
}

func (h CloseCargoLot) Handle(ctx context.Context, c CloseCargoLotCommand) (Receipt, error) {
	if h.Intents == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorizeLot(ctx, c.Scope, ActionCloseCargoLot, c.LotID); err != nil {
		return Receipt{}, err
	}
	intent, err := h.intentOfLot(ctx, h.Intents, c.Scope.TenantID, c.LotID)
	if err != nil {
		return Receipt{}, err
	}
	var m *phy.Membership
	lockHouse := func(txCtx context.Context) error {
		var err error
		if m, _, err = h.Memberships.LoadForCargo(txCtx, c.Scope.TenantID, intent.ShipmentID); errors.Is(err, phy.ErrNotAttached) {
			m, err = nil, nil
		}
		return err
	}
	return h.changeLotAfter(ctx, c.Scope, c.CommandID, hash, entered, c.LotID, c.ExpectedRevision, lockHouse, func(txCtx context.Context, l *phy.CargoLot) (int, error) {
		loaded, err := h.Lots.Loaded(txCtx, c.Scope.TenantID, c.LotID, "")
		if err != nil {
			return 0, err
		}
		var house int64
		if m != nil {
			if live, ok := m.Live(); ok {
				if house, err = h.Executions.HouseLoaded(txCtx, c.Scope.TenantID, live.ConsolidationID, l.Shipment()); err != nil {
					return 0, err
				}
			}
		}
		return 0, l.Close(loaded.Packs, house, intent, c.Scope.ActorID, c.OccurredAt)
	})
}
