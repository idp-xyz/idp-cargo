package settings

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)

func hours(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

func le(id string) Scope { return Scope{Kind: KindLegalEntity, ID: id} }

func amount(n int64) *int64 { return &n }

// twoLevels is the chain of a context with a company and legal entities.
func twoLevels(s Scope) (Chain, error) {
	switch {
	case s == TenantScope:
		return Chain{s}, nil
	case s.Kind == KindLegalEntity && s.ID != "":
		return Chain{s, TenantScope}, nil
	}
	return nil, errors.New("not a scope of the test context")
}

// fourLevels adds a branch ("<legal entity>|<branch>") and a transport mode ("<legal entity>|<branch>|<mode>") under
// the legal entity, as a later step of a context could.
func fourLevels(s Scope) (Chain, error) {
	parts := strings.Split(s.ID, "|")
	switch s.Kind {
	case "BRANCH":
		if len(parts) == 2 {
			return Chain{s, le(parts[0]), TenantScope}, nil
		}
	case "MODE":
		if len(parts) == 3 {
			return Chain{s, {Kind: "BRANCH", ID: parts[0] + "|" + parts[1]}, le(parts[0]), TenantScope}, nil
		}
	}
	return twoLevels(s)
}

// testCatalogue has three items: A (three values, default the middle one), B (two values, default the strict one) and P
// (a value with a threshold that may be given for a legal entity only); B B1 goes only with A X0.
func testCatalogue() *Catalogue {
	threshold := &Param{Name: "threshold", TitleZH: "阈值", TitleEN: "threshold", Kinds: []string{KindLegalEntity}, LooserWhenHigher: true}
	both := []string{KindTenant, KindLegalEntity}
	return &Catalogue{Context: "tst", Hierarchy: twoLevels, Items: []Item{
		{Key: "tst.A", TitleZH: "甲", TitleEN: "A", Default: "X1", Kinds: both, Control: true, Explanation: "the first",
			Values: []Value{{ID: "X0", TitleZH: "严", TitleEN: "strict"}, {ID: "X1", TitleZH: "中", TitleEN: "middle"}, {ID: "X2", TitleZH: "松", TitleEN: "loose"}}},
		{Key: "tst.B", TitleZH: "乙", TitleEN: "B", Default: "B0", Kinds: both, Explanation: "the second",
			Values: []Value{{ID: "B0", TitleZH: "严", TitleEN: "strict"}, {ID: "B1", TitleZH: "松", TitleEN: "loose"}}},
		{Key: "tst.P", TitleZH: "丙", TitleEN: "P", Default: "P0", Kinds: both, Control: true, Explanation: "the third",
			Values: []Value{{ID: "P0", TitleZH: "严", TitleEN: "strict"}, {ID: "P1", TitleZH: "阈", TitleEN: "threshold", Param: threshold},
				{ID: "P2", TitleZH: "松", TitleEN: "loose"}}},
	}, Rules: []Rule{func(c Config) error {
		if c["tst.B"].Value == "B1" && c["tst.A"].Value != "X0" {
			return Refuse(ErrRule, "tst.B B1 goes only with tst.A X0; set both in one change set, or keep tst.B at B0")
		}
		return nil
	}}}
}

func entry(scope Scope, key, value string) Entry { return Entry{Scope: scope, Key: key, Value: value} }

// propose proposes entries at hour h by who, effective at hour eff (-1 for at once); it fails the test on a refusal.
func propose(t *testing.T, l *Ledger, id, who string, h, eff int, entries ...Entry) (*Ledger, ChangeSet) {
	t.Helper()
	cs, err := tryPropose(l, id, who, h, eff, entries...)
	if err != nil {
		t.Fatalf("propose %s: %v", id, err)
	}
	return with(l, cs), cs
}

func tryPropose(l *Ledger, id, who string, h, eff int, entries ...Entry) (ChangeSet, error) {
	p := Proposal{ID: id, Entries: entries, Reason: "because " + id, By: who, At: hours(h)}
	if eff >= 0 {
		p.Effective = hours(eff)
	}
	return l.Propose(testCatalogue(), p)
}

func with(l *Ledger, cs ChangeSet) *Ledger { return NewLedger(append(l.Sets(), cs)) }

func approve(t *testing.T, l *Ledger, id, who string, h int) *Ledger {
	t.Helper()
	a, err := l.Approve(testCatalogue(), id, who, hours(h))
	if err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
	return replaceSet(l, id, func(c *ChangeSet) { c.Approval = &a })
}

func cancel(t *testing.T, l *Ledger, id, who string, h int) *Ledger {
	t.Helper()
	x, err := l.Cancel(testCatalogue(), id, who, "no longer wanted", hours(h))
	if err != nil {
		t.Fatalf("cancel %s: %v", id, err)
	}
	return replaceSet(l, id, func(c *ChangeSet) { c.Cancellation = &x })
}

func replaceSet(l *Ledger, id string, f func(*ChangeSet)) *Ledger {
	sets := l.Sets()
	for i := range sets {
		if sets[i].ID == id {
			f(&sets[i])
		}
	}
	return NewLedger(sets)
}

func refusedWith(t *testing.T, what string, err error, want error, mentions ...string) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrRefused) || !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want a refusal of kind %v", what, err, want)
	}
	for _, m := range mentions {
		if !strings.Contains(err.Error(), m) {
			t.Fatalf("%s: the refusal does not say %q: %v", what, m, err)
		}
	}
}

func valueAt(t *testing.T, l *Ledger, scope Scope, key string, at time.Time) (Setting, int) {
	t.Helper()
	cfg, version, err := l.ConfigAt(testCatalogue(), scope, at)
	if err != nil {
		t.Fatal(err)
	}
	return cfg[key], version
}

// The catalogue's own mistakes are found: a key or value that is not an identifier or is repeated, a default that is
// not a value, an item with one value or no scope, a parameter without a scope.
func TestCatalogueCheck(t *testing.T) {
	if err := testCatalogue().Check(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Catalogue){
		"a key that is not an identifier": func(c *Catalogue) { c.Items[0].Key = "A" },
		"a key used twice":                func(c *Catalogue) { c.Items[1].Key = "tst.A" },
		"a default that is not a value":   func(c *Catalogue) { c.Items[0].Default = "X9" },
		"one value only":                  func(c *Catalogue) { c.Items[1].Values = c.Items[1].Values[:1] },
		"a value used twice":              func(c *Catalogue) { c.Items[1].Values[1].ID = "B0" },
		"a value that is not an id":       func(c *Catalogue) { c.Items[1].Values[1].ID = "b-1" },
		"no scope":                        func(c *Catalogue) { c.Items[1].Kinds = nil },
		"no title":                        func(c *Catalogue) { c.Items[1].TitleEN = "" },
		"no explanation":                  func(c *Catalogue) { c.Items[1].Explanation = "" },
		"a parameter without a scope":     func(c *Catalogue) { c.Items[2].Values[1].Param.Kinds = nil },
		"no hierarchy":                    func(c *Catalogue) { c.Hierarchy = nil },
	} {
		c := testCatalogue()
		mutate(c)
		if c.Check() == nil {
			t.Errorf("%s was not found", name)
		}
	}
}

// The catalogue renders as a table with the values from strict to loose and the default in bold.
func TestCatalogueTable(t *testing.T) {
	table := testCatalogue().Table()
	row := ""
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(line, "| tst.A ") {
			row = line
		}
	}
	x0, x1, x2 := strings.Index(row, "X0 ("), strings.Index(row, "**X1 ("), strings.Index(row, "X2 (")
	if row == "" || x0 < 0 || x1 < x0 || x2 < x1 || strings.Count(row, "**") != 2 {
		t.Fatalf("the row of tst.A: %q", row)
	}
	if !strings.Contains(table, "with threshold") || !strings.Contains(table, "| yes |") || !strings.Contains(table, "| no |") {
		t.Fatalf("the table leaves out a parameter or the internal control column:\n%s", table)
	}
	if rank, ok := testCatalogue().Items[0].Rank("X2"); !ok || rank != 2 {
		t.Fatalf("rank of X2 = %d, %v", rank, ok)
	}
}

