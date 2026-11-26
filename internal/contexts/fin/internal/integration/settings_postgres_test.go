//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	finpg "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// settingsAuth grants the three actions of the settings (the review of a self-approval since V2.102 batch F) to the actors named, by
// name, as the platform does, and every other action of FIN to everyone but the "intruder".
type settingsAuth struct{ grants map[string][]string }

func (a settingsAuth) AuthorizeFinAction(_ context.Context, _, actor, action, _ string) error {
	switch action {
	case api.ActionConfigureSettings, api.ActionApproveSettings, api.ActionReviewSettingsExceptions:
		if !slices.Contains(a.grants[actor], action) {
			return fmt.Errorf("%s may not %s", actor, action)
		}
	default:
		if actor == "intruder" {
			return errors.New("forbidden")
		}
	}
	return nil
}

// settingsWorld is a tenant with two legal entities, LE in CNY and LE2 in USD, and the module with the settings'
// permissions: ann and eve configure, bob and carl approve, dan does both, and the others do neither.
type settingsWorld struct {
	*fixture
	t      *testing.T
	module *finmodule.Module
	n      int
	// released is the time just before the lock that queuedSettings held was released
	released time.Time
}

// settingsModule is FIN's module with the settings' permissions of the world below.
func settingsModule(t *testing.T, f *fixture, opts ...finmodule.Option) *finmodule.Module {
	t.Helper()
	m, err := finmodule.New(f.db, f.w, f.w, f.w, f.w, f.w, settingsAuth{grants: map[string][]string{
		"ann": {api.ActionConfigureSettings}, "eve": {api.ActionConfigureSettings},
		"bob": {api.ActionApproveSettings}, "carl": {api.ActionApproveSettings},
		"dan": {api.ActionConfigureSettings, api.ActionApproveSettings, api.ActionReviewSettingsExceptions}, "frank": {api.ActionConfigureSettings, api.ActionApproveSettings},
		"rita": {api.ActionReviewSettingsExceptions}, "pat": {api.ActionReviewSettingsExceptions}}}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newSettingsWorld(t *testing.T) *settingsWorld {
	t.Helper()
	f := newFixture(t)
	m := settingsModule(t, f)
	s := &settingsWorld{fixture: f, t: t, module: m}
	for _, p := range []struct{ le, currency string }{{"LE", "CNY"}, {"LE2", "USD"}} {
		if _, err := m.OpenFinancePolicy(context.Background(), api.OpenFinancePolicy{CommandID: "policy-" + p.le, TenantID: f.tenant, ActorID: "acct-1",
			LegalEntityID: p.le, FunctionalCurrency: kernel.CurrencyCode(p.currency), BookCode: "MAIN", OccurredAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *settingsWorld) command() string {
	s.n++
	return fmt.Sprintf("cmd-%d", s.n)
}

func tenantEntry(key, value string) api.FinSettingEntry {
	return api.FinSettingEntry{ScopeKind: "TENANT", Key: key, Value: value}
}

func leEntry(le, key, value string) api.FinSettingEntry {
	return api.FinSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: le, Key: key, Value: value}
}

func (s *settingsWorld) propose(id, actor string, effective time.Time, entries ...api.FinSettingEntry) (api.Receipt, error) {
	return s.module.ProposeFinSettingsChange(context.Background(), api.ProposeFinSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: actor,
		ChangeID: id, Entries: entries, Reason: "because " + id, EffectiveAt: effective})
}

func (s *settingsWorld) approve(id, actor string) (api.Receipt, error) {
	return s.module.ApproveFinSettingsChange(context.Background(), api.ApproveFinSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: actor, ChangeID: id})
}

func (s *settingsWorld) cancel(id, actor string) (api.Receipt, error) {
	return s.module.CancelFinSettingsChange(context.Background(), api.CancelFinSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: actor,
		ChangeID: id, Reason: "not wanted"})
}

func (s *settingsWorld) must(r api.Receipt, err error) api.Receipt {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
	return r
}

func (s *settingsWorld) refused(what string, err error, kind error, contains ...string) {
	s.t.Helper()
	if !errors.Is(err, kind) {
		s.t.Fatalf("%s: %v, want %v", what, err, kind)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			s.t.Fatalf("%s: the refusal does not say %q: %v", what, c, err)
		}
	}
}

// settings answers a scope's settings by key, with the version in force.
func (s *settingsWorld) settings(kind, id string, asOf time.Time) (map[string]api.FinSetting, int) {
	s.t.Helper()
	got, err := s.module.GetFinSettings(context.Background(), api.GetFinSettings{TenantID: s.tenant, ScopeKind: kind, ScopeID: id, AsOf: asOf})
	if err != nil {
		s.t.Fatal(err)
	}
	out := map[string]api.FinSetting{}
	for _, it := range got.Items {
		out[it.Key] = it
	}
	return out, got.Version
}

func (s *settingsWorld) changes() map[string]api.FinSettingsChange {
	s.t.Helper()
	list, err := s.module.ListFinSettingsChanges(context.Background(), api.ListFinSettingsChanges{TenantID: s.tenant})
	if err != nil {
		s.t.Fatal(err)
	}
	out := map[string]api.FinSettingsChange{}
	for _, c := range list {
		out[c.ChangeID] = c
	}
	return out
}

