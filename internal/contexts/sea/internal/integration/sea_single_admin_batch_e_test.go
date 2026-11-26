//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// V2.102 batch E, migration 000013: a tenant has one single administrator in force, the one its latest declaration names. A later
// onboarding that declares another replaces the earlier one from its declaration's time; the earlier may no longer approve their
// own loosening. A tenant's declarations are in a total order because no two have the same time.

func TestSeaTheLatestDeclarationNamesTheOnlyAdministrator(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("WITH_REASON")))
	s.must(s.declare("decl-1", "ob-1", "consultant-1", "owner-1", "dan"))
	s.must(s.approveWith(s.command(), "L1", "dan", "I accept it"))
	s.must(s.propose("T2", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("L2", "dan", time.Time{}, tenantEntry("WITH_REASON")))
	s.must(s.propose("L3", "frank", time.Time{}, tenantEntry("WITH_REASON")))
	s.must(s.declare("decl-2", "ob-2", "consultant-1", "owner-1", "frank"))
	r, err := s.approveWith(s.command(), "L2", "dan", "I accept it")
	s.refusedWith("the replaced administrator", r, err, api.ErrRejected, "SETTINGS_SAME_ACTOR", "not the single administrator this tenant declared (frank)")
	s.must(s.approveWith(s.command(), "L3", "frank", "I accept it"))
	// an onboarding that makes no declaration changes nothing: frank stays, dan stays replaced
	s.must(s.propose("T3", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("L4", "frank", time.Time{}, tenantEntry("WITH_REASON")))
	s.must(s.approveWith(s.command(), "L4", "frank", "I accept it"))
	if c := s.change("L2"); c.Status != api.SettingsProposed {
		t.Fatalf("the replaced administrator's change set: %+v", c)
	}
	now := time.Now().UTC()
	rep, err := s.module.GetSeaExceptionReport(context.Background(), api.GetSeaExceptionReport{TenantID: s.tenant, ActorID: "bob", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if err != nil || len(rep.Declarations) != 2 || rep.Declarations[0].AdministratorID != "dan" || rep.Declarations[1].AdministratorID != "frank" || !rep.Declarations[1].DeclaredAt.After(rep.Declarations[0].DeclaredAt) {
		t.Fatalf("the declarations, oldest first: %+v, %v", rep.Declarations, err)
	}
}

// A declaration that is not later than the tenant's latest is refused by the command (here a latest dated an hour on, written past
// the command), saying to send the same command again, and writes nothing; the database refuses two at one instant by name.
func TestSeaTwoDeclarationsOfATenantAreNeverAtOneInstant(t *testing.T) {
	s := newSettingsWorld(t)
	declaration := func(onboarding, at string) string {
		return fmt.Sprintf(`INSERT INTO cargo_sea.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,administrator,declared_at) VALUES ($1,'%s','consultant-1','owner-1','dan',%s)`,
			onboarding, at)
	}
	if err := s.rawTx(t, declaration("ob-future", "now() + interval '1 hour'")); err != nil {
		t.Fatal(err)
	}
	r, err := s.declare("decl-2", "ob-2", "consultant-1", "owner-1", "frank")
	s.refusedWith("a declaration before the latest", r, err, api.ErrRejected, "the declarations of a tenant are in order of time", "send the same command again", "ob-future")
	if n := s.count("single_administrator"); n != 1 {
		t.Fatalf("%d declarations, want 1", n)
	}
	err = s.rawTx(t, `INSERT INTO cargo_sea.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,administrator,declared_at)
      SELECT tenant_id,'ob-raw','consultant-1','owner-1','frank',declared_at FROM cargo_sea.single_administrator WHERE tenant_id=$1`)
	if sqlState(err) != "23505" || constraintOf(err) != "single_administrator_time_once" {
		t.Fatalf("two declarations at one instant: %v (state %s, constraint %s)", err, sqlState(err), constraintOf(err))
	}
}

// The approval's trigger reads the latest declaration at or before the approval: a declaration dated after the approval does not
// replace the earlier administrator, and one dated before it does. Written past SEA's commands.
func TestSeaTheApprovalTriggerReadsTheLatestDeclarationAtOrBeforeTheApproval(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("WITH_REASON")))
	self := func(id string) string {
		return fmt.Sprintf(`INSERT INTO cargo_sea.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
          version_no,in_force_at,auto,risk_acknowledgement)
        SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,proposed_by,now(),%s,greatest(effective_at,now()),false,'I accept' FROM cargo_sea.settings_change
        WHERE tenant_id=$1 AND change_id='%s'`, nextVersion, id)
	}
	declaration := func(onboarding, administrator, at string) string {
		return fmt.Sprintf(`INSERT INTO cargo_sea.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,administrator,declared_at) VALUES ($1,'%s','consultant-1','owner-1','%s',%s)`,
			onboarding, administrator, at)
	}
	s.must(s.declare("decl-1", "ob-1", "consultant-1", "owner-1", "dan"))
	if err := s.rawTx(t, declaration("ob-future", "frank", "now() + interval '1 hour'"), self("L1")); err != nil {
		t.Fatalf("the earlier administrator, with a later declaration dated after the approval: %v", err)
	}
	s.must(s.propose("T2", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("L2", "dan", time.Time{}, tenantEntry("WITH_REASON")))
	s.must(s.propose("L3", "frank", time.Time{}, tenantEntry("WITH_REASON")))
	replace := declaration("ob-frank", "frank", "(SELECT declared_at FROM cargo_sea.single_administrator WHERE tenant_id=$1 AND onboarding_id='ob-1') + interval '1 millisecond'")
	err := s.rawTx(t, replace, self("L2"))
	if sqlState(err) != "23514" || constraintOf(err) != "settings_approval_single_administrator" || !strings.Contains(err.Error(), "in force is not dan") {
		t.Fatalf("the replaced administrator's approval: %v (state %s, constraint %s)", err, sqlState(err), constraintOf(err))
	}
	if err = s.rawTx(t, replace, self("L3")); err != nil {
		t.Fatalf("the replacing administrator's approval: %v", err)
	}
	if n := s.count("single_administrator"); n != 3 {
		t.Fatalf("%d declarations, want 3", n)
	}
}
