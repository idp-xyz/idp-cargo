package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	app "go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// Onboardings keeps the records of customer onboarding (V2.102): rows only inserted.
type Onboardings struct{ DB *bentopg.DB }

var _ app.OnboardingStore = Onboardings{}

func (s Onboardings) Lock(ctx context.Context, tenant org.TenantID, id string) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cargo_org.onboarding/' || $1::text || '/' || $2::text, 0))`, string(tenant), id)
	return err
}

const onboardingColumns = `o.onboarding_id,o.command_id,o.request_hash,o.profile,o.overrides,o.plan,o.plan_sha256,o.risk_statement,o.finance_staff,o.single_administrator,o.proposed_by,
  o.proposed_at,k.confirmed_by,k.confirmed_at,k.acknowledged_text,k.command_id,k.administrator,k.plan_sha256`

func scanOnboarding(row interface{ Scan(...any) error }) (app.OnboardingRecord, error) {
	var r app.OnboardingRecord
	var overrides, plan []byte
	var by, text, command, administrator, digest *string
	var at *time.Time
	if err := row.Scan(&r.Onboarding.ID, &r.CommandID, &r.Hash, &r.Onboarding.Profile, &overrides, &plan, &r.Onboarding.PlanSHA256, &r.Onboarding.RiskStatement, &r.Onboarding.FinanceStaff,
		&r.Onboarding.SingleAdministrator, &r.Onboarding.ProposedBy, &r.Onboarding.ProposedAt, &by, &at, &text, &command, &administrator, &digest); err != nil {
		return r, err
	}
	r.Onboarding.ProposedAt = r.Onboarding.ProposedAt.UTC()
	if err := json.Unmarshal(overrides, &r.Onboarding.Overrides); err != nil {
		return r, err
	}
	if err := json.Unmarshal(plan, &r.Onboarding.Plan); err != nil {
		return r, err
	}
	if by != nil {
		r.Confirmation = &org.Confirmation{By: *by, At: at.UTC(), AcknowledgedText: *text, PlanSHA256: *digest}
		if administrator != nil {
			r.Confirmation.Administrator = *administrator
		}
		r.ConfirmCommandID = *command
	}
	return r, nil
}

func (s Onboardings) modules(ctx context.Context, q bentopg.Querier, tenant org.TenantID, id string) ([]org.ModuleState, error) {
	rows, err := q.Query(ctx, `SELECT module,state,change_id FROM cargo_org.onboarding_module WHERE tenant_id=$1 AND onboarding_id=$2 ORDER BY recorded_at, module, state`,
		string(tenant), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []org.ModuleState
	for rows.Next() {
		var m org.ModuleState
		if err = rows.Scan(&m.Module, &m.State, &m.ChangeID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s Onboardings) Find(ctx context.Context, tenant org.TenantID, id string) (app.OnboardingRecord, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.OnboardingRecord{}, false, err
	}
	r, err := scanOnboarding(q.QueryRow(ctx, `SELECT `+onboardingColumns+` FROM cargo_org.onboarding o
      LEFT JOIN cargo_org.onboarding_confirmation k ON k.tenant_id=o.tenant_id AND k.onboarding_id=o.onboarding_id
      WHERE o.tenant_id=$1 AND o.onboarding_id=$2`, string(tenant), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.OnboardingRecord{}, false, nil
	}
	if err != nil {
		return app.OnboardingRecord{}, false, err
	}
	r.Modules, err = s.modules(ctx, q, tenant, id)
	return r, err == nil, err
}

func (s Onboardings) List(ctx context.Context, tenant org.TenantID) ([]app.OnboardingRecord, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+onboardingColumns+` FROM cargo_org.onboarding o
      LEFT JOIN cargo_org.onboarding_confirmation k ON k.tenant_id=o.tenant_id AND k.onboarding_id=o.onboarding_id
      WHERE o.tenant_id=$1 ORDER BY o.proposed_at, o.onboarding_id`, string(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.OnboardingRecord
	for rows.Next() {
		r, err := scanOnboarding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close() // a connection runs one query at a time
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Modules, err = s.modules(ctx, q, tenant, out[i].Onboarding.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s Onboardings) Insert(ctx context.Context, tenant org.TenantID, o org.Onboarding, commandID, hash string) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	overrides := o.Overrides
	if overrides == nil {
		overrides = map[string][]org.OnboardingEntry{}
	}
	raw, err := json.Marshal(overrides)
	if err != nil {
		return err
	}
	plan := o.Plan
	if plan == nil {
		plan = map[string]org.ModulePlan{}
	}
	rawPlan, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.onboarding (tenant_id,onboarding_id,command_id,request_hash,profile,overrides,plan,plan_sha256,risk_statement,finance_staff,
        single_administrator,proposed_by,proposed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, string(tenant), o.ID, commandID, hash, o.Profile, raw, rawPlan,
		o.PlanSHA256, o.RiskStatement, o.FinanceStaff, o.SingleAdministrator, o.ProposedBy, o.ProposedAt)
	return err
}

func (s Onboardings) InsertConfirmation(ctx context.Context, tenant org.TenantID, id string, o org.Onboarding, commandID string, c org.Confirmation) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	var administrator *string
	if c.Administrator != "" {
		administrator = &c.Administrator
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.onboarding_confirmation (tenant_id,onboarding_id,command_id,proposed_by,proposed_at,confirmed_by,confirmed_at,
        acknowledged_text,single_administrator,administrator,plan_sha256) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, string(tenant), id, commandID, o.ProposedBy, o.ProposedAt,
		c.By, c.At, c.AcknowledgedText, o.SingleAdministrator, administrator, c.PlanSHA256)
	return err
}

func (s Onboardings) InsertModule(ctx context.Context, tenant org.TenantID, id string, k org.Confirmation, m org.ModuleState, at time.Time) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.onboarding_module (tenant_id,onboarding_id,module,state,change_id,recorded_by,confirmed_at,recorded_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, string(tenant), id, m.Module, m.State, m.ChangeID, k.By, k.At, at)
	return err
}
