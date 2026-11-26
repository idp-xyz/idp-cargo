package settings

import (
	"strings"
	"testing"
)

// V2.102 batch F: a change set held for an onboarding, the refusal of every other change set while one waits, the basis of a
// self-approval, and the review of a self-approval.

func tryProposeHeld(l *Ledger, id, who string, h int, hold string, entries ...Entry) (ChangeSet, error) {
	return l.Propose(testCatalogue(), Proposal{ID: id, Entries: entries, Reason: "because " + id, By: who, At: hours(h), Hold: hold})
}

func TestAHeldChangeSetIsNotApprovedEvenWhenItOnlyTightensAndItsApproverIsNotItsProposer(t *testing.T) {
	cat := testCatalogue()
	for name, entries := range map[string][]Entry{"tightens": {entry(TenantScope, "tst.A", "X0")}, "keeps": {entry(TenantScope, "tst.A", "X1")}, "loosens": {entry(TenantScope, "tst.A", "X2")}} {
		cs, err := tryProposeHeld(NewLedger(nil), "H1", "ann", 1, "ob-1", entries...)
		if err != nil || cs.Approval != nil || cs.Hold != "ob-1" || cs.Status(hours(2)) != StatusProposed {
			t.Fatalf("%s: %+v, %v", name, cs, err)
		}
	}
	l, _ := propose(t, NewLedger(nil), "N1", "ann", 1, -1, entry(TenantScope, "tst.A", "X0")) // an ordinary change set that only tightens is approved at once
	if cs, _ := l.Find("N1"); cs.Approval == nil || !cs.Approval.Auto {
		t.Fatalf("the ordinary change set: %+v", cs)
	}
	cs, err := tryProposeHeld(NewLedger(nil), "H1", "ann", 1, "ob-1", entry(TenantScope, "tst.A", "X2"))
	if err != nil {
		t.Fatal(err)
	}
	l = with(NewLedger(nil), cs)
	// its proposer may not approve it, with or without an acknowledgement and even as the single administrator
	for name, own := range map[string]Own{"alone": {}, "as the single administrator": {Administrators: []string{"ann"}, Acknowledgement: "I accept", Basis: "ob-0"}} {
		a, err := l.ApproveAs(cat, "H1", "ann", hours(2), own)
		refusedWith(t, name, err, ErrHeld, "held for onboarding ob-1", "may not approve it", "ConfirmOnboarding")
		if a != (Approval{}) {
			t.Fatalf("%s: %+v", name, a)
		}
	}
	a, err := l.Approve(cat, "H1", "olga", hours(2))
	if err != nil || a.Auto || a.By != "olga" || a.Version != 1 {
		t.Fatalf("the owner approves it: %+v, %v", a, err)
	}
}

