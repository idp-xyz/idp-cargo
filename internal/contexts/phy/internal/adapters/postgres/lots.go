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

// LotStore loads and writes cargo lots. Load locks the lot row FOR UPDATE, so lot commands and loads from one
// lot run one after another.
type LotStore struct{ DB *bentopg.DB }

var _ app.LotStore = LotStore{}

// evidenceRow is the evidence columns every fact row has, as row_to_json renders them.
type evidenceRow struct {
	EvidenceSource     string    `json:"evidence_source"`
	EvidenceReference  string    `json:"evidence_reference"`
	EvidenceSHA256     string    `json:"evidence_sha256"`
	EvidenceOccurredAt time.Time `json:"evidence_occurred_at"`
}

func (e evidenceRow) evidence() phy.Evidence {
	return phy.Evidence{Source: e.EvidenceSource, Reference: e.EvidenceReference, SHA256: e.EvidenceSHA256, OccurredAt: utc(e.EvidenceOccurredAt)}
}

type lotRow struct {
	ShipmentID          string    `json:"shipment_id"`
	FacilityLocation    string    `json:"facility_location"`
	FacilityCode        string    `json:"facility_code"`
	TransportMode       string    `json:"transport_mode"`
	State               string    `json:"state"`
	ExpectedFULRevision int64     `json:"expected_ful_revision"`
	ExpectedPlanVersion int       `json:"expected_plan_version"`
	ExpectedPacks       int64     `json:"expected_packs"`
	ExpectedWeightUnit  string    `json:"expected_weight_unit"`
	ExpectedGrossWeight int64     `json:"expected_gross_weight"`
	ExpectedVolumeUnit  string    `json:"expected_volume_unit"`
	ExpectedVolume      int64     `json:"expected_volume"`
	CompletedBy         string    `json:"completed_by"`
	CompletedAt         time.Time `json:"completed_at"`
	CompletedEnteredAt  time.Time `json:"completed_entered_at"`
	ClosedBy            string    `json:"closed_by"`
	ClosedAt            time.Time `json:"closed_at"`
	ClosedEnteredAt     time.Time `json:"closed_entered_at"`
	Revision            int64     `json:"revision"`
}

type receiptLineRow struct {
	evidenceRow
	LineNo      int       `json:"line_no"`
	Packs       int64     `json:"packs"`
	PackageType string    `json:"package_type"`
	Condition   string    `json:"condition"`
	ReceivedBy  string    `json:"received_by"`
	ReceivedAt  time.Time `json:"received_at"`
	EnteredAt   time.Time `json:"entered_at"`
}

type measurementRow struct {
	evidenceRow
	VersionNo       int       `json:"version_no"`
	GrossWeightUnit string    `json:"gross_weight_unit"`
	GrossWeight     int64     `json:"gross_weight"`
	VolumeUnit      string    `json:"volume_unit"`
	Volume          int64     `json:"volume"`
	Method          string    `json:"method"`
	MeasuredBy      string    `json:"measured_by"`
	MeasuredAt      time.Time `json:"measured_at"`
	EnteredAt       time.Time `json:"entered_at"`
}

type groupRow struct {
	VersionNo    int    `json:"version_no"`
	PackageCount int64  `json:"package_count"`
	PackageType  string `json:"package_type"`
	LengthMM     int64  `json:"length_mm"`
	WidthMM      int64  `json:"width_mm"`
	HeightMM     int64  `json:"height_mm"`
}

type screeningRow struct {
	evidenceRow
	ResultNo           int       `json:"result_no"`
	Outcome            string    `json:"outcome"`
	Screener           string    `json:"screener"`
	Method             string    `json:"method"`
	ScreeningReference string    `json:"screening_reference"`
	RecordedBy         string    `json:"recorded_by"`
	RecordedAt         time.Time `json:"recorded_at"`
	EnteredAt          time.Time `json:"entered_at"`
}

