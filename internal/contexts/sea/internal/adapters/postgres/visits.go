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
	app "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
)

// VisitStore loads and writes container visits.
type VisitStore struct{ DB *bentopg.DB }

var _ app.VisitStore = VisitStore{}

func (s VisitStore) Load(ctx context.Context, tenant sea.TenantID, id sea.VisitID) (*sea.Visit, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_sea.container_visit WHERE tenant_id=$1 AND visit_id=$2 FOR NO KEY UPDATE`, string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: visit %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return readVisit(ctx, q, tenant, id)
}

func (s VisitStore) Read(ctx context.Context, tenant sea.TenantID, id sea.VisitID) (*sea.Visit, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readVisit(ctx, q, tenant, id)
}

type sealRow struct {
	SealNo     int       `json:"seal_no"`
	Number     string    `json:"number"`
	Party      string    `json:"party"`
	AppliedAt  time.Time `json:"applied_at"`
	Source     string    `json:"source"`
	RecordedBy string    `json:"recorded_by"`
	RecordedAt time.Time `json:"recorded_at"`
	EnteredAt  time.Time `json:"entered_at"`
}

// readVisit reads the visit and its seals in one statement, so that an unlocked read is one snapshot.
func readVisit(ctx context.Context, q queryRower, tenant sea.TenantID, id sea.VisitID) (*sea.Visit, int64, error) {
	st := sea.VisitState{TenantID: tenant, VisitID: id}
	var tare, maxGross *int64
	var plate, execution, difference, stuffedBy, releaseReason, releasedBy *string
	var stuffedAt, releasedAt *time.Time
	var revision int64
	var seals []byte
	err := q.QueryRow(ctx, `SELECT v.movement_id,v.consolidation_id,v.load_unit_no,v.container_number,v.size_type,v.tare_kg,v.max_gross_kg,v.plate_source,
      v.status,v.execution_id,v.difference_reason,v.stuffed_by,v.stuffed_at,v.release_reason,v.released_by,v.released_at,v.assigned_by,v.assigned_at,
      v.entered_at,v.revision,
      (SELECT coalesce(json_agg(s ORDER BY s.seal_no), '[]') FROM cargo_sea.container_seal s WHERE s.tenant_id=v.tenant_id AND s.visit_id=v.visit_id)
      FROM cargo_sea.container_visit v WHERE v.tenant_id=$1 AND v.visit_id=$2`, string(tenant), string(id)).Scan(
		&st.MovementID, &st.ConsolidationID, &st.LoadUnitNo, &st.Number, &st.SizeType, &tare, &maxGross, &plate, &st.Status,
		&execution, &difference, &stuffedBy, &stuffedAt, &releaseReason, &releasedBy, &releasedAt, &st.AssignedBy, &st.AssignedAt, &st.EnteredAt,
		&revision, &seals)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: visit %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	var rows []sealRow
	if err = json.Unmarshal(seals, &rows); err != nil {
		return nil, 0, err
	}
	if tare != nil && maxGross != nil {
		st.Plate = sea.Plate{TareKg: *tare, MaxGrossKg: *maxGross, Source: deref(plate)}
	}
	st.ExecutionID, st.DifferenceReason, st.StuffedBy = deref(execution), deref(difference), deref(stuffedBy)
	st.ReleaseReason, st.ReleasedBy = deref(releaseReason), deref(releasedBy)
	st.StuffedAt, st.ReleasedAt = utcTime(stuffedAt), utcTime(releasedAt)
	st.AssignedAt, st.EnteredAt = st.AssignedAt.UTC(), st.EnteredAt.UTC()
	for _, r := range rows {
		st.Seals = append(st.Seals, sea.VisitSeal{No: r.SealNo, Number: r.Number, Party: r.Party, AppliedAt: utc(r.AppliedAt), Source: r.Source,
			By: r.RecordedBy, At: utc(r.RecordedAt), EnteredAt: utc(r.EnteredAt)})
	}
	v, err := sea.RestoreVisit(st)
	return v, revision, err
}

func utcTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

func (s VisitStore) Insert(ctx context.Context, v *sea.Visit) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := v.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_sea.container_visit (tenant_id,visit_id,movement_id,consolidation_id,load_unit_no,container_number,size_type,
      tare_kg,max_gross_kg,plate_source,status,assigned_by,assigned_at,entered_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING revision`,
		string(st.TenantID), string(st.VisitID), string(st.MovementID), st.ConsolidationID, st.LoadUnitNo, st.Number, st.SizeType,
		orNull(st.Plate.TareKg), orNull(st.Plate.MaxGrossKg), orNull(st.Plate.Source), string(st.Status), st.AssignedBy, st.AssignedAt, st.EnteredAt).Scan(&revision)
	if err != nil {
		return 0, visitWriteErr(err, st)
	}
	if err = insertSeals(ctx, q, st, v.AddedSeals()); err != nil {
		return 0, err
	}
	return revision, nil
}

