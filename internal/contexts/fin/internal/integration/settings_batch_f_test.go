//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// V2.102 batch F, FIN on PostgreSQL (migration 000022): a change set held for an onboarding, the refusal of every other change set
// while one waits, the basis of a single administrator's self-approval, and the review of a self-approval. rita holds the review
// permission, by name; dan holds the permissions of a configurer and an approver, and may be the single administrator.

func (s *settingsWorld) proposeHeld(id, actor, onboarding string, entries ...api.FinSettingEntry) (api.Receipt, error) {
	return s.module.ProposeFinSettingsChange(context.Background(), api.ProposeFinSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: actor,
		ChangeID: id, Entries: entries, Reason: "because " + id, OnboardingID: onboarding})
}

func (s *settingsWorld) review(command, id, actor, conclusion, note string) (api.Receipt, error) {
	return s.module.ReviewFinSelfApprovedSettingsChange(context.Background(), api.ReviewFinSelfApprovedSettingsChange{CommandID: command, TenantID: s.tenant, ActorID: actor,
		ChangeID: id, Conclusion: conclusion, Note: note})
}

func TestFinAHeldChangeSetWaitsForTheOwnerAndBlocksEveryOtherChangeSet(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("L0", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS"))) // in force, so that the next change set loosens
	s.must(s.propose("P0", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	// held, not approved even when it only tightens, with no version made
	r := s.must(s.proposeHeld("H1", "ann", "ob-1", tenantEntry("fin.F9", "RECONFIRM")))
	if r.NewRevision != 1 || r.Version != 0 {
		t.Fatalf("the receipt of a held change set: %+v", r)
	}
	_, err := s.proposeHeld("H1", "ann", "ob-1", tenantEntry("fin.F9", "RECONFIRM"))
	s.refused("the same change set under another command id", err, api.ErrConflict, "SETTINGS_DUPLICATE")
	c := s.changes()["H1"]
	if c.Status != api.SettingsProposed || c.AutoApproved || c.Loosening || c.HeldForOnboarding != "ob-1" || c.ApprovedBy != "" {
		t.Fatalf("the held change set: %+v", c)
	}
	if _, version := s.settings("TENANT", "", time.Time{}); version != 1 {
		t.Fatalf("a held change set makes no version: %d", version)
	}
	// no other change set is proposed or approved, in any of the ways, and the refusal names the onboarding and the way out
	_, err = s.propose("N1", "eve", time.Time{}, tenantEntry("fin.F7", "BLOCKS"))
	s.refused("another proposal", err, api.ErrRejected, "SETTINGS_ONBOARDING_WAITING", "onboarding ob-1 waits", "change set H1", "confirm the onboarding", "withdraw it")
	_, err = s.proposeHeld("H2", "ann", "ob-2", tenantEntry("fin.F9", "RECONFIRM"))
	s.refused("another onboarding's change set", err, api.ErrRejected, "SETTINGS_ONBOARDING_WAITING", "ob-1")
	_, err = s.approve("P0", "bob")
	s.refused("the approval of an older change set", err, api.ErrRejected, "SETTINGS_ONBOARDING_WAITING", "the approval of change set P0")
	// the proposer may not approve it, not even as the single administrator, and a person other than the proposer who is not the owner of the
	// onboarding is not stopped by FIN, which knows only that a second person approves it
	s.must(s.declare("decl-1", "ob-0", "consultant-1", "owner-1", "dan"))
	s.must(s.proposeHeld("H3", "dan", "ob-1", tenantEntry("fin.F9", "KEEP_AMOUNT")))
	for _, ack := range []string{"", "I accept"} {
		_, err = s.approveWith(s.command(), "H3", "dan", ack)
		s.refused("the proposer approving, acknowledgement "+ack, err, api.ErrRejected, "SETTINGS_HELD_FOR_ONBOARDING", "held for onboarding ob-1", "ConfirmOnboarding")
	}
	// the way out: a cancellation (the withdrawal), after which everything is as before
	s.must(s.cancel("H1", "ann"))
	if _, err = s.propose("N1", "eve", time.Time{}, tenantEntry("fin.F7", "BLOCKS")); err == nil {
		t.Fatal("H3 still waits")
	}
	s.must(s.cancel("H3", "dan"))
	s.must(s.propose("N1", "eve", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_ONLY")))
	s.must(s.approve("P0", "bob"))
	// a change set held and approved by a second person is in force with a version, and stops waiting
	s.must(s.proposeHeld("H4", "ann", "ob-3", tenantEntry("fin.F9", "RECONFIRM")))
	approved := s.must(s.approve("H4", "bob"))
	if approved.Version < 1 {
		t.Fatalf("the approval of a held change set: %+v", approved)
	}
	if c = s.changes()["H4"]; c.Status != api.SettingsInForce || c.ApprovedBy != "bob" || c.AutoApproved || c.HeldForOnboarding != "ob-3" {
		t.Fatalf("H4: %+v", c)
	}
	s.must(s.propose("N2", "eve", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER")))
}

func TestFinASelfApprovalRecordsTheDeclarationItRelaysOn(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.declare("decl-1", "ob-1", "consultant-1", "owner-1", "dan"))
	s.must(s.approveWith(s.command(), "L1", "dan", "I accept the risk"))
	c := s.changes()["L1"]
	if c.BasisOnboardingID != "ob-1" || c.RiskAcknowledgement != "I accept the risk" {
		t.Fatalf("the approval records its basis: %+v", c)
	}
	// a later declaration replaces it, and the next self-approval relies on that one
	s.must(s.propose("T2", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.declare("decl-2", "ob-2", "consultant-1", "owner-1", "frank"))
	s.must(s.propose("L2", "frank", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.approveWith(s.command(), "L2", "frank", "mine"))
	if c = s.changes()["L2"]; c.BasisOnboardingID != "ob-2" {
		t.Fatalf("the second approval: %+v", c)
	}
	// the approval of another person records none
	s.must(s.propose("T3", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L3", "ann", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.approve("L3", "bob"))
	if c = s.changes()["L3"]; c.BasisOnboardingID != "" || c.RiskAcknowledgement != "" {
		t.Fatalf("the second person's approval: %+v", c)
	}
	now := time.Now().UTC()
	rep, err := s.module.GetFinExceptionReport(context.Background(), api.GetFinExceptionReport{TenantID: s.tenant, ActorID: "bob", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if err != nil || len(rep.SelfApproved) != 2 {
		t.Fatalf("the report: %+v, %v", rep, err)
	}
	for _, line := range rep.SelfApproved {
		want := map[string][2]string{"L1": {"ob-1", "dan"}, "L2": {"ob-2", "frank"}}[line.ChangeID]
		if line.BasisOnboardingID != want[0] || line.BasisAdministrator != want[1] || line.BasisDeclaredAt.IsZero() || line.BasisDeclaredAt.After(line.ApprovedAt) {
			t.Errorf("%s: the basis %+v, want %v", line.ChangeID, line, want)
		}
	}
}

func TestFinTheReviewOfASelfApproval(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.declare("decl-1", "ob-1", "consultant-1", "owner-1", "dan"))
	s.must(s.approveWith(s.command(), "L1", "dan", "I accept"))
	s.must(s.propose("T2", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L2", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.approveWith(s.command(), "L2", "dan", "I accept again"))
	s.must(s.propose("T3", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L3", "ann", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.approve("L3", "bob"))

	_, err := s.review(s.command(), "L1", "bob", "ACCEPTED", "")
	s.refused("a person without the review permission", err, api.ErrUnauthorized, "ReviewSettingsExceptions")
	_, err = s.review(s.command(), "L1", "dan", "ACCEPTED", "")
	s.refused("the approver", err, api.ErrRejected, "SETTINGS_REVIEWER_IS_APPROVER", "have another holder of the review permission review it")
	_, err = s.review(s.command(), "L3", "rita", "ACCEPTED", "")
	s.refused("a change set approved by another person", err, api.ErrRejected, "SETTINGS_NOT_SELF_APPROVED", "exception report")
	_, err = s.review(s.command(), "T3", "rita", "ACCEPTED", "")
	s.refused("a change set approved automatically", err, api.ErrRejected, "SETTINGS_NOT_SELF_APPROVED")
	_, err = s.review(s.command(), "X9", "rita", "ACCEPTED", "")
	s.refused("a change set that does not exist", err, api.ErrNotFound)
	_, err = s.review(s.command(), "L1", "rita", "REJECTED", "x")
	s.refused("another conclusion", err, api.ErrRejected, "SETTINGS_REVIEW_CONCLUSION", "ACCEPTED or NOT_ACCEPTED")
	_, err = s.review(s.command(), "L1", "rita", "NOT_ACCEPTED", "")
	s.refused("NOT_ACCEPTED without a note", err, api.ErrRejected, "SETTINGS_REVIEW_CONCLUSION", "says why")
	_, err = s.review(s.command(), "L1", "rita", "ACCEPTED", strings.Repeat("x", 2001))
	s.refused("a note of 2001 characters", err, api.ErrRejected, "SETTINGS_REVIEW_CONCLUSION", "shorten it")
	if n := s.count("settings_self_approval_review"); n != 0 {
		t.Fatalf("a refused review wrote %d rows", n)
	}

	// the two pending, nothing reviewed
	now := time.Now().UTC()
	ask := api.ListFinSelfApprovalReviews{TenantID: s.tenant, ActorID: "rita", From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	got, err := s.module.ListFinSelfApprovalReviews(context.Background(), ask)
	if err != nil || len(got.Pending) != 2 || len(got.Reviewed) != 0 {
		t.Fatalf("pending: %+v, %v", got, err)
	}
	if p := got.Pending[0]; p.ChangeID != "L1" || p.BasisOnboardingID != "ob-1" || p.BasisAdministrator != "dan" || p.ApprovedBy != "dan" || len(p.Entries) != 1 {
		t.Fatalf("the pending line: %+v", p)
	}
	first := s.must(s.review("review-1", "L1", "rita", "ACCEPTED", ""))
	if first.SubjectID != "L1" || first.NewRevision != 1 {
		t.Fatalf("the receipt: %+v", first)
	}
	if again := s.must(s.review("review-1", "L1", "rita", "ACCEPTED", "")); again != first {
		t.Fatalf("a replay by its command id: %+v, want %+v", again, first)
	}
	_, err = s.review("review-1", "L1", "rita", "NOT_ACCEPTED", "changed my mind")
	s.refused("the command id with another conclusion", err, api.ErrIdempotencyConflict)
	_, err = s.review(s.command(), "L1", "rita", "NOT_ACCEPTED", "changed my mind")
	s.refused("a second review of the change set", err, api.ErrRejected, "SETTINGS_ALREADY_REVIEWED", "reviewed by rita", "propose a normal change set that restores the earlier values")
	before, _ := s.settings("TENANT", "", time.Time{})
	s.must(s.review(s.command(), "L2", "rita", "NOT_ACCEPTED", "  the risk is not one the customer may take  "))
	if after, _ := s.settings("TENANT", "", time.Time{}); after["fin.F7"].Value != before["fin.F7"].Value {
		t.Fatalf("a NOT_ACCEPTED changed a setting: %+v", after["fin.F7"])
	}
	got, err = s.module.ListFinSelfApprovalReviews(context.Background(), ask)
	if err != nil || len(got.Pending) != 0 || len(got.Reviewed) != 2 {
		t.Fatalf("reviewed: %+v, %v", got, err)
	}
	for _, r := range got.Reviewed {
		want := map[string][3]string{"L1": {"rita", "ACCEPTED", ""}, "L2": {"rita", "NOT_ACCEPTED", "the risk is not one the customer may take"}}[r.ChangeID]
		if r.ReviewedBy != want[0] || r.Conclusion != want[1] || r.Note != want[2] || r.ReviewedAt.IsZero() || r.ApprovedBy != "dan" {
			t.Errorf("%s: %+v, want %v", r.ChangeID, r, want)
		}
	}
	// the period is of the approvals: one that holds none lists none; only a holder of the permission asks
	ask.From, ask.To = now.Add(time.Hour), now.Add(2*time.Hour)
	if got, err = s.module.ListFinSelfApprovalReviews(context.Background(), ask); err != nil || len(got.Pending)+len(got.Reviewed) != 0 {
		t.Fatalf("a later period: %+v, %v", got, err)
	}
	ask.ActorID, ask.From, ask.To = "bob", now.Add(-time.Hour), now.Add(time.Hour)
	if _, err = s.module.ListFinSelfApprovalReviews(context.Background(), ask); err == nil || !strings.Contains(err.Error(), "ReviewSettingsExceptions") {
		t.Fatalf("a holder of ApproveSettings only: %v", err)
	}
	ask.ActorID, ask.To = "rita", ask.From
	if _, err = s.module.ListFinSelfApprovalReviews(context.Background(), ask); err == nil || !strings.Contains(err.Error(), "from before to") {
		t.Fatalf("an empty period: %v", err)
	}
}

// What the database refuses past FIN's commands, each rule by a raw write on its own (the application role, one transaction each).
func TestFinBatchFDatabaseRulesRefuseRawWrites(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("L1", "dan", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.declare("decl-1", "ob-1", "consultant-1", "owner-1", "dan"))
	check := func(name string, stmts []string, state, constraint string) {
		t.Helper()
		err := s.rawTx(t, stmts...)
		got, named := sqlState(err)
		if err == nil || got != state || named != constraint {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, got, named, state, constraint)
		}
	}
	hold := func(id, onboarding string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.settings_hold (tenant_id,change_id,onboarding_id) VALUES ($1,'%s','%s')`, id, onboarding)
	}
	// an approval by the proposer with an acknowledgement and a basis, or by another person with whatever the columns say
	approval := func(id, by, ack, basis string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
          version_no,in_force_at,auto,risk_acknowledgement,basis_onboarding_id)
        SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,%s,now(),%s,greatest(effective_at,now()),false,%s,%s
          FROM cargo_fin.settings_change WHERE tenant_id=$1 AND change_id='%s'`, by, nextVersion, ack, basis, id)
	}
	self := func(id, basis string) string { return approval(id, "proposed_by", "'I accept'", basis) }
	heldChange := func(id, by string) []string {
		return []string{rawChange(id, by, "now()", false), rawEntry(id, 1, "TENANT", "", "fin.F9", "RECONFIRM", "NULL"), hold(id, "ob-1")}
	}
	plain := func(id, by string) []string {
		return []string{rawChange(id, by, "now()", false), rawEntry(id, 1, "TENANT", "", "fin.F9", "RECONFIRM", "NULL")}
	}
	const violation = "23514"

	// the basis of a self-approval
	check("a basis that is not the declaration in force", []string{self("L1", "'ob-other'")}, violation, "settings_approval_basis")
	check("an acknowledgement without its basis", []string{self("L1", "NULL")}, violation, "settings_approval_basis")
	check("a basis without an acknowledgement", []string{approval("L1", "'bob'", "NULL", "'ob-1'")}, violation, "settings_approval_basis_with_acknowledgement")
	s.must(s.declare("decl-2", "ob-2", "consultant-1", "owner-1", "dan")) // a later declaration of the same administrator: the earlier one is no longer in force
	check("the earlier declaration after a later one", []string{self("L1", "'ob-1'")}, violation, "settings_approval_basis")

	// a held change set: no automatic approval, no approval by its proposer, and the hold is written with the change set and before its approval
	check("an automatic approval of a held change set", append(heldChange("R1", "dan"), rawApproval("R1", "dan", "now()", nextVersion, "now()", true)), violation, "settings_approval_held_auto")
	check("an approval by the proposer of a held change set", append(heldChange("R2", "dan"), self("R2", "'ob-2'")), violation, "settings_approval_held_proposer")
	check("a hold written after the approval", append(plain("R3", "ann"), rawApproval("R3", "ann", "now()", nextVersion, "now()", true), hold("R3", "ob-1")), violation, "settings_hold_unapproved")
	check("a hold of an onboarding that is no identifier", append(plain("R4", "ann"), hold("R4", "-x")), violation, "settings_hold_onboarding_shape")
	check("a hold written apart from its change set", []string{hold("T1", "ob-1")}, violation, "settings_hold_with_change")
	if n := s.count("settings_hold"); n != 0 {
		t.Fatalf("a refused write left %d holds", n)
	}

	// while a held change set waits, no other change set: one not held is not approved, not proposed, and another onboarding holds none
	s.must(s.propose("T2", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("PX", "ann", time.Time{}, tenantEntry("fin.F7", "DOES_NOT_BLOCK"))) // a loosening, waiting for a second person
	s.must(s.proposeHeld("H1", "ann", "ob-1", tenantEntry("fin.F9", "RECONFIRM")))
	check("the approval of a change set that is not held, while one waits", []string{approval("PX", "'bob'", "NULL", "NULL")}, violation, "settings_approval_onboarding_waiting")
	check("a change set that is not held, proposed while one waits", plain("R5", "eve"), violation, "settings_onboarding_waiting")
	check("a hold for another onboarding, while one waits", []string{rawChange("R6", "ann", "now()", false), rawEntry("R6", 1, "TENANT", "", "fin.F9", "RECONFIRM", "NULL"), hold("R6", "ob-2")},
		violation, "settings_hold_other_onboarding")
	// the held change set is approved by another person, and then nothing waits
	if err := s.rawTx(t, approval("H1", "'bob'", "NULL", "NULL")); err != nil {
		t.Fatalf("the approval of the held change set by a second person: %v", err)
	}
	if err := s.rawTx(t, plain("R5", "eve")...); err != nil {
		t.Fatalf("a change set after the held one is approved: %v", err)
	}

	// the review of a self-approval: L1 was approved by dan alone as the single administrator, T1 by its proposer automatically
	if err := s.rawTx(t, self("L1", "'ob-2'")); err != nil {
		t.Fatalf("a self-approval with the declaration in force: %v", err)
	}
	review := func(id, approvedBy, reviewer, at, conclusion, note string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.settings_self_approval_review (tenant_id,change_id,approved_by,reviewed_by,reviewed_at,conclusion,note)
          VALUES ($1,'%s','%s','%s',%s,'%s',%s)`, id, approvedBy, reviewer, at, conclusion, note)
	}
	check("a reviewer who is the approver", []string{review("L1", "dan", "dan", "now()", "ACCEPTED", "NULL")}, violation, "settings_review_reviewer")
	check("the review of an approval by another person", []string{review("PX", "bob", "rita", "now()", "ACCEPTED", "NULL")}, violation, "settings_review_self_approved")
	check("the review of an automatic approval", []string{review("T1", "ann", "rita", "now()", "ACCEPTED", "NULL")}, violation, "settings_review_self_approved")
	check("a review before the approval", []string{review("L1", "dan", "rita", "now() - interval '1 day'", "ACCEPTED", "NULL")}, violation, "settings_review_after_approval")
	check("a conclusion that is not one of the two", []string{review("L1", "dan", "rita", "now()", "REJECTED", "'x'")}, violation, "settings_review_conclusion")
	check("NOT_ACCEPTED without a note", []string{review("L1", "dan", "rita", "now()", "NOT_ACCEPTED", "NULL")}, violation, "settings_review_note_required")
	check("a blank note", []string{review("L1", "dan", "rita", "now()", "ACCEPTED", "'  '")}, violation, "settings_review_note_length")
	check("a note of 2001 characters", []string{review("L1", "dan", "rita", "now()", "ACCEPTED", "repeat('x',2001)")}, violation, "settings_review_note_length")
	check("a reviewer with spaces around the id", []string{review("L1", "dan", " rita", "now()", "ACCEPTED", "NULL")}, violation, "settings_review_reviewer_shape")
	check("an approver that is not the approval's", []string{review("L1", "zed", "rita", "now()", "ACCEPTED", "NULL")}, "23503", "settings_review_of_approval")
	if err := s.rawTx(t, review("L1", "dan", "rita", "now()", "ACCEPTED", "NULL")); err != nil {
		t.Fatalf("a review by another person of a self-approval: %v", err)
	}
	check("a second review of the change set", []string{review("L1", "dan", "pat", "now()", "NOT_ACCEPTED", "'no'")}, "23505", "settings_self_approval_review_pkey")
	for _, stmt := range []string{`UPDATE cargo_fin.settings_self_approval_review SET conclusion = 'NOT_ACCEPTED', note = 'x' WHERE tenant_id = $1`,
		`DELETE FROM cargo_fin.settings_self_approval_review WHERE tenant_id = $1`, `UPDATE cargo_fin.settings_hold SET onboarding_id = 'ob-9' WHERE tenant_id = $1`,
		`DELETE FROM cargo_fin.settings_hold WHERE tenant_id = $1`} {
		if err := s.rawTx(t, stmt); err == nil {
			t.Errorf("%s: no refusal", stmt)
		} else if state, _ := sqlState(err); state != "23001" && state != "42501" {
			t.Errorf("%s: %v (state %s)", stmt, err, state)
		}
	}
	if n := s.count("settings_self_approval_review"); n != 1 {
		t.Errorf("%d reviews", n)
	}
	// row security is enabled and forced on each new table
	for _, table := range []string{"settings_hold", "settings_self_approval_review"} {
		var enabled, forced bool
		err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
			return e.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
              WHERE n.nspname = 'cargo_fin' AND c.relname = $1`, table).Scan(&enabled, &forced)
		})
		if err != nil || !enabled || !forced {
			t.Errorf("%s: row security enabled %v, forced %v, %v", table, enabled, forced, err)
		}
	}
}
