package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// CarrierStore loads and writes the carrier's documents.
type CarrierStore struct{ DB *bentopg.DB }

var _ app.CarrierStore = CarrierStore{}

const carrierColumns = `document_id,kind,movement_id,consolidation_id,legal_entity_id,carrier_party_id,carrier_role,number,content,content_sha256,source_version,
  recorded_by,recorded_at,entered_at,state,verified_by,verified_at,verify_entered_at,cancel_reason,cancelled_by,cancelled_at,revision`

func scanCarrier(row pgx.Row, tenant doc.TenantID) (*doc.CarrierDocument, int64, error) {
	st := doc.CarrierDocumentState{TenantID: tenant}
	var id, kind, content, state string
	var verifiedBy, reason, cancelledBy *string
	var verifiedAt, verifyEntered, cancelledAt *time.Time
	err := row.Scan(&id, &kind, &st.MovementID, &st.ConsolidationID, &st.LegalEntityID, &st.CarrierPartyID, &st.CarrierRole, &st.Number, &content,
		&st.ContentSHA256, &st.SourceVersion, &st.RecordedBy, &st.RecordedAt, &st.EnteredAt, &state, &verifiedBy, &verifiedAt, &verifyEntered, &reason,
		&cancelledBy, &cancelledAt, &st.Revision)
	if err != nil {
		return nil, 0, err
	}
	st.DocumentID, st.Kind, st.Content, st.State = doc.DocumentID(id), doc.Kind(kind), []byte(content), doc.CarrierState(state)
	st.RecordedAt, st.EnteredAt = st.RecordedAt.UTC(), st.EnteredAt.UTC()
	st.VerifiedBy, st.CancelReason, st.CancelledBy = deref(verifiedBy), deref(reason), deref(cancelledBy)
	for from, to := range map[*time.Time]*time.Time{verifiedAt: &st.VerifiedAt, verifyEntered: &st.VerifyEntered, cancelledAt: &st.CancelledAt} {
		if from != nil {
			*to = from.UTC()
		}
	}
	x, err := doc.RestoreCarrierDocument(st)
	return x, st.Revision, err
}

func (s CarrierStore) one(ctx context.Context, q queryRower, tenant doc.TenantID, where, lock, what string, arg any) (*doc.CarrierDocument, int64, error) {
	x, revision, err := scanCarrier(q.QueryRow(ctx, `SELECT `+carrierColumns+` FROM cargo_doc.carrier_document WHERE tenant_id=$1 AND `+where+lock, string(tenant), arg), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: %s", bentorepo.ErrNotFound, what)
	}
	return x, revision, err
}

func (s CarrierStore) Load(ctx context.Context, tenant doc.TenantID, id string) (*doc.CarrierDocument, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.one(ctx, q, tenant, `document_id=$2`, ` FOR NO KEY UPDATE`, "carrier document "+id, id)
}

func (s CarrierStore) Read(ctx context.Context, tenant doc.TenantID, id string) (*doc.CarrierDocument, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.one(ctx, q, tenant, `document_id=$2`, "", "carrier document "+id, id)
}

func (s CarrierStore) ReadLive(ctx context.Context, tenant doc.TenantID, movement string) (*doc.CarrierDocument, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.one(ctx, q, tenant, `movement_id=$2 AND state<>'CANCELLED'`, "", "a carrier document of movement "+movement, movement)
}

// Insert writes a recorded carrier document at revision 1. A document id used already is ErrAlreadyExists, and a
// second document of a movement that is not cancelled a conflict, as the unique index answers when two commands race.
func (s CarrierStore) Insert(ctx context.Context, x *doc.CarrierDocument) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := x.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_doc.carrier_document (tenant_id,document_id,kind,movement_id,consolidation_id,legal_entity_id,carrier_party_id,
        carrier_role,number,content,content_sha256,source_version,recorded_by,recorded_at,entered_at,state)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING revision`, string(st.TenantID), string(st.DocumentID), string(st.Kind),
		st.MovementID, st.ConsolidationID, st.LegalEntityID, st.CarrierPartyID, st.CarrierRole, st.Number, string(st.Content), st.ContentSHA256,
		st.SourceVersion, st.RecordedBy, st.RecordedAt, st.EnteredAt, string(st.State)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "carrier_document_pkey":
			return 0, fmt.Errorf("%w: carrier document %s", bentorepo.ErrAlreadyExists, st.DocumentID)
		case "carrier_document_one_live":
			return 0, fmt.Errorf("%w: movement %s has a carrier document that is not cancelled", bentorepo.ErrConflict, st.MovementID)
		case "carrier_document_one_number":
			return 0, fmt.Errorf("%w: %s of carrier %s is on another carrier document that is not cancelled", doc.ErrNumberIssued, st.Number, st.CarrierPartyID)
		}
	}
	return revision, err
}

// Save writes the state, the verification and the cancellation at the expected revision.
func (s CarrierStore) Save(ctx context.Context, x *doc.CarrierDocument, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := x.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_doc.carrier_document SET state=$3,verified_by=$4,verified_at=$5,verify_entered_at=$6,cancel_reason=$7,cancelled_by=$8,
        cancelled_at=$9,revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND document_id=$2 AND revision=$10 RETURNING revision`, string(st.TenantID),
		string(st.DocumentID), string(st.State), orNull(st.VerifiedBy), orNull(st.VerifiedAt), orNull(st.VerifyEntered), orNull(st.CancelReason),
		orNull(st.CancelledBy), orNull(st.CancelledAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: carrier document %s", bentorepo.ErrConflict, st.DocumentID)
	}
	return revision, err
}

func (s CarrierStore) NumberMovement(ctx context.Context, tenant doc.TenantID, carrier, number string) (string, bool, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var movement string
	err = q.QueryRow(ctx, `SELECT movement_id FROM cargo_doc.carrier_document WHERE tenant_id=$1 AND carrier_party_id=$2 AND number=$3 AND state<>'CANCELLED'`,
		string(tenant), carrier, number).Scan(&movement)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return movement, err == nil, err
}
