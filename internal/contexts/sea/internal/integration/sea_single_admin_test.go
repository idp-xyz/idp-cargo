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

// V2.102 on PostgreSQL, migration 000013: a single administrator approves their own loosening, with a written risk
// acknowledgement, in a tenant that declared one.

func (s *settingsWorld) declare(command, onboarding, proposer, confirmer, administrator string) (api.Receipt, error) {
	return s.module.DeclareSeaSingleAdministrator(context.Background(), api.DeclareSeaSingleAdministrator{CommandID: command, TenantID: s.tenant, ActorID: confirmer,
		OnboardingID: onboarding, ProposedBy: proposer, AdministratorID: administrator})
}

func (s *settingsWorld) approveWith(command, id, actor, acknowledgement string) (api.Receipt, error) {
	return s.module.ApproveSeaSettingsChange(context.Background(), api.ApproveSeaSettingsChange{CommandID: command, TenantID: s.tenant, ActorID: actor, ChangeID: id,
		RiskAcknowledgement: acknowledgement})
}

func (s *settingsWorld) change(id string) api.SeaSettingsChange {
	s.t.Helper()
	list, err := s.module.ListSeaSettingsChanges(context.Background(), api.ListSeaSettingsChanges{TenantID: s.tenant})
	if err != nil {
		s.t.Fatal(err)
	}
	for _, c := range list {
		if c.ChangeID == id {
			return c
		}
	}
	s.t.Fatalf("no change set %s", id)
	return api.SeaSettingsChange{}
}