func (s *settingsWorld) count(table string) int {
	s.t.Helper()
	var n int
	err := s.within(s.t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM cargo_fin.`+table+` WHERE tenant_id=$1`, s.tenant).Scan(&n)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return n
}

// Propose, approve, cancel, the history and the settings as of a time, through the real commands: a change that only
// tightens is in force at once and makes a version; one that loosens waits for another actor; one in the future shows
// from its time; a change not in force can be cancelled and one in force cannot; the values are resolved through the
// legal entity, the company and the default, with the scope each comes from.
func TestFinSettingsProposeApproveCancelOnPostgres(t *testing.T) {
	s := newSettingsWorld(t)
	before := time.Now().UTC().Add(-time.Hour)
	cat, err := s.module.GetFinSettingsCatalogue(context.Background(), api.GetFinSettingsCatalogue{})
	if err != nil || len(cat.Items) != 8 || cat.Items[0].Key != "fin.F1" || cat.Items[0].Default != "NOT_ALLOWED" || len(cat.Items[0].Values) != 3 ||
		cat.Items[0].Values[1].Parameter != "threshold" || !cat.Items[0].InternalControl || !strings.Contains(cat.Table, "| fin.F9 |") {
		t.Fatalf("the catalogue: %+v, %v", cat, err)
	}
	if got, version := s.settings("LEGAL_ENTITY", "LE", time.Time{}); version != 0 || len(got) != 8 || got["fin.F7"].Value != "DOES_NOT_BLOCK" || !got["fin.F7"].Default ||
		got["fin.F1"].Value != "NOT_ALLOWED" || got["fin.F1"].Threshold != nil {
		t.Fatalf("nothing is set: %+v, version %d", got, version)
	}

	// the company tightens F7: approved on the spot by its proposer, which is version 1
	r := s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	if r.SubjectID != "c1" || r.NewRevision != 2 || r.Version != 1 {
		t.Fatalf("a tightening: %+v", r)
	}
	got, version := s.settings("LEGAL_ENTITY", "LE", time.Time{})
	if f7 := got["fin.F7"]; version != 1 || f7.Value != "BLOCKS" || f7.FromScopeKind != "TENANT" || f7.Default {
		t.Fatalf("a legal entity takes the company's value: %+v, version %d", f7, version)
	}
	// a legal entity looser than the company's value waits for a second person
	r = s.must(s.propose("c2", "ann", time.Time{}, leEntry("LE", "fin.F7", "DOES_NOT_BLOCK")))
	if r.NewRevision != 1 || r.Version != 0 {
		t.Fatalf("a loosening: %+v", r)
	}
	if got, version = s.settings("LEGAL_ENTITY", "LE", time.Time{}); got["fin.F7"].Value != "BLOCKS" || version != 1 {
		t.Fatalf("a change waiting for approval is in force: %+v, version %d", got["fin.F7"], version)
	}
	s.refused("a proposer with the approve permission approves", func() error { _, err := s.approve("c2", "ann"); return err }(), api.ErrUnauthorized)
	r2, err := s.propose("c2b", "dan", time.Time{}, leEntry("LE", "fin.F7", "DOES_NOT_BLOCK"))
	if err != nil || r2.Version != 0 {
		t.Fatalf("a second proposal: %+v, %v", r2, err)
	}
	s.refused("the proposer with both permissions approves", func() error { _, err := s.approve("c2b", "dan"); return err }(), api.ErrRejected,
		"SETTINGS_SAME_ACTOR", "another approver")
	s.refused("someone who may not approve", func() error { _, err := s.approve("c2", "eve"); return err }(), api.ErrUnauthorized)
	s.refused("an unknown change set", func() error { _, err := s.approve("nope", "bob"); return err }(), api.ErrNotFound)
	r = s.must(s.approve("c2", "bob"))
	if r.Version != 2 || r.NewRevision != 2 {
		t.Fatalf("the approval: %+v", r)
	}
	got, version = s.settings("LEGAL_ENTITY", "LE", time.Time{})
	if f7 := got["fin.F7"]; f7.Value != "DOES_NOT_BLOCK" || f7.FromScopeKind != "LEGAL_ENTITY" || f7.FromScopeID != "LE" || version != 2 {
		t.Fatalf("the approved change: %+v, version %d", f7, version)
	}
	if got, _ = s.settings("LEGAL_ENTITY", "LE2", time.Time{}); got["fin.F7"].Value != "BLOCKS" || got["fin.F7"].FromScopeKind != "TENANT" {
		t.Fatalf("another legal entity keeps the company's value: %+v", got["fin.F7"])
	}
	s.refused("a second approval", func() error { _, err := s.approve("c2", "carl"); return err }(), api.ErrRejected, "SETTINGS_ALREADY_APPROVED")
	// as of before anything, and the time of the read when none is given
	if g, err := s.module.GetFinSettings(context.Background(), api.GetFinSettings{TenantID: s.tenant, ScopeKind: "TENANT", AsOf: before}); err != nil || !g.AsOf.Equal(before.Truncate(time.Microsecond)) {
		t.Fatalf("the time of a read as of a time: %+v, %v", g.AsOf, err)
	}
	if g, err := s.module.GetFinSettings(context.Background(), api.GetFinSettings{TenantID: s.tenant, ScopeKind: "TENANT"}); err != nil || g.AsOf.IsZero() {
		t.Fatalf("the time of a read as of now: %+v, %v", g.AsOf, err)
	}
	if got, version = s.settings("TENANT", "", before); version != 0 || got["fin.F7"].Value != "DOES_NOT_BLOCK" || !got["fin.F7"].Default {
		t.Fatalf("as of before any change: %+v, version %d", got["fin.F7"], version)
	}

	// F1's threshold: an amount in the legal entity's functional currency, for a legal entity
	usd5000 := &kernel.Money{Currency: "USD", Minor: 500_000}
	cny5000 := &kernel.Money{Currency: "CNY", Minor: 500_000}
	f1 := func(le string, amount *kernel.Money) api.FinSettingEntry {
		return api.FinSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: le, Key: "fin.F1", Value: "NOT_ABOVE_THRESHOLD", Threshold: amount}
	}
	s.refused("a threshold in another currency", func() error { _, err := s.propose("t1", "ann", time.Time{}, f1("LE", usd5000)); return err }(),
		api.ErrRejected, "CNY", "USD")
	s.refused("no threshold", func() error { _, err := s.propose("t1", "ann", time.Time{}, f1("LE", nil)); return err }(), api.ErrRejected, "threshold")
	s.refused("a threshold for the company", func() error {
		_, err := s.propose("t1", "ann", time.Time{}, api.FinSettingEntry{ScopeKind: "TENANT", Key: "fin.F1", Value: "NOT_ABOVE_THRESHOLD", Threshold: cny5000})
		return err
	}(), api.ErrRejected, "legal entity")
	s.refused("a legal entity without a policy", func() error {
		_, err := s.propose("t1", "ann", time.Time{}, leEntry("LE-X", "fin.F7", "BLOCKS"))
		return err
	}(),
		api.ErrRejected, "LE-X")
	s.refused("an unknown item", func() error { _, err := s.propose("t1", "ann", time.Time{}, leEntry("LE", "fin.F5", "X")); return err }(), api.ErrRejected, "fin.F5")
	s.refused("an unknown value", func() error {
		_, err := s.propose("t1", "ann", time.Time{}, leEntry("LE", "fin.F7", "MAYBE"))
		return err
	}(), api.ErrRejected, "BLOCKS")
	s.refused("F8 without F3", func() error {
		_, err := s.propose("t1", "ann", time.Time{}, leEntry("LE", "fin.F8", "DEFAULT_TO_REMAINING"))
		return err
	}(),
		api.ErrRejected, "fin.F3", "one change set")
	s.refused("a past effective time", func() error { _, err := s.propose("t1", "ann", before, leEntry("LE", "fin.F7", "BLOCKS")); return err }(), api.ErrRejected,
		"SETTINGS_EFFECTIVE_TIME")
	s.refused("no permission to configure", func() error {
		_, err := s.propose("t1", "bob", time.Time{}, leEntry("LE", "fin.F7", "BLOCKS"))
		return err
	}(), api.ErrUnauthorized)
	s.refused("a change set ID in use", func() error {
		_, err := s.propose("c1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS"))
		return err
	}(), api.ErrConflict)
	s.must(s.propose("t2", "ann", time.Time{}, f1("LE", cny5000)))
	s.must(s.approve("t2", "carl"))
	if got, _ = s.settings("LEGAL_ENTITY", "LE", time.Time{}); got["fin.F1"].Value != "NOT_ABOVE_THRESHOLD" || got["fin.F1"].Threshold == nil ||
		*got["fin.F1"].Threshold != *cny5000 {
		t.Fatalf("F1 with its threshold: %+v", got["fin.F1"])
	}
	if got, _ = s.settings("LEGAL_ENTITY", "LE2", time.Time{}); got["fin.F1"].Value != "NOT_ALLOWED" || got["fin.F1"].Threshold != nil {
		t.Fatalf("the other legal entity: %+v", got["fin.F1"])
	}
	// a higher threshold loosens; a lower one is tighter than the legal entity's own value but, the company's F1 being
	// NOT_ALLOWED, still looser than the company's value, which is the floor, so it waits for approval too
	higher := &kernel.Money{Currency: "CNY", Minor: 600_000}
	lower := &kernel.Money{Currency: "CNY", Minor: 400_000}
	if r = s.must(s.propose("t3", "ann", time.Time{}, f1("LE", higher))); r.Version != 0 {
		t.Fatalf("a higher threshold: %+v", r)
	}
	if r = s.must(s.propose("t4", "ann", time.Time{}, f1("LE", lower))); r.Version != 0 || !s.changes()["t4"].Loosening {
		t.Fatalf("a lower threshold under the company's floor: %+v", r)
	}

	// in the future: a tightening approved on the spot shows from its time
	future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	r = s.must(s.propose("f1", "ann", future, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER")))
	future1 := r.Version
	if future1 == 0 {
		t.Fatalf("a future tightening: %+v", r)
	}
	if got, _ = s.settings("TENANT", "", time.Time{}); got["fin.F2"].Value != "SUBMITTER_ONLY" {
		t.Fatalf("a change before its time: %+v", got["fin.F2"])
	}
	if got, v := s.settings("LEGAL_ENTITY", "LE", future.Add(time.Minute)); got["fin.F2"].Value != "SUBMITTER_AND_PREVIEWER" || v != future1 {
		t.Fatalf("as of after its time: %+v, version %d", got["fin.F2"], v)
	}
	if c := s.changes()["f1"]; c.Status != "APPROVED" || !c.AutoApproved || c.Loosening || !c.EffectiveAt.Equal(future) || !c.InForceAt.Equal(future) {
		t.Fatalf("history of a future change: %+v", c)
	}
	// cancelled before it is in force: with a reason, by anyone who configures or approves; its version is not reused and it never
	// counts
	s.refused("someone who neither configures nor approves cancels", func() error { _, err := s.cancel("f1", "zed"); return err }(), api.ErrUnauthorized)
	if r = s.must(s.cancel("f1", "eve")); r.NewRevision != 3 {
		t.Fatalf("the cancellation of an approved change: %+v", r)
	}
	if got, v := s.settings("TENANT", "", future.Add(time.Minute)); got["fin.F2"].Value != "SUBMITTER_ONLY" || v >= future1 {
		t.Fatalf("a cancelled change is in force: %+v, version %d", got["fin.F2"], v)
	}
	s.refused("a second cancellation", func() error { _, err := s.cancel("f1", "eve"); return err }(), api.ErrRejected, "SETTINGS_CANCELLED")
	// proposed and cancelled; it can then no longer be approved
	s.must(s.propose("w1", "ann", time.Time{}, leEntry("LE2", "fin.F6", "INCLUDED_MARKED")))
	if r = s.must(s.cancel("w1", "dan")); r.NewRevision != 2 {
		t.Fatalf("the cancellation of a proposal: %+v", r)
	}
	s.refused("approving a cancelled change set", func() error { _, err := s.approve("w1", "bob"); return err }(), api.ErrRejected, "propose it again")
	// one in force cannot be cancelled: a rollback is a new change set
	s.refused("cancelling a change in force", func() error { _, err := s.cancel("c1", "ann"); return err }(), api.ErrRejected, "SETTINGS_IN_FORCE", "new change set")

	// the history, with who and why and when
	h := s.changes()
	if len(h) != 8 {
		t.Fatalf("%d change sets in the history: %+v", len(h), h)
	}
	c2 := h["c2"]
	if c2.Status != "IN_FORCE" || c2.ProposedBy != "ann" || c2.ApprovedBy != "bob" || c2.AutoApproved || !c2.Loosening || c2.Version != 2 || c2.Reason != "because c2" ||
		c2.ProposedAt.IsZero() || !c2.ApprovedAt.After(c2.ProposedAt) || len(c2.Entries) != 1 || c2.Entries[0].ScopeID != "LE" || c2.Entries[0].Key != "fin.F7" {
		t.Fatalf("history of c2: %+v", c2)
	}
	if c := h["c1"]; c.Status != "IN_FORCE" || !c.AutoApproved || c.ApprovedBy != "ann" || c.Version != 1 || c.Loosening {
		t.Fatalf("history of c1: %+v", c)
	}
	if c := h["f1"]; c.Status != "CANCELLED" || c.CancelledBy != "eve" || c.CancelReason != "not wanted" || c.CancelledAt.IsZero() {
		t.Fatalf("history of f1: %+v", c)
	}
	if c := h["c2b"]; c.Status != "PROPOSED" || c.ApprovedBy != "" || c.Version != 0 {
		t.Fatalf("history of c2b: %+v", c)
	}
	if c := h["t2"]; c.Entries[0].ThresholdMinor == nil || *c.Entries[0].ThresholdMinor != 500_000 {
		t.Fatalf("history of t2: %+v", c)
	}
	// every change set of the tenant is in the order proposed
	list, _ := s.module.ListFinSettingsChanges(context.Background(), api.ListFinSettingsChanges{TenantID: s.tenant})
	if list[0].ChangeID != "c1" || list[1].ChangeID != "c2" {
		t.Fatalf("the order of the history: %v", list)
	}
	// another tenant sees none of it
	other, err := s.module.ListFinSettingsChanges(context.Background(), api.ListFinSettingsChanges{TenantID: s.tenant + "-other"})
	if err != nil || len(other) != 0 {
		t.Fatalf("another tenant's history: %+v, %v", other, err)
	}
}

// Each command answers its receipt again when it is replayed, writes nothing twice, and refuses the same command id
// with another payload; a proposal, an approval and a cancellation each have one event, and a proposal approved on the
// spot has both.
func TestFinSettingsReplaysAndEventsOnPostgres(t *testing.T) {
	s := newSettingsWorld(t)
	ctx := context.Background()
	baseline := len(s.outboxTypes(t))
	propose := api.ProposeFinSettingsChange{CommandID: "p-1", TenantID: s.tenant, ActorID: "ann", ChangeID: "c1", Reason: "tighten",
		Entries: []api.FinSettingEntry{tenantEntry("fin.F7", "BLOCKS"), tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER"), tenantEntry("fin.F6", "SEPARATE")}}
	r := s.must(s.module.ProposeFinSettingsChange(ctx, propose))
	if again, err := s.module.ProposeFinSettingsChange(ctx, propose); err != nil || again != r {
		t.Fatalf("replay of the proposal: %+v, %v", again, err)
	}
	if c := s.changes()["c1"]; len(c.Entries) != 3 || c.Entries[0].Key != "fin.F7" || c.Entries[1].Key != "fin.F2" || c.Entries[2].Key != "fin.F6" {
		t.Fatalf("the entries are not in the order proposed: %+v", c.Entries)
	}
	other := propose
	other.Reason = "something else"
	if _, err := s.module.ProposeFinSettingsChange(ctx, other); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the command id with another payload: %v", err)
	}
	loosen := api.ProposeFinSettingsChange{CommandID: "p-2", TenantID: s.tenant, ActorID: "ann", ChangeID: "c2", Reason: "loosen",
		Entries: []api.FinSettingEntry{leEntry("LE", "fin.F7", "DOES_NOT_BLOCK")}}
	s.must(s.module.ProposeFinSettingsChange(ctx, loosen))
	approve := api.ApproveFinSettingsChange{CommandID: "a-1", TenantID: s.tenant, ActorID: "bob", ChangeID: "c2"}
	ra := s.must(s.module.ApproveFinSettingsChange(ctx, approve))
	if again, err := s.module.ApproveFinSettingsChange(ctx, approve); err != nil || again != ra {
		t.Fatalf("replay of the approval: %+v, %v", again, err)
	}
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	s.must(s.module.ProposeFinSettingsChange(ctx, api.ProposeFinSettingsChange{CommandID: "p-3", TenantID: s.tenant, ActorID: "ann", ChangeID: "c3",
		Reason: "later", EffectiveAt: future, Entries: []api.FinSettingEntry{tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER")}}))
	cancel := api.CancelFinSettingsChange{CommandID: "x-1", TenantID: s.tenant, ActorID: "ann", ChangeID: "c3", Reason: "changed my mind"}
	rc := s.must(s.module.CancelFinSettingsChange(ctx, cancel))
	if again, err := s.module.CancelFinSettingsChange(ctx, cancel); err != nil || again != rc {
		t.Fatalf("replay of the cancellation: %+v, %v", again, err)
	}
	cancel.Reason = "another reason"
	if _, err := s.module.CancelFinSettingsChange(ctx, cancel); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the cancellation with another payload: %v", err)
	}
	// replays and refusals wrote nothing more: three proposals, one approved after, two on the spot
	if n := s.count("settings_change"); n != 3 {
		t.Fatalf("%d proposals", n)
	}
	if n := s.count("settings_approval"); n != 3 {
		t.Fatalf("%d approvals", n)
	}
	if n := s.count("settings_cancellation"); n != 1 {
		t.Fatalf("%d cancellations", n)
	}
	types := s.outboxTypes(t)[baseline:]
	want := []string{"FinSettingsChangeProposed", "FinSettingsChangeApproved", "FinSettingsChangeProposed", "FinSettingsChangeApproved", "FinSettingsChangeProposed",
		"FinSettingsChangeApproved", "FinSettingsChangeCancelled"}
	// c1 proposed and approved on the spot; c2 proposed, then approved; c3 proposed and approved on the spot, then
	// cancelled
	slices.Sort(types)
	slices.Sort(want)
	if !slices.Equal(types, want) {
		t.Fatalf("events %v, want %v", types, want)
	}
	// the events name the change set and the version an approval made, and no actor or reason
	var subject, payload string
	err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT subject, payload->>'version' || ' ' || payload::text FROM bento.outbox WHERE scope=$1
          AND event_type='FinSettingsChangeApproved.v1' AND subject='c2'`, s.tenant).Scan(&subject, &payload)
	})
	if err != nil || subject != "c2" || !strings.HasPrefix(payload, "2 ") || strings.Contains(payload, "bob") || strings.Contains(payload, "ann") ||
		strings.Contains(payload, "loosen") {
		t.Fatalf("the approval's event: %s, %s, %v", subject, payload, err)
	}
}

