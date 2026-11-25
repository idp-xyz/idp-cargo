package settings

import (
	"errors"
	"strings"
	"testing"
)

// V2.102: a single administrator approves their own loosening, with a written risk acknowledgement, in a tenant that
// declared one; the ledger's report lists it, the exceptions and the items looser than the standard profile.

// ledgerWithALoosening is a ledger with one loosening change set, L1 (tst.A to X2), proposed by ann and waiting.
func ledgerWithALoosening(t *testing.T) *Ledger {
	t.Helper()
	l, cs := propose(t, NewLedger(nil), "L1", "ann", 1, -1, entry(TenantScope, "tst.A", "X2"))
	if !cs.Loosening || cs.Approval != nil {
		t.Fatalf("the change set: %+v", cs)
	}
	return l
}

func TestAProposerApprovesTheirOwnLooseningOnlyWithTheDeclarationAndAnAcknowledgement(t *testing.T) {
	l := ledgerWithALoosening(t)
	cat := testCatalogue()
	long := strings.Repeat("é", MaxAcknowledgement+1)
	for name, c := range map[string]struct {
		by   string
		own  Own
		kind error
		say  string
	}{
		"without the declaration":                          {"ann", Own{}, ErrSameActor, "have another approver approve it"},
		"without the declaration, with an acknowledgement": {"ann", Own{Acknowledgement: "I accept"}, ErrSameActor, "has not declared a single administrator"},
		"with the declaration, without an acknowledgement": {"ann", Own{Declared: true}, ErrAcknowledgement, "give it, or have another approver"},
		"with a blank acknowledgement":                     {"ann", Own{Declared: true, Acknowledgement: "  \n "}, ErrAcknowledgement, "1 to 2000 characters"},
		"with too long an acknowledgement":                 {"ann", Own{Declared: true, Acknowledgement: long}, ErrAcknowledgement, "shorten it"},
		"another approver with an acknowledgement":         {"bob", Own{Declared: true, Acknowledgement: "I accept"}, ErrAcknowledgement, "leave it out"},
	} {
		a, err := l.ApproveAs(cat, "L1", c.by, hours(2), c.own)
		if !errors.Is(err, c.kind) || !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.say) || a != (Approval{}) {
			t.Errorf("%s: %+v, %v", name, a, err)
		}
	}
	a, err := l.ApproveAs(cat, "L1", "ann", hours(2), Own{Declared: true, Acknowledgement: "  I accept the risk, being the only administrator.  "})
	if err != nil || a.Acknowledgement != "I accept the risk, being the only administrator." || a.Auto || !a.SelfApproved() || a.Version != 1 {
		t.Fatalf("the approval: %+v, %v", a, err)
	}
	exact := strings.Repeat("é", MaxAcknowledgement)
	if a, err = l.ApproveAs(cat, "L1", "ann", hours(2), Own{Declared: true, Acknowledgement: exact}); err != nil || a.Acknowledgement != exact {
		t.Fatalf("an acknowledgement of exactly %d characters: %v", MaxAcknowledgement, err)
	}
	// another approver as before; the declaration changes nothing for them
	if a, err = l.ApproveAs(cat, "L1", "bob", hours(2), Own{Declared: true}); err != nil || a.SelfApproved() || a.Acknowledgement != "" {
		t.Fatalf("another approver: %+v, %v", a, err)
	}
	// Approve is ApproveAs with nothing declared: the proposer is refused as before V2.102
	if _, err = l.Approve(cat, "L1", "ann", hours(2)); !errors.Is(err, ErrSameActor) || strings.Contains(err.Error(), "single administrator") {
		t.Fatalf("Approve by the proposer: %v", err)
	}
}

