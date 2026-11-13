package phy

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PHY V0.2: cargo lots and tender load executions.

func facility(f api.Facility) domain.Facility {
	return domain.Facility{Location: f.Location, Code: f.Code}
}

func evidence(e api.Evidence) domain.Evidence {
	return domain.Evidence{Source: e.Source, Reference: e.Reference, SHA256: e.SHA256, OccurredAt: stored(e.OccurredAt)}
}

func (m *Module) ReceiveCargo(ctx context.Context, cmd api.ReceiveCargo) (api.Receipt, error) {
	r, err := application.ReceiveCargo{Deps: m.deps(cmd.TenantID, cmd.CommandID), Intents: m.intents}.Handle(ctx,
		application.ReceiveCargoCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), LotID: domain.CargoLotID(cmd.LotID),
			ShipmentID: domain.ShipmentID(cmd.ShipmentID), Facility: facility(cmd.Facility), ExpectedRevision: cmd.ExpectedRevision,
			Line:       domain.ReceiptLine{Packs: cmd.Packs, PackageType: cmd.PackageType, Condition: domain.Condition(cmd.Condition), Evidence: evidence(cmd.Evidence)},
			OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) RecordCargoMeasurement(ctx context.Context, cmd api.RecordCargoMeasurement) (api.Receipt, error) {
	ms := domain.Measurement{GrossWeight: cmd.GrossWeight, Volume: cmd.Volume, Method: domain.Method(cmd.Method), Evidence: evidence(cmd.Evidence)}
	for _, g := range cmd.PackageGroups {
		ms.Groups = append(ms.Groups, domain.PackageGroup{Count: g.Count, PackageType: g.PackageType, LengthMM: g.LengthMM, WidthMM: g.WidthMM, HeightMM: g.HeightMM})
	}
	r, err := application.RecordCargoMeasurement{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx,
		application.RecordCargoMeasurementCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), LotID: domain.CargoLotID(cmd.LotID),
			ExpectedRevision: cmd.ExpectedRevision, Measurement: ms, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) RecordScreeningResult(ctx context.Context, cmd api.RecordScreeningResult) (api.Receipt, error) {
	r, err := application.RecordScreeningResult{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx,
		application.RecordScreeningResultCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), LotID: domain.CargoLotID(cmd.LotID),
			ExpectedRevision: cmd.ExpectedRevision, Screening: domain.Screening{Outcome: domain.Outcome(cmd.Outcome), Screener: cmd.Screener,
				Method: cmd.Method, Reference: cmd.Reference, Evidence: evidence(cmd.Evidence)}, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) CompleteReceipt(ctx context.Context, cmd api.CompleteReceipt) (api.Receipt, error) {
	r, err := application.CompleteReceipt{Deps: m.deps(cmd.TenantID, cmd.CommandID), Intents: m.intents}.Handle(ctx,
		application.CompleteReceiptCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), LotID: domain.CargoLotID(cmd.LotID),
			ExpectedRevision: cmd.ExpectedRevision, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) ResolveCargoDiscrepancy(ctx context.Context, cmd api.ResolveCargoDiscrepancy) (api.Receipt, error) {
	r, err := application.ResolveCargoDiscrepancy{Deps: m.deps(cmd.TenantID, cmd.CommandID), Intents: m.intents}.Handle(ctx,
		application.ResolveCargoDiscrepancyCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), LotID: domain.CargoLotID(cmd.LotID),
			ExpectedRevision: cmd.ExpectedRevision, DiscrepancyNo: cmd.DiscrepancyNo, Resolution: domain.Resolution(cmd.Resolution),
			Reference: cmd.Reference, Reason: cmd.Reason, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) CloseCargoLot(ctx context.Context, cmd api.CloseCargoLot) (api.Receipt, error) {
	r, err := application.CloseCargoLot{Deps: m.deps(cmd.TenantID, cmd.CommandID), Intents: m.intents}.Handle(ctx,
		application.CloseCargoLotCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), LotID: domain.CargoLotID(cmd.LotID),
			ExpectedRevision: cmd.ExpectedRevision, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) OpenLoadExecution(ctx context.Context, cmd api.OpenLoadExecution) (api.Receipt, error) {
	r, err := application.OpenLoadExecution{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx,
		application.OpenLoadExecutionCommand{CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID),
			ExecutionID: domain.LoadExecutionID(cmd.ExecutionID), ConsolidationID: domain.ConsolidationID(cmd.ConsolidationID),
			Kind: domain.ExecutionKind(cmd.Kind), Facility: facility(cmd.Facility), LoadUnitNo: cmd.LoadUnitNo, OccurredAt: stored(cmd.OccurredAt)})
	return toReceipt(r, err)
}