type cargoDiscrepancyRow struct {
	DiscrepancyNo       int       `json:"discrepancy_no"`
	Kind                string    `json:"kind"`
	ExpectedAmount      int64     `json:"expected_amount"`
	ObservedAmount      int64     `json:"observed_amount"`
	Unit                string    `json:"unit"`
	RaisedAt            time.Time `json:"raised_at"`
	EnteredAt           time.Time `json:"entered_at"`
	Resolution          string    `json:"resolution"`
	ResolutionReference string    `json:"resolution_reference"`
	ResolutionText      string    `json:"resolution_text"`
	ResolvedBy          string    `json:"resolved_by"`
	ResolvedAt          time.Time `json:"resolved_at"`
	ResolutionEnteredAt time.Time `json:"resolution_entered_at"`
}

const lotSelect = `SELECT row_to_json(l),
    (SELECT coalesce(json_agg(x ORDER BY x.line_no), '[]') FROM cargo_phy.receipt_line x WHERE x.tenant_id=l.tenant_id AND x.lot_id=l.lot_id),
    (SELECT coalesce(json_agg(x ORDER BY x.version_no), '[]') FROM cargo_phy.measurement_version x WHERE x.tenant_id=l.tenant_id AND x.lot_id=l.lot_id),
    (SELECT coalesce(json_agg(x ORDER BY x.version_no, x.group_no), '[]') FROM cargo_phy.measurement_package_group x
      WHERE x.tenant_id=l.tenant_id AND x.lot_id=l.lot_id),
    (SELECT coalesce(json_agg(x ORDER BY x.result_no), '[]') FROM cargo_phy.screening_result x WHERE x.tenant_id=l.tenant_id AND x.lot_id=l.lot_id),
    (SELECT coalesce(json_agg(x ORDER BY x.discrepancy_no), '[]') FROM cargo_phy.cargo_discrepancy x WHERE x.tenant_id=l.tenant_id AND x.lot_id=l.lot_id),
    cargo_phy.live_packs_of_lot(l.tenant_id, l.lot_id)
  FROM cargo_phy.cargo_lot l`