func TestNoOtherChangeSetIsProposedOrApprovedWhileOneHeldForAnOnboardingWaits(t *testing.T) {
	cat := testCatalogue()
	l, _ := propose(t, NewLedger(nil), "L0", "ann", 1, -1, entry(TenantScope, "tst.A", "X2")) // an ordinary loosening, waiting for a second person
	held, err := tryProposeHeld(l, "H1", "ann", 2, "ob-1", entry(TenantScope, "tst.B", "B0"))
	if err != nil {
		t.Fatal(err)
	}
	l = with(l, held)
	if w, ok := l.Waiting(); !ok || w.ID != "H1" || w.Hold != "ob-1" {
		t.Fatalf("waiting: %+v, %v", w, ok)
	}
	// another change set is refused, and so is another onboarding's, and the approval of an ordinary one
	_, err = tryPropose(l, "N2", "bob", 3, -1, entry(TenantScope, "tst.B", "B0"))
	refusedWith(t, "an ordinary proposal", err, ErrOnboardingWaiting, "onboarding ob-1 waits", "change set H1", "confirm the onboarding", "withdraw it")
	_, err = tryProposeHeld(l, "H2", "ann", 3, "ob-2", entry(TenantScope, "tst.B", "B0"))
	refusedWith(t, "another onboarding's", err, ErrOnboardingWaiting, "ob-1")
	_, err = l.Approve(cat, "L0", "bob", hours(3))
	refusedWith(t, "the approval of an ordinary change set", err, ErrOnboardingWaiting, "the approval of change set L0", "ob-1")
	// the held change set itself is approved, and a cancellation is the way out
	if a, err := l.Approve(cat, "H1", "olga", hours(3)); err != nil || a.By != "olga" {
		t.Fatalf("the held change set: %+v, %v", a, err)
	}
	if _, err = l.Cancel(cat, "H1", "ann", "withdrawn", hours(3)); err != nil {
		t.Fatalf("a cancellation is the way out: %v", err)
	}
	// once approved, cancelled or refused it no longer waits
	approved := approve(t, l, "H1", "olga", 3)
	if _, ok := approved.Waiting(); ok {
		t.Fatal("an approved held change set still waits")
	}
	if _, err = tryPropose(approved, "N3", "bob", 4, -1, entry(TenantScope, "tst.B", "B0")); err != nil {
		t.Fatalf("after the confirmation: %v", err)
	}
	x, err := l.Cancel(cat, "H1", "ann", "withdrawn", hours(3))
	if err != nil {
		t.Fatal(err)
	}
	cancelled := replaceSet(l, "H1", func(c *ChangeSet) { c.Cancellation = &x })
	if _, ok := cancelled.Waiting(); ok {
		t.Fatal("a cancelled held change set still waits")
	}
	if _, err = tryPropose(cancelled, "N4", "bob", 4, -1, entry(TenantScope, "tst.B", "B0")); err != nil {
		t.Fatalf("after the withdrawal: %v", err)
	}
	r, err := l.Refuse("H1", "olga", "not this", hours(3))
	if err != nil {
		t.Fatal(err)
	}
	refused := replaceSet(l, "H1", func(c *ChangeSet) { c.Cancellation = &r })
	if _, ok := refused.Waiting(); ok {
		t.Fatal("a refused held change set still waits")
	}
	// a hold is an onboarding's name
	if _, err = tryProposeHeld(NewLedger(nil), "H9", "ann", 1, "has space", entry(TenantScope, "tst.A", "X0")); err == nil || !strings.Contains(err.Error(), "SETTINGS_INVALID") {
		t.Fatalf("a hold that is not an identifier: %v", err)
	}
}

func TestASelfApprovalRecordsTheDeclarationItRelaysOn(t *testing.T) {
	l := ledgerWithALoosening(t)
	a, err := l.ApproveAs(testCatalogue(), "L1", "ann", hours(2), Own{Administrators: []string{"ann"}, Acknowledgement: "I accept", Basis: "ob-7"})
	if err != nil || a.Basis != "ob-7" {
		t.Fatalf("%+v, %v", a, err)
	}
	r := replaceSet(l, "L1", func(c *ChangeSet) { c.Approval = &a }).ExceptionReport(testCatalogue(), hours(10), hours(0), hours(24))
	if len(r.SelfApproved) != 1 || r.SelfApproved[0].Basis != "ob-7" {
		t.Fatalf("the report: %+v", r.SelfApproved)
	}
	d, ok := DeclarationInForce([]Declaration{{OnboardingID: "ob-1", Administrator: "dan", At: hours(1)}, {OnboardingID: "ob-2", Administrator: "eve", At: hours(5)}}, hours(3))
	if !ok || d.OnboardingID != "ob-1" || d.Administrator != "dan" {
		t.Fatalf("the declaration in force: %+v, %v", d, ok)
	}
}

