package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// ExecutionStore loads and writes load executions. Load locks the execution row FOR UPDATE.
type ExecutionStore struct{ DB *bentopg.DB }

var _ app.ExecutionStore = ExecutionStore{}

type executionRow struct {
	ConsolidationID         string    `json:"consolidation_id"`
	Kind                    string    `json:"kind"`
	TransportMode           string    `json:"transport_mode"`
	FacilityLocation        string    `json:"facility_location"`
	FacilityCode            string    `json:"facility_code"`
	LoadUnitNo              int       `json:"load_unit_no"`
	State                   string    `json:"state"`
	OpenedBy                string    `json:"opened_by"`
	OpenedAt                time.Time `json:"opened_at"`
	OpenedEnteredAt         time.Time `json:"opened_entered_at"`
	StartedBy               string    `json:"started_by"`
	StartedAt               time.Time `json:"started_at"`
	StartedEnteredAt        time.Time `json:"started_entered_at"`
	ClosedBy                string    `json:"closed_by"`
	ClosedAt                time.Time `json:"closed_at"`
	ClosedEnteredAt         time.Time `json:"closed_entered_at"`
	CloseEvidenceSource     string    `json:"close_evidence_source"`
	CloseEvidenceReference  string    `json:"close_evidence_reference"`
	CloseEvidenceSHA256     string    `json:"close_evidence_sha256"`
	CloseEvidenceOccurredAt time.Time `json:"close_evidence_occurred_at"`
	Revision                int64     `json:"revision"`
}

type loadLineRow struct {
	evidenceRow
	LineNo          int       `json:"line_no"`
	ShipmentID      string    `json:"shipment_id"`
	LotID           string    `json:"lot_id"`
	EntryNo         int       `json:"entry_no"`
	Packs           int64     `json:"packs"`
	GrossWeightUnit string    `json:"gross_weight_unit"`
	GrossWeight     int64     `json:"gross_weight"`
	VolumeUnit      string    `json:"volume_unit"`
	Volume          int64     `json:"volume"`
	Basis           string    `json:"basis"`
	MeasurementNo   int       `json:"measurement_no"`
	LoadedBy        string    `json:"loaded_by"`
	LoadedAt        time.Time `json:"loaded_at"`
	EnteredAt       time.Time `json:"entered_at"`
}

type reversalRow struct {
	evidenceRow
	ReversalNo  int       `json:"reversal_no"`
	LineNo      int       `json:"line_no"`
	Packs       int64     `json:"packs"`
	GrossWeight int64     `json:"gross_weight"`
	Volume      *int64    `json:"volume"`
	Reason      string    `json:"reason"`
	ReversedBy  string    `json:"reversed_by"`
	ReversedAt  time.Time `json:"reversed_at"`
	EnteredAt   time.Time `json:"entered_at"`
}

type holdRow struct {
	evidenceRow
	HoldNo                    int       `json:"hold_no"`
	Reason                    string    `json:"reason"`
	LotID                     string    `json:"lot_id"`
	HoldText                  string    `json:"hold_text"`
	PlacedBy                  string    `json:"placed_by"`
	PlacedAt                  time.Time `json:"placed_at"`
	EnteredAt                 time.Time `json:"entered_at"`
	ReleaseEvidenceSource     string    `json:"release_evidence_source"`
	ReleaseEvidenceReference  string    `json:"release_evidence_reference"`
	ReleaseEvidenceSHA256     string    `json:"release_evidence_sha256"`
	ReleaseEvidenceOccurredAt time.Time `json:"release_evidence_occurred_at"`
	ReleasedBy                string    `json:"released_by"`
	ReleasedAt                time.Time `json:"released_at"`
	ReleaseEnteredAt          time.Time `json:"release_entered_at"`
}

type carrierRow struct {
	evidenceRow
	EvidenceNo      int       `json:"evidence_no"`
	Fact            string    `json:"fact"`
	ShipmentID      string    `json:"shipment_id"`
	Pieces          int64     `json:"pieces"`
	GrossWeightUnit string    `json:"gross_weight_unit"`
	GrossWeight     int64     `json:"gross_weight"`
	TransportRef    string    `json:"transport_ref"`
	RecordedBy      string    `json:"recorded_by"`
	RecordedAt      time.Time `json:"recorded_at"`
	EnteredAt       time.Time `json:"entered_at"`
}