// scanLot rebuilds a lot from a lotSelect row.
func scanLot(row pgx.Row, tenant phy.TenantID, id phy.CargoLotID) (app.LotRead, error) {
	var rootJSON, lineJSON, measureJSON, groupJSON, screenJSON, discrepancyJSON []byte
	var loaded int64
	if err := row.Scan(&rootJSON, &lineJSON, &measureJSON, &groupJSON, &screenJSON, &discrepancyJSON, &loaded); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.LotRead{}, fmt.Errorf("%w: cargo lot %s", bentorepo.ErrNotFound, id)
		}
		return app.LotRead{}, err
	}
	var root lotRow
	var lines []receiptLineRow
	var measures []measurementRow
	var groups []groupRow
	var screens []screeningRow
	var discrepancies []cargoDiscrepancyRow
	if err := errors.Join(json.Unmarshal(rootJSON, &root), json.Unmarshal(lineJSON, &lines), json.Unmarshal(measureJSON, &measures),
		json.Unmarshal(groupJSON, &groups), json.Unmarshal(screenJSON, &screens), json.Unmarshal(discrepancyJSON, &discrepancies)); err != nil {
		return app.LotRead{}, err
	}
	st := phy.CargoLotState{TenantID: tenant, LotID: id, ShipmentID: phy.ShipmentID(root.ShipmentID),
		Facility: phy.Facility{Location: unlocode(root.FacilityLocation), Code: root.FacilityCode}, Mode: phy.Mode(root.TransportMode),
		State: phy.LotState(root.State), Expected: phy.Expected{Revision: root.ExpectedFULRevision, PlanVersion: root.ExpectedPlanVersion,
			Scope: phy.CargoScope{Packs: root.ExpectedPacks, GrossWeight: quantity(root.ExpectedWeightUnit, root.ExpectedGrossWeight),
				Volume: quantity(root.ExpectedVolumeUnit, root.ExpectedVolume)}},
		CompletedBy: root.CompletedBy, CompletedAt: utc(root.CompletedAt), CompletedEnteredAt: utc(root.CompletedEnteredAt),
		ClosedBy: root.ClosedBy, ClosedAt: utc(root.ClosedAt), ClosedEnteredAt: utc(root.ClosedEnteredAt)}
	for _, x := range lines {
		st.Lines = append(st.Lines, phy.ReceiptLine{No: x.LineNo, Packs: x.Packs, PackageType: x.PackageType, Condition: phy.Condition(x.Condition),
			Evidence: x.evidence(), By: x.ReceivedBy, At: utc(x.ReceivedAt), EnteredAt: utc(x.EnteredAt)})
	}
	for _, x := range measures {
		m := phy.Measurement{No: x.VersionNo, GrossWeight: quantity(x.GrossWeightUnit, x.GrossWeight), Volume: quantity(x.VolumeUnit, x.Volume),
			Method: phy.Method(x.Method), Evidence: x.evidence(), By: x.MeasuredBy, At: utc(x.MeasuredAt), EnteredAt: utc(x.EnteredAt)}
		for _, g := range groups {
			if g.VersionNo == x.VersionNo {
				m.Groups = append(m.Groups, phy.PackageGroup{Count: g.PackageCount, PackageType: g.PackageType, LengthMM: g.LengthMM,
					WidthMM: g.WidthMM, HeightMM: g.HeightMM})
			}
		}
		st.Measurements = append(st.Measurements, m)
	}
	for _, x := range screens {
		st.Screenings = append(st.Screenings, phy.Screening{No: x.ResultNo, Outcome: phy.Outcome(x.Outcome), Screener: x.Screener, Method: x.Method,
			Reference: x.ScreeningReference, Evidence: x.evidence(), By: x.RecordedBy, At: utc(x.RecordedAt), EnteredAt: utc(x.EnteredAt)})
	}
	for _, x := range discrepancies {
		st.Discrepancies = append(st.Discrepancies, phy.Discrepancy{No: x.DiscrepancyNo, Kind: phy.DiscrepancyKind(x.Kind), Expected: x.ExpectedAmount,
			Observed: x.ObservedAmount, Unit: x.Unit, RaisedAt: utc(x.RaisedAt), EnteredAt: utc(x.EnteredAt), Resolution: phy.Resolution(x.Resolution),
			Reference: x.ResolutionReference, Text: x.ResolutionText, ResolvedBy: x.ResolvedBy, ResolvedAt: utc(x.ResolvedAt),
			ResolutionEnteredAt: utc(x.ResolutionEnteredAt)})
	}
	l, err := phy.RestoreCargoLot(st)
	return app.LotRead{Lot: l, Revision: root.Revision, Loaded: loaded}, err
}

func (s LotStore) Load(ctx context.Context, tenant phy.TenantID, id phy.CargoLotID) (*phy.CargoLot, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_phy.cargo_lot WHERE tenant_id=$1 AND lot_id=$2 FOR UPDATE`, string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: cargo lot %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	r, err := scanLot(q.QueryRow(ctx, lotSelect+` WHERE l.tenant_id=$1 AND l.lot_id=$2`, string(tenant), string(id)), tenant, id)
	return r.Lot, r.Revision, err
}

func (s LotStore) Read(ctx context.Context, tenant phy.TenantID, id phy.CargoLotID) (*phy.CargoLot, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	r, err := scanLot(q.QueryRow(ctx, lotSelect+` WHERE l.tenant_id=$1 AND l.lot_id=$2`, string(tenant), string(id)), tenant, id)
	return r.Lot, r.Revision, err
}

// ByShipment reads a shipment's lots, by lot id.
func (s LotStore) ByShipment(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) ([]app.LotRead, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT lot_id FROM cargo_phy.cargo_lot WHERE tenant_id=$1 AND shipment_id=$2 ORDER BY lot_id`, string(tenant), string(shipment))
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []app.LotRead
	for _, id := range ids {
		r, err := scanLot(q.QueryRow(ctx, lotSelect+` WHERE l.tenant_id=$1 AND l.lot_id=$2`, string(tenant), id), tenant, phy.CargoLotID(id))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s LotStore) Loaded(ctx context.Context, tenant phy.TenantID, id phy.CargoLotID) (int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = q.QueryRow(ctx, `SELECT cargo_phy.live_packs_of_lot($1,$2)`, string(tenant), string(id)).Scan(&n)
	return n, err
}