func TestTheReviewOfASelfApproval(t *testing.T) {
	l := ledgerWithALoosening(t)
	a, err := l.ApproveAs(testCatalogue(), "L1", "ann", hours(2), Own{Administrators: []string{"ann"}, Acknowledgement: "I accept", Basis: "ob-1"})
	if err != nil {
		t.Fatal(err)
	}
	self := replaceSet(l, "L1", func(c *ChangeSet) { c.Approval = &a })
	cs, _ := self.Find("L1")
	long := strings.Repeat("é", MaxReviewNote)
	ok := func(who, conclusion, note string) (SelfReview, error) {
		return ReviewSelfApproval(cs, nil, who, conclusion, note, hours(5))
	}
	r, err := ok("olga", ConclusionAccepted, "")
	if err != nil || r.ChangeID != "L1" || r.By != "olga" || r.Conclusion != ConclusionAccepted || r.Note != "" || !r.At.Equal(hours(5)) {
		t.Fatalf("an ACCEPTED review without a note: %+v, %v", r, err)
	}
	if r, err = ok("olga", ConclusionNotAccepted, "  too loose  "); err != nil || r.Note != "too loose" {
		t.Fatalf("a NOT_ACCEPTED review: %+v, %v", r, err)
	}
	if _, err = ok("olga", ConclusionAccepted, long); err != nil {
		t.Fatalf("a note of exactly %d characters: %v", MaxReviewNote, err)
	}
	for name, c := range map[string]struct {
		who, conclusion, note string
		kind                  error
		say                   string
	}{
		"the approver reviews":        {"ann", ConclusionAccepted, "", ErrReviewer, "have another holder of the review permission review it"},
		"no conclusion":               {"olga", "", "x", ErrConclusion, "ACCEPTED or NOT_ACCEPTED"},
		"another conclusion":          {"olga", "accepted", "x", ErrConclusion, "choose one"},
		"NOT_ACCEPTED without a note": {"olga", ConclusionNotAccepted, "", ErrConclusion, "says why"},
		"NOT_ACCEPTED, a blank note":  {"olga", ConclusionNotAccepted, " \n", ErrConclusion, "says why"},
		"a blank note":                {"olga", ConclusionAccepted, "  ", ErrConclusion, "1 to 2000 characters"},
		"a note too long":             {"olga", ConclusionAccepted, long + "é", ErrConclusion, "shorten it"},
		"a blank reviewer":            {" ", ConclusionAccepted, "", ErrInvalid, "names its reviewer"},
		"a padded reviewer":           {" olga", ConclusionAccepted, "", ErrInvalid, "names its reviewer"},
	} {
		if _, err := ok(c.who, c.conclusion, c.note); err == nil {
			t.Errorf("%s: no refusal", name)
		} else {
			refusedWith(t, name, err, c.kind, c.say)
		}
	}
	if _, err = ReviewSelfApproval(cs, nil, "olga", ConclusionAccepted, "", hours(1)); err == nil {
		t.Error("a review before the approval")
	}
	// once
	done := SelfReview{ChangeID: "L1", By: "pat", At: hours(4), Conclusion: ConclusionNotAccepted, Note: "no"}
	_, err = ReviewSelfApproval(cs, &done, "olga", ConclusionAccepted, "", hours(5))
	refusedWith(t, "a second review", err, ErrReviewed, "reviewed by pat", "NOT_ACCEPTED", "propose a normal change set that restores the earlier values")
	// a change set that was not self-approved: waiting, approved by another person, or approved automatically
	waiting := ledgerWithALoosening(t)
	w, _ := waiting.Find("L1")
	_, err = ReviewSelfApproval(w, nil, "olga", ConclusionAccepted, "", hours(5))
	refusedWith(t, "a change set not approved", err, ErrNotSelfApproved, "exception report")
	other := approve(t, ledgerWithALoosening(t), "L1", "bob", 2)
	o, _ := other.Find("L1")
	_, err = ReviewSelfApproval(o, nil, "olga", ConclusionAccepted, "", hours(5))
	refusedWith(t, "an approval by another person", err, ErrNotSelfApproved)
	auto, _ := propose(t, NewLedger(nil), "T1", "ann", 1, -1, entry(TenantScope, "tst.A", "X0"))
	at, _ := auto.Find("T1")
	_, err = ReviewSelfApproval(at, nil, "olga", ConclusionAccepted, "", hours(5))
	refusedWith(t, "an automatic approval", err, ErrNotSelfApproved)
	// the split of a period's self-approvals
	lines := []SelfApprovedLine{{ChangeID: "L1"}, {ChangeID: "L2"}}
	pending, reviewed := SplitReviews(lines, []SelfReview{{ChangeID: "L2", By: "olga", Conclusion: ConclusionAccepted}, {ChangeID: "L9"}})
	if len(pending) != 1 || pending[0].ChangeID != "L1" || len(reviewed) != 1 || reviewed[0].ChangeID != "L2" || reviewed[0].Review.By != "olga" {
		t.Fatalf("%+v %+v", pending, reviewed)
	}
}