// A value in force for a scope is its own, else the company's, else the default; and says where it comes from.
func TestResolutionThroughTheChain(t *testing.T) {
	l := NewLedger(nil)
	if s, v := valueAt(t, l, le("LE1"), "tst.A", hours(1)); s.Value != "X1" || s.Set || v != 0 {
		t.Fatalf("nothing set: %+v, version %d", s, v)
	}
	l, _ = propose(t, l, "c1", "ann", 0, -1, entry(TenantScope, "tst.A", "X0"))
	if s, _ := valueAt(t, l, le("LE1"), "tst.A", hours(1)); s.Value != "X0" || s.From != TenantScope || !s.Set {
		t.Fatalf("the company's value is not used by the legal entity: %+v", s)
	}
	l, _ = propose(t, l, "c2", "ann", 1, -1, entry(le("LE1"), "tst.A", "X0"), entry(le("LE1"), "tst.B", "B0"))
	l, _ = propose(t, l, "c3", "ann", 2, -1, entry(le("LE2"), "tst.A", "X0"))
	if s, _ := valueAt(t, l, le("LE3"), "tst.B", hours(3)); s.Value != "B0" || s.Set {
		t.Fatalf("B at LE3 was never set; the default applies and says so: %+v", s)
	}
	if s, _ := valueAt(t, l, le("LE1"), "tst.B", hours(3)); s.Value != "B0" || !s.Set || s.From != le("LE1") {
		t.Fatalf("B at LE1 was set to its strict value, which is not the same as not set: %+v", s)
	}
	for _, c := range []struct {
		scope Scope
		want  Scope
	}{{le("LE1"), le("LE1")}, {le("LE2"), le("LE2")}, {le("LE3"), TenantScope}, {TenantScope, TenantScope}} {
		if s, _ := valueAt(t, l, c.scope, "tst.A", hours(3)); s.From != c.want || s.Value != "X0" {
			t.Fatalf("A at %v comes from %v, want %v", c.scope, s.From, c.want)
		}
	}
	if _, _, err := l.ConfigAt(testCatalogue(), Scope{Kind: "BRANCH", ID: "x"}, hours(3)); !errors.Is(err, ErrScope) {
		t.Fatalf("a scope the context does not know: %v", err)
	}
}

// A level can be added to the chain without changing a stored row: the same entries, read through a chain with a branch
// and a transport mode, resolve for them through the legal entity and the company, and an entry at the new level takes
// precedence for it alone.
func TestALevelCanBeAddedWithoutChangingStoredRows(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "c1", "ann", 0, -1, entry(TenantScope, "tst.A", "X0"), entry(le("LE1"), "tst.P", "P0"))
	rows := l.Sets()
	wide := testCatalogue()
	wide.Hierarchy = fourLevels
	read := func(scope Scope, key string) Setting {
		cfg, _, err := NewLedger(rows).ConfigAt(wide, scope, hours(2))
		if err != nil {
			t.Fatal(err)
		}
		return cfg[key]
	}
	branch, mode := Scope{Kind: "BRANCH", ID: "LE1|SH"}, Scope{Kind: "MODE", ID: "LE1|SH|SEA"}
	if s := read(mode, "tst.A"); s.Value != "X0" || s.From != TenantScope {
		t.Fatalf("a mode takes the company's value: %+v", s)
	}
	if s := read(mode, "tst.P"); s.From != le("LE1") {
		t.Fatalf("a mode takes the legal entity's value: %+v", s)
	}
	// an entry at the new level is stored by its kind and ID like any other, and the other scopes do not see it
	rows = append(rows, ChangeSet{ID: "c2", By: "ann", At: hours(1), Effective: hours(1), Entries: []Entry{entry(branch, "tst.A", "X2")},
		Approval: &Approval{By: "ann", At: hours(1), Version: 2, InForce: hours(1), Auto: true}})
	if s := read(mode, "tst.A"); s.Value != "X2" || s.From != branch {
		t.Fatalf("a mode takes its branch's value: %+v", s)
	}
	if s := read(le("LE1"), "tst.A"); s.Value != "X0" {
		t.Fatalf("the legal entity does not see its branch's value: %+v", s)
	}
}

// Each kind of move: tighter and the same value are approved on the spot by the proposer; looser waits for a second
// person; a higher parameter on the same value is looser and a lower one is not.
func TestLoosening(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "base", "ann", 0, -1, entry(TenantScope, "tst.A", "X1"), entry(TenantScope, "tst.B", "B0"), entry(TenantScope, "tst.P", "P0"))
	for _, c := range []struct {
		name  string
		entry Entry
		loose bool
	}{
		{"A tighter", entry(TenantScope, "tst.A", "X0"), false},
		{"A kept", entry(TenantScope, "tst.A", "X1"), false},
		{"A looser", entry(TenantScope, "tst.A", "X2"), true},
		{"B kept", entry(TenantScope, "tst.B", "B0"), false},
		{"B looser", entry(TenantScope, "tst.B", "B1"), true},
		{"P looser", entry(TenantScope, "tst.P", "P2"), true},
		{"P looser to the threshold", Entry{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount(500)}, true},
		{"A looser than the default at a legal entity", entry(le("LE1"), "tst.A", "X2"), true},
		{"A kept at a legal entity", entry(le("LE1"), "tst.A", "X1"), false},
		{"A tighter at a legal entity", entry(le("LE1"), "tst.A", "X0"), false},
	} {
		entries := []Entry{c.entry}
		if c.entry.Key == "tst.B" && c.loose {
			entries = append(entries, entry(TenantScope, "tst.A", "X0")) // B1 goes with A X0 (a tightening, so B is the one that loosens)
		}
		cs, err := tryPropose(l, "x", "ann", 1, -1, entries...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if cs.Loosening != c.loose || (cs.Approval != nil) == c.loose {
			t.Errorf("%s: loosening %v, approved %v; want loosening %v", c.name, cs.Loosening, cs.Approval != nil, c.loose)
		}
		if cs.Approval != nil && (!cs.Approval.Auto || cs.Approval.By != "ann" || cs.Approval.Version != 2 || !cs.Approval.InForce.Equal(hours(1))) {
			t.Errorf("%s: the approval on the spot: %+v", c.name, cs.Approval)
		}
	}
	// one loosening entry makes the whole change set loosening
	cs, _ := tryPropose(l, "mix", "ann", 1, -1, entry(TenantScope, "tst.A", "X0"), entry(TenantScope, "tst.P", "P2"))
	if !cs.Loosening || cs.Approval != nil {
		t.Fatalf("a change set with one loosening entry: %+v", cs)
	}
}

// A value with a parameter that lets more through is looser when the parameter is higher, tighter when it is lower, and
// kept when it is the same; moving to the loose value or to the strict one is looser or tighter whatever the parameter.
func TestAHigherParameterLoosens(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "p0", "ann", 0, -1, entry(TenantScope, "tst.P", "P2")) // the company's floor is the loosest value
	l = approve(t, l, "p0", "bob", 1)
	l, _ = propose(t, l, "p1", "ann", 2, -1, Entry{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount(1000)})
	for _, c := range []struct {
		name  string
		entry Entry
		loose bool
	}{
		{"higher", Entry{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount(1001)}, true},
		{"lower", Entry{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount(999)}, false},
		{"the same", Entry{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount(1000)}, false},
		{"to the loose value", entry(le("LE1"), "tst.P", "P2"), true},
		{"to the strict value", entry(le("LE1"), "tst.P", "P0"), false},
	} {
		cs, err := tryPropose(l, "x", "ann", 3, -1, c.entry)
		if err != nil || cs.Loosening != c.loose {
			t.Errorf("%s: %+v, %v; want loosening %v", c.name, cs, err, c.loose)
		}
	}
}

