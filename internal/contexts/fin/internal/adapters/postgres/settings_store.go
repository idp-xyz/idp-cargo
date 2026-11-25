package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// SettingsLockSQL takes the advisory lock of a tenant's settings ($1 is the tenant) until the transaction ends.
const SettingsLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('cargo_fin.settings/' || $1::text, 0))`

// SettingsLockSharedSQL takes the same lock in shared mode until the transaction ends: the FIN commands that read the
// settings for a rule take it, so that a settings command (which takes it exclusively) waits for them and they wait for
// it. Lock order: a pool command holds its pool, its houses' jobs and its policy before it takes this lock; a settings
// command takes this lock first and no lock of a pool, a job or a policy (it reads the policy without a lock), so no
// cycle.
const SettingsLockSharedSQL = `SELECT pg_advisory_xact_lock_shared(hashtextextended('cargo_fin.settings/' || $1::text, 0))`

// SettingsStore keeps the change sets of FIN's settings: a proposal with its entries, an approval and a cancellation,
// each a row of its own that is only inserted.
type SettingsStore struct{ DB *bentopg.DB }

var _ app.SettingsStore = SettingsStore{}

func (s SettingsStore) Lock(ctx context.Context, tenant fin.TenantID) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, SettingsLockSQL, string(tenant))
	return err
}

func (s SettingsStore) LockShared(ctx context.Context, tenant fin.TenantID) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, SettingsLockSharedSQL, string(tenant))
	return err
}

func readSettings(ctx context.Context, q bentopg.Querier, tenant fin.TenantID) (*settings.Ledger, error) {
	rows, err := q.Query(ctx, `SELECT c.change_id,c.reason,c.proposed_by,c.proposed_at,c.effective_at,c.loosening,
        a.approved_by,a.approved_at,a.version_no,a.in_force_at,a.auto, x.cancelled_by,x.cancelled_at,x.reason,x.kind,x.cancelled_role
      FROM cargo_fin.settings_change c
      LEFT JOIN cargo_fin.settings_approval a ON a.tenant_id=c.tenant_id AND a.change_id=c.change_id
      LEFT JOIN cargo_fin.settings_cancellation x ON x.tenant_id=c.tenant_id AND x.change_id=c.change_id
      WHERE c.tenant_id=$1 ORDER BY c.proposed_at, c.change_id`, string(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close() // closed again before the entries are read: a connection runs one query at a time
	var sets []settings.ChangeSet
	for rows.Next() {
		var c settings.ChangeSet
		var approvedBy, cancelledBy, cancelReason, cancelKind, cancelRole *string
		var approvedAt, inForce, cancelledAt *time.Time
		var version *int
		var auto *bool
		if err = rows.Scan(&c.ID, &c.Reason, &c.By, &c.At, &c.Effective, &c.Loosening, &approvedBy, &approvedAt, &version, &inForce, &auto,
			&cancelledBy, &cancelledAt, &cancelReason, &cancelKind, &cancelRole); err != nil {
			return nil, err
		}
		c.At, c.Effective = utc(c.At), utc(c.Effective)
		if approvedBy != nil {
			c.Approval = &settings.Approval{By: *approvedBy, At: utc(*approvedAt), Version: *version, InForce: utc(*inForce), Auto: *auto}
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
	// only the entries of the change sets read above: a change set committed between the two statements (they run at
	// READ COMMITTED, and a reader outside the settings lock may meet one) is a later version that this ledger does not
	// have
	ids := make([]string, len(sets))
	for i, c := range sets {
		ids[i] = c.ID
	}
	entries, err := q.Query(ctx, `SELECT change_id,scope_kind,scope_id,item_key,value_id,param_minor FROM cargo_fin.settings_change_entry
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
		if err = entries.Scan(&id, &e.Scope.Kind, &e.Scope.ID, &e.Key, &e.Value, &e.Param); err != nil {
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

func (s SettingsStore) Load(ctx context.Context, tenant fin.TenantID) (*settings.Ledger, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readSettings(ctx, q, tenant)
}

func (s SettingsStore) Read(ctx context.Context, tenant fin.TenantID) (*settings.Ledger, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readSettings(ctx, q, tenant)
}

func (s SettingsStore) InsertChange(ctx context.Context, tenant fin.TenantID, c settings.ChangeSet) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
      VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(tenant), c.ID, c.Reason, c.By, c.At, c.Effective, c.Loosening)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: settings change set %s", bentorepo.ErrAlreadyExists, c.ID)
	}
	if err != nil {
		return clockSkew(err)
	}
	for i, e := range c.Entries {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_fin.settings_change_entry (tenant_id,change_id,entry_no,scope_kind,scope_id,item_key,value_id,param_minor)
          VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, string(tenant), c.ID, i+1, e.Scope.Kind, e.Scope.ID, e.Key, e.Value, e.Param); err != nil {
			return err
		}
	}
	if c.Approval != nil {
		return s.InsertApproval(ctx, tenant, c.ID, *c.Approval)
	}
	return nil
}

func (s SettingsStore) InsertApproval(ctx context.Context, tenant fin.TenantID, id string, a settings.Approval) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `INSERT INTO cargo_fin.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,
        approved_at,version_no,in_force_at,auto)
      SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,$3,$4,$5,$6,$7 FROM cargo_fin.settings_change
      WHERE tenant_id=$1 AND change_id=$2`, string(tenant), id, a.By, a.At, a.Version, a.InForce, a.Auto)
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
		return settings.Refuse(settings.ErrClockSkew, "the clock of FIN's application server and the clock of the database differ by more than a minute, "+
			"so the time of the change cannot be recorded; synchronise the servers' clocks (for example with NTP), then try the command again")
	}
	return err
}

func (s SettingsStore) InsertCancellation(ctx context.Context, tenant fin.TenantID, id string, c settings.Cancellation) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	kind := "CANCELLED"
	if c.Refused {
		kind = "REFUSED"
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason,kind,cancelled_role) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		string(tenant), id, c.By, c.At, c.Reason, kind, c.Role)
	return clockSkew(err)
}