// LockShipments locks the shipments' lots FOR SHARE, in lot order, so that no discrepancy opens meanwhile.
func (s LotStore) LockShipments(ctx context.Context, tenant phy.TenantID, shipments []phy.ShipmentID) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `SELECT 1 FROM cargo_phy.cargo_lot WHERE tenant_id=$1 AND shipment_id = ANY($2) ORDER BY lot_id FOR SHARE`,
		string(tenant), ids(shipments))
	return err
}

func (s LotStore) OpenDiscrepancies(ctx context.Context, tenant phy.TenantID, shipments []phy.ShipmentID) (int, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = q.QueryRow(ctx, `SELECT count(*) FROM cargo_phy.cargo_discrepancy d JOIN cargo_phy.cargo_lot l ON l.tenant_id=d.tenant_id AND l.lot_id=d.lot_id
      WHERE d.tenant_id=$1 AND l.shipment_id = ANY($2) AND d.resolved_at IS NULL`, string(tenant), ids(shipments)).Scan(&n)
	return n, err
}

func ids[T ~string](list []T) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = string(v)
	}
	return out
}

// Insert writes a new lot at revision 1 with its first receipt line. A lot of the shipment in force at the
// facility refuses it.
func (s LotStore) Insert(ctx context.Context, l *phy.CargoLot) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := l.State()
	e := st.Expected.Scope
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_phy.cargo_lot (tenant_id,lot_id,shipment_id,facility_location,facility_code,transport_mode,state,
        expected_ful_revision,expected_plan_version,expected_packs,expected_weight_unit,expected_gross_weight,expected_volume_unit,expected_volume)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING revision`,
		string(st.TenantID), string(st.LotID), string(st.ShipmentID), st.Facility.Location.Code, st.Facility.Code, string(st.Mode), string(st.State),
		st.Expected.Revision, st.Expected.PlanVersion, e.Packs, string(e.GrossWeight.Unit), e.GrossWeight.Amount, orNull(string(e.Volume.Unit)),
		orNull(e.Volume.Amount)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "cargo_lot_pkey":
			return 0, fmt.Errorf("%w: cargo lot %s", bentorepo.ErrAlreadyExists, st.LotID)
		case "cargo_lot_one_live":
			return 0, fmt.Errorf("%w: %s at %s %s", phy.ErrLotInForce, st.ShipmentID, st.Facility.Location.Code, st.Facility.Code)
		}
	}
	if err != nil {
		return 0, err
	}
	return revision, insertLotFacts(ctx, q, l)
}

// Save writes the lot's state and what was added or resolved since it was loaded, if the stored revision is
// still the expected one.
func (s LotStore) Save(ctx context.Context, l *phy.CargoLot, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := l.State()
	e := st.Expected.Scope
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_phy.cargo_lot SET state=$3,expected_ful_revision=$4,expected_plan_version=$5,expected_packs=$6,
        expected_weight_unit=$7,expected_gross_weight=$8,expected_volume_unit=$9,expected_volume=$10,completed_by=$11,completed_at=$12,
        completed_entered_at=$13,closed_by=$14,closed_at=$15,closed_entered_at=$16,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND lot_id=$2 AND revision=$17 RETURNING revision`,
		string(st.TenantID), string(st.LotID), string(st.State), st.Expected.Revision, st.Expected.PlanVersion, e.Packs, string(e.GrossWeight.Unit),
		e.GrossWeight.Amount, orNull(string(e.Volume.Unit)), orNull(e.Volume.Amount), orNull(st.CompletedBy), orNull(st.CompletedAt),
		orNull(st.CompletedEnteredAt), orNull(st.ClosedBy), orNull(st.ClosedAt), orNull(st.ClosedEnteredAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: cargo lot %s", bentorepo.ErrConflict, st.LotID)
	}
	if err != nil {
		return 0, err
	}
	return revision, insertLotFacts(ctx, q, l)
}