// A legal entity may not be set looser than the company's value without a second person, the floor, even when the
// move is not looser than its own value; the company's entries of the same change set count.
func TestTheFloor(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "c1", "ann", 0, -1, entry(TenantScope, "tst.A", "X0")) // tighter than the default: approved at once
	// the legal entity has no value of its own, and the company's is X0: X1 at the legal entity is looser than the
	// floor
	cs, err := tryPropose(l, "x", "ann", 1, -1, entry(le("LE1"), "tst.A", "X1"))
	if err != nil || !cs.Loosening {
		t.Fatalf("a legal entity looser than the company: %+v, %v", cs, err)
	}
	// the legal entity's own value is X2, looser than the floor and approved by a second person; moving it to X1 is
	// tighter than its own value but still looser than the company's, so it is a loosening again
	l, _ = propose(t, l, "c2", "ann", 1, -1, entry(le("LE1"), "tst.A", "X2"))
	l = approve(t, l, "c2", "bob", 2)
	cs, err = tryPropose(l, "c3", "ann", 3, -1, entry(le("LE1"), "tst.A", "X1"))
	if err != nil || !cs.Loosening {
		t.Fatalf("tighter than its own value, looser than the floor: %+v, %v", cs, err)
	}
	// to the company's own value it is not
	if cs, err = tryPropose(l, "c3", "ann", 3, -1, entry(le("LE1"), "tst.A", "X0")); err != nil || cs.Loosening {
		t.Fatalf("to the company's value: %+v, %v", cs, err)
	}
	// the company moves up in the same change set: X1 is then not looser than the floor, but the company's own entry is
	// a loosening itself
	cs, err = tryPropose(l, "c4", "ann", 3, -1, entry(TenantScope, "tst.A", "X1"), entry(le("LE2"), "tst.A", "X1"))
	if err != nil || !cs.Loosening {
		t.Fatalf("the company loosens: %+v, %v", cs, err)
	}
	// the company tightens in the same change set as a legal entity is set between the two: measured against the
	// company's new value it is looser, though it is not looser than the old one
	l2 := NewLedger(nil)
	l2, _ = propose(t, l2, "d1", "ann", 0, -1, entry(TenantScope, "tst.A", "X1"))
	cs, err = tryPropose(l2, "d2", "ann", 1, -1, entry(TenantScope, "tst.A", "X0"), entry(le("LE1"), "tst.A", "X1"))
	if err != nil || !cs.Loosening {
		t.Fatalf("a legal entity looser than the company's new value: %+v, %v", cs, err)
	}
}

// A change set that loosens is approved by another person, takes the next version and takes effect at its effective
// time, or at the approval when that is later; the proposer cannot approve it, and nothing is approved twice.
func TestApproval(t *testing.T) {
	l := NewLedger(nil)
	l, auto := propose(t, l, "c1", "ann", 0, -1, entry(TenantScope, "tst.A", "X0"))
	if auto.Approval == nil || auto.Approval.Version != 1 {
		t.Fatalf("the first approval: %+v", auto.Approval)
	}
	l, waiting := propose(t, l, "c2", "ann", 1, 5, entry(TenantScope, "tst.A", "X2"))
	if waiting.Approval != nil || waiting.Status(hours(9)) != StatusProposed {
		t.Fatalf("a loosening waits: %+v", waiting)
	}
	if s, v := valueAt(t, l, TenantScope, "tst.A", hours(9)); s.Value != "X0" || v != 1 {
		t.Fatalf("a change set waiting for approval is in force: %+v, version %d", s, v)
	}
	_, err := l.Approve(testCatalogue(), "c2", "ann", hours(2))
	refusedWith(t, "the proposer approves", err, ErrSameActor, "another approver")
	_, err = l.Approve(testCatalogue(), "nope", "bob", hours(2))
	refusedWith(t, "an unknown change set", err, ErrUnknownChange)
	_, err = l.Approve(testCatalogue(), "c2", "bob", hours(0))
	refusedWith(t, "an approval before the proposal", err, ErrInvalid)
	_, err = l.Approve(testCatalogue(), "c1", "bob", hours(2))
	refusedWith(t, "an approval of a change set approved on the spot", err, ErrAlreadyApproved)
	l = approve(t, l, "c2", "bob", 2)
	c2, _ := l.Find("c2")
	if a := c2.Approval; a.Version != 2 || a.Auto || a.By != "bob" || !a.InForce.Equal(hours(5)) {
		t.Fatalf("the approval of c2: %+v", a)
	}
	if s, v := valueAt(t, l, TenantScope, "tst.A", hours(4)); s.Value != "X0" || v != 1 {
		t.Fatalf("before its effective time: %+v, version %d", s, v)
	}
	if s, v := valueAt(t, l, TenantScope, "tst.A", hours(5)); s.Value != "X2" || v != 2 {
		t.Fatalf("at its effective time: %+v, version %d", s, v)
	}
	_, err = l.Approve(testCatalogue(), "c2", "carl", hours(3))
	refusedWith(t, "a second approval", err, ErrAlreadyApproved)
	// approved after its effective time it takes effect at the approval, never before
	l, _ = propose(t, l, "c3", "ann", 6, 7, entry(TenantScope, "tst.B", "B0"), entry(le("LE1"), "tst.A", "X0"))
	l, _ = propose(t, l, "c4", "ann", 6, 7, entry(le("LE9"), "tst.P", "P2"))
	l = approve(t, l, "c4", "bob", 10)
	c4, _ := l.Find("c4")
	if !c4.Approval.InForce.Equal(hours(10)) {
		t.Fatalf("approved after its effective time: in force from %v, want the approval's", c4.Approval.InForce)
	}
	if s, _ := valueAt(t, l, le("LE9"), "tst.P", hours(9)); s.Set {
		t.Fatalf("a change made retroactive by a late approval: %+v", s)
	}
}

// A change set not in force can be cancelled with a reason, proposed or approved with a time to come; one in force
// cannot, and a cancelled one is never in force nor approved.
func TestCancellation(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "waiting", "ann", 0, -1, entry(TenantScope, "tst.A", "X2"))
	l, _ = propose(t, l, "now", "ann", 0, -1, entry(TenantScope, "tst.P", "P0"))
	l, _ = propose(t, l, "later", "ann", 0, 10, entry(TenantScope, "tst.B", "B0"))
	if _, err := l.Cancel(testCatalogue(), "now", "bob", "x", hours(1)); !errors.Is(err, ErrInForce) || !strings.Contains(err.Error(), "new change set") {
		t.Fatalf("a change set in force: %v", err)
	}
	if _, err := l.Cancel(testCatalogue(), "later", "bob", "x", hours(10)); !errors.Is(err, ErrInForce) {
		t.Fatalf("at its effective time it is in force: %v", err)
	}
	if _, err := l.Cancel(testCatalogue(), "later", "bob", " ", hours(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a cancellation needs a reason: %v", err)
	}
	if _, err := l.Cancel(testCatalogue(), "later", "bob", "x", hours(-1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a cancellation before the proposal: %v", err)
	}
	if _, err := l.Cancel(testCatalogue(), "nope", "bob", "x", hours(1)); !errors.Is(err, ErrUnknownChange) {
		t.Fatalf("an unknown change set: %v", err)
	}
	l = cancel(t, l, "later", "bob", 9) // approved, its time to come
	l = cancel(t, l, "waiting", "bob", 9)
	if _, err := l.Cancel(testCatalogue(), "waiting", "bob", "x", hours(2)); !errors.Is(err, ErrCancelled) {
		t.Fatalf("a second cancellation: %v", err)
	}
	if _, err := l.Approve(testCatalogue(), "waiting", "bob", hours(2)); !errors.Is(err, ErrCancelled) || !strings.Contains(err.Error(), "propose it again") {
		t.Fatalf("approving a cancelled change set: %v", err)
	}
	if s, _ := valueAt(t, l, TenantScope, "tst.A", hours(20)); s.Set {
		t.Fatalf("a cancelled change set is in force: %+v", s)
	}
	if c, _ := l.Find("later"); c.Status(hours(20)) != StatusCancelled || c.Cancellation.Reason == "" {
		t.Fatalf("status of a cancelled change set: %+v", c)
	}
	if s, v := valueAt(t, l, TenantScope, "tst.B", hours(20)); s.Set || v != 1 {
		t.Fatalf("the cancelled approval makes no version in force, and its number is not used again: %+v, version %d", s, v)
	}
}

// The version in force is the number of the latest approved change set effective at or before the time, and "as of T"
// answers the settings then, whatever order the approvals came in.
func TestVersionsAndAsOf(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "early", "ann", 0, 10, entry(TenantScope, "tst.B", "B0")) // version 1, in force at 10
	l, _ = propose(t, l, "late", "ann", 1, 20, entry(TenantScope, "tst.A", "X0"))  // version 2, in force at 20
	l, _ = propose(t, l, "again", "ann", 2, 30, entry(TenantScope, "tst.A", "X0")) // version 3, in force at 30
	l, _ = propose(t, l, "wait", "ann", 3, 15, entry(TenantScope, "tst.P", "P2"))  // not approved
	for _, c := range []struct {
		at      int
		version int
	}{{5, 0}, {10, 1}, {19, 1}, {20, 2}, {29, 2}, {30, 3}, {100, 3}} {
		if _, v := valueAt(t, l, TenantScope, "tst.A", hours(c.at)); v != c.version {
			t.Errorf("version in force at hour %d = %d, want %d", c.at, v, c.version)
		}
	}
	if s, _ := valueAt(t, l, TenantScope, "tst.A", hours(19)); s.Set {
		t.Errorf("as of hour 19 A was set: %+v", s)
	}
	if s, _ := valueAt(t, l, TenantScope, "tst.A", hours(25)); s.Value != "X0" {
		t.Errorf("as of hour 25: %+v", s)
	}
	if s, _ := valueAt(t, l, TenantScope, "tst.P", hours(100)); s.Value != "P0" {
		t.Errorf("a change set not approved is in force: %+v", s)
	}
	for i, c := range l.Sets() { // versions follow the approvals, from 1 without a gap
		if a := c.Approval; a != nil && a.Version != i+1 && c.ID != "wait" {
			t.Errorf("%s has version %d", c.ID, a.Version)
		}
	}
	// a later entry of the same scope and item replaces the earlier
	l, _ = propose(t, l, "last", "ann", 4, 40, entry(TenantScope, "tst.A", "X0"), entry(le("LE1"), "tst.A", "X0"))
	if _, v := valueAt(t, l, le("LE1"), "tst.A", hours(40)); v != 4 {
		t.Errorf("version in force at hour 40 = %d, want 4", v)
	}
}

