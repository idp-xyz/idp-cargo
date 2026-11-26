package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// LegalEntityStore reads, adds and saves legal entities (since V2.106; the demo's seed wrote them before).
type LegalEntityStore struct{ DB *bentopg.DB }

// Load reads a legal entity and locks its row.
func (s LegalEntityStore) Load(ctx context.Context, tenant org.TenantID, id org.LegalEntityID) (*org.LegalEntity, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var name, jurisdiction, evidence, status string
	var revision int64
	err = q.QueryRow(ctx, `SELECT registered_name,jurisdiction,registration_evidence_ref,status,revision FROM cargo_org.legal_entity
      WHERE tenant_id=$1 AND legal_entity_id=$2 FOR UPDATE`, string(tenant), string(id)).Scan(&name, &jurisdiction, &evidence, &status, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	e, err := org.RestoreLegalEntity(tenant, id, name, jurisdiction, evidence, org.LegalEntityStatus(status))
	return e, revision, err
}

// Insert adds a legal entity at revision 1.
func (s LegalEntityStore) Insert(ctx context.Context, e *org.LegalEntity) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_org.legal_entity (tenant_id,legal_entity_id,registered_name,jurisdiction,registration_evidence_ref,status)
      VALUES ($1,$2,$3,$4,$5,$6) RETURNING revision`, string(e.Tenant()), string(e.ID()), e.RegisteredName(), e.Jurisdiction(), e.RegistrationEvidence(),
		string(e.Status())).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, bentorepo.ErrAlreadyExists
	}
	return revision, err
}

// Save writes a legal entity's status at the revision it was loaded at.
func (s LegalEntityStore) Save(ctx context.Context, e *org.LegalEntity, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_org.legal_entity SET status=$3,revision=revision+1 WHERE tenant_id=$1 AND legal_entity_id=$2 AND revision=$4
      RETURNING revision`, string(e.Tenant()), string(e.ID()), string(e.Status()), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, bentorepo.ErrConflict
	}
	return revision, err
}

// PartySummaryRecord is a party in a list.
type PartySummaryRecord struct {
	PartyID, LegalName, EnglishName, Country, Status string
	Revision                                         int64
	Roles                                            []org.Role
}

// PartyListing lists parties for people's lists (since V2.106).
type PartyListing struct{ DB *bentopg.DB }

// List answers at most limit parties after the id after, in id order, narrowed by status, role and a name that
// contains the text (case-insensitive).
func (s PartyListing) List(ctx context.Context, tenant org.TenantID, status, role, name, after string, limit int) ([]PartySummaryRecord, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	pattern := ""
	if name = strings.TrimSpace(name); name != "" {
		pattern = "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(name)) + "%"
	}
	rows, err := q.Query(ctx, `SELECT p.party_id,p.legal_name,coalesce(p.english_name,''),p.registration_country,p.status,p.revision
      FROM cargo_org.business_party p
      WHERE p.tenant_id=$1 AND p.party_id > $2 AND ($3='' OR p.status=$3)
        AND ($4='' OR EXISTS (SELECT 1 FROM cargo_org.party_role r WHERE r.tenant_id=p.tenant_id AND r.party_id=p.party_id AND r.role_code=$4 AND r.approval_state='APPROVED'))
        AND ($5='' OR lower(p.legal_name) LIKE $5 OR lower(coalesce(p.english_name,'')) LIKE $5)
      ORDER BY p.party_id LIMIT $6`, string(tenant), after, status, role, pattern, limit)
	if err != nil {
		return nil, err
	}
	var out []PartySummaryRecord
	for rows.Next() {
		var r PartySummaryRecord
		if err = rows.Scan(&r.PartyID, &r.LegalName, &r.EnglishName, &r.Country, &r.Status, &r.Revision); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Roles, err = rolesOf(ctx, q, tenant, org.PartyID(out[i].PartyID)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func rolesOf(ctx context.Context, q bentopg.Querier, tenant org.TenantID, party org.PartyID) ([]org.Role, error) {
	rows, err := q.Query(ctx, `SELECT role_code,legal_entity_id,valid_from,valid_until,evidence_ref,approved_by,approved_at FROM cargo_org.party_role
      WHERE tenant_id=$1 AND party_id=$2 AND approval_state='APPROVED' ORDER BY role_code,legal_entity_id,valid_from`, string(tenant), string(party))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []org.Role
	for rows.Next() {
		var r org.Role
		var code, entity string
		var until *time.Time
		if err = rows.Scan(&code, &entity, &r.ValidFrom, &until, &r.EvidenceRef, &r.ApprovedBy, &r.ApprovedAt); err != nil {
			return nil, err
		}
		r.Code, r.LegalEntityID = org.RoleCode(code), org.LegalEntityID(entity)
		if until != nil {
			r.ValidUntil = *until
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LegalEntityListing lists a tenant's legal entities.
type LegalEntityListing struct{ DB *bentopg.DB }

func (s LegalEntityListing) List(ctx context.Context, tenant org.TenantID) ([]LegalEntityRecord, []string, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, `SELECT legal_entity_id,revision,registered_name,jurisdiction,status FROM cargo_org.legal_entity WHERE tenant_id=$1
      ORDER BY legal_entity_id`, string(tenant))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []LegalEntityRecord
	var ids []string
	for rows.Next() {
		var id string
		var r LegalEntityRecord
		if err = rows.Scan(&id, &r.Revision, &r.RegisteredName, &r.Jurisdiction, &r.Status); err != nil {
			return nil, nil, err
		}
		out, ids = append(out, r), append(ids, id)
	}
	return out, ids, rows.Err()
}