func insertLotFacts(ctx context.Context, q bentopg.Executor, l *phy.CargoLot) error {
	st := l.State()
	tenant, lot := string(st.TenantID), string(st.LotID)
	lines, measurements, screenings, discrepancies, resolved := l.Added()
	for _, x := range lines {
		ev := x.Evidence
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.receipt_line (tenant_id,lot_id,line_no,packs,package_type,condition,evidence_source,
            evidence_reference,evidence_sha256,evidence_occurred_at,received_by,received_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			tenant, lot, x.No, x.Packs, x.PackageType, string(x.Condition), ev.Source, ev.Reference, orNull(ev.SHA256), ev.OccurredAt, x.By, x.At,
			x.EnteredAt); err != nil {
			return err
		}
	}
	for _, x := range measurements {
		ev := x.Evidence
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.measurement_version (tenant_id,lot_id,version_no,gross_weight_unit,gross_weight,volume_unit,
            volume,method,evidence_source,evidence_reference,evidence_sha256,evidence_occurred_at,measured_by,measured_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			tenant, lot, x.No, string(x.GrossWeight.Unit), x.GrossWeight.Amount, orNull(string(x.Volume.Unit)), orNull(x.Volume.Amount), string(x.Method),
			ev.Source, ev.Reference, orNull(ev.SHA256), ev.OccurredAt, x.By, x.At, x.EnteredAt); err != nil {
			return err
		}
		for i, g := range x.Groups {
			if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.measurement_package_group (tenant_id,lot_id,version_no,group_no,package_count,package_type,
                length_mm,width_mm,height_mm) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				tenant, lot, x.No, i+1, g.Count, g.PackageType, g.LengthMM, g.WidthMM, g.HeightMM); err != nil {
				return err
			}
		}
	}
	for _, x := range screenings {
		ev := x.Evidence
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.screening_result (tenant_id,lot_id,result_no,outcome,screener,method,screening_reference,
            evidence_source,evidence_reference,evidence_sha256,evidence_occurred_at,recorded_by,recorded_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			tenant, lot, x.No, string(x.Outcome), x.Screener, x.Method, x.Reference, ev.Source, ev.Reference, orNull(ev.SHA256), ev.OccurredAt,
			x.By, x.At, x.EnteredAt); err != nil {
			return err
		}
	}
	for _, x := range discrepancies {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_phy.cargo_discrepancy (tenant_id,lot_id,discrepancy_no,kind,expected_amount,observed_amount,unit,
            raised_at,entered_at,resolution,resolution_reference,resolution_text,resolved_by,resolved_at,resolution_entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			tenant, lot, x.No, string(x.Kind), x.Expected, x.Observed, x.Unit, x.RaisedAt, x.EnteredAt, orNull(string(x.Resolution)),
			orNull(x.Reference), orNull(x.Text), orNull(x.ResolvedBy), orNull(x.ResolvedAt), orNull(x.ResolutionEnteredAt)); err != nil {
			return err
		}
	}
	for _, x := range resolved {
		tag, err := q.Exec(ctx, `UPDATE cargo_phy.cargo_discrepancy SET resolution=$4,resolution_reference=$5,resolution_text=$6,resolved_by=$7,
            resolved_at=$8,resolution_entered_at=$9 WHERE tenant_id=$1 AND lot_id=$2 AND discrepancy_no=$3 AND resolved_at IS NULL`,
			tenant, lot, x.No, string(x.Resolution), orNull(x.Reference), orNull(x.Text), x.ResolvedBy, x.ResolvedAt, x.ResolutionEnteredAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: discrepancy %d of lot %s", bentorepo.ErrConflict, x.No, lot)
		}
	}
	return nil
}
