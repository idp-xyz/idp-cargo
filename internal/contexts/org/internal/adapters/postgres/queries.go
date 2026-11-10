package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// PartyReader reads a party with its approved roles, English name and active identifiers for queries. One
// statement means one snapshot under READ COMMITTED, so the revision matches them without locking the party row.
type PartyReader struct{ DB *bentopg.DB }

func (r PartyReader) Read(ctx context.Context, tenant org.TenantID, party org.PartyID) (*org.BusinessParty, int64, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.Query(ctx, `SELECT p.legal_name,p.registration_country,p.status,p.revision,coalesce(p.english_name,''),`+identifierColumns+`,
        r.role_code,r.legal_entity_id,r.valid_from,r.valid_until,r.evidence_ref,r.approved_by,r.approved_at
      FROM cargo_org.business_party p
      LEFT JOIN cargo_org.party_role r
        ON r.tenant_id=p.tenant_id AND r.party_id=p.party_id AND r.approval_state='APPROVED'
      WHERE p.tenant_id=$1 AND p.party_id=$2
      ORDER BY r.valid_from,r.role_id`, string(tenant), string(party))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var name, country, status, english, identifiers string
	var revision int64
	var roles []org.Role
	found := false
	for rows.Next() {
		var code, entity, evidence, approver *string
		var from, until, approvedAt *time.Time
		if err = rows.Scan(&name, &country, &status, &revision, &english, &identifiers, &code, &entity, &from, &until, &evidence, &approver, &approvedAt); err != nil {
			return nil, 0, err
		}
		found = true
		if code == nil {
			continue
		}
		role := org.Role{Code: org.RoleCode(*code), LegalEntityID: org.LegalEntityID(*entity), ValidFrom: *from,
			EvidenceRef: *evidence, ApprovedBy: *approver, ApprovedAt: *approvedAt}
		if until != nil {
			role.ValidUntil = *until
		}
		roles = append(roles, role)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, bentorepo.ErrNotFound
	}
	aggregate, err := org.RestoreParty(tenant, party, name, country, org.PartyStatus(status), roles)
	if err != nil {
		return nil, 0, err
	}
	stored, err := decodeIdentifiers(identifiers)
	if err != nil {
		return nil, 0, err
	}
	return aggregate, revision, aggregate.RestoreIdentity(english, stored)
}

// LegalEntityStatuses reads legal entity status for queries.
type LegalEntityStatuses struct{ DB *bentopg.DB }

func (s LegalEntityStatuses) Status(ctx context.Context, tenant org.TenantID, id org.LegalEntityID) (org.LegalEntityStatus, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return "", err
	}
	var status string
	err = q.QueryRow(ctx, `SELECT status FROM cargo_org.legal_entity WHERE tenant_id=$1 AND legal_entity_id=$2`,
		string(tenant), string(id)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", bentorepo.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return org.LegalEntityStatus(status), nil
}
