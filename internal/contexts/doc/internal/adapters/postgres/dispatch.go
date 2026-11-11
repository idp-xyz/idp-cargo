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
	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// DispatchStore loads and writes dispatches with their provision records.
type DispatchStore struct{ DB *bentopg.DB }

var _ app.DispatchStore = DispatchStore{}

type (
	contactRow struct {
		No             int     `json:"contact_no"`
		Key            string  `json:"contact_key"`
		PartyRole      string  `json:"party_role"`
		PartyID        *string `json:"party_id"`
		OrderID        string  `json:"order_id"`
		Source         string  `json:"source"`
		Classification string  `json:"classification"`
		LegalPath      string  `json:"legal_path"`
		Status         string  `json:"status"`
		OmitReason     *string `json:"omit_reason"`
		NoticeVersion  *string `json:"consent_notice_version"`
		Provide        *bool   `json:"consent_provide_to_others"`
		CrossBorder    *bool   `json:"consent_cross_border"`
		Basis          *string `json:"consent_basis"`
		ORGRevision    *int64  `json:"consent_org_revision"`
	}
	outcomeRow struct {
		No        int       `json:"outcome_no"`
		Outcome   string    `json:"outcome"`
		Reference *string   `json:"reference"`
		By        string    `json:"recorded_by"`
		At        time.Time `json:"recorded_at"`
		EnteredAt time.Time `json:"entered_at"`
	}
)

func dispatchChild(table, order string) string {
	return `(SELECT coalesce(json_agg(c ORDER BY c.` + order + `), '[]') FROM cargo_doc.` + table +
		` c WHERE c.tenant_id=x.tenant_id AND c.dispatch_id=x.dispatch_id)`
}

const dispatchColumns = `x.dispatch_id,x.document_id,x.version_no,x.kind,x.version_status,x.recipient_party_id,x.recipient_country,x.recipient_role,
  x.ordering_party_id,x.channel,x.copy_sha256,x.template,x.register_year,x.register_state,x.prepared_by,x.prepared_at,x.entered_at,x.revision,`

func scanDispatch(row pgx.Row, tenant doc.TenantID) (*doc.Dispatch, int64, error) {
	st := doc.DispatchState{TenantID: tenant}
	var document, kind string
	var ordering *string
	var revision int64
	var contacts, outcomes []byte
	err := row.Scan(&st.DispatchID, &document, &st.Version, &kind, &st.VersionStatus, &st.Recipient.PartyID, &st.Recipient.Country, &st.Recipient.Role,
		&ordering, &st.Channel, &st.CopySHA256, &st.Template, &st.RegisterYear, &st.RegisterState, &st.PreparedBy, &st.PreparedAt, &st.EnteredAt,
		&revision, &contacts, &outcomes)
	if err != nil {
		return nil, 0, err
	}
	var cr []contactRow
	var or []outcomeRow
	if err = errors.Join(json.Unmarshal(contacts, &cr), json.Unmarshal(outcomes, &or)); err != nil {
		return nil, 0, err
	}
	st.DocumentID, st.Kind, st.OrderingParty = doc.DocumentID(document), doc.Kind(kind), deref(ordering)
	st.PreparedAt, st.EnteredAt = st.PreparedAt.UTC(), st.EnteredAt.UTC()
	for _, c := range cr {
		d := doc.DispatchContact{No: c.No, Key: c.Key, PartyRole: c.PartyRole, PartyID: deref(c.PartyID), OrderID: c.OrderID, Source: c.Source,
			Classification: c.Classification, LegalPath: c.LegalPath, Status: c.Status, OmitReason: deref(c.OmitReason)}
		if c.Provide != nil {
			d.Consent = &doc.Consent{NoticeVersion: deref(c.NoticeVersion), ProvideToOthers: *c.Provide, CrossBorder: c.CrossBorder != nil && *c.CrossBorder,
				Basis: deref(c.Basis)}
			if c.ORGRevision != nil {
				d.Consent.ORGRevision = *c.ORGRevision
			}
		}
		st.Contacts = append(st.Contacts, d)
	}
	for _, o := range or {
		st.Outcomes = append(st.Outcomes, doc.Outcome{No: o.No, Outcome: o.Outcome, Reference: deref(o.Reference), By: o.By, At: utc(o.At),
			EnteredAt: utc(o.EnteredAt)})
	}
	x, err := doc.RestoreDispatch(st)
	return x, revision, err
}

