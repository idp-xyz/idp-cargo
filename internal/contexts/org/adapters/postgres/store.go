//go:build bento

// Package postgres provides scoped, transaction-bound ORG persistence adapters.
// It does not expose a shared MasterData entity to COM/BOOK/FIN.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/org/application"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/domain"
)

type PartyStore struct{ DB *bentopg.DB }

func (s PartyStore) Load(ctx context.Context, tenant org.TenantID, party org.PartyID) (*org.BusinessParty, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var name, country, status string
	var rev int64
	err = q.QueryRow(ctx, `SELECT legal_name,registration_country,status,revision FROM cargo_org.business_party
        WHERE tenant_id=$1 AND party_id=$2`, string(tenant), string(party)).Scan(&name, &country, &status, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.Query(ctx, `SELECT role_code,legal_entity_id,valid_from,valid_until,evidence_ref,approved_by,approved_at
      FROM cargo_org.party_role WHERE tenant_id=$1 AND party_id=$2 AND approval_state='APPROVED'
      ORDER BY valid_from,role_id`, string(tenant), string(party))
	if err != nil {
		return nil, 0, err
	}
	var roles []org.Role
	for rows.Next() {
		var code, le, evidence, approver string
		var start, approvedAt time.Time
		var until *time.Time
		if err = rows.Scan(&code, &le, &start, &until, &evidence, &approver, &approvedAt); err != nil {
			rows.Close()
			return nil, 0, err
		}
		role := org.Role{Code: org.RoleCode(code), LegalEntityID: org.LegalEntityID(le), ValidFrom: start, EvidenceRef: evidence, ApprovedBy: approver, ApprovedAt: approvedAt}
		if until != nil {
			role.ValidUntil = *until
		}
		roles = append(roles, role)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	aggregate, err := org.RestoreParty(tenant, party, name, country, org.PartyStatus(status), roles)
	return aggregate, rev, err
}
func (s PartyStore) Save(ctx context.Context, p *org.BusinessParty, expected int64) (int64, error) {
	if p == nil || expected < 1 {
		return 0, org.ErrInvalidIdentity
	}
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var newRevision int64
	err = q.QueryRow(ctx, `UPDATE cargo_org.business_party
      SET legal_name=$3,status=$4,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND party_id=$2 AND revision=$5
      RETURNING revision`, string(p.Tenant()), string(p.ID()), p.Name(), string(p.Status()), expected).Scan(&newRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, bentorepo.ErrConflict
	}
	if err != nil {
		return 0, err
	}
	for _, r := range p.ApprovedRoles() {
		// Stable ID for immutable approved role facts. No hidden business role renaming.
		// timestamptz keeps microseconds; hashing finer precision would change the ID after a reload.
		from, to := r.ValidFrom.UTC().Truncate(time.Microsecond), r.ValidUntil.UTC().Truncate(time.Microsecond)
		sig := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", r.Code, r.LegalEntityID, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), r.EvidenceRef)
		dig := sha256.Sum256([]byte(sig))
		roleID := hex.EncodeToString(dig[:])
		var until any
		if !r.ValidUntil.IsZero() {
			until = to
		}
		_, err = q.Exec(ctx, `INSERT INTO cargo_org.party_role
          (tenant_id,party_id,role_id,legal_entity_id,role_code,valid_from,valid_until,approval_state,evidence_ref,approved_by,approved_at)
          VALUES ($1,$2,$3,$4,$5,$6,$7,'APPROVED',$8,$9,$10)
          ON CONFLICT (tenant_id,party_id,role_id) DO NOTHING`,
			string(p.Tenant()), string(p.ID()), roleID, string(r.LegalEntityID), string(r.Code), from, until, r.EvidenceRef, r.ApprovedBy, r.ApprovedAt.UTC())
		if err != nil {
			return 0, err
		}
	}
	return newRevision, nil
}

type LegalEntityEligibility struct{ DB *bentopg.DB }

func (s LegalEntityEligibility) RequireVerifiedLegalEntity(ctx context.Context, t org.TenantID, id org.LegalEntityID) error {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return err
	}
	var status string
	err = q.QueryRow(ctx, `SELECT status FROM cargo_org.legal_entity WHERE tenant_id=$1 AND legal_entity_id=$2 FOR SHARE`, string(t), string(id)).Scan(&status)
	// Explicit failure semantics: wrong-tenant/nonexistent/suspended all deny.
	if errors.Is(err, pgx.ErrNoRows) {
		return org.ErrInvalidTransition
	}
	if err != nil {
		return err
	}
	if status != "VERIFIED" {
		return org.ErrInvalidTransition
	}
	return nil
}

type CommandReceipts struct{ DB *bentopg.DB }

func (s CommandReceipts) Find(ctx context.Context, t org.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	var hash, kind, party string
	var rev int64
	err = q.QueryRow(ctx, `SELECT request_sha256,result_type,result_id,result_revision
       FROM cargo_org.command_receipt WHERE tenant_id=$1 AND command_id=$2`, string(t), id).Scan(&hash, &kind, &party, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	if kind != "BusinessPartyRoleApproved" || rev <= 0 {
		return app.Receipt{}, false, errors.New("org: legacy or invalid receipt requires reconciliation")
	}
	return app.Receipt{CommandID: id, PartyID: org.PartyID(party), NewRevision: rev, RequestHash: hash}, true, nil
}
func (s CommandReceipts) Save(ctx context.Context, t org.TenantID, r app.Receipt) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.command_receipt
     (tenant_id,command_id,request_sha256,result_type,result_id,result_revision)
     VALUES($1,$2,$3,'BusinessPartyRoleApproved',$4,$5)`, string(t), r.CommandID, r.RequestHash, string(r.PartyID), r.NewRevision)
	return err
}
