package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// SettingsLockSQL takes the advisory lock of a tenant's settings ($1 is the tenant) until the transaction ends.
const SettingsLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('cargo_sea.settings/' || $1::text, 0))`

// SettingsLockSharedSQL takes the same lock in shared mode until the transaction ends: the release of a conflict, which
// reads the settings, takes it, so that a settings command (which takes it exclusively) waits for the release and the
// release waits for it. Lock order: a release holds its command id, its visit and its movement before it takes this
// lock; a settings command takes this lock first and no lock of a visit or a movement (it reads the movements it needs
// without a lock), so no cycle.
const SettingsLockSharedSQL = `SELECT pg_advisory_xact_lock_shared(hashtextextended('cargo_sea.settings/' || $1::text, 0))`

// SettingsStore keeps the change sets of SEA's settings: a proposal with its entries, an approval and a cancellation,
// each a row of its own that is only inserted. It is FIN's store (fin/internal/adapters/postgres/settings_store.go)
// over SEA's tables, for items with no parameter.
type SettingsStore struct{ DB *bentopg.DB }

var _ app.SettingsStore = SettingsStore{}

func (s SettingsStore) Lock(ctx context.Context, tenant sea.TenantID) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, SettingsLockSQL, string(tenant))
	return err
}

func (s SettingsStore) LockShared(ctx context.Context, tenant sea.TenantID) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, SettingsLockSharedSQL, string(tenant))
	return err
}

func readSettings(ctx context.Context, q bentopg.Querier, tenant sea.TenantID) (*settings.Ledger, error) {
	rows, err := q.Query(ctx, `SELECT c.change_id,c.reason,c.proposed_by,c.proposed_at,c.effective_at,c.loosening,
        a.approved_by,a.approved_at,a.version_no,a.in_force_at,a.auto,a.risk_acknowledgement, x.cancelled_by,x.cancelled_at,x.reason,x.kind,x.cancelled_role
      FROM cargo_sea.settings_change c
      LEFT JOIN cargo_sea.settings_approval a ON a.tenant_id=c.tenant_id AND a.change_id=c.change_id
      LEFT JOIN cargo_sea.settings_cancellation x ON x.tenant_id=c.tenant_id AND x.change_id=c.change_id
      WHERE c.tenant_id=$1 ORDER BY c.proposed_at, c.change_id`, string(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close() // closed again before the entries are read: a connection runs one query at a time
	var sets []settings.ChangeSet
	for rows.Next() {
		var c settings.ChangeSet
		var approvedBy, acknowledgement, cancelledBy, cancelReason, cancelKind, cancelRole *string
		var approvedAt, inForce, cancelledAt *time.Time
		var version *int
		var auto *bool
		if err = rows.Scan(&c.ID, &c.Reason, &c.By, &c.At, &c.Effective, &c.Loosening, &approvedBy, &approvedAt, &version, &inForce, &auto, &acknowledgement,
			&cancelledBy, &cancelledAt, &cancelReason, &cancelKind, &cancelRole); err != nil {
			return nil, err
		}
		c.At, c.Effective = utc(c.At), utc(c.Effective)
		if approvedBy != nil {
			c.Approval = &settings.Approval{By: *approvedBy, At: utc(*approvedAt), Version: *version, InForce: utc(*inForce), Auto: *auto}
			if acknowledgement != nil {
				c.Approval.Acknowledgement = *acknowledgement
			}
		}
		if cancelledBy != nil {
			c.Cancellation = &settings.Cancellation{By: *cancelledBy, At: utc(*cancelledAt), Reason: *cancelReason, Refused: *cancelKind == "REFUSED", Role: *cancelRole}
		}
		sets = append(sets, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// only the entries of the change sets read above: a change set committed between the two statements (they run
	// at READ COMMITTED, and a reader outside the settings lock may meet one) is a later version that this ledger
	// does not have
	ids := make([]string, len(sets))
	for i, c := range sets {
		ids[i] = c.ID
	}
	entries, err := q.Query(ctx, `SELECT change_id,scope_kind,scope_id,item_key,value_id FROM cargo_sea.settings_change_entry
      WHERE tenant_id=$1 AND change_id = ANY($2) ORDER BY change_id, entry_no`, string(tenant), ids)
	if err != nil {
		return nil, err
	}
	defer entries.Close()
	byID := map[string]int{}
	for i, c := range sets {
		byID[c.ID] = i
	}
	for entries.Next() {
		var id string
		var e settings.Entry
		if err = entries.Scan(&id, &e.Scope.Kind, &e.Scope.ID, &e.Key, &e.Value); err != nil {
			return nil, err
		}
		i, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("settings entry of unknown change set %s", id)
		}
		sets[i].Entries = append(sets[i].Entries, e)
	}
	return settings.NewLedger(sets), entries.Err()
}

func (s SettingsStore) Load(ctx context.Context, tenant sea.TenantID) (*settings.Ledger, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readSettings(ctx, q, tenant)
}

func (s SettingsStore) Read(ctx context.Context, tenant sea.TenantID) (*settings.Ledger, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readSettings(ctx, q, tenant)
}

func (s SettingsStore) InsertChange(ctx context.Context, tenant sea.TenantID, c settings.ChangeSet) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_sea.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
      VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(tenant), c.ID, c.Reason, c.By, c.At, c.Effective, c.Loosening)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: settings change set %s", bentorepo.ErrAlreadyExists, c.ID)
	}
	if err != nil {
		return clockSkew(err)
	}
	for i, e := range c.Entries {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_sea.settings_change_entry (tenant_id,change_id,entry_no,scope_kind,scope_id,item_key,value_id)
          VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(tenant), c.ID, i+1, e.Scope.Kind, e.Scope.ID, e.Key, e.Value); err != nil {
			return err
		}
	}
	if c.Approval != nil {
		return s.InsertApproval(ctx, tenant, c.ID, *c.Approval)
	}
	return nil
}

func (s SettingsStore) InsertApproval(ctx context.Context, tenant sea.TenantID, id string, a settings.Approval) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	var acknowledgement *string
	if a.Acknowledgement != "" {
		acknowledgement = &a.Acknowledgement
	}
	tag, err := q.Exec(ctx, `INSERT INTO cargo_sea.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,
        approved_at,version_no,in_force_at,auto,risk_acknowledgement)
      SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,$3,$4,$5,$6,$7,$8 FROM cargo_sea.settings_change
      WHERE tenant_id=$1 AND change_id=$2`, string(tenant), id, a.By, a.At, a.Version, a.InForce, a.Auto, acknowledgement)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: settings change set %s", bentorepo.ErrNotFound, id)
	}
	return clockSkew(err)
}

// clockSkew answers the database's refusal of a recorded time outside the window around its own clock (the trigger
// settings_time_window) as a refusal that names the cause and its way out; any other error is as it was.
func clockSkew(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "settings_time_window" {
		return settings.Refuse(settings.ErrClockSkew, "the clock of SEA's application server and the clock of the database differ by more than a minute, "+
			"so the time of the change cannot be recorded; synchronise the servers' clocks (for example with NTP), then try the command again")
	}
	return err
}

func (s SettingsStore) InsertCancellation(ctx context.Context, tenant sea.TenantID, id string, c settings.Cancellation) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	kind := "CANCELLED"
	if c.Refused {
		kind = "REFUSED"
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_sea.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind,cancelled_role) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		string(tenant), id, c.By, c.At, c.Reason, kind, c.Role)
	return clockSkew(err)
}

// KnowsLegalEntity says whether the legal entity has an HBL series or a movement in SEA, in the transaction's snapshot.
func (s SettingsStore) KnowsLegalEntity(ctx context.Context, tenant sea.TenantID, legalEntity string) (bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return false, err
	}
	var known bool
	err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cargo_sea.hbl_series WHERE tenant_id=$1 AND legal_entity_id=$2)
      OR EXISTS (SELECT 1 FROM cargo_sea.ocean_movement WHERE tenant_id=$1 AND operating_legal_entity_id=$2)`, string(tenant), legalEntity).Scan(&known)
	return known, err
}

