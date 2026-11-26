//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// V2.102 batch E, migration 000022: a tenant has one single administrator in force, the one its latest declaration names. A later
// onboarding that declares another replaces the earlier one from its declaration's time; the earlier may no longer approve their
// own loosening. A tenant's declarations are in a total order because no two have the same time.

// By the command: dan is declared, then frank by a later onboarding; dan may no longer approve their own loosening and frank may; the
// declaration that is not later than the tenant's latest is refused with the way out and writes nothing.
func TestFinTheLatestDeclarationNamesTheOnlyAdministrator(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.declare("decl-1", "ob-1", "consultant-1", "owner-1", "dan"))
	s.must(s.approveWith(s.command(), "L1", "dan", "I accept it"))
	s.must(s.propose("T2", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L2", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.propose("L3", "frank", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.declare("decl-2", "ob-2", "consultant-1", "owner-1", "frank"))
	_, err := s.approveWith(s.command(), "L2", "dan", "I accept it")
	s.refused("the replaced administrator", err, api.ErrRejected, "SETTINGS_SAME_ACTOR", "not the single administrator this tenant declared (frank)")
	s.must(s.approveWith(s.command(), "L3", "frank", "I accept it"))
	// an onboarding that makes no declaration changes nothing: frank stays, dan stays replaced
	s.must(s.propose("T3", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L4", "frank", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.approveWith(s.command(), "L4", "frank", "I accept it"))
	if c := s.changes()["L2"]; c.Status != api.SettingsProposed {
		t.Fatalf("the replaced administrator's change set: %+v", c)
	}
	now := time.Now().UTC()
	rep, err := s.module.GetFinExceptionReport(context.Background(), api.GetFinExceptionReport{TenantID: s.tenant, ActorID: "bob", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if err != nil || len(rep.Declarations) != 2 || rep.Declarations[0].AdministratorID != "dan" || rep.Declarations[1].AdministratorID != "frank" || !rep.Declarations[1].DeclaredAt.After(rep.Declarations[0].DeclaredAt) {
		t.Fatalf("the declarations, oldest first: %+v, %v", rep.Declarations, err)
	}
}

// Two declarations of a tenant never have one time: the command refuses a declaration that is not later than the tenant's latest,
// saying to send the same command again, and writes nothing; the database refuses two at one instant by name.
func TestFinTwoDeclarationsOfATenantAreNeverAtOneInstant(t *testing.T) {
	s := newSettingsWorld(t)
	at := time.Now().UTC().Truncate(time.Microsecond)
	stopped := &settingsWorld{fixture: s.fixture, t: t, module: settingsModule(t, s.fixture, finmodule.WithClock(fixedClock{at}))}
	stopped.must(stopped.declare("decl-1", "ob-1", "consultant-1", "owner-1", "dan"))
	_, err := stopped.declare("decl-2", "ob-2", "consultant-1", "owner-1", "frank")
	s.refused("a declaration at the instant of another", err, api.ErrRejected, "the declarations of a tenant are in order of time", "send the same command again", "ob-1")
	earlier := &settingsWorld{fixture: s.fixture, t: t, module: settingsModule(t, s.fixture, finmodule.WithClock(fixedClock{at.Add(-time.Second)}))}
	_, err = earlier.declare("decl-3", "ob-3", "consultant-1", "owner-1", "frank")
	s.refused("a declaration before the latest", err, api.ErrRejected, "the declarations of a tenant are in order of time")
	if n := s.count("single_administrator"); n != 1 {
		t.Fatalf("%d declarations, want 1", n)
	}
	err = s.rawTx(t, fmt.Sprintf(`INSERT INTO cargo_fin.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,administrator,declared_at)
      SELECT tenant_id,'ob-raw','consultant-1','owner-1','frank',declared_at FROM cargo_fin.single_administrator WHERE tenant_id=$1`))
	if state, name := sqlState(err); state != "23505" || name != "single_administrator_time_once" {
		t.Fatalf("two declarations at one instant: %v (state %s, constraint %s)", err, state, name)
	}
	later := &settingsWorld{fixture: s.fixture, t: t, module: settingsModule(t, s.fixture, finmodule.WithClock(fixedClock{at.Add(time.Microsecond)}))}
	later.must(later.declare("decl-4", "ob-4", "consultant-1", "owner-1", "frank"))
}

// The approval's trigger reads the latest declaration at or before the approval: a declaration dated after the approval does not
// replace the earlier administrator, and one dated before it does. Written past FIN's commands.
func TestFinTheApprovalTriggerReadsTheLatestDeclarationAtOrBeforeTheApproval(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	self := func(id string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
          version_no,in_force_at,auto,risk_acknowledgement,basis_onboarding_id)
        SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,proposed_by,now(),(SELECT coalesce(max(version_no),0)+1 FROM cargo_fin.settings_approval
          WHERE tenant_id=$1),greatest(effective_at,now()),false,'I accept',(SELECT onboarding_id FROM cargo_fin.single_administrator WHERE tenant_id=$1 AND declared_at <= now() ORDER BY declared_at DESC LIMIT 1) FROM cargo_fin.settings_change WHERE tenant_id=$1 AND change_id='%s'`, id)
	}
	declaration := func(onboarding, administrator, at string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,administrator,declared_at) VALUES ($1,'%s','consultant-1','owner-1','%s',%s)`,
			onboarding, administrator, at)
	}
	s.must(s.declare("decl-1", "ob-1", "consultant-1", "owner-1", "dan"))
	// a later declaration (an hour on) is not in force at the approval: dan, the latest at or before it, approves
	if err := s.rawTx(t, declaration("ob-future", "frank", "now() + interval '1 hour'"), self("L1")); err != nil {
		t.Fatalf("the earlier administrator, with a later declaration dated after the approval: %v", err)
	}
	s.must(s.propose("T2", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L2", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.propose("L3", "frank", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	// a declaration dated just after dan's and before the approval replaces dan from its time
	replace := declaration("ob-frank", "frank", "(SELECT declared_at FROM cargo_fin.single_administrator WHERE tenant_id=$1 AND onboarding_id='ob-1') + interval '1 millisecond'")
	err := s.rawTx(t, replace, self("L2"))
	if state, name := sqlState(err); state != "23514" || name != "settings_approval_single_administrator" || !strings.Contains(err.Error(), "in force is not dan") {
		t.Fatalf("the replaced administrator's approval: %v (state %s, constraint %s)", err, state, name)
	}
	if err = s.rawTx(t, replace, self("L3")); err != nil {
		t.Fatalf("the replacing administrator's approval: %v", err)
	}
	if n := s.count("single_administrator"); n != 3 {
		t.Fatalf("%d declarations, want 3 (dan's, the later one's, frank's)", n)
	}
}