// holdSettings holds the tenant's settings lock, as FIN's settings commands take it, on a connection of its own until
// release is called. pid is its backend.
func holdSettings(t *testing.T, tenant string) (pid int, release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, finpg.SettingsLockSQL, tenant); err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}
	if err != nil {
		t.Fatalf("holding the settings lock: %v", err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
			conn.Close(ctx)
		}
	}
	t.Cleanup(release)
	return pid, release
}

// queuedSettings holds the tenant's settings lock, starts the commands one after another (each only once the sessions
// before it wait in the lock the holder keeps, which pg_blocking_pids shows), releases it, and answers what each
// returned: the first started is granted the lock first, so the order of the commits is forced.
func (s *settingsWorld) queuedSettings(commands ...func() error) []error {
	s.t.Helper()
	pid, release := holdSettings(s.t, s.tenant)
	errs := make([]error, len(commands))
	var wg sync.WaitGroup
	for i, run := range commands {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = run() }()
		waitBlockedBy(s.t, pid, i+1)
	}
	// each waits in the advisory lock of the settings, not in something else
	conn, err := pgx.Connect(context.Background(), os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		s.t.Fatal(err)
	}
	var advisory int
	if err = conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE pid <> $1 AND wait_event_type = 'Lock' AND wait_event = 'advisory'
        AND query LIKE '%cargo_fin.settings/%'`, pid).Scan(&advisory); err != nil || advisory < len(commands) {
		s.t.Fatalf("%d sessions wait in the settings lock, want %d (%v)", advisory, len(commands), err)
	}
	conn.Close(context.Background())
	s.released = time.Now().UTC()
	release()
	wg.Wait()
	return errs
}

func pairOf(errs []error) (ok int, refused error) {
	for _, err := range errs {
		if err == nil {
			ok++
		} else {
			refused = err
		}
	}
	return ok, refused
}

// Two approvals of one change set, started together while the settings lock is held: the first is granted, the second
// is refused as approved already, and the change set has one approval and the next version.
func TestFinSettingsTwoApprovalsOfOneChangeSet(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("c1", "ann", time.Time{}, leEntry("LE", "fin.F7", "BLOCKS"), leEntry("LE", "fin.F2", "SUBMITTER_AND_PREVIEWER")))
	s.must(s.propose("c2", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS")))
	s.must(s.propose("c3", "ann", time.Time{}, leEntry("LE", "fin.F7", "DOES_NOT_BLOCK")))
	errs := s.queuedSettings(
		func() error { _, err := s.approve("c3", "bob"); return err },
		func() error { _, err := s.approve("c3", "carl"); return err })
	if errs[0] != nil || !errors.Is(errs[1], api.ErrRejected) || !strings.Contains(errs[1].Error(), "SETTINGS_ALREADY_APPROVED") {
		t.Fatalf("two approvals: %v", errs)
	}
	c := s.changes()["c3"]
	if c.ApprovedBy != "bob" || c.Version != 3 || s.count("settings_approval") != 3 {
		t.Fatalf("after two approvals: %+v, %d approvals", c, s.count("settings_approval"))
	}
	// two different change sets approved together are numbered in the order they were granted the lock
	s.must(s.propose("d1", "ann", time.Time{}, leEntry("LE2", "fin.F7", "BLOCKS")))
	s.must(s.propose("d2", "ann", time.Time{}, leEntry("LE2", "fin.F2", "SUBMITTER_AND_PREVIEWER")))
	// (the two were tightening values: approved on the spot) so a loosening pair
	s.must(s.propose("d3", "ann", time.Time{}, leEntry("LE2", "fin.F7", "DOES_NOT_BLOCK")))
	s.must(s.propose("d4", "ann", time.Time{}, leEntry("LE2", "fin.F2", "SUBMITTER_ONLY")))
	errs = s.queuedSettings(
		func() error { _, err := s.approve("d4", "bob"); return err },
		func() error { _, err := s.approve("d3", "carl"); return err })
	if ok, _ := pairOf(errs); ok != 2 {
		t.Fatalf("two approvals of two change sets: %v", errs)
	}
	h := s.changes()
	if h["d4"].Version+1 != h["d3"].Version {
		t.Fatalf("versions of two approvals granted in turn: %d, %d", h["d4"].Version, h["d3"].Version)
	}
}

// An approval and a cancellation of one change set, started together while the settings lock is held, in each order.
// Approved first, a change set with a time to come is cancelled (its approval stays, its version is not reused), and
// one already in force is not; cancelled first, it is not approved.
func TestFinSettingsApprovalRacesCancellation(t *testing.T) {
	future := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Microsecond)
	for _, c := range []struct {
		name                string
		effective           time.Time
		approveFirst        bool
		approveOK, cancelOK bool
		refusal             string
	}{
		{"approved first, its time to come", future, true, true, true, ""},
		{"approved first, in force at once", time.Time{}, true, true, false, "SETTINGS_IN_FORCE"},
		{"cancelled first", future, false, false, true, "SETTINGS_CANCELLED"},
		{"cancelled first, in force at once", time.Time{}, false, false, true, "SETTINGS_CANCELLED"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSettingsWorld(t)
			s.must(s.propose("c1", "ann", c.effective, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
			doApprove := func() error { _, err := s.approve("c1", "bob"); return err }
			doCancel := func() error { _, err := s.cancel("c1", "eve"); return err }
			commands := []func() error{doApprove, doCancel}
			if !c.approveFirst {
				commands = []func() error{doCancel, doApprove}
			}
			errs := s.queuedSettings(commands...)
			errApprove, errCancel := errs[0], errs[1]
			if !c.approveFirst {
				errApprove, errCancel = errs[1], errs[0]
			}
			if (errApprove == nil) != c.approveOK || (errCancel == nil) != c.cancelOK {
				t.Fatalf("approval %v, cancellation %v; want ok %v and %v", errApprove, errCancel, c.approveOK, c.cancelOK)
			}
			if refused := errors.Join(errApprove, errCancel); c.refusal != "" && (!errors.Is(refused, api.ErrRejected) || !strings.Contains(refused.Error(), c.refusal)) {
				t.Fatalf("the refusal: %v, want %s", refused, c.refusal)
			}
			h := s.changes()["c1"]
			approvals, cancellations := s.count("settings_approval"), s.count("settings_cancellation")
			switch {
			case c.approveOK && c.cancelOK && (h.Status != "CANCELLED" || approvals != 1 || cancellations != 1 || h.Version != 1):
				t.Fatalf("approved, then cancelled: %+v, %d, %d", h, approvals, cancellations)
			case c.approveOK && !c.cancelOK && (h.Status != "IN_FORCE" || approvals != 1 || cancellations != 0):
				t.Fatalf("approved and in force: %+v, %d, %d", h, approvals, cancellations)
			case !c.approveOK && (h.Status != "CANCELLED" || approvals != 0 || cancellations != 1 || h.Version != 0):
				t.Fatalf("cancelled: %+v, %d, %d", h, approvals, cancellations)
			}
			if got, _ := s.settings("LEGAL_ENTITY", "LE", future.Add(time.Hour)); got["fin.F3"].Value == "ONLY_WHEN_AMOUNT_MOVES" && h.Status == "CANCELLED" {
				t.Fatalf("a cancelled change set is in force: %+v", got["fin.F3"])
			}
		})
	}
}

// rawTx runs statements in a transaction of the tenant on the application role; $1 is the tenant.
func (f *fixture) rawTx(t *testing.T, stmts ...string) error {
	t.Helper()
	return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, s := range stmts {
			if _, err := e.Exec(ctx, s, f.tenant); err != nil {
				return err
			}
		}
		return nil
	})
}

func rawChange(id, by, effective string, loosening bool) string {
	return fmt.Sprintf(`INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
      VALUES ($1,'%s','a reason','%s',now(),%s,%v)`, id, by, effective, loosening)
}

func rawEntry(id string, no int, kind, scopeID, key, value, param string) string {
	return fmt.Sprintf(`INSERT INTO cargo_fin.settings_change_entry (tenant_id,change_id,entry_no,scope_kind,scope_id,item_key,value_id,param_minor)
      VALUES ($1,'%s',%d,'%s','%s','%s','%s',%s)`, id, no, kind, scopeID, key, value, param)
}

// rawApproval approves the change set as the columns of its row say, at and inForce being SQL over proposed_at and
// effective_at.
func rawApproval(id, by, at string, version any, inForce string, auto bool) string {
	return fmt.Sprintf(`INSERT INTO cargo_fin.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
        version_no,in_force_at,auto)
      SELECT tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,'%s',%s,%v,%s,%v FROM cargo_fin.settings_change
      WHERE tenant_id=$1 AND change_id='%s'`, by, at, version, inForce, auto, id)
}

func rawCancellation(id, at string) string {
	return fmt.Sprintf(`INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason) VALUES ($1,'%s','x',%s,'a reason')`, id, at)
}

const nextVersion = `(SELECT coalesce(max(version_no),0)+1 FROM cargo_fin.settings_approval WHERE tenant_id=$1)`

// What the database refuses past FIN's commands on the application role, each by its name: an entry's scope, item and
// value, its parameter and the parameter's range, an effective time before the proposal, a change set without entries
// or with a gap in their numbers or whose entry comes later, an approval by the proposer or an automatic one by
// another, an automatic one of a change set that loosens, not at the proposal, or not with its change set, an approval
// before the proposal or not taking effect at the later of its time and the effective time, an approval after a
// cancellation, a cancellation of a change set in force or before it was proposed, a second approval or cancellation, a
// version twice or skipped, a change of any row, another tenant's rows, and a write at REPEATABLE READ. Each rule is
// shown on a change set the commands made, so that what is refused is the rule and not something else.
func TestFinSettingsDatabaseRulesRefuseRawWrites(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES"))) // loosening, waits
	s.must(s.propose("L2", "ann", time.Time{}, leEntry("LE2", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
	s.must(s.propose("T1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS"))) // tightening, approved at once: version 1
	if err := s.rawTx(t, rawChange("R3", "ann", "now()", false), rawEntry("R3", 1, "LEGAL_ENTITY", "LE", "fin.F7", "BLOCKS", "NULL")); err != nil {
		t.Fatal(err) // a change set with no approval, to show an approval not written with it
	}
	if err := s.rawTx(t, rawCancellation("L2", "now()")); err != nil {
		t.Fatal(err) // L2 is cancelled
	}
	later := "proposed_at + interval '1 minute'"
	type refusal struct {
		name       string
		stmts      []string
		state      string
		constraint string
	}
	chg := func(id string, loosening bool) []string {
		return []string{rawChange(id, "ann", "now()", loosening), rawEntry(id, 1, "LEGAL_ENTITY", "LE", "fin.F7", "BLOCKS", "NULL")}
	}
	with := func(base []string, more ...string) []string { return append(slices.Clone(base), more...) }
	for _, c := range []refusal{
		{"the company has no ID", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "x", "fin.F7", "BLOCKS", "NULL")}, "23514", "settings_entry_scope"},
		{"a legal entity needs its ID", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "LEGAL_ENTITY", "", "fin.F7", "BLOCKS", "NULL")}, "23514", "settings_entry_scope"},
		{"another kind of scope", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "BRANCH", "LE/B", "fin.F7", "BLOCKS", "NULL")}, "23514", "settings_entry_scope"},
		{"a value the item does not have", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "fin.F7", "NOPE", "NULL")}, "23514", "settings_entry_value_known"},
		{"another item's value", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "fin.F9", "BLOCKS", "NULL")}, "23514", "settings_entry_value_known"},
		{"an item of no catalogue", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "TENANT", "", "fin.F5", "BLOCKS", "NULL")}, "23514", "settings_entry_value_known"},
		{"a threshold without its amount", []string{rawChange("R1", "ann", "now()", true), rawEntry("R1", 1, "LEGAL_ENTITY", "LE", "fin.F1", "NOT_ABOVE_THRESHOLD", "NULL")}, "23514", "settings_entry_param"},
		{"an amount for a value without one", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 1, "LEGAL_ENTITY", "LE", "fin.F7", "BLOCKS", "5")}, "23514", "settings_entry_param"},
		{"a threshold of nothing", []string{rawChange("R1", "ann", "now()", true), rawEntry("R1", 1, "LEGAL_ENTITY", "LE", "fin.F1", "NOT_ABOVE_THRESHOLD", "0")}, "23514", "settings_entry_param_range"},
		{"a threshold too large", []string{rawChange("R1", "ann", "now()", true), rawEntry("R1", 1, "LEGAL_ENTITY", "LE", "fin.F1", "NOT_ABOVE_THRESHOLD", "1000000000000001")}, "23514", "settings_entry_param_range"},
		{"a threshold for the company", []string{rawChange("R1", "ann", "now()", true), rawEntry("R1", 1, "TENANT", "", "fin.F1", "NOT_ABOVE_THRESHOLD", "5")}, "23514", "settings_entry_param_range"},
		{"an effective time before the proposal", []string{rawChange("R1", "ann", "now() - interval '1 second'", false)}, "23514", "settings_change_effective"},
		{"a change set without entries", []string{rawChange("R1", "ann", "now()", false)}, "23514", "settings_change_entries"},
		{"entries that skip a number", []string{rawChange("R1", "ann", "now()", false), rawEntry("R1", 2, "TENANT", "", "fin.F7", "BLOCKS", "NULL")}, "23514", "settings_change_entries"},
		{"an entry written after its change set", []string{rawEntry("R3", 2, "LEGAL_ENTITY", "LE", "fin.F2", "SUBMITTER_ONLY", "NULL")}, "23514", "settings_entry_with_change"},
		{"an entry twice for one scope and item", with(chg("R1", false), rawEntry("R1", 2, "LEGAL_ENTITY", "LE", "fin.F7", "BLOCKS", "NULL")), "23505", "settings_entry_once"},
		{"approval by the proposer", []string{rawApproval("L1", "ann", "proposed_at", 91, "effective_at", false)}, "23514", "settings_approval_other_actor"},
		{"an automatic approval by another", with(chg("R4", false), rawApproval("R4", "bob", "proposed_at", 91, "effective_at", true)), "23514", "settings_approval_other_actor"},
		{"an automatic approval of a change set that loosens", with(chg("R4", true), rawApproval("R4", "ann", "proposed_at", 91, "effective_at", true)), "23514", "settings_approval_tightening_only"},
		{"an automatic approval later than the proposal", with(chg("R4", false), rawApproval("R4", "ann", later, 91, later, true)), "23514", "settings_approval_automatic_at_proposal"},
		{"an automatic approval not written with its change set", []string{rawApproval("R3", "ann", "proposed_at", 91, "effective_at", true)}, "23514", "settings_approval_with_change"},
		{"an approval before the proposal", []string{rawApproval("L1", "bob", "proposed_at - interval '1 hour'", 91, "effective_at", false)}, "23514", "settings_approval_after_proposal"},
		{"an approval that does not take effect when it should", []string{rawApproval("L1", "bob", later, 91, "proposed_at", false)}, "23514", "settings_approval_in_force"},
		{"an approval of a cancelled change set", []string{rawApproval("L2", "bob", later, 91, "greatest(effective_at, "+later+")", false)}, "23514", "settings_approval_cancelled"},
		{"a second approval", []string{rawApproval("T1", "bob", later, 91, "greatest(effective_at, "+later+")", false)}, "23505", "settings_approval_pkey"},
		{"a version twice", []string{rawApproval("L1", "bob", later, 1, "greatest(effective_at, "+later+")", false)}, "23505", "settings_approval_version_once"},
		{"a version skipped", []string{rawApproval("L1", "bob", later, 9, "greatest(effective_at, "+later+")", false)}, "23514", "settings_versions_consistent"},
		{"an approval naming another proposer", []string{`INSERT INTO cargo_fin.settings_approval (tenant_id,change_id,proposed_by,proposed_at,effective_at,loosening,approved_by,approved_at,
          version_no,in_force_at,auto) SELECT tenant_id,change_id,'zed',proposed_at,effective_at,loosening,'bob',proposed_at,91,effective_at,false
          FROM cargo_fin.settings_change WHERE tenant_id=$1 AND change_id='L1'`}, "23503", "settings_approval_matches_change"},
		{"a cancellation of a change set in force", []string{rawCancellation("T1", "now()")}, "23514", "settings_cancellation_in_force"},
		{"a cancellation before the proposal", []string{rawCancellation("L1", "now() - interval '2 days'")}, "23514", "settings_cancellation_after_proposal"},
		{"a second cancellation", []string{rawCancellation("L2", "now()")}, "23505", "settings_cancellation_pkey"},
		{"a cancellation at the moment the change set takes effect", with(chg("R6", true), rawApproval("R6", "bob", "proposed_at", nextVersion, "proposed_at", false),
			rawCancellation("R6", "now()")), "23514", "settings_cancellation_in_force"},
		{"a cancellation of a change set that is not there", []string{rawCancellation("NOPE", "now()")}, "23503", ""},
		{"a change set without a reason", []string{`INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'R7',' ','ann',now(),now(),false)`}, "23514", "settings_change_reason_check"},
		{"a change set without a proposer", []string{`INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1,'R7','r',' ',now(),now(),false)`}, "23514", "settings_change_proposed_by_check"},
		{"a cancellation without a reason", []string{`INSERT INTO cargo_fin.settings_cancellation (tenant_id,change_id,cancelled_by,cancelled_at,reason)
          VALUES ($1,'L1','x',now(),' ')`}, "23514", "settings_cancellation_reason_check"},
		{"another tenant's change set", []string{`INSERT INTO cargo_fin.settings_change (tenant_id,change_id,reason,proposed_by,proposed_at,effective_at,loosening)
          VALUES ($1 || '-other','X','r','ann',now(),now(),false)`}, "42501", ""},
	} {
		err := s.rawTx(t, c.stmts...)
		state, constraint := sqlState(err)
		if err == nil || state != c.state || (c.constraint != "" && constraint != c.constraint) {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", c.name, err, state, constraint, c.state, c.constraint)
		}
	}
	// the rows are never changed or deleted, whatever the table
	for _, table := range []string{"settings_change", "settings_change_entry", "settings_approval", "settings_cancellation"} {
		for _, stmt := range []string{`UPDATE cargo_fin.` + table + ` SET tenant_id = tenant_id WHERE tenant_id = $1`, `DELETE FROM cargo_fin.` + table + ` WHERE tenant_id = $1`} {
			if err := s.rawTx(t, stmt); err == nil {
				t.Errorf("%s: no refusal", stmt)
			} else if state, _ := sqlState(err); state != "23001" && state != "42501" {
				t.Errorf("%s: %v (state %s)", stmt, err, state)
			}
		}
	}
	// nothing the refusals tried is left, and the change sets the commands made are as they were
	if n := s.count("settings_change"); n != 4 {
		t.Errorf("%d change sets, want L1, L2, T1 and R3", n)
	}
	if n := s.count("settings_approval"); n != 1 {
		t.Errorf("%d approvals, want T1's", n)
	}
	// row security is enabled and forced, and hides another tenant's rows
	for _, table := range []string{"settings_change", "settings_change_entry", "settings_approval", "settings_cancellation"} {
		var enabled, forced bool
		var others int
		err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
			if err := e.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                WHERE n.nspname = 'cargo_fin' AND c.relname = $1`, table).Scan(&enabled, &forced); err != nil {
				return err
			}
			return e.QueryRow(ctx, `SELECT count(*) FROM cargo_fin.`+table+` WHERE tenant_id <> $1`, s.tenant).Scan(&others)
		})
		if err != nil || !enabled || !forced || others != 0 {
			t.Errorf("%s: row security enabled %v, forced %v, other tenants' rows seen %d, %v", table, enabled, forced, others, err)
		}
	}
	// a write at REPEATABLE READ is refused before it runs, on each table
	conn, err := pgx.Connect(context.Background(), os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	for _, table := range []string{"settings_change", "settings_change_entry", "settings_approval", "settings_cancellation"} {
		tx, err := conn.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id',$1,true)`, s.tenant)
		if err == nil {
			_, err = tx.Exec(context.Background(), `INSERT INTO cargo_fin.`+table+` (tenant_id) VALUES ($1)`, s.tenant)
		}
		if state, constraint := sqlState(err); state != "23514" || constraint != "fin_read_committed" {
			t.Errorf("%s at REPEATABLE READ: %v (state %s, constraint %s)", table, err, state, constraint)
		}
		_ = tx.Rollback(context.Background())
	}
}