type loadingDiscrepancyRow struct {
	DiscrepancyNo       int       `json:"discrepancy_no"`
	Kind                string    `json:"kind"`
	EvidenceNo          int       `json:"evidence_no"`
	ShipmentID          string    `json:"shipment_id"`
	ExpectedPieces      int64     `json:"expected_pieces"`
	ObservedPieces      int64     `json:"observed_pieces"`
	RaisedAt            time.Time `json:"raised_at"`
	EnteredAt           time.Time `json:"entered_at"`
	Resolution          string    `json:"resolution"`
	ResolutionReference string    `json:"resolution_reference"`
	ResolutionText      string    `json:"resolution_text"`
	ResolvedBy          string    `json:"resolved_by"`
	ResolvedAt          time.Time `json:"resolved_at"`
	ResolutionEnteredAt time.Time `json:"resolution_entered_at"`
}

const executionSelect = `SELECT row_to_json(x),
    (SELECT coalesce(json_agg(c ORDER BY c.line_no), '[]') FROM cargo_phy.load_line c WHERE c.tenant_id=x.tenant_id AND c.execution_id=x.execution_id),
    (SELECT coalesce(json_agg(c ORDER BY c.reversal_no), '[]') FROM cargo_phy.reversal_line c WHERE c.tenant_id=x.tenant_id AND c.execution_id=x.execution_id),
    (SELECT coalesce(json_agg(c ORDER BY c.hold_no), '[]') FROM cargo_phy.hold_entry c WHERE c.tenant_id=x.tenant_id AND c.execution_id=x.execution_id),
    (SELECT coalesce(json_agg(c ORDER BY c.evidence_no), '[]') FROM cargo_phy.carrier_load_evidence c
      WHERE c.tenant_id=x.tenant_id AND c.execution_id=x.execution_id),
    (SELECT coalesce(json_agg(c ORDER BY c.discrepancy_no), '[]') FROM cargo_phy.loading_discrepancy c
      WHERE c.tenant_id=x.tenant_id AND c.execution_id=x.execution_id)
  FROM cargo_phy.load_execution x WHERE x.tenant_id=$1 AND x.execution_id=$2`

