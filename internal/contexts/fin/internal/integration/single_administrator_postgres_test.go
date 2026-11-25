//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// V2.102 on PostgreSQL, migration 000022: a single administrator approves their own loosening, with a written risk
// acknowledgement, in a tenant that declared one (SEA's tests of the same are in sea_single_admin_test.go).

func (s *settingsWorld) declare(command, onboarding, proposer, confirmer string) (api.Receipt, error) {
	return s.module.DeclareFinSingleAdministrator(context.Background(), api.DeclareFinSingleAdministrator{CommandID: command, TenantID: s.tenant, ActorID: confirmer,
		OnboardingID: onboarding, ProposedBy: proposer})
}

func (s *settingsWorld) approveWith(command, id, actor, acknowledgement string) (api.Receipt, error) {
	return s.module.ApproveFinSettingsChange(context.Background(), api.ApproveFinSettingsChange{CommandID: command, TenantID: s.tenant, ActorID: actor, ChangeID: id,
		RiskAcknowledgement: acknowledgement})
}

func TestFinASingleAdministratorApprovesTheirOwnLoosening(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	_, err := s.approveWith(s.command(), "L1", "dan", "")
	s.refused("without the declaration", err, api.ErrRejected, "SETTINGS_SAME_ACTOR", "have another approver approve it")
	_, err = s.approveWith(s.command(), "L1", "dan", "I accept it")
	s.refused("an acknowledgement without the declaration", err, api.ErrRejected, "SETTINGS_SAME_ACTOR", "has not declared a single administrator")

	_, err = s.declare("decl-1", "ob-1", "dan", "dan")
	s.refused("one person for both", err, api.ErrRejected, "two different people")
	first := s.must(s.declare("decl-2", "ob-1", "consultant-1", "owner-1"))
	if first.SubjectID != "ob-1" || first.NewRevision != 1 {
		t.Fatalf("the declaration's receipt: %+v", first)
	}
	if again := s.must(s.declare("decl-2", "ob-1", "consultant-1", "owner-1")); again != first {
		t.Fatalf("a replay: %+v, want %+v", again, first)
	}
	if _, err = s.declare("decl-3", "ob-1", "consultant-1", "owner-1"); err == nil {
		t.Fatal("a second declaration of the same onboarding")
	}

	_, err = s.approveWith(s.command(), "L1", "dan", "")
	s.refused("without an acknowledgement", err, api.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT", "written risk acknowledgement of 1 to 2000 characters")
	_, err = s.approveWith(s.command(), "L1", "dan", " \n ")
	s.refused("a blank acknowledgement", err, api.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT")
	_, err = s.approveWith(s.command(), "L1", "dan", strings.Repeat("x", 2001))
	s.refused("too long an acknowledgement", err, api.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT", "shorten it")
	if got, _ := s.settings("TENANT", "", time.Time{}); got["fin.F7"].Value != "BLOCKS" {
		t.Fatalf("nothing was approved: %+v", got["fin.F7"])
	}
	approved := s.must(s.approveWith("self-1", "L1", "dan", "  I am the only administrator and accept the risk.  "))
	if approved.Version != 2 {
		t.Fatalf("the approval: %+v", approved)
	}
	if got, version := s.settings("TENANT", "", time.Time{}); got["fin.F7"].Value != "DOES_NOT_BLOCK" || version != 2 {
		t.Fatalf("F7 after the approval: %+v, version %d", got["fin.F7"], version)
	}
	c := s.changes()["L1"]
	if c.Status != api.SettingsInForce || c.ProposedBy != "dan" || c.ApprovedBy != "dan" || c.AutoApproved || c.RiskAcknowledgement != "I am the only administrator and accept the risk." {
		t.Fatalf("the change set: %+v", c)
	}
	if again := s.must(s.approveWith("self-1", "L1", "dan", "  I am the only administrator and accept the risk.  ")); again != approved {
		t.Fatalf("a replay: %+v, want %+v", again, approved)
	}
	_, err = s.approveWith("self-1", "L1", "dan", "another text")
	s.refused("the command id with another acknowledgement", err, api.ErrIdempotencyConflict)

	s.must(s.propose("T2", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L2", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	_, err = s.approveWith(s.command(), "L2", "bob", "from someone else")
	s.refused("an acknowledgement by another approver", err, api.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT", "leave it out")
	s.must(s.approve("L2", "bob"))
	if c = s.changes()["L2"]; c.RiskAcknowledgement != "" || c.ApprovedBy != "bob" {
		t.Fatalf("the second person's approval: %+v", c)
	}
	if c = s.changes()["T2"]; !c.AutoApproved || c.RiskAcknowledgement != "" {
		t.Fatalf("a tightening: %+v", c)
	}

	now := time.Now().UTC()
	ask := api.GetFinExceptionReport{TenantID: s.tenant, ActorID: "bob", From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	rep, err := s.module.GetFinExceptionReport(context.Background(), ask)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SelfApproved) != 1 || rep.SelfApproved[0].ChangeID != "L1" || rep.SelfApproved[0].ApprovedBy != "dan" ||
		rep.SelfApproved[0].Acknowledgement != "I am the only administrator and accept the risk." || len(rep.SelfApproved[0].Entries) != 1 {
		t.Fatalf("self approved: %+v", rep.SelfApproved)
	}
	if len(rep.Declarations) != 1 || rep.Declarations[0].OnboardingID != "ob-1" || rep.Declarations[0].ProposedBy != "consultant-1" || rep.Declarations[0].ConfirmedBy != "owner-1" {
		t.Fatalf("declarations: %+v", rep.Declarations)
	}
	ask.ActorID = "ann"
	if _, err = s.module.GetFinExceptionReport(context.Background(), ask); err == nil || !strings.Contains(err.Error(), "ApproveSettings") {
		t.Fatalf("a holder of ConfigureSettings only: %v", err)
	}
	ask.ActorID, ask.To = "bob", ask.From
	if _, err = s.module.GetFinExceptionReport(context.Background(), ask); err == nil || !strings.Contains(err.Error(), "from before to") {
		t.Fatalf("an empty period: %v", err)
	}
}

func TestFinSingleAdministratorDatabaseRulesRefuseRawWrites(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	self := func(id, ack string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
          version_no,in_force_at,auto,risk_acknowledgement)
        SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,proposed_by,now(),(SELECT coalesce(max(version_no),0)+1 FROM cargo_fin.settings_approval
          WHERE tenant_id=$1),greatest(effective_at,now()),false,%s FROM cargo_fin.settings_change WHERE tenant_id=$1 AND change_id='%s'`, ack, id)
	}
	declaration := func(onboarding, proposer, confirmer, at string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,declared_at) VALUES ($1,'%s','%s','%s',%s)`,
			onboarding, proposer, confirmer, at)
	}
	check := func(name string, stmts []string, state, constraint string) {
		t.Helper()
		err := s.rawTx(t, stmts...)
		got, name2 := sqlState(err)
		if err == nil || got != state || (constraint != "" && name2 != constraint) {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, got, name2, state, constraint)
		}
	}
	check("an approval by the proposer in a tenant with no declaration", []string{self("L1", "'I accept'")}, "23514", "settings_approval_single_administrator")
	check("a declaration recorded after the approval", []string{declaration("ob-late", "consultant-1", "owner-1", "now() + interval '1 hour'"), self("L1", "'I accept'")},
		"23514", "settings_approval_single_administrator")
	check("a declaration of one person", []string{declaration("ob-1", "dan", "dan", "now()")}, "23514", "single_administrator_two_people")
	check("a declaration of an onboarding that is no identifier", []string{declaration("-x", "a", "b", "now()")}, "23514", "single_administrator_onboarding_id_check")
	check("another tenant's declaration", []string{`INSERT INTO cargo_fin.single_administrator (tenant_id,onboarding_id,proposed_by,confirmed_by,declared_at)
      VALUES ($1 || '-other','ob-1','a','b',now())`}, "42501", "")
	s.must(s.declare("decl-1", "ob-1", "consultant-1", "owner-1"))
	check("an approval by the proposer without an acknowledgement", []string{self("L1", "NULL")}, "23514", "settings_approval_other_actor")
	check("a blank acknowledgement", []string{self("L1", "'  '")}, "23514", "settings_approval_acknowledgement_length")
	check("an acknowledgement of 2001 characters", []string{self("L1", "repeat('x',2001)")}, "23514", "settings_approval_acknowledgement_length")
	check("an acknowledgement on another person's approval", []string{strings.Replace(self("L1", "'I accept'"), "proposed_by,now(),", "'bob',now(),", 1)}, "23514",
		"settings_approval_acknowledgement_self")
	check("an acknowledgement on an automatic approval", []string{rawChange("R9", "dan", "now()", true), rawEntry("R9", 1, "LEGAL_ENTITY", "LE", "fin.F7", "BLOCKS", "NULL"),
		strings.Replace(self("R9", "'I accept'"), "false,'I accept'", "true,'I accept'", 1)}, "23514", "settings_approval_acknowledgement_self")
	check("an acknowledgement on a change set that does not loosen", []string{rawChange("R9", "dan", "now()", false), rawEntry("R9", 1, "LEGAL_ENTITY", "LE", "fin.F7", "BLOCKS", "NULL"),
		self("R9", "'I accept'")}, "23514", "settings_approval_acknowledgement_loosening")
	for _, stmt := range []string{`UPDATE cargo_fin.single_administrator SET proposed_by = 'x' WHERE tenant_id = $1`, `DELETE FROM cargo_fin.single_administrator WHERE tenant_id = $1`} {
		if err := s.rawTx(t, stmt); err == nil {
			t.Errorf("%s: no refusal", stmt)
		} else if state, _ := sqlState(err); state != "23001" && state != "42501" {
			t.Errorf("%s: %v (state %s)", stmt, err, state)
		}
	}
	if err := s.rawTx(t, self("L1", "'I accept'")); err != nil {
		t.Fatalf("a self approval with an acknowledgement and a declaration: %v", err)
	}
	if n := s.count("single_administrator"); n != 1 {
		t.Errorf("%d declarations", n)
	}
}