// What a proposal is refused for, each naming the item and the way out.
func TestValidation(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "existing", "ann", 0, -1, entry(TenantScope, "tst.P", "P0"))
	good := entry(TenantScope, "tst.A", "X0")
	bad := func(name string, err error, want error, mentions ...string) {
		t.Helper()
		refusedWith(t, name, err, want, mentions...)
	}
	try := func(entries ...Entry) error { _, err := tryPropose(l, "x", "ann", 1, -1, entries...); return err }
	bad("an unknown item", try(entry(TenantScope, "tst.Z", "X0")), ErrUnknownItem, "tst.Z", "catalogue")
	bad("an unknown value", try(entry(TenantScope, "tst.A", "X9")), ErrUnknownValue, "tst.A", "X0, X1, X2")
	bad("a scope of another kind", try(entry(Scope{Kind: "BRANCH", ID: "b"}, "tst.A", "X0")), ErrScope, "tst.A")
	bad("a legal entity without an ID", try(entry(Scope{Kind: KindLegalEntity}, "tst.A", "X0")), ErrScope)
	bad("a parameter that is missing", try(entry(le("LE1"), "tst.P", "P1")), ErrParameter, "tst.P P1", "threshold")
	bad("a parameter that is not asked for", try(Entry{Scope: le("LE1"), Key: "tst.P", Value: "P0", Param: amount(5)}), ErrParameter, "tst.P P0")
	bad("a parameter of nothing", try(Entry{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount(0)}), ErrParameter, "1 to")
	bad("a parameter too large", try(Entry{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount(MaxParam + 1)}), ErrParameter, "1 to")
	bad("a parameter for the company", try(Entry{Scope: TenantScope, Key: "tst.P", Value: "P1", Param: amount(5)}), ErrParameter, "legal entity", "currency")
	bad("a rule across items", try(entry(TenantScope, "tst.B", "B1")), ErrRule, "tst.B B1", "tst.A X0", "one change set")
	bad("a rule across items, the other entry too late", try(entry(TenantScope, "tst.B", "B1"), entry(TenantScope, "tst.A", "X1")), ErrRule, "tst.B B1")
	bad("no entries", try(), ErrInvalid)
	bad("an entry twice", try(good, good), ErrInvalid, "twice")
	_, err := tryPropose(l, "x", "ann", 1, -1, make([]Entry, MaxEntries+1)...)
	bad("too many entries", err, ErrInvalid)
	_, err = l.Propose(testCatalogue(), Proposal{ID: "x", Entries: []Entry{good}, By: "ann", At: hours(1)})
	bad("no reason", err, ErrInvalid, "reason")
	_, err = l.Propose(testCatalogue(), Proposal{ID: "x", Entries: []Entry{good}, Reason: "   ", By: "ann", At: hours(1)})
	bad("a reason of blanks", err, ErrInvalid, "reason")
	if cs, err := l.Propose(testCatalogue(), Proposal{ID: "x", Entries: []Entry{good}, Reason: "  why  ", By: "ann", At: hours(1)}); err != nil || cs.Reason != "why" {
		t.Fatalf("a reason with blanks around it: %+v, %v", cs.Reason, err)
	}
	only := testCatalogue()
	only.Items[1].Kinds = []string{KindTenant}
	_, err = l.Propose(only, Proposal{ID: "x", Entries: []Entry{entry(le("LE1"), "tst.B", "B0")}, Reason: "r", By: "ann", At: hours(1)})
	bad("an item that is not set at a legal entity", err, ErrScope, "tst.B", "tenant")
	_, err = l.Propose(testCatalogue(), Proposal{ID: "x", Entries: []Entry{good}, Reason: strings.Repeat("r", MaxReason+1), By: "ann", At: hours(1)})
	bad("a reason too long", err, ErrInvalid, "reason")
	_, err = l.Propose(testCatalogue(), Proposal{ID: "x", Entries: []Entry{good}, Reason: "r", By: " ", At: hours(1)})
	bad("no actor", err, ErrInvalid, "actor")
	_, err = l.Propose(testCatalogue(), Proposal{ID: "a b", Entries: []Entry{good}, Reason: "r", By: "ann", At: hours(1)})
	bad("an ID that is not one", err, ErrInvalid, "ID")
	_, err = l.Propose(testCatalogue(), Proposal{ID: "existing", Entries: []Entry{good}, Reason: "r", By: "ann", At: hours(1)})
	bad("an ID in use", err, ErrDuplicate, "another ID")
	_, err = tryPropose(l, "x", "ann", 5, 4, good)
	bad("an effective time before now", err, ErrEffective, "new change set")
	// what is allowed together passes: a rule across items kept by the same change set
	if _, err = tryPropose(l, "x", "ann", 1, -1, entry(TenantScope, "tst.B", "B1"), entry(TenantScope, "tst.A", "X0")); err != nil {
		t.Fatalf("B1 with A X0: %v", err)
	}
	// the rule is checked on the configuration, not only on the entries named: A moves away from X0 under B1
	l, _ = propose(t, l, "b1", "ann", 1, -1, entry(TenantScope, "tst.B", "B1"), entry(TenantScope, "tst.A", "X0"))
	l = approve(t, l, "b1", "bob", 2)
	bad("a change that breaks a rule for what is set already", func() error {
		_, err := tryPropose(l, "y", "ann", 2, -1, entry(TenantScope, "tst.A", "X1"))
		return err
	}(),
		ErrRule, "tst.B B1")
}

// A rule across items is checked for every scope a change set touches, a legal entity that inherits a company entry
// included, and for what takes effect later.
func TestValidationOfTheWholeConfiguration(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "c1", "ann", 0, -1, entry(TenantScope, "tst.A", "X0"), entry(TenantScope, "tst.B", "B1"), entry(le("LE1"), "tst.A", "X0"))
	l = approve(t, l, "c1", "bob", 1)
	// the company's A leaves X0, but LE1 keeps its own A X0 and the company's configuration breaks the rule
	_, err := tryPropose(l, "c2", "ann", 2, -1, entry(TenantScope, "tst.A", "X2"))
	refusedWith(t, "the company's configuration", err, ErrRule, "tenant")
	// a legal entity that sets only B inherits the company's A
	l2 := NewLedger(nil)
	l2, _ = propose(t, l2, "d1", "ann", 0, -1, entry(TenantScope, "tst.A", "X1"))
	_, err = tryPropose(l2, "d2", "ann", 1, -1, entry(le("LE1"), "tst.B", "B1"))
	refusedWith(t, "a legal entity inheriting the company's A", err, ErrRule, "legal entity LE1")
	// a change at hour 5 that is fine then is checked against what takes effect later: B1 from hour 10 needs A X0 then
	l3 := NewLedger(nil)
	l3, _ = propose(t, l3, "e1", "ann", 0, 10, entry(TenantScope, "tst.A", "X0"), entry(TenantScope, "tst.B", "B1"))
	_, err = tryPropose(l3, "e2", "ann", 1, 5, entry(TenantScope, "tst.A", "X1"))
	if err != nil {
		t.Fatalf("A X1 at hour 5 is valid until hour 10 and the later change set sets A itself: %v", err)
	}
	l4 := NewLedger(nil)
	l4, _ = propose(t, l4, "f1", "ann", 0, -1, entry(TenantScope, "tst.A", "X0"))
	l4, _ = propose(t, l4, "f2", "ann", 0, 10, entry(TenantScope, "tst.B", "B1")) // loosening: waits
	l4 = approve(t, l4, "f2", "bob", 1)
	_, err = tryPropose(l4, "f3", "ann", 2, 5, entry(TenantScope, "tst.A", "X1")) // would leave B1 without A X0 from hour 10
	refusedWith(t, "an earlier change that breaks a later one", err, ErrRule, "tst.B B1")
	// cancelling a change set that a later one depends on breaks that one
	l5 := NewLedger(nil)
	l5, _ = propose(t, l5, "g1", "ann", 0, 10, entry(TenantScope, "tst.A", "X0"))
	l5, _ = propose(t, l5, "g2", "ann", 1, 20, entry(TenantScope, "tst.B", "B1"))
	l5 = approve(t, l5, "g2", "bob", 2)
	_, err = l5.Cancel(testCatalogue(), "g1", "ann", "wrong", hours(3))
	refusedWith(t, "a cancellation that breaks a later change set", err, ErrRule, "tst.B B1")
}