var errRollBack = errors.New("roll back")

// Every value of FIN's catalogue is accepted by the database's list of values, with F1's threshold for a legal entity:
// the list in the migration and the catalogue in the code agree.
func TestFinSettingsDatabaseKnowsEveryValueOfTheCatalogue(t *testing.T) {
	s := newSettingsWorld(t)
	cat, err := s.module.GetFinSettingsCatalogue(context.Background(), api.GetFinSettingsCatalogue{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range cat.Items {
		for _, v := range it.Values {
			n++
			param := "NULL"
			if v.Parameter != "" {
				param = "123"
			}
			id := fmt.Sprintf("V%d", n)
			for _, scope := range []struct{ kind, id string }{{"TENANT", ""}, {"LEGAL_ENTITY", "LE"}} {
				if v.Parameter != "" && scope.kind == "TENANT" {
					continue
				}
				err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
					for _, stmt := range []string{rawChange(id, "ann", "now()", false), rawEntry(id, 1, scope.kind, scope.id, it.Key, v.ID, param)} {
						if _, err := e.Exec(ctx, stmt, s.tenant); err != nil {
							return err
						}
					}
					return errRollBack
				})
				if !errors.Is(err, errRollBack) {
					t.Errorf("%s %s at %s: %v", it.Key, v.ID, scope.kind, err)
				}
			}
		}
	}
	if n != 18 {
		t.Errorf("%d values in the catalogue, the migration lists 18", n)
	}
}