func executionCommand(commandID, tenant, actor, execution string, expected int64, at time.Time) application.ExecutionCommand {
	return application.ExecutionCommand{CommandID: commandID, Scope: scope(tenant, actor), ExecutionID: domain.LoadExecutionID(execution),
		ExpectedRevision: expected, OccurredAt: stored(at)}
}

func (m *Module) StartLoading(ctx context.Context, cmd api.StartLoading) (api.Receipt, error) {
	r, err := application.StartLoading{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.StartLoadingCommand{
		ExecutionCommand: executionCommand(cmd.CommandID, cmd.TenantID, cmd.ActorID, cmd.ExecutionID, cmd.ExpectedRevision, cmd.OccurredAt)})
	return toReceipt(r, err)
}

func (m *Module) RecordCargoLoaded(ctx context.Context, cmd api.RecordCargoLoaded) (api.Receipt, error) {
	r, err := application.RecordCargoLoaded{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.RecordCargoLoadedCommand{
		ExecutionCommand: executionCommand(cmd.CommandID, cmd.TenantID, cmd.ActorID, cmd.ExecutionID, cmd.ExpectedRevision, cmd.OccurredAt),
		LotID:            domain.CargoLotID(cmd.LotID), Packs: cmd.Packs, GrossWeight: cmd.GrossWeight, Volume: cmd.Volume, Evidence: evidence(cmd.Evidence)})
	return toReceipt(r, err)
}

func (m *Module) RecordCargoUnloaded(ctx context.Context, cmd api.RecordCargoUnloaded) (api.Receipt, error) {
	r, err := application.RecordCargoUnloaded{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.RecordCargoUnloadedCommand{
		ExecutionCommand: executionCommand(cmd.CommandID, cmd.TenantID, cmd.ActorID, cmd.ExecutionID, cmd.ExpectedRevision, cmd.OccurredAt),
		LineNo:           cmd.LineNo, Packs: cmd.Packs, Reason: domain.ReversalReason(cmd.Reason), Evidence: evidence(cmd.Evidence)})
	return toReceipt(r, err)
}

func (m *Module) PlaceLoadExecutionOnHold(ctx context.Context, cmd api.PlaceLoadExecutionOnHold) (api.Receipt, error) {
	r, err := application.PlaceLoadExecutionOnHold{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.PlaceLoadExecutionOnHoldCommand{
		ExecutionCommand: executionCommand(cmd.CommandID, cmd.TenantID, cmd.ActorID, cmd.ExecutionID, cmd.ExpectedRevision, cmd.OccurredAt),
		Reason:           domain.HoldReason(cmd.Reason), Text: cmd.Text, Evidence: evidence(cmd.Evidence)})
	return toReceipt(r, err)
}

func (m *Module) ReleaseLoadExecutionHold(ctx context.Context, cmd api.ReleaseLoadExecutionHold) (api.Receipt, error) {
	r, err := application.ReleaseLoadExecutionHold{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.ReleaseLoadExecutionHoldCommand{
		ExecutionCommand: executionCommand(cmd.CommandID, cmd.TenantID, cmd.ActorID, cmd.ExecutionID, cmd.ExpectedRevision, cmd.OccurredAt),
		HoldNo:           cmd.HoldNo, Evidence: evidence(cmd.Evidence)})
	return toReceipt(r, err)
}

func (m *Module) CloseLoadExecution(ctx context.Context, cmd api.CloseLoadExecution) (api.Receipt, error) {
	r, err := application.CloseLoadExecution{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.CloseLoadExecutionCommand{
		ExecutionCommand: executionCommand(cmd.CommandID, cmd.TenantID, cmd.ActorID, cmd.ExecutionID, cmd.ExpectedRevision, cmd.OccurredAt),
		Evidence:         evidence(cmd.Evidence)})
	return toReceipt(r, err)
}

func (m *Module) RecordCarrierLoadEvidence(ctx context.Context, cmd api.RecordCarrierLoadEvidence) (api.Receipt, error) {
	r, err := application.RecordCarrierLoadEvidence{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.RecordCarrierLoadEvidenceCommand{
		ExecutionCommand: executionCommand(cmd.CommandID, cmd.TenantID, cmd.ActorID, cmd.ExecutionID, cmd.ExpectedRevision, cmd.OccurredAt),
		Evidence: domain.CarrierEvidence{Fact: domain.CarrierFact(cmd.Fact), ShipmentID: domain.ShipmentID(cmd.ShipmentID), Pieces: cmd.Pieces,
			GrossWeight: cmd.GrossWeight, Transport: cmd.Transport, Evidence: evidence(cmd.Evidence)}})
	return toReceipt(r, err)
}

