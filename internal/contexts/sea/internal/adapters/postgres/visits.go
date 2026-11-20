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
	"go.idp.xyz/idp-cargo/internal/kernel"
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

type assertionRow struct {
	Version           int        `json:"version"`
	MassGrams         int64      `json:"mass_grams"`
	Method            string     `json:"method"`
	PartyID           string     `json:"responsible_party_id"`
	PartyRevision     int64      `json:"responsible_party_revision"`
	DeterminedAt      time.Time  `json:"determined_at"`
	SubmittedAt       time.Time  `json:"submitted_at"`
	LateReason        *string    `json:"late_reason"`
	RecordedBy        string     `json:"recorded_by"`
	RecordedAt        time.Time  `json:"recorded_at"`
	EnteredAt         time.Time  `json:"entered_at"`
	Response          *string    `json:"response"`
	ResponseSource    *string    `json:"response_source"`
	RespondedAt       *time.Time `json:"responded_at"`
	ResponseBy        *string    `json:"response_by"`
	ResponseEnteredAt *time.Time `json:"response_entered_at"`
}

type eventRow struct {
	EventNo    int       `json:"event_no"`
	Kind       string    `json:"kind"`
	Location   string    `json:"location_code"`
	OccurredAt time.Time `json:"occurred_at"`
	Source     string    `json:"source"`
	Reference  string    `json:"reference"`
	RecordedBy string    `json:"recorded_by"`
	RecordedAt time.Time `json:"recorded_at"`
	EnteredAt  time.Time `json:"entered_at"`
}