func (s SettingsStore) LegalEntities(ctx context.Context, tenant sea.TenantID) ([]string, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT legal_entity_id FROM cargo_sea.hbl_series WHERE tenant_id=$1
      UNION SELECT operating_legal_entity_id FROM cargo_sea.ocean_movement WHERE tenant_id=$1 ORDER BY 1`, string(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s SettingsStore) OpenMovements(ctx context.Context, tenant sea.TenantID, legalEntity string) ([]sea.MovementID, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT movement_id FROM cargo_sea.ocean_movement WHERE tenant_id=$1 AND operating_legal_entity_id=$2 AND state='OPEN' ORDER BY movement_id`,
		string(tenant), legalEntity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sea.MovementID
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, sea.MovementID(id))
	}
	return out, rows.Err()
}

// Declarations lists the tenant's declarations of a single administrator, in the order recorded. Called in a write
// transaction (RequireExecutor) it is read under the settings lock the command holds; a query reads its snapshot.
func (s SettingsStore) Declarations(ctx context.Context, tenant sea.TenantID) ([]settings.Declaration, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readDeclarations(ctx, q, tenant)
}

func (s SettingsStore) DeclarationsForWrite(ctx context.Context, tenant sea.TenantID) ([]settings.Declaration, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readDeclarations(ctx, q, tenant)
}

func readDeclarations(ctx context.Context, q bentopg.Querier, tenant sea.TenantID) ([]settings.Declaration, error) {
	rows, err := q.Query(ctx, `SELECT onboarding_id,proposed_by,confirmed_by,administrator,declared_at FROM cargo_sea.single_administrator WHERE tenant_id=$1
      ORDER BY declared_at, onboarding_id`, string(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []settings.Declaration
	for rows.Next() {
		var d settings.Declaration
		if err = rows.Scan(&d.OnboardingID, &d.ProposedBy, &d.ConfirmedBy, &d.Administrator, &d.At); err != nil {
			return nil, err
		}
		d.At = utc(d.At)
		out = append(out, d)
	}
	return out, rows.Err()
}

// InsertDeclaration records a declaration of a single administrator.
func (s SettingsStore) InsertDeclaration(ctx context.Context, tenant sea.TenantID, d settings.Declaration) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_sea.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,administrator,declared_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		string(tenant), d.OnboardingID, d.ProposedBy, d.ConfirmedBy, d.Administrator, d.At)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: the declaration of onboarding %s", bentorepo.ErrAlreadyExists, d.OnboardingID)
	}
	return err
}
