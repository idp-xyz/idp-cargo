// Package postgres persists DOC documents, issuance authorities, command receipts and Outbox events in the
// tenant's RLS scope.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

func orNull[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// DocumentStore loads and writes documents.
type DocumentStore struct{ DB *bentopg.DB }

var _ app.DocumentStore = DocumentStore{}

type (
	versionRow struct {
		VersionNo int       `json:"version_no"`
		Content   string    `json:"content"`
		SHA256    string    `json:"content_sha256"`
		Reason    *string   `json:"reason"`
		DraftedBy string    `json:"drafted_by"`
		DraftedAt time.Time `json:"drafted_at"`
		EnteredAt time.Time `json:"entered_at"`
	}
	approvalRow struct {
		VersionNo    int       `json:"version_no"`
		ApprovedBy   string    `json:"approved_by"`
		ApprovedAt   time.Time `json:"approved_at"`
		SelfApproved bool      `json:"self_approved"`
		EnteredAt    time.Time `json:"entered_at"`
	}
	issueRow struct {
		VersionNo        int       `json:"version_no"`
		IssuedBy         string    `json:"issued_by"`
		IssuedAt         time.Time `json:"issued_at"`
		AuthorityID      string    `json:"authority_id"`
		AuthorityVersion int64     `json:"authority_version"`
		CreditBasis      string    `json:"credit_basis"`
		DecisionID       *string   `json:"credit_decision_id"`
		OverrideBy       *string   `json:"credit_override_by"`
		OverrideReason   *string   `json:"credit_override_reason"`
		EnteredAt        time.Time `json:"entered_at"`
	}
	discardRow struct {
		VersionNo   int       `json:"version_no"`
		DiscardedBy string    `json:"discarded_by"`
		DiscardedAt time.Time `json:"discarded_at"`
		Reason      string    `json:"reason"`
		EnteredAt   time.Time `json:"entered_at"`
	}
)

func child(table string) string {
	return `(SELECT coalesce(json_agg(x ORDER BY x.version_no), '[]') FROM cargo_doc.` + table +
		` x WHERE x.tenant_id=d.tenant_id AND x.document_id=d.document_id)`
}

func (s DocumentStore) Load(ctx context.Context, tenant doc.TenantID, id doc.DocumentID) (*doc.Document, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_doc.transport_document WHERE tenant_id=$1 AND document_id=$2 FOR NO KEY UPDATE`, string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: document %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return readDocument(ctx, q, tenant, id)
}

func (s DocumentStore) Read(ctx context.Context, tenant doc.TenantID, id doc.DocumentID) (*doc.Document, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readDocument(ctx, q, tenant, id)
}

// readDocument reads a document with its versions and their records in one statement, one snapshot.
func readDocument(ctx context.Context, q queryRower, tenant doc.TenantID, id doc.DocumentID) (*doc.Document, int64, error) {
	st := doc.DocumentState{TenantID: tenant, DocumentID: id}
	var kind, state string
	var shipment, cancelReason, cancelledBy *string
	var cancelledAt *time.Time
	var revision int64
	var versions, approvals, issues, discards []byte
	err := q.QueryRow(ctx, `SELECT d.kind,d.movement_id,d.consolidation_id,d.shipment_id,d.number,d.legal_entity_id,d.state,d.cancel_reason,d.cancelled_by,
        d.cancelled_at,d.revision,`+child("document_version")+`,`+child("document_version_approval")+`,`+child("document_version_issue")+`,`+
		child("document_version_discard")+`
      FROM cargo_doc.transport_document d WHERE d.tenant_id=$1 AND d.document_id=$2`, string(tenant), string(id)).Scan(&kind, &st.MovementID,
		&st.ConsolidationID, &shipment, &st.Number, &st.LegalEntityID, &state, &cancelReason, &cancelledBy, &cancelledAt, &revision, &versions, &approvals,
		&issues, &discards)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: document %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	var vr []versionRow
	var ar []approvalRow
	var ir []issueRow
	var xr []discardRow
	if err = errors.Join(json.Unmarshal(versions, &vr), json.Unmarshal(approvals, &ar), json.Unmarshal(issues, &ir), json.Unmarshal(discards, &xr)); err != nil {
		return nil, 0, err
	}
	st.Kind, st.State, st.ShipmentID = doc.Kind(kind), doc.State(state), deref(shipment)
	if cancelledAt != nil {
		st.CancelReason, st.CancelledBy, st.CancelledAt = deref(cancelReason), deref(cancelledBy), cancelledAt.UTC()
	}
	for _, v := range vr {
		st.Versions = append(st.Versions, doc.Version{No: v.VersionNo, Content: []byte(v.Content), SHA256: v.SHA256, Reason: deref(v.Reason),
			DraftedBy: v.DraftedBy, DraftedAt: utc(v.DraftedAt), EnteredAt: utc(v.EnteredAt)})
	}
	at := func(no int) *doc.Version {
		if no < 1 || no > len(st.Versions) {
			return &doc.Version{}
		}
		return &st.Versions[no-1]
	}
	for _, a := range ar {
		at(a.VersionNo).Approval = &doc.Approval{By: a.ApprovedBy, At: utc(a.ApprovedAt), SelfApproved: a.SelfApproved, EnteredAt: utc(a.EnteredAt)}
	}
	for _, i := range ir {
		at(i.VersionNo).Issue = &doc.Issue{By: i.IssuedBy, At: utc(i.IssuedAt), AuthorityID: i.AuthorityID, AuthorityVersion: i.AuthorityVersion,
			Credit:    doc.Credit{Basis: i.CreditBasis, DecisionID: deref(i.DecisionID), By: deref(i.OverrideBy), Reason: deref(i.OverrideReason)},
			EnteredAt: utc(i.EnteredAt)}
	}
	for _, x := range xr {
		at(x.VersionNo).Discard = &doc.Discard{By: x.DiscardedBy, At: utc(x.DiscardedAt), Reason: x.Reason, EnteredAt: utc(x.EnteredAt)}
	}
	m, err := doc.RestoreDocument(st)
	return m, revision, err
}

// Insert writes a new document at revision 1 with its first version.
func (s DocumentStore) Insert(ctx context.Context, m *doc.Document) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := m.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_doc.transport_document (tenant_id,document_id,kind,movement_id,consolidation_id,shipment_id,number,
        legal_entity_id,state) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING revision`, string(st.TenantID), string(st.DocumentID), string(st.Kind),
		st.MovementID, st.ConsolidationID, orNull(st.ShipmentID), st.Number, st.LegalEntityID, string(st.State)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "transport_document_pkey":
			return 0, fmt.Errorf("%w: document %s", bentorepo.ErrAlreadyExists, st.DocumentID)
		case "transport_document_one_house", "transport_document_one_master":
			return 0, fmt.Errorf("%w: the %s of %s%s has a document already", bentorepo.ErrConflict, st.Kind, st.ShipmentID, st.ConsolidationID)
		}
	}
	if err != nil {
		return 0, err
	}
	return revision, writeAdded(ctx, q, m)
}