func readExecution(ctx context.Context, q queryRower, tenant phy.TenantID, id phy.LoadExecutionID) (*phy.LoadExecution, int64, error) {
	var rootJSON, lineJSON, reversalJSON, holdJSON, carrierJSON, discrepancyJSON []byte
	err := q.QueryRow(ctx, executionSelect, string(tenant), string(id)).Scan(&rootJSON, &lineJSON, &reversalJSON, &holdJSON, &carrierJSON, &discrepancyJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: load execution %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	var root executionRow
	var lines []loadLineRow
	var reversals []reversalRow
	var holds []holdRow
	var carrier []carrierRow
	var discrepancies []loadingDiscrepancyRow
	if err = errors.Join(json.Unmarshal(rootJSON, &root), json.Unmarshal(lineJSON, &lines), json.Unmarshal(reversalJSON, &reversals),
		json.Unmarshal(holdJSON, &holds), json.Unmarshal(carrierJSON, &carrier), json.Unmarshal(discrepancyJSON, &discrepancies)); err != nil {
		return nil, 0, err
	}
	st := phy.LoadExecutionState{TenantID: tenant, ExecutionID: id, ConsolidationID: phy.ConsolidationID(root.ConsolidationID),
		Kind: phy.ExecutionKind(root.Kind), Mode: phy.Mode(root.TransportMode),
		Facility: phy.Facility{Location: unlocode(root.FacilityLocation), Code: root.FacilityCode}, LoadUnitNo: root.LoadUnitNo,
		State:    phy.ExecutionState(root.State),
		OpenedBy: root.OpenedBy, OpenedAt: utc(root.OpenedAt), OpenedEnteredAt: utc(root.OpenedEnteredAt),
		StartedBy: root.StartedBy, StartedAt: utc(root.StartedAt), StartedEnteredAt: utc(root.StartedEnteredAt),
		ClosedBy: root.ClosedBy, ClosedAt: utc(root.ClosedAt), ClosedEnteredAt: utc(root.ClosedEnteredAt),
		CloseEvidence: phy.Evidence{Source: root.CloseEvidenceSource, Reference: root.CloseEvidenceReference, SHA256: root.CloseEvidenceSHA256,
			OccurredAt: utc(root.CloseEvidenceOccurredAt)}}
	volumes := map[int]string{}
	for _, x := range lines {
		volumes[x.LineNo] = x.VolumeUnit
		st.Lines = append(st.Lines, phy.LoadLine{No: x.LineNo, ShipmentID: phy.ShipmentID(x.ShipmentID), LotID: phy.CargoLotID(x.LotID),
			EntryNo: x.EntryNo, Packs: x.Packs, GrossWeight: quantity(x.GrossWeightUnit, x.GrossWeight), Volume: quantity(x.VolumeUnit, x.Volume),
			Basis: phy.Basis(x.Basis), MeasurementNo: x.MeasurementNo, Evidence: x.evidence(), By: x.LoadedBy, At: utc(x.LoadedAt),
			EnteredAt: utc(x.EnteredAt)})
	}
	weights := map[int]string{}
	for _, l := range st.Lines {
		weights[l.No] = string(l.GrossWeight.Unit)
	}
	for _, x := range reversals {
		r := phy.Reversal{No: x.ReversalNo, LineNo: x.LineNo, Packs: x.Packs, GrossWeight: quantity(weights[x.LineNo], x.GrossWeight),
			Reason: phy.ReversalReason(x.Reason), Evidence: x.evidence(), By: x.ReversedBy, At: utc(x.ReversedAt), EnteredAt: utc(x.EnteredAt)}
		if x.Volume != nil {
			r.Volume = quantity(volumes[x.LineNo], *x.Volume)
		}
		st.Reversals = append(st.Reversals, r)
	}
	for _, x := range holds {
		st.Holds = append(st.Holds, phy.Hold{No: x.HoldNo, Reason: phy.HoldReason(x.Reason), LotID: phy.CargoLotID(x.LotID), Text: x.HoldText,
			Evidence: x.evidence(), By: x.PlacedBy, At: utc(x.PlacedAt), EnteredAt: utc(x.EnteredAt),
			ReleaseEvidence: phy.Evidence{Source: x.ReleaseEvidenceSource, Reference: x.ReleaseEvidenceReference, SHA256: x.ReleaseEvidenceSHA256,
				OccurredAt: utc(x.ReleaseEvidenceOccurredAt)},
			ReleasedBy: x.ReleasedBy, ReleasedAt: utc(x.ReleasedAt), ReleaseEnteredAt: utc(x.ReleaseEnteredAt)})
	}
	for _, x := range carrier {
		st.CarrierEvidence = append(st.CarrierEvidence, phy.CarrierEvidence{No: x.EvidenceNo, Fact: phy.CarrierFact(x.Fact),
			ShipmentID: phy.ShipmentID(x.ShipmentID), Pieces: x.Pieces, GrossWeight: quantity(x.GrossWeightUnit, x.GrossWeight),
			Transport: x.TransportRef, Evidence: x.evidence(), By: x.RecordedBy, At: utc(x.RecordedAt), EnteredAt: utc(x.EnteredAt)})
	}
	for _, x := range discrepancies {
		st.Discrepancies = append(st.Discrepancies, phy.LoadingDiscrepancy{No: x.DiscrepancyNo, Kind: phy.LoadingDiscrepancyKind(x.Kind),
			EvidenceNo: x.EvidenceNo, ShipmentID: phy.ShipmentID(x.ShipmentID), Expected: x.ExpectedPieces, Observed: x.ObservedPieces,
			RaisedAt: utc(x.RaisedAt), EnteredAt: utc(x.EnteredAt), Resolution: phy.Resolution(x.Resolution), Reference: x.ResolutionReference,
			Text: x.ResolutionText, ResolvedBy: x.ResolvedBy, ResolvedAt: utc(x.ResolvedAt), ResolutionEnteredAt: utc(x.ResolutionEnteredAt)})
	}
	x, err := phy.RestoreLoadExecution(st)
	return x, root.Revision, err
}

func (s ExecutionStore) Consolidation(ctx context.Context, tenant phy.TenantID, id phy.LoadExecutionID) (phy.ConsolidationID, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return "", err
	}
	var c string
	err = q.QueryRow(ctx, `SELECT consolidation_id FROM cargo_phy.load_execution WHERE tenant_id=$1 AND execution_id=$2`, string(tenant), string(id)).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: load execution %s", bentorepo.ErrNotFound, id)
	}
	return phy.ConsolidationID(c), err
}