// A change set approved by its proposer alone, because it only tightens or keeps, never ends up loosening: a proposal,
// an approval or a cancellation that would move what it is measured against is refused, naming the way out.
func TestAnAutoApprovedChangeSetStaysTightening(t *testing.T) {
	// the cancellation of what it was measured against: C loosens to X2 from hour 10 (approved by bob), B sets X1 from
	// hour 20, tighter than C and so approved on the spot; without C, B would loosen X0
	l := NewLedger(nil)
	l, _ = propose(t, l, "base", "ann", 0, -1, entry(TenantScope, "tst.A", "X0"))
	l, _ = propose(t, l, "c", "ann", 1, 10, entry(TenantScope, "tst.A", "X2"))
	l = approve(t, l, "c", "bob", 2)
	l, b := propose(t, l, "b", "ann", 3, 20, entry(TenantScope, "tst.A", "X1"))
	if b.Loosening || b.Approval == nil {
		t.Fatalf("B tightens against C: %+v", b)
	}
	_, err := l.Cancel(testCatalogue(), "c", "ann", "wrong", hours(4))
	refusedWith(t, "cancelling what a tightening was measured against", err, ErrUnapproved, "change set b", "cancel change set b", "ConfigureSettings or of ApproveSettings")
	l = cancel(t, l, "b", "ann", 5) // the way out: cancel B first, then C
	cancel(t, l, "c", "ann", 6)

	// the approval of a change set waiting for a second person, which holds a tightening entry that comes before B's
	// time
	l2 := NewLedger(nil)
	l2, s2 := propose(t, l2, "s2", "ann", 0, 10, entry(TenantScope, "tst.P", "P2"), entry(TenantScope, "tst.A", "X0"))
	if !s2.Loosening {
		t.Fatalf("s2 has a loosening entry: %+v", s2)
	}
	l2, b2 := propose(t, l2, "b", "ann", 1, 20, entry(TenantScope, "tst.A", "X1")) // kept at X1, as s2 is not approved
	if b2.Loosening || b2.Approval == nil {
		t.Fatalf("b keeps X1: %+v", b2)
	}
	_, err = l2.Approve(testCatalogue(), "s2", "bob", hours(2))
	refusedWith(t, "an approval that would take effect before a change set with a lower version", err, ErrOrder, "change set b", "cancel change set b")

	// and the proposal of a tightening that would make a kept value of a later time loosen
	l3 := NewLedger(nil)
	l3, _ = propose(t, l3, "b", "ann", 0, 20, entry(TenantScope, "tst.A", "X1"))
	_, err = tryPropose(l3, "tighten", "ann", 1, 10, entry(TenantScope, "tst.A", "X0"))
	refusedWith(t, "a tightening before a kept value", err, ErrOrder, "change set b", "at or after")
	// from the same hour or later it is the later change set that is measured, and tightening is fine
	if _, err = tryPropose(l3, "tighten", "ann", 1, 25, entry(TenantScope, "tst.A", "X0")); err != nil {
		t.Fatalf("a tightening after B: %v", err)
	}
}

// A parameter lets more through when higher only where the catalogue says so: without it, a higher amount of the same
// value is not a loosening.
func TestAParameterLoosensOnlyWhereTheCatalogueSays(t *testing.T) {
	cat := testCatalogue()
	cat.Items[2].Values[1].Param.LooserWhenHigher = false
	l := NewLedger(nil)
	p := func(id string, h int, amount int64) (ChangeSet, error) {
		return l.Propose(cat, Proposal{ID: id, By: "ann", Reason: "r", At: hours(h),
			Entries: []Entry{{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount64(amount)}}})
	}
	first, err := p("a", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	first.Approval = &Approval{By: "bob", At: hours(1), Version: 1, InForce: hours(1)}
	l = NewLedger([]ChangeSet{first})
	tenant, _ := l.Propose(cat, Proposal{ID: "t", By: "ann", Reason: "r", At: hours(2), Entries: []Entry{entry(TenantScope, "tst.P", "P2")}})
	tenant.Approval = &Approval{By: "bob", At: hours(2), Version: 2, InForce: hours(2)}
	l = NewLedger([]ChangeSet{first, tenant})
	if cs, err := p("b", 3, 2000); err != nil || cs.Loosening {
		t.Fatalf("a higher amount where the catalogue does not say so: %+v, %v", cs, err)
	}
}

func amount64(n int64) *int64 { return &n }

// A change set is measured against what is in force at its own effective time, not against a change set that takes
// effect after it.
func TestALooseningIsMeasuredAtTheEffectiveTime(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "base", "ann", 0, -1, entry(TenantScope, "tst.A", "X0"))
	l, _ = propose(t, l, "later", "ann", 1, 20, entry(TenantScope, "tst.A", "X2"))
	l = approve(t, l, "later", "bob", 2)
	cs, err := tryPropose(l, "x", "ann", 3, 10, entry(TenantScope, "tst.A", "X1"))
	if err != nil || !cs.Loosening {
		t.Fatalf("X1 from hour 10 loosens X0, whatever comes at hour 20: %+v, %v", cs, err)
	}
}

// The ledger keeps its own copy of a proposal's entries and parameters.
func TestAProposalIsNotChangedByItsCaller(t *testing.T) {
	n := int64(700)
	entries := []Entry{{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: &n}}
	l := NewLedger(nil)
	got, err := l.Propose(testCatalogue(), Proposal{ID: "a", By: "ann", Reason: "r", At: hours(0), Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	n, entries[0].Value = 1, "P0"
	if *got.Entries[0].Param != 700 || got.Entries[0].Value != "P1" {
		t.Fatalf("the proposal followed its caller: %+v", got.Entries[0])
	}
}

// Two change sets that take effect at the same time are ordered by their versions, whatever order they are kept in.
func TestChangeSetsInForceTogetherAreOrderedByVersion(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "first", "ann", 0, 10, entry(TenantScope, "tst.A", "X0"))
	l, _ = propose(t, l, "second", "ann", 1, 10, entry(TenantScope, "tst.A", "X2"))
	l = approve(t, l, "second", "bob", 2)
	sets := l.Sets()
	sets[0], sets[1] = sets[1], sets[0]
	if s, v := valueAt(t, NewLedger(sets), TenantScope, "tst.A", hours(10)); s.Value != "X2" || v != 2 {
		t.Fatalf("at the time both are in force: %+v, version %d", s, v)
	}
}

// History is written forwards: a proposal, an approval, a cancellation or a refusal stamped before the latest time
// recorded for the tenant is refused, so that what was answered for a moment in the past never changes.
func TestTimeIsRecordedForwards(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "waits", "ann", 0, -1, entry(TenantScope, "tst.A", "X2"))
	l, _ = propose(t, l, "later", "ann", 10, -1, entry(TenantScope, "tst.P", "P0"))
	cat := testCatalogue()
	if _, err := tryPropose(l, "x", "ann", 5, -1, entry(TenantScope, "tst.B", "B0")); !errors.Is(err, ErrClock) || !strings.Contains(err.Error(), "try the command again") {
		t.Fatalf("a proposal stamped before the latest record: %v", err)
	}
	if _, err := l.Approve(cat, "waits", "bob", hours(5)); !errors.Is(err, ErrClock) {
		t.Fatalf("an approval stamped before the latest record: %v", err)
	}
	if _, err := l.Cancel(cat, "waits", "bob", "r", hours(5)); !errors.Is(err, ErrClock) {
		t.Fatalf("a cancellation stamped before the latest record: %v", err)
	}
	if _, err := l.Refuse("waits", "bob", "r", hours(5)); !errors.Is(err, ErrClock) {
		t.Fatalf("a refusal stamped before the latest record: %v", err)
	}
	if _, err := l.Approve(cat, "waits", "bob", hours(10)); err != nil {
		t.Fatalf("a time equal to the latest record is not before it: %v", err)
	}
	// the latest record may be a cancellation, or a proposal that waits: each counts
	l2 := NewLedger(nil)
	l2, _ = propose(t, l2, "a", "ann", 0, -1, entry(TenantScope, "tst.A", "X2"))
	l2 = cancel(t, l2, "a", "ann", 6)
	if _, err := tryPropose(l2, "x", "ann", 5, -1, entry(TenantScope, "tst.B", "B0")); !errors.Is(err, ErrClock) {
		t.Fatalf("a proposal stamped before the latest cancellation: %v", err)
	}
	l3 := NewLedger(nil)
	l3, _ = propose(t, l3, "w1", "ann", 0, -1, entry(TenantScope, "tst.A", "X2"))
	l3, _ = propose(t, l3, "w2", "ann", 12, -1, entry(TenantScope, "tst.P", "P2")) // waits: no approval, no cancellation
	if _, err := l3.Approve(cat, "w1", "bob", hours(11)); !errors.Is(err, ErrClock) {
		t.Fatalf("an approval stamped before the latest proposal: %v", err)
	}
}