// readDispatches reads dispatches with their contacts and outcomes in one statement, one snapshot.
func readDispatches(ctx context.Context, q bentopg.Querier, tenant doc.TenantID, where string, args ...any) ([]*doc.Dispatch, []int64, error) {
	rows, err := q.Query(ctx, `SELECT `+dispatchColumns+dispatchChild("dispatch_contact", "contact_no")+`,`+dispatchChild("dispatch_outcome", "outcome_no")+`
      FROM cargo_doc.document_dispatch x WHERE x.tenant_id=$1 AND `+where+` ORDER BY x.prepared_at, x.dispatch_id`, append([]any{string(tenant)}, args...)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*doc.Dispatch
	var revisions []int64
	for rows.Next() {
		x, revision, err := scanDispatch(rows, tenant)
		if err != nil {
			return nil, nil, err
		}
		out, revisions = append(out, x), append(revisions, revision)
	}
	return out, revisions, rows.Err()
}

func (s DispatchStore) one(ctx context.Context, q bentopg.Querier, tenant doc.TenantID, id string) (*doc.Dispatch, int64, error) {
	got, revisions, err := readDispatches(ctx, q, tenant, `x.dispatch_id=$2`, id)
	if err != nil {
		return nil, 0, err
	}
	if len(got) == 0 {
		return nil, 0, fmt.Errorf("%w: dispatch %s", bentorepo.ErrNotFound, id)
	}
	return got[0], revisions[0], nil
}

func (s DispatchStore) Load(ctx context.Context, tenant doc.TenantID, id string) (*doc.Dispatch, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_doc.document_dispatch WHERE tenant_id=$1 AND dispatch_id=$2 FOR NO KEY UPDATE`, string(tenant), id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: dispatch %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return s.one(ctx, q, tenant, id)
}

func (s DispatchStore) Read(ctx context.Context, tenant doc.TenantID, id string) (*doc.Dispatch, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.one(ctx, q, tenant, id)
}

// Insert writes a prepared dispatch at revision 1 with its provision records.
func (s DispatchStore) Insert(ctx context.Context, x *doc.Dispatch) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := x.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_doc.document_dispatch (tenant_id,dispatch_id,document_id,version_no,kind,version_status,recipient_party_id,
        recipient_country,recipient_role,ordering_party_id,channel,copy_sha256,template,register_year,register_state,state,prepared_by,prepared_at,entered_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'PREPARED',$16,$17,$18) RETURNING revision`, string(st.TenantID), st.DispatchID,
		string(st.DocumentID), st.Version, string(st.Kind), st.VersionStatus, st.Recipient.PartyID, st.Recipient.Country, st.Recipient.Role,
		orNull(st.OrderingParty), st.Channel, st.CopySHA256, st.Template, st.RegisterYear, st.RegisterState, st.PreparedBy, st.PreparedAt,
		st.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "document_dispatch_pkey" {
		return 0, fmt.Errorf("%w: dispatch %s", bentorepo.ErrAlreadyExists, st.DispatchID)
	}
	if err != nil {
		return 0, err
	}
	for _, c := range st.Contacts {
		var notice, basis, provide, cross, orgRevision any
		if k := c.Consent; k != nil {
			notice, basis, provide, cross, orgRevision = orNull(k.NoticeVersion), orNull(k.Basis), k.ProvideToOthers, k.CrossBorder, k.ORGRevision
		}
		if _, err = q.Exec(ctx, `INSERT INTO cargo_doc.dispatch_contact (tenant_id,dispatch_id,contact_no,contact_key,party_role,party_id,order_id,source,
            classification,legal_path,status,omit_reason,consent_notice_version,consent_provide_to_others,consent_cross_border,consent_basis,
            consent_org_revision) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, string(st.TenantID), st.DispatchID, c.No, c.Key,
			c.PartyRole, orNull(c.PartyID), c.OrderID, c.Source, c.Classification, c.LegalPath, c.Status, orNull(c.OmitReason), notice, provide, cross, basis,
			orgRevision); err != nil {
			return 0, err
		}
	}
	return revision, writeOutcomes(ctx, q, x)
}

// Save writes the state and the outcomes recorded since the dispatch was loaded, at the expected revision.
func (s DispatchStore) Save(ctx context.Context, x *doc.Dispatch, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := x.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_doc.document_dispatch SET state=$3,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND dispatch_id=$2 AND revision=$4 RETURNING revision`, string(st.TenantID), st.DispatchID, x.Current(), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: dispatch %s", bentorepo.ErrConflict, st.DispatchID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writeOutcomes(ctx, q, x)
}

func writeOutcomes(ctx context.Context, q bentopg.Executor, x *doc.Dispatch) error {
	st := x.State()
	for _, o := range x.AddedOutcomes() {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_doc.dispatch_outcome (tenant_id,dispatch_id,outcome_no,outcome,reference,recorded_by,recorded_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, string(st.TenantID), st.DispatchID, o.No, o.Outcome, orNull(o.Reference), o.By, o.At, o.EnteredAt); err != nil {
			return err
		}
	}
	return nil
}

// List lists a document's dispatches in the order they were prepared.
func (s DispatchStore) List(ctx context.Context, tenant doc.TenantID, document doc.DocumentID) ([]*doc.Dispatch, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	out, _, err := readDispatches(ctx, q, tenant, `x.document_id=$2`, string(document))
	return out, err
}

// Provisions lists the dispatches prepared in [from, until).
func (s DispatchStore) Provisions(ctx context.Context, tenant doc.TenantID, from, until time.Time) ([]*doc.Dispatch, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	out, _, err := readDispatches(ctx, q, tenant, `x.prepared_at >= $2 AND x.prepared_at < $3`, from, until)
	return out, err
}