func TestTheReportListsExceptionsLooserItemsSelfApprovalsAndChangers(t *testing.T) {
	cat := testCatalogue()
	l := NewLedger(nil)
	// ann proposes tst.A at the company looser (X2) and a tighter entity value; she approves the loosening herself as the single administrator
	l, _ = propose(t, l, "L1", "ann", 1, -1, entry(TenantScope, "tst.A", "X2"))
	a, err := l.ApproveAs(cat, "L1", "ann", hours(2), Own{Declared: true, Acknowledgement: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	l = replaceSet(l, "L1", func(c *ChangeSet) { c.Approval = &a })
	// the company tightens tst.A to X0 and an entity keeps X2: an approved exception, by bob and carl
	l, _ = propose(t, l, "L2", "bob", 3, -1, entry(le("LE1"), "tst.A", "X2"), entry(TenantScope, "tst.A", "X0"))
	l = approve(t, l, "L2", "carl", 4)
	r := l.ExceptionReport(cat, hours(10), hours(0), hours(24))
	if len(r.Exceptions) != 1 || r.Exceptions[0].Key != "tst.A" || r.Exceptions[0].Scope != le("LE1") || r.Exceptions[0].TenantValue != "X0" ||
		r.Exceptions[0].ProposedBy != "bob" || r.Exceptions[0].ApprovedBy != "carl" {
		t.Fatalf("exceptions: %+v", r.Exceptions)
	}
	if len(r.Looser) != 1 || r.Looser[0].Scope != le("LE1") || r.Looser[0].Value != "X2" || r.Looser[0].Standard != "X1" || r.Looser[0].ProposedBy != "bob" || r.Looser[0].ApprovedBy != "carl" {
		t.Fatalf("looser than standard: %+v", r.Looser)
	}
	if len(r.SelfApproved) != 1 || r.SelfApproved[0].ChangeID != "L1" || r.SelfApproved[0].By != "ann" || r.SelfApproved[0].Acknowledgement != "accepted" ||
		len(r.SelfApproved[0].Entries) != 1 {
		t.Fatalf("self approved: %+v", r.SelfApproved)
	}
	want := []Changer{{"ann", 1, 1}, {"bob", 1, 0}, {"carl", 0, 1}}
	// L2 was approved by carl and, being proposed by bob, bob proposed one: ann proposed and approved L1
	if len(r.Changers) != 3 || r.Changers[0] != want[0] || r.Changers[1] != want[1] || r.Changers[2] != want[2] {
		t.Fatalf("changers: %+v", r.Changers)
	}
	// a period that holds none of it lists no self approval and no changer; the state as of a time is the state then
	r = l.ExceptionReport(cat, hours(10), hours(100), hours(101))
	if len(r.SelfApproved) != 0 || len(r.Changers) != 0 || len(r.Exceptions) != 1 {
		t.Fatalf("a later period: %+v", r)
	}
	if r = l.ExceptionReport(cat, hours(2), hours(0), hours(1)); len(r.Looser) != 1 || r.Looser[0].Scope != TenantScope || len(r.Exceptions) != 0 {
		t.Fatalf("as of hour 2: %+v", r)
	}
	// the end of the period is excluded and its start included
	if r = l.ExceptionReport(cat, hours(10), hours(2), hours(3)); len(r.SelfApproved) != 1 {
		t.Fatalf("the start is included: %+v", r.SelfApproved)
	}
	if r = l.ExceptionReport(cat, hours(10), hours(0), hours(2)); len(r.SelfApproved) != 0 {
		t.Fatalf("the end is excluded: %+v", r.SelfApproved)
	}
}

func TestACatalogueVersionChangesWithWhatAConfigurationDependsOn(t *testing.T) {
	v := testCatalogue().Version()
	if v == "" || len(v) != 16 || v != testCatalogue().Version() {
		t.Fatalf("version %q", v)
	}
	for name, edit := range map[string]func(*Catalogue){
		"a default": func(c *Catalogue) { c.Items[0].Default = "X2" },
		"the order": func(c *Catalogue) {
			c.Items[0].Values[0], c.Items[0].Values[1] = c.Items[0].Values[1], c.Items[0].Values[0]
		},
		"a value":          func(c *Catalogue) { c.Items[1].Values[1].ID = "B2" },
		"a scope":          func(c *Catalogue) { c.Items[1].Kinds = []string{KindTenant} },
		"a control":        func(c *Catalogue) { c.Items[1].Control = true },
		"a parameter":      func(c *Catalogue) { c.Items[2].Values[1].Param.LooserWhenHigher = false },
		"an item":          func(c *Catalogue) { c.Items = c.Items[:2] },
		"the context name": func(c *Catalogue) { c.Context = "other" },
	} {
		c := testCatalogue()
		edit(c)
		if c.Version() == v {
			t.Errorf("%s does not change the version", name)
		}
	}
	c := testCatalogue()
	c.Items[0].TitleEN, c.Items[0].Explanation = "renamed", "reworded"
	if c.Version() != v {
		t.Error("a title or an explanation changes the version")
	}
}

// An entry set to the standard value (the default) is not looser than the standard, and one looser is.
func TestTheReportLeavesOutAnEntryAtTheStandardValue(t *testing.T) {
	l, _ := propose(t, NewLedger(nil), "S1", "ann", 1, -1, entry(TenantScope, "tst.A", "X1"), entry(TenantScope, "tst.B", "B0"))
	if r := l.ExceptionReport(testCatalogue(), hours(5), hours(0), hours(9)); len(r.Looser) != 0 {
		t.Fatalf("looser than standard: %+v", r.Looser)
	}
}