// The proposer approves their own loosening only after the tenant's declaration, only with an acknowledgement, and
// another approver gives none; the acknowledgement is stored with the approval, listed with the change set and in the
// exception report, and a command id is bound to its acknowledgement.
func TestSeaASingleAdministratorApprovesTheirOwnLoosening(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("WITH_REASON")))
	r, err := s.approveWith(s.command(), "L1", "dan", "")
	s.refusedWith("without the declaration", r, err, api.ErrRejected, "SETTINGS_SAME_ACTOR", "have another approver approve it")
	r, err = s.approveWith(s.command(), "L1", "dan", "I accept it")
	s.refusedWith("an acknowledgement without the declaration", r, err, api.ErrRejected, "SETTINGS_SAME_ACTOR", "has not declared a single administrator")

	// the declaration: two different people, idempotent by command id, by the owner who confirmed
	r, err = s.declare("decl-1", "ob-1", "dan", "dan", "dan")
	s.refusedWith("one person for both", r, err, api.ErrRejected, "two different people")
	r, err = s.declare("decl-1b", "ob-1", "consultant-1", "owner-1", "consultant-1")
	s.refusedWith("the consultant as the administrator", r, err, api.ErrRejected, "who is not the consultant")
	r, err = s.declare("decl-1c", "ob-1", "consultant-1", "owner-1", " ")
	s.refusedWith("no administrator", r, err, api.ErrRejected, "the administrator")
	first := s.must(s.declare("decl-2", "ob-1", "consultant-1", "owner-1", "dan"))
	if first.ChangeID != "ob-1" || first.NewRevision != 1 {
		t.Fatalf("the declaration's receipt: %+v", first)
	}
	if again := s.must(s.declare("decl-2", "ob-1", "consultant-1", "owner-1", "dan")); again != first {
		t.Fatalf("a replay: %+v, want %+v", again, first)
	}
	if r, err = s.declare("decl-3", "ob-1", "consultant-1", "owner-1", "dan"); err == nil {
		t.Fatalf("a second declaration of the same onboarding: %+v", r)
	}

	// the declaration names dan: another proposer who holds the permissions is not the administrator, with or without an acknowledgement
	s.must(s.propose("L9", "frank", time.Time{}, tenantEntry("WITH_REASON")))
	for _, ack := range []string{"", "I accept it"} {
		r, err = s.approveWith(s.command(), "L9", "frank", ack)
		s.refusedWith("a proposer who is not the declared administrator, acknowledgement "+ack, r, err, api.ErrRejected, "SETTINGS_SAME_ACTOR",
			"not the single administrator this tenant declared (dan)", "have the declared administrator propose it")
	}
	r, err = s.approveWith(s.command(), "L1", "dan", "")
	s.refusedWith("without an acknowledgement", r, err, api.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT", "written risk acknowledgement of 1 to 2000 characters")
	r, err = s.approveWith(s.command(), "L1", "dan", " \n ")
	s.refusedWith("a blank acknowledgement", r, err, api.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT")
	r, err = s.approveWith(s.command(), "L1", "dan", strings.Repeat("x", 2001))
	s.refusedWith("too long an acknowledgement", r, err, api.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT", "shorten it")
	if v, _ := s.s1("TENANT", "", time.Time{}); v.Value != "SUPERVISOR_ONLY" {
		t.Fatalf("nothing was approved: %+v", v)
	}
	approved := s.must(s.approveWith("self-1", "L1", "dan", "  I am the only administrator and accept the risk.  "))
	if approved.SettingsVersion != 2 {
		t.Fatalf("the approval: %+v", approved)
	}
	if v, version := s.s1("TENANT", "", time.Time{}); v.Value != "WITH_REASON" || version != 2 {
		t.Fatalf("S1 after the approval: %+v, version %d", v, version)
	}
	c := s.change("L1")
	if c.Status != api.SettingsInForce || c.ProposedBy != "dan" || c.ApprovedBy != "dan" || c.AutoApproved || c.RiskAcknowledgement != "I am the only administrator and accept the risk." {
		t.Fatalf("the change set: %+v", c)
	}
	// a command id is bound to its acknowledgement: the same command and text answers its receipt, another text is a reuse
	if again := s.must(s.approveWith("self-1", "L1", "dan", "  I am the only administrator and accept the risk.  ")); again != approved {
		t.Fatalf("a replay: %+v, want %+v", again, approved)
	}
	r, err = s.approveWith("self-1", "L1", "dan", "another text")
	s.refusedWith("the command id with another acknowledgement", r, err, api.ErrIdempotencyConflict)

	// another approver gives no acknowledgement, and the second person still approves as before
	s.must(s.propose("T2", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("L2", "dan", time.Time{}, tenantEntry("WITH_REASON")))
	r, err = s.approveWith(s.command(), "L2", "bob", "from someone else")
	s.refusedWith("an acknowledgement by another approver", r, err, api.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT", "leave it out")
	s.must(s.approve("L2", "bob"))
	if c = s.change("L2"); c.RiskAcknowledgement != "" || c.ApprovedBy != "bob" {
		t.Fatalf("the second person's approval: %+v", c)
	}
	// a change set that only tightens is approved when it is proposed, with no acknowledgement
	if c = s.change("T2"); !c.AutoApproved || c.RiskAcknowledgement != "" {
		t.Fatalf("a tightening: %+v", c)
	}

	// the exception report: for a holder of ApproveSettings, in the period, with the declaration, the self approval and its acknowledgement
	now := time.Now().UTC()
	ask := api.GetSeaExceptionReport{TenantID: s.tenant, ActorID: "bob", From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	rep, err := s.module.GetSeaExceptionReport(context.Background(), ask)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SelfApproved) != 1 || rep.SelfApproved[0].ChangeID != "L1" || rep.SelfApproved[0].ApprovedBy != "dan" ||
		rep.SelfApproved[0].Acknowledgement != "I am the only administrator and accept the risk." || len(rep.SelfApproved[0].Entries) != 1 {
		t.Fatalf("self approved: %+v", rep.SelfApproved)
	}
	if len(rep.Declarations) != 1 || rep.Declarations[0].OnboardingID != "ob-1" || rep.Declarations[0].ProposedBy != "consultant-1" || rep.Declarations[0].ConfirmedBy != "owner-1" || rep.Declarations[0].AdministratorID != "dan" {
		t.Fatalf("declarations: %+v", rep.Declarations)
	}
	ask.ActorID = "ann"
	if _, err = s.module.GetSeaExceptionReport(context.Background(), ask); err == nil || !strings.Contains(err.Error(), "ApproveSettings") {
		t.Fatalf("a holder of ConfigureSettings only: %v", err)
	}
}

// What the database refuses past SEA's commands on the application role, each by its name: an approval by the
// proposer in a tenant with no declaration, or whose declaration came after it; without an acknowledgement; with a
// blank or too long one; with one on another person's approval or on a change set that does not loosen; and the
// declaration itself: two different people, only inserted, the tenant's own.
func TestSeaSingleAdministratorDatabaseRulesRefuseRawWrites(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("WITH_REASON")))
	self := func(id, ack string) string {
		return fmt.Sprintf(`INSERT INTO cargo_sea.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
          version_no,in_force_at,auto,risk_acknowledgement)
        SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,proposed_by,now(),%s,greatest(effective_at,now()),false,%s FROM cargo_sea.settings_change
        WHERE tenant_id=$1 AND change_id='%s'`, nextVersion, ack, id)
	}
	byAnother := func(id, ack string) string {
		return strings.Replace(self(id, ack), "proposed_by,now(),", "'bob',now(),", 1)
	}
	declaration := func(onboarding, proposer, confirmer, at string) string {
		return fmt.Sprintf(`INSERT INTO cargo_sea.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,administrator,declared_at) VALUES ($1,'%s','%s','%s','dan',%s)`,
			onboarding, proposer, confirmer, at)
	}
	check := func(name string, stmts []string, state, constraint string) {
		t.Helper()
		err := s.rawTx(t, stmts...)
		if err == nil || sqlState(err) != state || (constraint != "" && constraintOf(err) != constraint) {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, sqlState(err), constraintOf(err), state, constraint)
		}
	}
	check("an approval by the proposer in a tenant with no declaration", []string{self("L1", "'I accept'")}, "23514", "settings_approval_single_administrator")
	check("a declaration recorded after the approval", []string{declaration("ob-late", "consultant-1", "owner-1", "now() + interval '1 hour'"), self("L1", "'I accept'")},
		"23514", "settings_approval_single_administrator")
	check("a declaration of one person", []string{strings.Replace(declaration("ob-1", "dan", "dan", "now()"), "'dan',now()", "'eve',now()", 1)}, "23514", "single_administrator_two_people")
	check("a declaration whose administrator is the consultant", []string{declaration("ob-1", "dan", "owner-1", "now()")}, "23514", "single_administrator_not_the_consultant")
	check("a declaration with a blank administrator", []string{strings.Replace(declaration("ob-1", "consultant-1", "owner-1", "now()"), "'dan',now()", "' ',now()", 1)}, "23514",
		"single_administrator_administrator_check")
	check("a declaration of an onboarding that is no identifier", []string{declaration("-x", "a", "b", "now()")}, "23514", "single_administrator_onboarding_id_check")
	check("another tenant's declaration", []string{`INSERT INTO cargo_sea.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,administrator,declared_at)
      VALUES ($1 || '-other','ob-1','a','b','c',now())`}, "42501", "")
	s.must(s.declare("decl-1", "ob-1", "consultant-1", "owner-1", "dan"))
	s.must(s.propose("L8", "frank", time.Time{}, tenantEntry("WITH_REASON")))
	check("an approval by a proposer the declaration does not name", []string{self("L8", "'I accept'")}, "23514", "settings_approval_single_administrator")
	check("an approval by the proposer without an acknowledgement", []string{self("L1", "NULL")}, "23514", "settings_approval_other_actor")
	check("a blank acknowledgement", []string{self("L1", "'  '")}, "23514", "settings_approval_acknowledgement_length")
	check("an acknowledgement of 2001 characters", []string{self("L1", "repeat('x',2001)")}, "23514", "settings_approval_acknowledgement_length")
	// bob is declared an administrator too (by another onboarding), so that the trigger lets his row through to the check on its own
	s.must(s.declare("decl-bob", "ob-bob", "consultant-1", "owner-1", "bob"))
	check("an acknowledgement on another person's approval", []string{byAnother("L1", "'I accept'")}, "23514", "settings_approval_acknowledgement_self")
	check("an acknowledgement on an automatic approval", []string{rawChange("R9", "dan", "now()", true), rawEntry("R9", 1, "LEGAL_ENTITY", "LE", "sea.S1", "SUPERVISOR_ONLY"),
		strings.Replace(self("R9", "'I accept'"), "false,'I accept'", "true,'I accept'", 1)}, "23514", "settings_approval_acknowledgement_self")
	check("an acknowledgement on a change set that does not loosen", []string{rawChange("R9", "dan", "now()", false), rawEntry("R9", 1, "LEGAL_ENTITY", "LE", "sea.S1", "SUPERVISOR_ONLY"),
		self("R9", "'I accept'")}, "23514", "settings_approval_acknowledgement_loosening")
	for _, stmt := range []string{`UPDATE cargo_sea.single_administrator SET proposed_by = 'x' WHERE tenant_id = $1`, `DELETE FROM cargo_sea.single_administrator WHERE tenant_id = $1`} {
		if err := s.rawTx(t, stmt); err == nil || (sqlState(err) != "23001" && sqlState(err) != "42501") {
			t.Errorf("%s: %v", stmt, err)
		}
	}
	// written as the command writes it, with the declaration in place, the same approval is accepted: what was refused above was the rule
	if err := s.rawTx(t, self("L1", "'I accept'")); err != nil {
		t.Fatalf("a self approval with an acknowledgement and a declaration: %v", err)
	}
	if s.count("single_administrator") != 2 {
		t.Errorf("%d declarations", s.count("single_administrator"))
	}
}