// Change sets take effect in the order of their versions: one that would take effect before an approved change set with
// a lower version that is still to come is refused when it is proposed (approved on the spot) or approved, and the way
// out is to choose a later time or cancel the other.
func TestChangeSetsTakeEffectInTheOrderOfTheirVersions(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "c1", "ann", 0, 2, entry(TenantScope, "tst.A", "X0")) // version 1, in force at hour 2
	_, err := tryPropose(l, "c2", "ann", 1, -1, entry(TenantScope, "tst.B", "B0"))
	refusedWith(t, "a change set in force now, after one in force later", err, ErrOrder, "c1", "version 1", "at or after", "cancel change set c1")
	if _, err = tryPropose(l, "c2", "ann", 1, 2, entry(TenantScope, "tst.B", "B0")); err != nil {
		t.Fatalf("at the same time as the earlier version: %v", err)
	}
	// a loosening waits; its approval, later, is measured the same way
	l, _ = propose(t, l, "wait", "ann", 1, 1, entry(TenantScope, "tst.A", "X2"))
	_, err = l.Approve(testCatalogue(), "wait", "bob", hours(1))
	refusedWith(t, "an approval in force before a change set with a lower version", err, ErrOrder, "c1")
	l = cancel(t, l, "c1", "ann", 1)
	l = approve(t, l, "wait", "bob", 1)
	for _, at := range []int{0, 1, 2, 5} {
		_, v := valueAt(t, l, TenantScope, "tst.A", hours(at))
		if want := map[int]int{0: 0, 1: 2, 2: 2, 5: 2}[at]; v != want {
			t.Errorf("version in force at hour %d = %d, want %d", at, v, want)
		}
	}
	// the version in force never goes backwards in time
	last := 0
	l, _ = propose(t, l, "c3", "ann", 2, 4, entry(TenantScope, "tst.P", "P0"))
	for h := 0; h <= 8; h++ {
		_, v := valueAt(t, l, TenantScope, "tst.A", hours(h))
		if v < last {
			t.Fatalf("the version in force went from %d back to %d at hour %d", last, v, h)
		}
		last = v
	}
}

// A value of a scope below the company that is looser than the company's is marked as an exception, with the change set
// that set it; it stays when the company tightens, and is listed.
func TestAnApprovedExceptionIsMarkedAndStaysWhenTheCompanyTightens(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "co", "ann", 0, -1, entry(TenantScope, "tst.A", "X2"))
	l = approve(t, l, "co", "bob", 1)
	l, _ = propose(t, l, "le", "ann", 2, -1, entry(le("LE1"), "tst.A", "X2"), entry(le("LE1"), "tst.B", "B0"))
	l, _ = propose(t, l, "le2", "ann", 2, -1, entry(le("LE2"), "tst.A", "X0"), entry(le("LE0"), "tst.A", "X2"))
	if s, _ := valueAt(t, l, le("LE1"), "tst.A", hours(3)); s.Exception || s.ChangeID != "le" {
		t.Fatalf("a value not looser than the company's: %+v", s)
	}
	l, _ = propose(t, l, "tight", "ann", 3, -1, entry(TenantScope, "tst.A", "X0"))
	s, _ := valueAt(t, l, le("LE1"), "tst.A", hours(4))
	if s.Value != "X2" || !s.Exception || s.ChangeID != "le" || s.From != le("LE1") {
		t.Fatalf("the legal entity's own value after the company tightened: %+v", s)
	}
	if s, _ := valueAt(t, l, le("LE1"), "tst.B", hours(4)); s.Exception {
		t.Fatalf("a value as strict as the company's: %+v", s)
	}
	if s, _ := valueAt(t, l, TenantScope, "tst.A", hours(4)); s.Exception {
		t.Fatalf("the company's own value: %+v", s)
	}
	if s, _ := valueAt(t, l, le("LE2"), "tst.A", hours(4)); s.Value != "X0" || s.Exception {
		t.Fatalf("a legal entity stricter than the company is not an exception: %+v", s)
	}
	xs := l.Exceptions(testCatalogue(), hours(4))
	if len(xs) != 2 || xs[0] != (Exception{Scope: le("LE0"), Key: "tst.A", Value: "X2", TenantValue: "X0", ChangeID: "le2"}) ||
		xs[1] != (Exception{Scope: le("LE1"), Key: "tst.A", Value: "X2", TenantValue: "X0", ChangeID: "le"}) {
		t.Fatalf("exceptions, in the order of scope: %+v", xs)
	}
	if xs = l.Exceptions(testCatalogue(), hours(2)); len(xs) != 0 {
		t.Fatalf("exceptions before the company tightened: %+v", xs)
	}
}