func (m *Module) ResolveLoadingDiscrepancy(ctx context.Context, cmd api.ResolveLoadingDiscrepancy) (api.Receipt, error) {
	r, err := application.ResolveLoadingDiscrepancy{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.ResolveLoadingDiscrepancyCommand{
		ExecutionCommand: executionCommand(cmd.CommandID, cmd.TenantID, cmd.ActorID, cmd.ExecutionID, cmd.ExpectedRevision, cmd.OccurredAt),
		DiscrepancyNo:    cmd.DiscrepancyNo, Resolution: domain.Resolution(cmd.Resolution), Reference: cmd.Reference, Reason: cmd.Reason})
	return toReceipt(r, err)
}

func (m *Module) GetCargoLot(ctx context.Context, q api.GetCargoLot) (api.CargoLot, error) {
	r, err := m.reads(q.TenantID).Lot(ctx, domain.TenantID(q.TenantID), domain.CargoLotID(q.LotID))
	if err != nil {
		return api.CargoLot{}, classify(err)
	}
	return lotView(r), nil
}

func (m *Module) GetLoadExecution(ctx context.Context, q api.GetLoadExecution) (api.LoadExecution, error) {
	r, err := m.reads(q.TenantID).Execution(ctx, domain.TenantID(q.TenantID), domain.LoadExecutionID(q.ExecutionID))
	if err != nil {
		return api.LoadExecution{}, classify(err)
	}
	return executionView(r), nil
}

func (m *Module) ListOpenDiscrepancies(ctx context.Context, q api.ListOpenDiscrepancies) ([]api.OpenDiscrepancy, error) {
	list, err := m.reads(q.TenantID).OpenDiscrepancies(ctx, domain.TenantID(q.TenantID), application.DiscrepancyFilter{
		ConsolidationID: domain.ConsolidationID(q.ConsolidationID), ShipmentID: domain.ShipmentID(q.ShipmentID), Limit: q.Limit})
	if err != nil {
		return nil, classify(err)
	}
	out := make([]api.OpenDiscrepancy, 0, len(list))
	for _, d := range list {
		out = append(out, api.OpenDiscrepancy{LotID: string(d.LotID), ExecutionID: string(d.ExecutionID), ConsolidationID: string(d.ConsolidationID),
			ShipmentID: string(d.ShipmentID), DiscrepancyNo: d.No, Kind: d.Kind, Expected: d.Expected, Observed: d.Observed, Unit: d.Unit, RaisedAt: d.RaisedAt})
	}
	return out, nil
}

// GetPhysicalLoadEvidence answers PhysicalLoadEvidence v1: the shipment's lots and, of every load execution
// with lines of it, its own lines, the master's and its carrier evidence and their open discrepancies.
func (m *Module) GetPhysicalLoadEvidence(ctx context.Context, q api.GetPhysicalLoadEvidence) (api.PhysicalLoadEvidence, error) {
	shipment := domain.ShipmentID(q.ShipmentID)
	lots, executions, err := m.reads(q.TenantID).Evidence(ctx, domain.TenantID(q.TenantID), shipment)
	if err != nil {
		return api.PhysicalLoadEvidence{}, classify(err)
	}
	out := api.PhysicalLoadEvidence{TenantID: q.TenantID, ShipmentID: q.ShipmentID, Lots: []api.LotEvidence{}, Tenders: []api.TenderEvidence{}}
	byID := map[domain.CargoLotID]*domain.CargoLot{}
	for _, r := range lots {
		byID[r.Lot.ID()] = r.Lot
		v := lotView(r)
		e := api.LotEvidence{LotID: v.LotID, Revision: v.Revision, Facility: v.Facility, State: v.State, Completed: r.Lot.Completed(),
			ReceivedPacks: v.ReceivedPacks, LoadedPacks: v.LoadedPacks}
		if n := len(v.Measurements); n > 0 {
			e.Measurement = v.Measurements[n-1]
		}
		if n := len(v.Screenings); n > 0 {
			e.Screening = v.Screenings[n-1]
		}
		for _, d := range v.Discrepancies {
			if d.Resolution == "" {
				e.OpenDiscrepancies = append(e.OpenDiscrepancies, d)
			}
		}
		out.ReceivedPacks += v.ReceivedPacks
		out.Lots = append(out.Lots, e)
	}
	for _, r := range executions {
		v := executionView(r)
		t := api.TenderEvidence{ExecutionID: v.ExecutionID, Revision: v.Revision, ConsolidationID: v.ConsolidationID, Kind: v.Kind, State: v.State,
			Facility: v.Facility, LoadUnitNo: v.LoadUnitNo, ClosedAt: v.ClosedAt, CloseEvidence: v.CloseEvidence}
		for _, l := range v.Lines {
			if l.ShipmentID == q.ShipmentID {
				t.Lines = append(t.Lines, l)
			}
		}
		t.LoadedPacks, t.GrossWeight, t.Volume = r.Execution.HouseMeasure(shipment, byID)
		pieces := r.Execution.HousePieces(shipment)
		t.Departed, t.Arrived, t.Offloaded = pieces.Departed, pieces.Arrived, pieces.Offloaded
		for _, e := range v.CarrierEvidence {
			if e.ShipmentID == "" || e.ShipmentID == q.ShipmentID {
				t.CarrierEvidence = append(t.CarrierEvidence, e)
			}
		}
		for _, d := range v.Discrepancies {
			if d.Resolution == "" && (d.ShipmentID == "" || d.ShipmentID == q.ShipmentID) {
				t.OpenDiscrepancies = append(t.OpenDiscrepancies, d)
			}
		}
		out.LoadedPacks += t.LoadedPacks
		out.Tenders = append(out.Tenders, t)
	}
	return out, nil
}