func (s ExecutionStore) Load(ctx context.Context, tenant phy.TenantID, id phy.LoadExecutionID) (*phy.LoadExecution, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_phy.load_execution WHERE tenant_id=$1 AND execution_id=$2 FOR UPDATE`, string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: load execution %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return readExecution(ctx, q, tenant, id)
}

func (s ExecutionStore) Read(ctx context.Context, tenant phy.TenantID, id phy.LoadExecutionID) (*phy.LoadExecution, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readExecution(ctx, q, tenant, id)
}

// UnitUses returns, by load unit, the stuffing opened on each of a consolidation's units.
func (s ExecutionStore) UnitUses(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) (map[int]app.UnitUse, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT load_unit_no, execution_id, state FROM cargo_phy.load_execution
      WHERE tenant_id=$1 AND consolidation_id=$2 AND load_unit_no IS NOT NULL`, string(tenant), string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]app.UnitUse{}
	for rows.Next() {
		var no int
		var execution, state string
		if err = rows.Scan(&no, &execution, &state); err != nil {
			return nil, err
		}
		out[no] = app.UnitUse{ExecutionID: phy.LoadExecutionID(execution), State: phy.ExecutionState(state)}
	}
	return out, rows.Err()
}

// ByConsolidation reads a consolidation's executions, by execution id.
func (s ExecutionStore) ByConsolidation(ctx context.Context, tenant phy.TenantID, id phy.ConsolidationID) ([]app.ExecutionRead, error) {
	return s.list(ctx, tenant, `SELECT execution_id FROM cargo_phy.load_execution WHERE tenant_id=$1 AND consolidation_id=$2 ORDER BY execution_id`, string(id))
}

// ByShipment reads the executions with load lines of a shipment, by execution id.
func (s ExecutionStore) ByShipment(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) ([]app.ExecutionRead, error) {
	return s.list(ctx, tenant, `SELECT DISTINCT execution_id FROM cargo_phy.load_line WHERE tenant_id=$1 AND shipment_id=$2 ORDER BY execution_id`,
		string(shipment))
}

func (s ExecutionStore) list(ctx context.Context, tenant phy.TenantID, sql string, arg string) ([]app.ExecutionRead, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, sql, string(tenant), arg)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []app.ExecutionRead
	for _, id := range list {
		x, revision, err := readExecution(ctx, q, tenant, phy.LoadExecutionID(id))
		if err != nil {
			return nil, err
		}
		out = append(out, app.ExecutionRead{Execution: x, Revision: revision})
	}
	return out, nil
}

// WithLiveLot returns the LOADING or ON_HOLD executions with live packs of a lot, by execution id.
func (s ExecutionStore) WithLiveLot(ctx context.Context, tenant phy.TenantID, lot phy.CargoLotID) ([]phy.LoadExecutionID, error) {
	return s.withLiveLot(ctx, tenant, lot, "")
}

// StuffingsWithLiveLot returns the LOADING or ON_HOLD stuffings with live packs of a lot, by execution id.
func (s ExecutionStore) StuffingsWithLiveLot(ctx context.Context, tenant phy.TenantID, lot phy.CargoLotID) ([]phy.LoadExecutionID, error) {
	return s.withLiveLot(ctx, tenant, lot, string(phy.ExecutionStuffing))
}