// readVisit reads the visit and its seals in one statement, so that an unlocked read is one snapshot.
func readVisit(ctx context.Context, q queryRower, tenant sea.TenantID, id sea.VisitID) (*sea.Visit, int64, error) {
	st := sea.VisitState{TenantID: tenant, VisitID: id}
	var tare, maxGross *int64
	var plate, execution, difference, stuffedBy, releaseReason, releasedBy *string
	var stuffedAt, releasedAt, phySealedAt *time.Time
	var phySeal *string
	var revision int64
	var seals, assertions, events []byte
	err := q.QueryRow(ctx, `SELECT v.movement_id,v.consolidation_id,v.load_unit_no,v.container_number,v.size_type,v.tare_kg,v.max_gross_kg,v.plate_source,
      v.status,v.execution_id,v.difference_reason,v.stuffed_by,v.stuffed_at,v.stuffed_phy_seal,v.stuffed_phy_sealed_at,v.release_reason,v.released_by,v.released_at,v.assigned_by,v.assigned_at,
      v.entered_at,v.revision,
      (SELECT coalesce(json_agg(s ORDER BY s.seal_no), '[]') FROM cargo_sea.container_seal s WHERE s.tenant_id=v.tenant_id AND s.visit_id=v.visit_id),
      (SELECT coalesce(json_agg(a ORDER BY a.version), '[]') FROM cargo_sea.vgm_assertion a WHERE a.tenant_id=v.tenant_id AND a.visit_id=v.visit_id),
      (SELECT coalesce(json_agg(e ORDER BY e.event_no), '[]') FROM cargo_sea.container_event e WHERE e.tenant_id=v.tenant_id AND e.visit_id=v.visit_id)
      FROM cargo_sea.container_visit v WHERE v.tenant_id=$1 AND v.visit_id=$2`, string(tenant), string(id)).Scan(
		&st.MovementID, &st.ConsolidationID, &st.LoadUnitNo, &st.Number, &st.SizeType, &tare, &maxGross, &plate, &st.Status,
		&execution, &difference, &stuffedBy, &stuffedAt, &phySeal, &phySealedAt, &releaseReason, &releasedBy, &releasedAt, &st.AssignedBy, &st.AssignedAt, &st.EnteredAt,
		&revision, &seals, &assertions, &events)
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
	st.StuffedPhySeal, st.StuffedPhySealedAt = deref(phySeal), utcTime(phySealedAt)
	st.AssignedAt, st.EnteredAt = st.AssignedAt.UTC(), st.EnteredAt.UTC()
	for _, r := range rows {
		st.Seals = append(st.Seals, sea.VisitSeal{No: r.SealNo, Number: r.Number, Party: r.Party, AppliedAt: utc(r.AppliedAt), Source: r.Source,
			By: r.RecordedBy, At: utc(r.RecordedAt), EnteredAt: utc(r.EnteredAt)})
	}
	var asserted []assertionRow
	if err = json.Unmarshal(assertions, &asserted); err != nil {
		return nil, 0, err
	}
	for _, a := range asserted {
		st.Assertions = append(st.Assertions, sea.VgmAssertion{Version: a.Version, MassGrams: a.MassGrams, Method: a.Method, PartyID: a.PartyID,
			PartyRevision: a.PartyRevision, DeterminedAt: utc(a.DeterminedAt), SubmittedAt: utc(a.SubmittedAt), LateReason: deref(a.LateReason),
			By: a.RecordedBy, At: utc(a.RecordedAt), EnteredAt: utc(a.EnteredAt), Response: deref(a.Response), ResponseSource: deref(a.ResponseSource),
			RespondedAt: utcTime(a.RespondedAt), ResponseBy: deref(a.ResponseBy), ResponseEnteredAt: utcTime(a.ResponseEnteredAt)})
	}
	var happened []eventRow
	if err = json.Unmarshal(events, &happened); err != nil {
		return nil, 0, err
	}
	for _, e := range happened {
		st.Events = append(st.Events, sea.VisitEvent{No: e.EventNo, Kind: e.Kind, Location: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: e.Location},
			OccurredAt: utc(e.OccurredAt), Source: e.Source, Reference: e.Reference, By: e.RecordedBy, At: utc(e.RecordedAt), EnteredAt: utc(e.EnteredAt)})
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
      release_reason=$8, released_by=$9, released_at=$10, stuffed_phy_seal=$12, stuffed_phy_sealed_at=$13, revision=revision+1, updated_at=now()
      WHERE tenant_id=$1 AND visit_id=$2 AND revision=$11 RETURNING revision`,
		string(st.TenantID), string(st.VisitID), string(st.Status), orNull(st.ExecutionID), orNull(st.DifferenceReason), orNull(st.StuffedBy),
		orNull(st.StuffedAt), orNull(st.ReleaseReason), orNull(st.ReleasedBy), orNull(st.ReleasedAt), expected,
		orNull(st.StuffedPhySeal), orNull(st.StuffedPhySealedAt)).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: visit %s", bentorepo.ErrConflict, st.VisitID)
	}
	if err != nil {
		return 0, visitWriteErr(err, st)
	}
	if err = insertSeals(ctx, q, st, v.AddedSeals()); err != nil {
		return 0, err
	}
	if err = writeVGM(ctx, q, v); err != nil {
		return 0, err
	}
	if err = writeEvents(ctx, q, v); err != nil {
		return 0, err
	}
	return revision, nil
}

func writeEvents(ctx context.Context, q bentopg.Executor, v *sea.Visit) error {
	st := v.State()
	for _, e := range v.AddedEvents() {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_sea.container_event (tenant_id,visit_id,event_no,kind,location_code,occurred_at,source,reference,recorded_by,recorded_at,entered_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			string(st.TenantID), string(st.VisitID), e.No, e.Kind, e.Location.Code, e.OccurredAt, e.Source, e.Reference, e.By, e.At, e.EnteredAt); err != nil {
			return visitWriteErr(err, st)
		}
	}
	return nil
}

func writeVGM(ctx context.Context, q bentopg.Executor, v *sea.Visit) error {
	st := v.State()
	for _, a := range v.AddedAssertions() {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_sea.vgm_assertion (tenant_id,visit_id,version,mass_grams,method,responsible_party_id,
        responsible_party_revision,determined_at,submitted_at,late_reason,recorded_by,recorded_at,entered_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			string(st.TenantID), string(st.VisitID), a.Version, a.MassGrams, a.Method, a.PartyID, a.PartyRevision, a.DeterminedAt, a.SubmittedAt,
			orNull(a.LateReason), a.By, a.At, a.EnteredAt); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `INSERT INTO cargo_sea.vgm_signatory (tenant_id,visit_id,version,name) VALUES ($1,$2,$3,$4)`,
			string(st.TenantID), string(st.VisitID), a.Version, a.Signatory); err != nil {
			return err
		}
	}
	if version := v.RespondedAssertion(); version > 0 {
		a := st.Assertions[version-1]
		tag, err := q.Exec(ctx, `UPDATE cargo_sea.vgm_assertion SET response=$4, response_source=$5, responded_at=$6, response_by=$7, response_entered_at=$8
        WHERE tenant_id=$1 AND visit_id=$2 AND version=$3 AND response IS NULL`,
			string(st.TenantID), string(st.VisitID), version, a.Response, a.ResponseSource, a.RespondedAt, a.ResponseBy, a.ResponseEnteredAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: VGM assertion %d", bentorepo.ErrConflict, version)
		}
	}
	return nil
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
		case "container_event_source":
			return fmt.Errorf("%w: the source reference is already recorded", sea.ErrInvalidChange)
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

// LogSignatoryRead logs a read of the authorised person's name and answers the log entry's id. An unknown assertion
// has ok false and writes no log.
func (s VisitStore) LogSignatoryRead(ctx context.Context, id sea.VisitID, version int, actor, purpose string) (string, bool, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var access *string
	if err = q.QueryRow(ctx, `SELECT cargo_sea.log_vgm_signatory_read($1,$2,$3,$4)::text`, string(id), version, actor, purpose).Scan(&access); err != nil {
		return "", false, err
	}
	if access == nil {
		return "", false, nil
	}
	return *access, true, nil
}

// Signatory reads the name against a log entry committed by an earlier transaction. The database answers nothing
// for an entry of this transaction, of another tenant or more than a minute old.
func (s VisitStore) Signatory(ctx context.Context, access string) (string, bool, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var name *string
	if err = q.QueryRow(ctx, `SELECT cargo_sea.read_logged_vgm_signatory($1::uuid)`, access).Scan(&name); err != nil {
		return "", false, err
	}
	if name == nil {
		return "", false, nil
	}
	return *name, true, nil
}

// SignatoryAccess lists the logged reads of one assertion, newest first. The log holds no name.
func (s VisitStore) SignatoryAccess(ctx context.Context, tenant sea.TenantID, id sea.VisitID, version int) ([]app.SignatoryAccess, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT access_id::text, actor_id, purpose, recorded_at FROM cargo_sea.vgm_signatory_access
      WHERE tenant_id=$1 AND visit_id=$2 AND version=$3 ORDER BY recorded_at DESC, access_id`, string(tenant), string(id), version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.SignatoryAccess
	for rows.Next() {
		var a app.SignatoryAccess
		if err = rows.Scan(&a.AccessID, &a.ActorID, &a.Purpose, &a.RecordedAt); err != nil {
			return nil, err
		}
		a.RecordedAt = a.RecordedAt.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}