// The database keeps "not both" by itself, without the application's lock, as the rows are written: a cancellation not
// yet committed makes a later approval wait for it and then be refused, and an approval not yet committed makes a
// cancellation wait and then be refused when the change set is in force. The first connection holds the change set's
// lock by writing its row; the second is shown waiting for it.
func TestFinSettingsDatabaseOrdersAnApprovalAndACancellation(t *testing.T) {
	for _, c := range []struct {
		name           string
		holder, second func(id string) string
		constraint     string
	}{
		{"a cancellation, then an approval", func(id string) string { return rawCancellation(id, "now()") },
			func(id string) string {
				return rawApproval(id, "bob", "proposed_at + interval '1 minute'", nextVersion, "greatest(effective_at, proposed_at + interval '1 minute')", false)
			}, "settings_approval_cancelled"},
		{"an approval, then a cancellation", func(id string) string {
			return rawApproval(id, "bob", "now()", nextVersion, "greatest(effective_at, now())", false)
		}, func(id string) string { return rawCancellation(id, "now() + interval '1 minute'") }, "settings_cancellation_in_force"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSettingsWorld(t)
			s.must(s.propose("L1", "ann", time.Time{}, leEntry("LE", "fin.F3", "ONLY_WHEN_AMOUNT_MOVES")))
			ctx := context.Background()
			conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, s.tenant); err == nil {
				_, err = tx.Exec(ctx, c.holder("L1"), s.tenant)
			}
			if err == nil {
				err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
			}
			if err != nil {
				t.Fatalf("the first write: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- s.rawTx(t, c.second("L1")) }()
			waitBlockedBy(t, pid, 1)
			if err, returned := returnsWithin(done, 200*time.Millisecond); returned {
				t.Fatalf("the second write returned (%v) while the first was not committed", err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatalf("committing the first write: %v", err)
			}
			err, returned := returnsWithin(done, 10*time.Second)
			if state, constraint := sqlState(err); !returned || state != "23514" || constraint != c.constraint {
				t.Fatalf("the second write: %v (state %s, constraint %s), want %s", err, state, constraint, c.constraint)
			}
		})
	}
}

// A proposal takes the tenant's settings lock too: while it is held, two proposals wait and then commit one after the
// other in the order they started, the first making version 1 and the second version 2.
func TestFinSettingsProposalsWaitForTheSettingsLock(t *testing.T) {
	s := newSettingsWorld(t)
	errs := s.queuedSettings(
		func() error {
			_, err := s.propose("c1", "ann", time.Time{}, tenantEntry("fin.F7", "BLOCKS"))
			return err
		},
		func() error {
			_, err := s.propose("c2", "ann", time.Time{}, tenantEntry("fin.F2", "SUBMITTER_AND_PREVIEWER"))
			return err
		})
	if ok, _ := pairOf(errs); ok != 2 {
		t.Fatalf("two proposals: %v", errs)
	}
	h := s.changes()
	if h["c1"].Version != 1 || h["c2"].Version != 2 {
		t.Fatalf("versions of two proposals granted in turn: %d, %d", h["c1"].Version, h["c2"].Version)
	}
}