func evidenceView(e domain.Evidence) api.Evidence {
	return api.Evidence{Source: e.Source, Reference: e.Reference, SHA256: e.SHA256, OccurredAt: e.OccurredAt}
}

func lotView(r application.LotRead) api.CargoLot {
	st := r.Lot.State()
	out := api.CargoLot{TenantID: string(st.TenantID), LotID: string(st.LotID), Revision: r.Revision, ShipmentID: string(st.ShipmentID),
		Facility: api.Facility{Location: st.Facility.Location, Code: st.Facility.Code}, Mode: string(st.Mode), State: string(st.State),
		ExpectedFULRevision: st.Expected.Revision, ExpectedPlanVersion: st.Expected.PlanVersion, ExpectedPacks: st.Expected.Scope.Packs,
		ExpectedGrossWeight: st.Expected.Scope.GrossWeight, ExpectedVolume: st.Expected.Scope.Volume, ReceivedPacks: r.Lot.Received(),
		LoadedPacks: r.Loaded, Screening: string(r.Lot.Screened()), CompletedAt: st.CompletedAt, ClosedAt: st.ClosedAt}
	for _, x := range st.Lines {
		out.Lines = append(out.Lines, api.ReceiptLine{LineNo: x.No, Packs: x.Packs, PackageType: x.PackageType, Condition: string(x.Condition),
			Evidence: evidenceView(x.Evidence), ReceivedAt: x.At, EnteredAt: x.EnteredAt})
	}
	for _, x := range st.Measurements {
		ms := api.Measurement{Version: x.No, GrossWeight: x.GrossWeight, Volume: x.Volume, Method: string(x.Method), Evidence: evidenceView(x.Evidence),
			MeasuredAt: x.At, EnteredAt: x.EnteredAt}
		for _, g := range x.Groups {
			ms.PackageGroups = append(ms.PackageGroups, api.PackageGroup{Count: g.Count, PackageType: g.PackageType, LengthMM: g.LengthMM,
				WidthMM: g.WidthMM, HeightMM: g.HeightMM})
		}
		out.Measurements = append(out.Measurements, ms)
	}
	for _, x := range st.Screenings {
		out.Screenings = append(out.Screenings, api.Screening{ResultNo: x.No, Outcome: string(x.Outcome), Screener: x.Screener, Method: x.Method,
			Reference: x.Reference, Evidence: evidenceView(x.Evidence), RecordedAt: x.At, EnteredAt: x.EnteredAt})
	}
	for _, x := range st.Discrepancies {
		out.Discrepancies = append(out.Discrepancies, api.Discrepancy{DiscrepancyNo: x.No, Kind: string(x.Kind), Expected: x.Expected, Observed: x.Observed,
			Unit: x.Unit, RaisedAt: x.RaisedAt, Resolution: string(x.Resolution), Reference: x.Reference, Reason: x.Text, ResolvedAt: x.ResolvedAt})
	}
	return out
}