func (s ExecutionStore) withLiveLot(ctx context.Context, tenant phy.TenantID, lot phy.CargoLotID, kind string) ([]phy.LoadExecutionID, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `WITH reversed AS (SELECT execution_id, line_no, sum(packs) AS packs FROM cargo_phy.reversal_line
          WHERE tenant_id=$1 GROUP BY execution_id, line_no)
      SELECT l.execution_id FROM cargo_phy.load_line l
        JOIN cargo_phy.load_execution x ON x.tenant_id=l.tenant_id AND x.execution_id=l.execution_id
        LEFT JOIN reversed r ON r.execution_id=l.execution_id AND r.line_no=l.line_no
      WHERE l.tenant_id=$1 AND l.lot_id=$2 AND x.state IN ('LOADING','ON_HOLD') AND ($3='' OR x.kind=$3)
      GROUP BY l.execution_id HAVING sum(l.packs - coalesce(r.packs, 0)) > 0 ORDER BY l.execution_id`, string(tenant), string(lot), kind)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := make([]phy.LoadExecutionID, len(list))
	for i, id := range list {
		out[i] = phy.LoadExecutionID(id)
	}
	return out, err
}

func (s ExecutionStore) HouseLoaded(ctx context.Context, tenant phy.TenantID, c phy.ConsolidationID, shipment phy.ShipmentID) (int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = q.QueryRow(ctx, `SELECT cargo_phy.live_packs_of_house($1,$2,$3)`, string(tenant), string(c), string(shipment)).Scan(&n)
	return n, err
}

// Insert writes a new execution at revision 1.
func (s ExecutionStore) Insert(ctx context.Context, x *phy.LoadExecution) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := x.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_phy.load_execution (tenant_id,execution_id,consolidation_id,kind,transport_mode,facility_location,
        facility_code,load_unit_no,state,opened_by,opened_at,opened_entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING revision`,
		string(st.TenantID), string(st.ExecutionID), string(st.ConsolidationID), string(st.Kind), string(st.Mode), st.Facility.Location.Code,
		st.Facility.Code, orNull(st.LoadUnitNo), string(st.State), st.OpenedBy, st.OpenedAt, st.OpenedEnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "load_execution_pkey" {
		return 0, fmt.Errorf("%w: load execution %s", bentorepo.ErrAlreadyExists, st.ExecutionID)
	}
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "load_execution_one_per_unit" {
		return 0, fmt.Errorf("%w: unit %d of consolidation %s", phy.ErrLoadUnitInUse, st.LoadUnitNo, st.ConsolidationID)
	}
	if err != nil {
		return 0, err
	}
	return revision, insertExecutionFacts(ctx, q, x)
}

// Save writes the execution's state and what was added, released or resolved since it was loaded, if the
// stored revision is still the expected one.
func (s ExecutionStore) Save(ctx context.Context, x *phy.LoadExecution, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := x.State()
	ev := st.CloseEvidence
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_phy.load_execution SET state=$3,started_by=$4,started_at=$5,started_entered_at=$6,closed_by=$7,closed_at=$8,
        closed_entered_at=$9,close_evidence_source=$10,close_evidence_reference=$11,close_evidence_sha256=$12,close_evidence_occurred_at=$13,
        revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND execution_id=$2 AND revision=$14 RETURNING revision`,
		string(st.TenantID), string(st.ExecutionID), string(st.State), orNull(st.StartedBy), orNull(st.StartedAt), orNull(st.StartedEnteredAt),
		orNull(st.ClosedBy), orNull(st.ClosedAt), orNull(st.ClosedEnteredAt), orNull(ev.Source), orNull(ev.Reference), orNull(ev.SHA256),
		orNull(ev.OccurredAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: load execution %s", bentorepo.ErrConflict, st.ExecutionID)
	}
	if err != nil {
		return 0, err
	}
	return revision, insertExecutionFacts(ctx, q, x)
}