func (s VisitStore) Save(ctx context.Context, v *sea.Visit, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := v.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_sea.container_visit SET status=$3, execution_id=$4, difference_reason=$5, stuffed_by=$6, stuffed_at=$7,
      release_reason=$8, released_by=$9, released_at=$10, revision=revision+1, updated_at=now()
      WHERE tenant_id=$1 AND visit_id=$2 AND revision=$11 RETURNING revision`,
		string(st.TenantID), string(st.VisitID), string(st.Status), orNull(st.ExecutionID), orNull(st.DifferenceReason), orNull(st.StuffedBy),
		orNull(st.StuffedAt), orNull(st.ReleaseReason), orNull(st.ReleasedBy), orNull(st.ReleasedAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: visit %s", bentorepo.ErrConflict, st.VisitID)
	}
	if err != nil {
		return 0, visitWriteErr(err, st)
	}
	if err = insertSeals(ctx, q, st, v.AddedSeals()); err != nil {
		return 0, err
	}
	return revision, nil
}

func insertSeals(ctx context.Context, q bentopg.Executor, st sea.VisitState, seals []sea.VisitSeal) error {
	for _, s := range seals {
		_, err := q.Exec(ctx, `INSERT INTO cargo_sea.container_seal (tenant_id,visit_id,seal_no,number,party,applied_at,source,recorded_by,recorded_at,entered_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, string(st.TenantID), string(st.VisitID), s.No, s.Number, s.Party, s.AppliedAt, s.Source, s.By, s.At, s.EnteredAt)
		if err != nil {
			return err
		}
	}
	return nil
}

func visitWriteErr(err error, st sea.VisitState) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "container_visit_pkey":
			return fmt.Errorf("%w: visit %s", bentorepo.ErrAlreadyExists, st.VisitID)
		case "container_visit_live_number":
			return fmt.Errorf("%w: %s", app.ErrContainerInUse, st.Number)
		case "container_visit_live_unit":
			return fmt.Errorf("%w: unit %d of %s", app.ErrUnitHasContainer, st.LoadUnitNo, st.MovementID)
		}
	}
	return err
}

func (s VisitStore) OfMovement(ctx context.Context, tenant sea.TenantID, movement sea.MovementID) ([]sea.VisitID, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT visit_id FROM cargo_sea.container_visit WHERE tenant_id=$1 AND movement_id=$2 ORDER BY assigned_at, visit_id`,
		string(tenant), string(movement))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []sea.VisitID
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, sea.VisitID(id))
	}
	return ids, rows.Err()
}

func (s VisitStore) LiveByNumber(ctx context.Context, tenant sea.TenantID, number string) (sea.VisitID, bool, error) {
	return s.live(ctx, `SELECT visit_id FROM cargo_sea.container_visit WHERE tenant_id=$1 AND container_number=$2 AND status<>'RELEASED'`, string(tenant), number)
}

func (s VisitStore) LiveByUnit(ctx context.Context, tenant sea.TenantID, movement sea.MovementID, unit int) (sea.VisitID, bool, error) {
	return s.live(ctx, `SELECT visit_id FROM cargo_sea.container_visit WHERE tenant_id=$1 AND movement_id=$2 AND load_unit_no=$3 AND status<>'RELEASED'`,
		string(tenant), string(movement), unit)
}

func (s VisitStore) live(ctx context.Context, sql string, args ...any) (sea.VisitID, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var id string
	err = q.QueryRow(ctx, sql, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return sea.VisitID(id), err == nil, err
}
