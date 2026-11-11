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
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

type PartyStore struct{ DB *bentopg.DB }

func (s PartyStore) Load(ctx context.Context, tenant org.TenantID, party org.PartyID) (*org.BusinessParty, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var name, country, status, english string
	var rev int64
	// Under READ COMMITTED each statement takes a new snapshot; locking the root row keeps the role
	// and identifier queries below consistent with this revision. Concurrent commands on one party serialize here.
	err = q.QueryRow(ctx, `SELECT legal_name,registration_country,status,revision,coalesce(english_name,'') FROM cargo_org.business_party
        WHERE tenant_id=$1 AND party_id=$2 FOR NO KEY UPDATE`, string(tenant), string(party)).Scan(&name, &country, &status, &rev, &english)
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
	if err != nil {
		return nil, 0, err
	}
	identifiers, err := activeIdentifiers(ctx, q, tenant, party)
	if err != nil {
		return nil, 0, err
	}
	return aggregate, rev, aggregate.RestoreIdentity(english, identifiers)
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
      SET legal_name=$3,status=$4,english_name=$6,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND party_id=$2 AND revision=$5
      RETURNING revision`, string(p.Tenant()), string(p.ID()), p.Name(), string(p.Status()), expected, nullable(p.EnglishName())).Scan(&newRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, bentorepo.ErrConflict
	}
	if err != nil {
		return 0, err
	}
	if err = insertApprovedRoles(ctx, q, p); err != nil {
		return 0, err
	}
	if err = saveIdentifiers(ctx, q, p, newRevision); err != nil {
		return 0, err
	}
	return newRevision, nil
}

// Insert persists a new party at revision 1 together with any approved roles it already carries.
func (s PartyStore) Insert(ctx context.Context, p *org.BusinessParty) (int64, error) {
	if p == nil {
		return 0, org.ErrInvalidIdentity
	}
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_org.business_party (tenant_id,party_id,legal_name,registration_country,status,english_name)
      VALUES ($1,$2,$3,$4,$5,$6) RETURNING revision`,
		string(p.Tenant()), string(p.ID()), p.Name(), p.Country(), string(p.Status()), nullable(p.EnglishName())).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "business_party_pkey" {
		return 0, bentorepo.ErrAlreadyExists
	}
	if err != nil {
		return 0, err
	}
	if err = insertApprovedRoles(ctx, q, p); err != nil {
		return 0, err
	}
	if err = saveIdentifiers(ctx, q, p, revision); err != nil {
		return 0, err
	}
	return revision, nil
}

func insertApprovedRoles(ctx context.Context, q bentopg.Executor, p *org.BusinessParty) error {
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
		_, err := q.Exec(ctx, `INSERT INTO cargo_org.party_role
          (tenant_id,party_id,role_id,legal_entity_id,role_code,valid_from,valid_until,approval_state,evidence_ref,approved_by,approved_at)
          VALUES ($1,$2,$3,$4,$5,$6,$7,'APPROVED',$8,$9,$10)
          ON CONFLICT (tenant_id,party_id,role_id) DO NOTHING`,
			string(p.Tenant()), string(p.ID()), roleID, string(r.LegalEntityID), string(r.Code), from, until, r.EvidenceRef, r.ApprovedBy, r.ApprovedAt.UTC())
		if err != nil {
			return err
		}
	}
	return nil
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
	var entry, state *string
	var entryVersion, persons *int64
	err = q.QueryRow(ctx, `SELECT request_sha256,result_type,result_id,result_revision,result_entry_id,result_entry_version,result_persons,result_state
       FROM cargo_org.command_receipt WHERE tenant_id=$1 AND command_id=$2`, string(t), id).Scan(&hash, &kind, &party, &rev, &entry, &entryVersion, &persons,
		&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	if (kind != app.ResultRoleApproval && kind != app.ResultAddressBook && kind != app.ResultPartyIdentity && kind != app.ResultTransferRegister) || rev <= 0 {
		return app.Receipt{}, false, errors.New("org: legacy or invalid receipt requires reconciliation")
	}
	r := app.Receipt{CommandID: id, ResultType: kind, PartyID: org.PartyID(party), NewRevision: rev, RequestHash: hash}
	if entry != nil && entryVersion != nil {
		r.EntryID, r.EntryVersion = *entry, *entryVersion
	}
	if persons != nil && state != nil {
		r.Persons, r.State = *persons, *state
	}
	return r, true, nil
}
func (s CommandReceipts) Save(ctx context.Context, t org.TenantID, r app.Receipt) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	kind := r.ResultType
	if kind == "" {
		kind = app.ResultRoleApproval
	}
	var entry, entryVersion, persons, state any
	if r.EntryID != "" {
		entry, entryVersion = r.EntryID, r.EntryVersion
	}
	if r.State != "" {
		persons, state = r.Persons, r.State
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.command_receipt
     (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,result_entry_id,result_entry_version,result_persons,result_state)
     VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, string(t), r.CommandID, r.RequestHash, kind, string(r.PartyID), r.NewRevision, entry, entryVersion, persons,
		state)
	return err
}
