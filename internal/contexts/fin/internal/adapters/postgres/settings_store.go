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
        a.approved_by,a.approved_at,a.version_no,a.in_force_at,a.auto,a.risk_acknowledgement,a.basis_onboarding_id, x.cancelled_by,x.cancelled_at,x.reason,x.kind,x.cancelled_role,
        h.onboarding_id
      FROM cargo_fin.settings_change c
      LEFT JOIN cargo_fin.settings_hold h ON h.tenant_id=c.tenant_id AND h.change_id=c.change_id
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
		var approvedBy, acknowledgement, basis, cancelledBy, cancelReason, cancelKind, cancelRole, hold *string
		var approvedAt, inForce, cancelledAt *time.Time
		var version *int
		var auto *bool
		if err = rows.Scan(&c.ID, &c.Reason, &c.By, &c.At, &c.Effective, &c.Loosening, &approvedBy, &approvedAt, &version, &inForce, &auto, &acknowledgement, &basis,
			&cancelledBy, &cancelledAt, &cancelReason, &cancelKind, &cancelRole, &hold); err != nil {
			return nil, err
		}
		if hold != nil {
			c.Hold = *hold
		}
		c.At, c.Effective = utc(c.At), utc(c.Effective)
		if approvedBy != nil {
			c.Approval = &settings.Approval{By: *approvedBy, At: utc(*approvedAt), Version: *version, InForce: utc(*inForce), Auto: *auto}
			if acknowledgement != nil {
				c.Approval.Acknowledgement = *acknowledgement
			}
			if basis != nil {
				c.Approval.Basis = *basis
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
	if c.Hold != "" {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_fin.settings_hold (tenant_id,change_id,onboarding_id) VALUES ($1,$2,$3)`, string(tenant), c.ID, c.Hold); err != nil {
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
	var acknowledgement, basis *string
	if a.Acknowledgement != "" {
		acknowledgement, basis = &a.Acknowledgement, &a.Basis
	}
	tag, err := q.Exec(ctx, `INSERT INTO cargo_fin.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,
        approved_at,version_no,in_force_at,auto,risk_acknowledgement,basis_onboarding_id)
      SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,$3,$4,$5,$6,$7,$8,$9 FROM cargo_fin.settings_change
      WHERE tenant_id=$1 AND change_id=$2`, string(tenant), id, a.By, a.At, a.Version, a.InForce, a.Auto, acknowledgement, basis)
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

// Declarations lists the tenant's declarations of a single administrator, in the order recorded, in a query's snapshot.
func (s SettingsStore) Declarations(ctx context.Context, tenant fin.TenantID) ([]settings.Declaration, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readDeclarations(ctx, q, tenant)
}

// DeclarationsForWrite lists them in a write transaction, under the settings lock the command holds.
func (s SettingsStore) DeclarationsForWrite(ctx context.Context, tenant fin.TenantID) ([]settings.Declaration, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readDeclarations(ctx, q, tenant)
}

func readDeclarations(ctx context.Context, q bentopg.Querier, tenant fin.TenantID) ([]settings.Declaration, error) {
	rows, err := q.Query(ctx, `SELECT onboarding_id,proposed_by,confirmed_by,administrator,declared_at FROM cargo_fin.single_administrator WHERE tenant_id=$1
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
func (s SettingsStore) InsertDeclaration(ctx context.Context, tenant fin.TenantID, d settings.Declaration) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_fin.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,administrator,declared_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		string(tenant), d.OnboardingID, d.ProposedBy, d.ConfirmedBy, d.Administrator, d.At)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "single_administrator_time_once" { // two declarations of a tenant at one instant: the later retries
			return fmt.Errorf("%w: a declaration of the tenant is at the same instant as this one's; the declarations of a tenant are in order of time, so send the same command again", bentorepo.ErrConflict)
		}
		return fmt.Errorf("%w: the declaration of onboarding %s", bentorepo.ErrAlreadyExists, d.OnboardingID)
	}
	return err
}

// WorkActors lists, for the entry times from (included) to to (excluded), each actor who submitted an apportionment of a
// consolidation cost pool and how many, and how many of those state an attribution of their own (not one only carried
// forward from an earlier apportionment): the person who confirmed it.
func (s SettingsStore) WorkActors(ctx context.Context, tenant fin.TenantID, from, to time.Time) ([]app.ActorWork, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT a.made_by, count(*),
        count(*) FILTER (WHERE EXISTS (SELECT 1 FROM cargo_fin.pool_apportionment_attribution t WHERE t.tenant_id=a.tenant_id AND t.pool_id=a.pool_id
          AND t.apportionment_no=a.apportionment_no AND NOT t.carried))
      FROM cargo_fin.pool_apportionment a WHERE a.tenant_id=$1 AND a.entered_at >= $2 AND a.entered_at < $3 GROUP BY a.made_by ORDER BY a.made_by`,
		string(tenant), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.ActorWork
	for rows.Next() {
		var w app.ActorWork
		if err = rows.Scan(&w.Actor, &w.Submitted, &w.Confirmed); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Reviews lists the reviews of self-approved change sets, in the order of their times, in a query's snapshot.
func (s SettingsStore) Reviews(ctx context.Context, tenant fin.TenantID) ([]settings.SelfReview, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readReviews(ctx, q, tenant)
}

// ReviewsForWrite lists them in a write transaction, under the settings lock the command holds.
func (s SettingsStore) ReviewsForWrite(ctx context.Context, tenant fin.TenantID) ([]settings.SelfReview, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, err
	}
	return readReviews(ctx, q, tenant)
}

func readReviews(ctx context.Context, q bentopg.Querier, tenant fin.TenantID) ([]settings.SelfReview, error) {
	rows, err := q.Query(ctx, `SELECT change_id,reviewed_by,reviewed_at,conclusion,coalesce(note,'') FROM cargo_fin.settings_self_approval_review WHERE tenant_id=$1
      ORDER BY reviewed_at, change_id`, string(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []settings.SelfReview
	for rows.Next() {
		var r settings.SelfReview
		if err = rows.Scan(&r.ChangeID, &r.By, &r.At, &r.Conclusion, &r.Note); err != nil {
			return nil, err
		}
		r.At = utc(r.At)
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertReview records the review of a self-approved change set.
func (s SettingsStore) InsertReview(ctx context.Context, tenant fin.TenantID, r settings.SelfReview, approvedBy string) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	var note *string
	if r.Note != "" {
		note = &r.Note
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_fin.settings_self_approval_review (tenant_id,change_id,approved_by,reviewed_by,reviewed_at,conclusion,note) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		string(tenant), r.ChangeID, approvedBy, r.By, r.At, r.Conclusion, note)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: the review of change set %s", bentorepo.ErrAlreadyExists, r.ChangeID)
	}
	return err
}