func insertExecutionFacts(ctx context.Context, q bentopg.Executor, x *phy.LoadExecution) error {
	st := x.State()
	tenant, id := string(st.TenantID), string(st.ExecutionID)
	c := x.Added()
	for _, l := range c.Lines {
		ev := l.Evidence
		var volume any
		if l.Volume.Unit != "" {
			volume = l.Volume.Amount
		}
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.load_line (tenant_id,execution_id,line_no,consolidation_id,shipment_id,lot_id,entry_no,packs,
            gross_weight_unit,gross_weight,volume_unit,volume,basis,measurement_no,evidence_source,evidence_reference,evidence_sha256,
            evidence_occurred_at,loaded_by,loaded_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
			tenant, id, l.No, string(st.ConsolidationID), string(l.ShipmentID), string(l.LotID), l.EntryNo, l.Packs, string(l.GrossWeight.Unit),
			l.GrossWeight.Amount, orNull(string(l.Volume.Unit)), volume, string(l.Basis), orNull(l.MeasurementNo), ev.Source, ev.Reference,
			orNull(ev.SHA256), ev.OccurredAt, l.By, l.At, l.EnteredAt); err != nil {
			return err
		}
	}
	for _, r := range c.Reversals {
		ev := r.Evidence
		var volume any
		if r.Volume.Unit != "" {
			volume = r.Volume.Amount
		}
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.reversal_line (tenant_id,execution_id,reversal_no,line_no,packs,gross_weight,volume,reason,
            evidence_source,evidence_reference,evidence_sha256,evidence_occurred_at,reversed_by,reversed_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			tenant, id, r.No, r.LineNo, r.Packs, r.GrossWeight.Amount, volume, string(r.Reason), ev.Source, ev.Reference, orNull(ev.SHA256),
			ev.OccurredAt, r.By, r.At, r.EnteredAt); err != nil {
			return err
		}
	}
	for _, h := range c.Holds {
		ev, rel := h.Evidence, h.ReleaseEvidence
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.hold_entry (tenant_id,execution_id,hold_no,reason,lot_id,hold_text,evidence_source,
            evidence_reference,evidence_sha256,evidence_occurred_at,placed_by,placed_at,entered_at,release_evidence_source,release_evidence_reference,
            release_evidence_sha256,release_evidence_occurred_at,released_by,released_at,release_entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
			tenant, id, h.No, string(h.Reason), orNull(string(h.LotID)), orNull(h.Text), ev.Source, ev.Reference, orNull(ev.SHA256), ev.OccurredAt,
			h.By, h.At, h.EnteredAt, orNull(rel.Source), orNull(rel.Reference), orNull(rel.SHA256), orNull(rel.OccurredAt), orNull(h.ReleasedBy),
			orNull(h.ReleasedAt), orNull(h.ReleaseEnteredAt)); err != nil {
			return err
		}
	}
	for _, h := range c.Released {
		rel := h.ReleaseEvidence
		tag, err := q.Exec(ctx, `UPDATE cargo_phy.hold_entry SET release_evidence_source=$4,release_evidence_reference=$5,release_evidence_sha256=$6,
            release_evidence_occurred_at=$7,released_by=$8,released_at=$9,release_entered_at=$10
            WHERE tenant_id=$1 AND execution_id=$2 AND hold_no=$3 AND released_at IS NULL`,
			tenant, id, h.No, rel.Source, rel.Reference, orNull(rel.SHA256), rel.OccurredAt, h.ReleasedBy, h.ReleasedAt, h.ReleaseEnteredAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: hold %d of %s", bentorepo.ErrConflict, h.No, id)
		}
	}
	for _, e := range c.Evidence {
		ev := e.Evidence
		_, err := q.Exec(ctx, `INSERT INTO cargo_phy.carrier_load_evidence (tenant_id,execution_id,evidence_no,fact,shipment_id,pieces,gross_weight_unit,
            gross_weight,transport_ref,evidence_source,evidence_reference,evidence_sha256,evidence_occurred_at,recorded_by,recorded_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			tenant, id, e.No, string(e.Fact), orNull(string(e.ShipmentID)), e.Pieces, orNull(string(e.GrossWeight.Unit)), orNull(e.GrossWeight.Amount),
			e.Transport, ev.Source, ev.Reference, orNull(ev.SHA256), ev.OccurredAt, e.By, e.At, e.EnteredAt)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "carrier_load_evidence_source_reference" {
			return fmt.Errorf("%w: %s %s is recorded on another execution", phy.ErrEvidenceReused, ev.Source, ev.Reference)
		}
		if err != nil {
			return err
		}
	}
	for _, d := range c.Discrepancies {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.loading_discrepancy (tenant_id,execution_id,discrepancy_no,kind,evidence_no,shipment_id,
            expected_pieces,observed_pieces,raised_at,entered_at,resolution,resolution_reference,resolution_text,resolved_by,resolved_at,
            resolution_entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			tenant, id, d.No, string(d.Kind), d.EvidenceNo, orNull(string(d.ShipmentID)), d.Expected, d.Observed, d.RaisedAt, d.EnteredAt,
			orNull(string(d.Resolution)), orNull(d.Reference), orNull(d.Text), orNull(d.ResolvedBy), orNull(d.ResolvedAt),
			orNull(d.ResolutionEnteredAt)); err != nil {
			return err
		}
	}
	for _, d := range c.Resolved {
		tag, err := q.Exec(ctx, `UPDATE cargo_phy.loading_discrepancy SET resolution=$4,resolution_reference=$5,resolution_text=$6,resolved_by=$7,
            resolved_at=$8,resolution_entered_at=$9 WHERE tenant_id=$1 AND execution_id=$2 AND discrepancy_no=$3 AND resolved_at IS NULL`,
			tenant, id, d.No, string(d.Resolution), orNull(d.Reference), orNull(d.Text), d.ResolvedBy, d.ResolvedAt, d.ResolutionEnteredAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: loading discrepancy %d of %s", bentorepo.ErrConflict, d.No, id)
		}
	}
	return nil
}

// Discrepancies lists open discrepancies of cargo lots and load executions.
type Discrepancies struct{ DB *bentopg.DB }

var _ app.DiscrepancyList = Discrepancies{}

// ListOpen lists open discrepancies by the time they were raised. A lot's discrepancy belongs to the
// consolidation its shipment is live in.
func (s Discrepancies) ListOpen(ctx context.Context, tenant phy.TenantID, f app.DiscrepancyFilter) ([]app.OpenDiscrepancy, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT o.lot_id, o.execution_id, o.consolidation_id, o.shipment_id, o.discrepancy_no, o.kind, o.expected, o.observed,
        o.unit, o.raised_at FROM (
        SELECT l.lot_id, '' AS execution_id, coalesce(e.consolidation_id, '') AS consolidation_id, l.shipment_id, d.discrepancy_no, d.kind,
          d.expected_amount AS expected, d.observed_amount AS observed, d.unit, d.raised_at
        FROM cargo_phy.cargo_discrepancy d JOIN cargo_phy.cargo_lot l ON l.tenant_id=d.tenant_id AND l.lot_id=d.lot_id
          LEFT JOIN cargo_phy.membership_entry e ON e.tenant_id=l.tenant_id AND e.shipment_id=l.shipment_id AND e.stage='MAIN' AND e.ended_at IS NULL
        WHERE d.tenant_id=$1 AND d.resolved_at IS NULL
        UNION ALL
        SELECT '', x.execution_id, x.consolidation_id, coalesce(d.shipment_id, ''), d.discrepancy_no, d.kind, d.expected_pieces, d.observed_pieces,
          'PCS', d.raised_at
        FROM cargo_phy.loading_discrepancy d JOIN cargo_phy.load_execution x ON x.tenant_id=d.tenant_id AND x.execution_id=d.execution_id
        WHERE d.tenant_id=$1 AND d.resolved_at IS NULL) o
      WHERE ($2='' OR o.consolidation_id=$2) AND ($3='' OR o.shipment_id=$3)
      ORDER BY o.raised_at, o.lot_id, o.execution_id, o.discrepancy_no LIMIT $4`,
		string(tenant), string(f.ConsolidationID), string(f.ShipmentID), f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.OpenDiscrepancy
	for rows.Next() {
		var d app.OpenDiscrepancy
		var lot, execution, consolidation, shipment string
		if err = rows.Scan(&lot, &execution, &consolidation, &shipment, &d.No, &d.Kind, &d.Expected, &d.Observed, &d.Unit, &d.RaisedAt); err != nil {
			return nil, err
		}
		d.LotID, d.ExecutionID, d.ConsolidationID, d.ShipmentID = phy.CargoLotID(lot), phy.LoadExecutionID(execution), phy.ConsolidationID(consolidation),
			phy.ShipmentID(shipment)
		d.RaisedAt = d.RaisedAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}