// The limits of a proposal, each at its boundary: the largest accepted and one more refused.
func TestBoundariesOfAProposal(t *testing.T) {
	l := NewLedger(nil)
	good := entry(TenantScope, "tst.A", "X0")
	at := func(p Proposal) error {
		if p.ID == "" {
			p.ID = "x"
		}
		if p.By == "" {
			p.By = "ann"
		}
		if p.Reason == "" {
			p.Reason = "r"
		}
		p.At = hours(1)
		_, err := l.Propose(testCatalogue(), p)
		return err
	}
	with := func(f func(*Proposal)) error { p := Proposal{Entries: []Entry{good}}; f(&p); return at(p) }
	for _, c := range []struct {
		name        string
		ok, tooMuch func(*Proposal)
	}{
		{"reason", func(p *Proposal) { p.Reason = strings.Repeat("é", MaxReason) }, func(p *Proposal) { p.Reason = strings.Repeat("é", MaxReason+1) }},
		{"actor", func(p *Proposal) { p.By = strings.Repeat("a", 128) }, func(p *Proposal) { p.By = strings.Repeat("a", 129) }},
		{"effective time", func(p *Proposal) { p.Effective = hours(1).Add(366 * 24 * time.Hour) }, func(p *Proposal) { p.Effective = hours(1).Add(366*24*time.Hour + time.Nanosecond) }},
		{"identifier", func(p *Proposal) { p.ID = "a" + strings.Repeat("b", 127) }, func(p *Proposal) { p.ID = "a" + strings.Repeat("b", 128) }},
	} {
		keep := with
		if err := keep(c.ok); err != nil {
			t.Errorf("%s at its limit: %v", c.name, err)
		}
		if err := keep(c.tooMuch); !errors.Is(err, ErrRefused) {
			t.Errorf("%s one over its limit: %v", c.name, err)
		}
	}
	if err := with(func(p *Proposal) { p.Effective = hours(1).Add(MaxAhead + time.Hour) }); !errors.Is(err, ErrEffective) || !strings.Contains(err.Error(), "nearer time") {
		t.Errorf("an effective time too far ahead: %v", err)
	}
	// 64 entries are accepted, 65 refused
	var many []Entry
	for i := 0; i < 22 && len(many) < MaxEntries; i++ {
		for _, k := range []string{"tst.A", "tst.B", "tst.P"} {
			if len(many) < MaxEntries {
				many = append(many, entry(le(fmt.Sprintf("LE%d", i)), k, map[string]string{"tst.A": "X0", "tst.B": "B0", "tst.P": "P0"}[k]))
			}
		}
	}
	if err := with(func(p *Proposal) { p.Entries = many }); err != nil {
		t.Errorf("64 entries: %v", err)
	}
	if err := with(func(p *Proposal) { p.Entries = append(slices.Clone(many), entry(TenantScope, "tst.A", "X0")) }); !errors.Is(err, ErrInvalid) {
		t.Errorf("65 entries: %v", err)
	}
	// a parameter of 1 and of the largest
	for _, n := range []int64{1, MaxParam} {
		if err := with(func(p *Proposal) {
			p.Entries = []Entry{{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount(n)}}
		}); err != nil {
			t.Errorf("a parameter of %d: %v", n, err)
		}
	}
	// a cancellation and a refusal: a reason of the largest length, at the very time of the proposal
	l, _ = propose(t, l, "c1", "ann", 1, -1, entry(TenantScope, "tst.A", "X2"))
	long := strings.Repeat("r", MaxReason)
	if _, err := l.Cancel(testCatalogue(), "c1", "bob", long, hours(1)); err != nil {
		t.Errorf("a cancellation at the proposal time with a reason of %d: %v", MaxReason, err)
	}
	if _, err := l.Cancel(testCatalogue(), "c1", "bob", long+"r", hours(1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("a cancellation reason one over: %v", err)
	}
	if _, err := l.Refuse("c1", "bob", long, hours(1)); err != nil {
		t.Errorf("a refusal at the proposal time with a reason of %d: %v", MaxReason, err)
	}
	if _, err := l.Refuse("c1", "bob", long+"r", hours(1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("a refusal reason one over: %v", err)
	}
	if _, err := l.Cancel(testCatalogue(), "c1", strings.Repeat("b", 128), "r", hours(1)); err != nil {
		t.Errorf("an actor of 128 bytes cancels: %v", err)
	}
	if _, err := l.Cancel(testCatalogue(), "c1", strings.Repeat("b", 129), "r", hours(1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("an actor of 129 bytes cancels: %v", err)
	}
	if _, err := l.Refuse("c1", strings.Repeat("b", 129), "r", hours(1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("an actor of 129 bytes refuses: %v", err)
	}
	if _, err := l.Approve(testCatalogue(), "c1", strings.Repeat("b", 128), hours(1)); err != nil {
		t.Errorf("an actor of 128 bytes approves: %v", err)
	}
	if _, err := l.Approve(testCatalogue(), "c1", strings.Repeat("b", 129), hours(1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("an actor of 129 bytes approves: %v", err)
	}
}

// Actors are compared as they are written: "Ann" is not "ann", because an actor ID is an identifier that identity keeps
// case-sensitive (two principals that differ in case are two accounts), so "Ann" may approve what "ann" proposed.
func TestActorsAreComparedExactly(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "c1", "ann", 0, -1, entry(TenantScope, "tst.A", "X2"))
	if _, err := l.Approve(testCatalogue(), "c1", "Ann", hours(1)); err != nil {
		t.Fatalf("a different actor by case: %v", err)
	}
	if _, err := l.Approve(testCatalogue(), "c1", "ann", hours(1)); !errors.Is(err, ErrSameActor) {
		t.Fatalf("the same actor: %v", err)
	}
}

// An approver refuses a change set that waits for approval, with a reason: it is then refused, never approved, and
// cannot be cancelled again; a change set approved already is not refused but cancelled.
func TestAnApproverRefusesAProposal(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "waits", "ann", 0, -1, entry(TenantScope, "tst.A", "X2"))
	l, _ = propose(t, l, "done", "ann", 0, 5, entry(TenantScope, "tst.P", "P0"))
	x, err := l.Refuse("waits", "bob", " no thanks ", hours(1))
	if err != nil || !x.Refused || x.Reason != "no thanks" {
		t.Fatalf("a refusal: %+v, %v", x, err)
	}
	l = replaceSet(l, "waits", func(c *ChangeSet) { c.Cancellation = &x })
	if c, _ := l.Find("waits"); c.Status(hours(2)) != StatusRefused {
		t.Fatalf("status: %v", c.Status(hours(2)))
	}
	if _, err = l.Approve(testCatalogue(), "waits", "carl", hours(2)); !errors.Is(err, ErrCancelled) || !strings.Contains(err.Error(), "propose it again") {
		t.Fatalf("approving a refused change set: %v", err)
	}
	if _, err = l.Refuse("waits", "bob", "r", hours(2)); !errors.Is(err, ErrCancelled) {
		t.Fatalf("a second refusal: %v", err)
	}
	if _, err = l.Refuse("done", "bob", "r", hours(2)); !errors.Is(err, ErrRefusedChange) || !strings.Contains(err.Error(), "CancelFinSettingsChange") {
		t.Fatalf("refusing an approved change set: %v", err)
	}
	if _, err = l.Refuse("done", "bob", " ", hours(2)); !errors.Is(err, ErrRefusedChange) && !errors.Is(err, ErrInvalid) {
		t.Fatalf("a refusal without a reason: %v", err)
	}
	l2, _ := propose(t, NewLedger(nil), "w", "ann", 0, -1, entry(TenantScope, "tst.A", "X2"))
	if _, err = l2.Refuse("w", "bob", " ", hours(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a refusal without a reason: %v", err)
	}
	if _, err = l2.Refuse("nope", "bob", "r", hours(1)); !errors.Is(err, ErrUnknownChange) || !strings.Contains(err.Error(), "ListFinSettingsChanges") {
		t.Fatalf("an unknown change set: %v", err)
	}
}

// Every refusal says what to do next.
func TestEveryRefusalSaysTheWayOut(t *testing.T) {
	cat := testCatalogue()
	l := NewLedger(nil)
	l, _ = propose(t, l, "waits", "ann", 0, -1, entry(TenantScope, "tst.A", "X2"))
	l, _ = propose(t, l, "done", "ann", 1, -1, entry(TenantScope, "tst.P", "P0"))
	l, _ = propose(t, l, "gone", "ann", 2, -1, entry(TenantScope, "tst.P", "P2"))
	l = cancel(t, l, "gone", "ann", 2)
	good := entry(TenantScope, "tst.A", "X0")
	prop := func(p Proposal) error { _, err := l.Propose(cat, p); return err }
	base := Proposal{ID: "n", Entries: []Entry{good}, Reason: "r", By: "ann", At: hours(3)}
	mod := func(f func(*Proposal)) Proposal { p := base; f(&p); return p }
	for _, c := range []struct {
		name string
		err  error
		way  string
	}{
		{"id", prop(mod(func(p *Proposal) { p.ID = "a b" })), "choose one"},
		{"actor", prop(mod(func(p *Proposal) { p.By = "" })), "ask the platform"},
		{"reason", prop(mod(func(p *Proposal) { p.Reason = "" })), "give it"},
		{"entries", prop(mod(func(p *Proposal) { p.Entries = nil })), "propose the rest"},
		{"duplicate", prop(mod(func(p *Proposal) { p.ID = "done" })), "another ID"},
		{"effective", prop(mod(func(p *Proposal) { p.Effective = hours(1) })), "new change set"},
		{"ahead", prop(mod(func(p *Proposal) { p.Effective = hours(3).Add(MaxAhead + time.Hour) })), "nearer time"},
		{"clock", prop(mod(func(p *Proposal) { p.At = hours(1) })), "try the command again"},
		{"item", prop(mod(func(p *Proposal) { p.Entries = []Entry{entry(TenantScope, "tst.Z", "X0")} })), "read the catalogue"},
		{"value", prop(mod(func(p *Proposal) { p.Entries = []Entry{entry(TenantScope, "tst.A", "X9")} })), "choose one of"},
		{"scope", prop(mod(func(p *Proposal) { p.Entries = []Entry{entry(Scope{Kind: "BRANCH", ID: "b"}, "tst.A", "X0")} })), "choose a scope of the context"},
		{"parameter missing", prop(mod(func(p *Proposal) { p.Entries = []Entry{entry(le("LE1"), "tst.P", "P1")} })), "or choose another value"},
		{"parameter unexpected", prop(mod(func(p *Proposal) {
			p.Entries = []Entry{{Scope: le("LE1"), Key: "tst.P", Value: "P0", Param: amount(1)}}
		})), "leave it out"},
		{"parameter range", prop(mod(func(p *Proposal) {
			p.Entries = []Entry{{Scope: le("LE1"), Key: "tst.P", Value: "P1", Param: amount(0)}}
		})), "1 to"},
		{"parameter scope", prop(mod(func(p *Proposal) {
			p.Entries = []Entry{{Scope: TenantScope, Key: "tst.P", Value: "P1", Param: amount(1)}}
		})), "or choose another value"},
		{"rule", prop(mod(func(p *Proposal) { p.Entries = []Entry{entry(TenantScope, "tst.B", "B1")} })), "one change set"},
		{"twice", prop(mod(func(p *Proposal) { p.Entries = []Entry{good, good} })), "propose one value"},
		{"approve unknown", func() error { _, err := l.Approve(cat, "nope", "bob", hours(3)); return err }(), "ListFinSettingsChanges"},
		{"approve cancelled", func() error { _, err := l.Approve(cat, "gone", "bob", hours(3)); return err }(), "propose it again"},
		{"approve twice", func() error { _, err := l.Approve(cat, "done", "bob", hours(3)); return err }(), "nothing more to do"},
		{"approve same actor", func() error { _, err := l.Approve(cat, "waits", "ann", hours(3)); return err }(), "another approver"},
		{"approve actor", func() error { _, err := l.Approve(cat, "waits", "", hours(3)); return err }(), "try the command again"},
		{"approve clock", func() error { _, err := l.Approve(cat, "waits", "bob", hours(1)); return err }(), "try the command again"},
		{"cancel unknown", func() error { _, err := l.Cancel(cat, "nope", "bob", "r", hours(3)); return err }(), "ListFinSettingsChanges"},
		{"cancel twice", func() error { _, err := l.Cancel(cat, "gone", "bob", "r", hours(3)); return err }(), "nothing more to do"},
		{"cancel in force", func() error { _, err := l.Cancel(cat, "done", "bob", "r", hours(3)); return err }(), "restores the earlier values"},
		{"cancel reason", func() error { _, err := l.Cancel(cat, "waits", "bob", "", hours(3)); return err }(), "give it"},
		{"cancel actor", func() error { _, err := l.Cancel(cat, "waits", "", "r", hours(3)); return err }(), "try the command again"},
		{"refuse approved", func() error { _, err := l.Refuse("done", "bob", "r", hours(3)); return err }(), "CancelFinSettingsChange"},
		{"refuse reason", func() error { _, err := l.Refuse("waits", "bob", "", hours(3)); return err }(), "give it"},
	} {
		if c.err == nil || !errors.Is(c.err, ErrRefused) || !strings.Contains(c.err.Error(), c.way) {
			t.Errorf("%s: %v, want a refusal saying %q", c.name, c.err, c.way)
		}
	}
	// the ones that need a history of their own
	l3 := NewLedger(nil)
	l3, _ = propose(t, l3, "c1", "ann", 0, 5, entry(TenantScope, "tst.A", "X0"))
	if _, err := tryPropose(l3, "c2", "ann", 1, -1, entry(TenantScope, "tst.B", "B0")); err == nil || !strings.Contains(err.Error(), "choose a time at or after") {
		t.Errorf("order: %v", err)
	}
	l4 := NewLedger(nil)
	l4, _ = propose(t, l4, "base", "ann", 0, -1, entry(TenantScope, "tst.A", "X0"))
	l4, _ = propose(t, l4, "c", "ann", 1, 10, entry(TenantScope, "tst.A", "X2"))
	l4 = approve(t, l4, "c", "bob", 2)
	l4, _ = propose(t, l4, "b", "ann", 3, 20, entry(TenantScope, "tst.A", "X1"))
	if _, err := l4.Cancel(cat, "c", "ann", "r", hours(4)); err == nil || !strings.Contains(err.Error(), "propose it again for a second person") {
		t.Errorf("unapproved: %v", err)
	}
	if _, _, err := l.ConfigAt(cat, Scope{Kind: "BRANCH", ID: "x"}, hours(1)); err == nil || !strings.Contains(err.Error(), "choose a scope") {
		t.Errorf("a scope the context does not know: %v", err)
	}
}

// A rule added to a catalogue later does not refuse every command because of a change set that was valid when it was
// approved: only what takes effect from the time of the new command is checked.
func TestANewRuleDoesNotRefuseOldHistory(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "first", "ann", 0, -1, entry(TenantScope, "tst.A", "X2")) // loose: waits
	l = approve(t, l, "first", "bob", 1)
	l, _ = propose(t, l, "second", "ann", 2, -1, entry(TenantScope, "tst.A", "X0"))
	cat := testCatalogue()
	cat.Rules = append(cat.Rules, func(c Config) error {
		if c["tst.A"].Value == "X2" {
			return Refuse(ErrRule, "tst.A X2 is not allowed any more; choose another value")
		}
		return nil
	})
	if _, err := l.Propose(cat, Proposal{ID: "third", By: "ann", Reason: "r", At: hours(5), Entries: []Entry{entry(TenantScope, "tst.P", "P0")}}); err != nil {
		t.Fatalf("a command after a rule was added, with X2 in the history: %v", err)
	}
	if _, err := l.Propose(cat, Proposal{ID: "fourth", By: "ann", Reason: "r", At: hours(5), Entries: []Entry{entry(TenantScope, "tst.A", "X2")}}); !errors.Is(err, ErrRule) {
		t.Fatalf("the new rule for a new value: %v", err)
	}
}

// Approving a change set checks the configuration again: another change set approved since it was proposed may have
// made it invalid (tst.B B1 goes only with tst.A X0).
func TestAnApprovalChecksTheConfigurationAgain(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "a0", "ann", 0, -1, entry(le("LE1"), "tst.A", "X0")) // tighter than the default: on the spot
	l, _ = propose(t, l, "s", "ann", 1, -1, entry(le("LE1"), "tst.B", "B1"))  // loosening: waits; valid with A X0
	l, _ = propose(t, l, "e2", "ann", 2, -1, entry(le("LE1"), "tst.A", "X2")) // loosening: waits too; s is not in force, so it passes
	l = approve(t, l, "e2", "bob", 3)
	_, err := l.Approve(testCatalogue(), "s", "carl", hours(4))
	refusedWith(t, "an approval that makes the configuration invalid", err, ErrRule, "tst.B B1", "legal entity LE1")
}

// The exceptions are listed in the order of scope and then of key, whatever order the entries were stored in.
func TestTheExceptionsAreListedInTheOrderOfScopeAndKey(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "le", "ann", 0, -1, entry(le("LE2"), "tst.P", "P2"), entry(le("LE2"), "tst.A", "X2"), entry(le("LE1"), "tst.P", "P2"), entry(le("LE1"), "tst.A", "X2"))
	l = approve(t, l, "le", "bob", 1)
	xs := l.Exceptions(testCatalogue(), hours(2))
	var got []string
	for _, x := range xs {
		got = append(got, x.Scope.ID+"/"+x.Key)
	}
	if want := "LE1/tst.A LE1/tst.P LE2/tst.A LE2/tst.P"; strings.Join(got, " ") != want {
		t.Fatalf("exceptions: %v, want %s", got, want)
	}
}

// The refusals whose way out is to cancel a change set say who may cancel it, and the cancellation records the role.
func TestTheRefusalsThatSayCancelFirstSayWhoMayCancel(t *testing.T) {
	l := NewLedger(nil)
	l, _ = propose(t, l, "far", "ann", 0, 300, entry(TenantScope, "tst.A", "X0")) // tightens: approved on the spot, in force at hour 300
	_, err := tryPropose(l, "now", "ann", 1, -1, entry(TenantScope, "tst.B", "B0"))
	if !errors.Is(err, ErrOrder) || !strings.Contains(err.Error(), "cancel change set far first") || !strings.Contains(err.Error(), "ConfigureSettings or of ApproveSettings") {
		t.Fatalf("the order rule's way out: %v", err)
	}
	x, err := l.Cancel(testCatalogue(), "far", "bob", "in the way", hours(2))
	if err != nil || x.Role != RoleConfigurer {
		t.Fatalf("a cancellation: %+v, %v", x, err)
	}
	l = replaceSet(l, "far", func(c *ChangeSet) { c.Cancellation = &x })
	l, _ = propose(t, l, "waits", "ann", 3, -1, entry(TenantScope, "tst.A", "X2"))
	if r, err := l.Refuse("waits", "bob", "no", hours(4)); err != nil || r.Role != RoleApprover || !r.Refused {
		t.Fatalf("a refusal: %+v, %v", r, err)
	}
	l, _ = propose(t, l, "loose", "ann", 5, -1, entry(TenantScope, "tst.A", "X2"))
	l = approve(t, l, "loose", "bob", 6)
	_, err = l.Refuse("loose", "bob", "no", hours(7))
	if !errors.Is(err, ErrRefusedChange) || !strings.Contains(err.Error(), "ConfigureSettings or of ApproveSettings") {
		t.Fatalf("the refusal of an approved change set: %v", err)
	}
}