// Save writes the state and what the document added since it was loaded, at the expected revision.
func (s DocumentStore) Save(ctx context.Context, m *doc.Document, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := m.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_doc.transport_document SET state=$3,cancel_reason=$4,cancelled_by=$5,cancelled_at=$6,movement_id=$8,
        consolidation_id=$9,revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND document_id=$2 AND revision=$7 RETURNING revision`,
		string(st.TenantID), string(st.DocumentID), string(st.State), orNull(st.CancelReason), orNull(st.CancelledBy), orNull(st.CancelledAt), expected,
		st.MovementID, st.ConsolidationID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: document %s", bentorepo.ErrConflict, st.DocumentID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writeAdded(ctx, q, m)
}

func writeAdded(ctx context.Context, q bentopg.Executor, m *doc.Document) error {
	st := m.State()
	exec := func(sql string, args ...any) error {
		_, err := q.Exec(ctx, sql, append([]any{string(st.TenantID), string(st.DocumentID)}, args...)...)
		return err
	}
	versions, approvals, issues, discards := m.Added()
	for _, v := range versions {
		if err := exec(`INSERT INTO cargo_doc.document_version (tenant_id,document_id,version_no,content,content_sha256,source_movement_id,source_version,
            reason,drafted_by,drafted_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, v.No, string(v.Content), v.SHA256, v.Doc.Source.MovementID,
			v.Doc.Source.Version, orNull(v.Reason), v.DraftedBy, v.DraftedAt, v.EnteredAt); err != nil {
			return err
		}
	}
	for _, v := range approvals {
		a := v.Approval
		if err := exec(`INSERT INTO cargo_doc.document_version_approval (tenant_id,document_id,version_no,approved_by,approved_at,self_approved,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7)`, v.No, a.By, a.At, a.SelfApproved, a.EnteredAt); err != nil {
			return err
		}
	}
	for _, v := range issues {
		i := v.Issue
		if err := exec(`INSERT INTO cargo_doc.document_version_issue (tenant_id,document_id,version_no,issued_by,issued_at,authority_id,authority_version,
            credit_basis,credit_decision_id,credit_override_by,credit_override_reason,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, v.No, i.By,
			i.At, i.AuthorityID, i.AuthorityVersion, i.Credit.Basis, orNull(i.Credit.DecisionID), orNull(i.Credit.By), orNull(i.Credit.Reason),
			i.EnteredAt); err != nil {
			return err
		}
		// the number is issued for the shipment (HAWB) or movement (MAWB) once, by the first document; a successor
		// document of the same shipment issues it again, another shipment never
		if err := exec(`INSERT INTO cargo_doc.issued_number (tenant_id,first_document_id,legal_entity_id,kind,number,subject_id,first_issued_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (tenant_id, legal_entity_id, kind, number) DO NOTHING`, st.LegalEntityID, string(st.Kind), st.Number,
			st.Subject(), i.At); err != nil {
			return err
		}
		subject, _, err := DocumentStore{}.issuedFor(ctx, q, st.TenantID, st.LegalEntityID, st.Kind, st.Number)
		if err != nil {
			return err
		}
		if subject != st.Subject() {
			return fmt.Errorf("%w: %s %s was issued for %s", doc.ErrNumberIssued, st.Kind, st.Number, subject)
		}
	}
	for _, v := range discards {
		x := v.Discard
		if err := exec(`INSERT INTO cargo_doc.document_version_discard (tenant_id,document_id,version_no,discarded_by,discarded_at,reason,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7)`, v.No, x.By, x.At, x.Reason, x.EnteredAt); err != nil {
			return err
		}
	}
	return nil
}

func (DocumentStore) issuedFor(ctx context.Context, q queryRower, tenant doc.TenantID, legalEntity string, kind doc.Kind, number string) (string, bool, error) {
	var subject string
	err := q.QueryRow(ctx, `SELECT subject_id FROM cargo_doc.issued_number WHERE tenant_id=$1 AND legal_entity_id=$2 AND kind=$3 AND number=$4`,
		string(tenant), legalEntity, string(kind), number).Scan(&subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return subject, err == nil, err
}

// IssuedFor returns the shipment or movement a number was issued for.
func (s DocumentStore) IssuedFor(ctx context.Context, tenant doc.TenantID, legalEntity string, kind doc.Kind, number string) (string, bool, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	return s.issuedFor(ctx, q, tenant, legalEntity, kind, number)
}

// List lists the documents of a shipment or a consolidation.
func (s DocumentStore) List(ctx context.Context, tenant doc.TenantID, shipment, consolidation string) ([]app.Summary, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT d.document_id,d.kind,d.state,d.number,d.revision,
        coalesce((SELECT max(i.version_no) FROM cargo_doc.document_version_issue i WHERE i.tenant_id=d.tenant_id AND i.document_id=d.document_id),0),
        (SELECT max(v.version_no) FROM cargo_doc.document_version v WHERE v.tenant_id=d.tenant_id AND v.document_id=d.document_id)
      FROM cargo_doc.transport_document d
      WHERE d.tenant_id=$1 AND (($2::text <> '' AND d.shipment_id=$2) OR ($3::text <> '' AND d.consolidation_id=$3))
      ORDER BY d.kind DESC, d.number, d.document_id`, string(tenant), shipment, consolidation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.Summary
	for rows.Next() {
		var s app.Summary
		var kind, state string
		if err = rows.Scan(&s.DocumentID, &kind, &state, &s.Number, &s.Revision, &s.CurrentVersion, &s.LatestVersion); err != nil {
			return nil, err
		}
		s.Kind, s.State = doc.Kind(kind), doc.State(state)
		if s.State == doc.StateCancelled {
			s.CurrentVersion = 0
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AuthorityStore loads and writes issuance authorities.
type AuthorityStore struct{ DB *bentopg.DB }

var _ app.AuthorityStore = AuthorityStore{}

const authorityColumns = `authority_id,legal_entity_id,kind,issue_place,airline_party_id,qualification,valid_from,valid_until,evidence_ref,recorded_by,
  recorded_at,entered_at,withdrawn_by,withdrawn_at,withdraw_reason,withdraw_entered_at,revision`

func scanAuthority(row pgx.Row, tenant doc.TenantID) (*doc.IssuanceAuthority, int64, error) {
	a := doc.IssuanceAuthority{TenantID: tenant}
	var kind string
	var airline, withdrawnBy, reason *string
	var until, withdrawnAt, withdrawEntered *time.Time
	var revision int64
	err := row.Scan(&a.AuthorityID, &a.LegalEntityID, &kind, &a.IssuePlace, &airline, &a.Qualification, &a.Validity.From, &until, &a.EvidenceRef,
		&a.RecordedBy, &a.RecordedAt, &a.EnteredAt, &withdrawnBy, &withdrawnAt, &reason, &withdrawEntered, &revision)
	if err != nil {
		return nil, 0, err
	}
	a.Kind, a.AirlinePartyID, a.Version = doc.Kind(kind), deref(airline), revision
	a.Validity.From, a.RecordedAt, a.EnteredAt = a.Validity.From.UTC(), a.RecordedAt.UTC(), a.EnteredAt.UTC()
	if until != nil {
		a.Validity.Until = until.UTC()
	}
	if withdrawnAt != nil {
		a.WithdrawnBy, a.WithdrawnAt, a.WithdrawReason = deref(withdrawnBy), withdrawnAt.UTC(), deref(reason)
		if withdrawEntered != nil {
			a.WithdrawEntered = withdrawEntered.UTC()
		}
	}
	restored, err := doc.RestoreIssuanceAuthority(a)
	return restored, revision, err
}

func (s AuthorityStore) read(ctx context.Context, q queryRower, tenant doc.TenantID, id, lock string) (*doc.IssuanceAuthority, int64, error) {
	a, revision, err := scanAuthority(q.QueryRow(ctx, `SELECT `+authorityColumns+` FROM cargo_doc.issuance_authority WHERE tenant_id=$1 AND authority_id=$2`+lock,
		string(tenant), id), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: issuance authority %s", bentorepo.ErrNotFound, id)
	}
	return a, revision, err
}

func (s AuthorityStore) Load(ctx context.Context, tenant doc.TenantID, id string) (*doc.IssuanceAuthority, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.read(ctx, q, tenant, id, " FOR UPDATE")
}

func (s AuthorityStore) Read(ctx context.Context, tenant doc.TenantID, id string) (*doc.IssuanceAuthority, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.read(ctx, q, tenant, id, "")
}

func (s AuthorityStore) Insert(ctx context.Context, a *doc.IssuanceAuthority) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_doc.issuance_authority (tenant_id,authority_id,legal_entity_id,kind,issue_place,airline_party_id,qualification,
        valid_from,valid_until,evidence_ref,recorded_by,recorded_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING revision`,
		string(a.TenantID), a.AuthorityID, a.LegalEntityID, string(a.Kind), a.IssuePlace, orNull(a.AirlinePartyID), a.Qualification, a.Validity.From,
		orNull(a.Validity.Until), a.EvidenceRef, a.RecordedBy, a.RecordedAt, a.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "issuance_authority_pkey" {
		return 0, fmt.Errorf("%w: issuance authority %s", bentorepo.ErrAlreadyExists, a.AuthorityID)
	}
	return revision, err
}

func (s AuthorityStore) Save(ctx context.Context, a *doc.IssuanceAuthority, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_doc.issuance_authority SET withdrawn_by=$3,withdrawn_at=$4,withdraw_reason=$5,withdraw_entered_at=$6,
        revision=revision+1 WHERE tenant_id=$1 AND authority_id=$2 AND revision=$7 RETURNING revision`, string(a.TenantID), a.AuthorityID,
		a.WithdrawnBy, a.WithdrawnAt, a.WithdrawReason, a.WithdrawEntered, expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: issuance authority %s", bentorepo.ErrConflict, a.AuthorityID)
	}
	return revision, err
}

// InForce finds the authority in force and holds it FOR SHARE, so a withdrawal waits for the issue to commit.
func (s AuthorityStore) InForce(ctx context.Context, tenant doc.TenantID, legalEntity string, kind doc.Kind, place, airline string,
	at time.Time) (*doc.IssuanceAuthority, bool, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, false, err
	}
	a, _, err := scanAuthority(q.QueryRow(ctx, `SELECT `+authorityColumns+` FROM cargo_doc.issuance_authority
      WHERE tenant_id=$1 AND legal_entity_id=$2 AND kind=$3 AND issue_place=$4 AND coalesce(airline_party_id,'')=$5
        AND valid_from <= $6 AND (valid_until IS NULL OR $6 < valid_until) AND (withdrawn_at IS NULL OR $6 < withdrawn_at)
      ORDER BY recorded_at, authority_id LIMIT 1 FOR SHARE`, string(tenant), legalEntity, string(kind), place, airline, at), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return a, a.Covers(legalEntity, kind, place, airline, at), nil
}

// CommandReceipts stores the result of every committed DOC command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

var _ app.Receipts = CommandReceipts{}

func (s CommandReceipts) Find(ctx context.Context, tenant doc.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var version *int
	err = q.QueryRow(ctx, `SELECT request_sha256,result_id,result_revision,result_version FROM cargo_doc.command_receipt WHERE tenant_id=$1 AND command_id=$2`,
		string(tenant), id).Scan(&r.RequestHash, &r.SubjectID, &r.NewRevision, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	if version != nil {
		r.Version = *version
	}
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant doc.TenantID, r app.Receipt, kind string) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_doc.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,result_version)
      VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(tenant), r.CommandID, r.RequestHash, kind, r.SubjectID, r.NewRevision, orNull(r.Version))
	return err
}

// Outbox writes DOC events to the Bento Outbox in the command's transaction.
type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueDOCEvent(ctx context.Context, e doc.Event) error {
	envelope, err := DOCEnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// DOCEnvelope maps a DOC domain event to its Bento envelope; a retried command yields the same event ID.
func DOCEnvelope(commandID string, e doc.Event, recordedAt time.Time) (eventing.Envelope, error) {
	digest := sha256.Sum256([]byte(commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.DocumentID + "\x00" + strconv.Itoa(e.Version)))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), SubjectID: e.DocumentID, Kind: e.Kind, Version: e.Version,
		ShipmentID: e.ShipmentID, ConsolidationID: e.ConsolidationID, MovementID: e.MovementID})
	if err != nil {
		return eventing.Envelope{}, err
	}
	envelope := eventing.Envelope{SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(hex.EncodeToString(digest[:])), Source: api.Source,
		Type: eventing.EventType(e.Type), Version: 1, Scope: string(e.TenantID), Subject: e.DocumentID, PartitionKey: string(e.TenantID) + ":" + e.DocumentID,
		OccurredAt: e.At.UTC(), RecordedAt: recordedAt.UTC(), ContentType: eventing.JSONContentType, Payload: payload}
	return envelope, envelope.Validate()
}

// Conflict reports whether a commit failed on one of DOC's deferred checks.
func Conflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" &&
		(pgErr.ConstraintName == "transport_document_consistent" || pgErr.ConstraintName == "document_records_with_document")
}