func executionView(r application.ExecutionRead) api.LoadExecution {
	x := r.Execution
	st := x.State()
	out := api.LoadExecution{TenantID: string(st.TenantID), ExecutionID: string(st.ExecutionID), Revision: r.Revision,
		ConsolidationID: string(st.ConsolidationID), Kind: string(st.Kind), Mode: string(st.Mode),
		Facility: api.Facility{Location: st.Facility.Location, Code: st.Facility.Code}, LoadUnitNo: st.LoadUnitNo, State: string(st.State),
		TenderedPacks: x.Tendered(),
		OpenedAt:      st.OpenedAt, StartedAt: st.StartedAt, ClosedAt: st.ClosedAt}
	if !st.ClosedAt.IsZero() {
		out.CloseEvidence = evidenceView(st.CloseEvidence)
	}
	for _, l := range st.Lines {
		packs, weight, volume := x.LineLive(l.No)
		line := api.LoadLine{LineNo: l.No, ShipmentID: string(l.ShipmentID), LotID: string(l.LotID), EntryNo: l.EntryNo, Packs: l.Packs,
			GrossWeight: l.GrossWeight, Volume: l.Volume, Basis: string(l.Basis), MeasurementNo: l.MeasurementNo, LivePacks: packs,
			LiveGrossWeight: kernel.Quantity{Unit: l.GrossWeight.Unit, Amount: weight}, Evidence: evidenceView(l.Evidence), LoadedAt: l.At,
			EnteredAt: l.EnteredAt}
		if l.Volume.Unit != "" {
			line.LiveVolume = kernel.Quantity{Unit: l.Volume.Unit, Amount: volume}
		}
		out.Lines = append(out.Lines, line)
	}
	for _, v := range st.Reversals {
		out.Reversals = append(out.Reversals, api.Reversal{ReversalNo: v.No, LineNo: v.LineNo, Packs: v.Packs, GrossWeight: v.GrossWeight, Volume: v.Volume,
			Reason: string(v.Reason), Evidence: evidenceView(v.Evidence), ReversedAt: v.At, EnteredAt: v.EnteredAt})
	}
	for _, h := range st.Holds {
		hold := api.Hold{HoldNo: h.No, Reason: string(h.Reason), LotID: string(h.LotID), Text: h.Text, Evidence: evidenceView(h.Evidence),
			PlacedAt: h.At, Released: !h.Placed(), ReleasedAt: h.ReleasedAt}
		if !h.Placed() {
			hold.ReleaseEvidence = evidenceView(h.ReleaseEvidence)
		}
		out.Holds = append(out.Holds, hold)
	}
	for _, e := range st.CarrierEvidence {
		out.CarrierEvidence = append(out.CarrierEvidence, api.CarrierEvidence{EvidenceNo: e.No, Fact: string(e.Fact), ShipmentID: string(e.ShipmentID),
			Pieces: e.Pieces, GrossWeight: e.GrossWeight, Transport: e.Transport, Evidence: evidenceView(e.Evidence), RecordedAt: e.At, EnteredAt: e.EnteredAt})
	}
	for _, d := range st.Discrepancies {
		out.Discrepancies = append(out.Discrepancies, api.LoadingDiscrepancy{DiscrepancyNo: d.No, Kind: string(d.Kind), EvidenceNo: d.EvidenceNo,
			ShipmentID: string(d.ShipmentID), Expected: d.Expected, Observed: d.Observed, RaisedAt: d.RaisedAt, Resolution: string(d.Resolution),
			Reference: d.Reference, Reason: d.Text, ResolvedAt: d.ResolvedAt})
	}
	return out
}

// physicalRejection reports whether err is a business rule of cargo lots or load executions.
func physicalRejection(err error) bool {
	for _, kind := range []error{domain.ErrKindNotStuffed, domain.ErrNotFrozen, domain.ErrLoadUnitInUse, domain.ErrInvalidLot, domain.ErrLotInForce, domain.ErrInvalidEvidence, domain.ErrInvalidReceipt, domain.ErrNoMeasurement,
		domain.ErrOpenDiscrepancy, domain.ErrUnaccounted, domain.ErrScreeningForAir, domain.ErrNotResolvable, domain.ErrLiveLoadLines,
		domain.ErrConservation, domain.ErrExecutionsClosed, domain.ErrNotReconciled, domain.ErrInvalidExecution, domain.ErrKindNotTendered,
		domain.ErrNotLoading, domain.ErrInvalidLoad, domain.ErrOnHold, domain.ErrHoldNotReleasable, domain.ErrInvalidCarrier,
		domain.ErrEvidenceReused} {
		if errors.Is(err, kind) {
			return true
		}
	}
	return false
}
