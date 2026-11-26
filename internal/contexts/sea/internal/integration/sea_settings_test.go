//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	seamodule "go.idp.xyz/idp-cargo/internal/contexts/sea"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	seapg "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/adapters/postgres"
)

// V2.100 on PostgreSQL, migration 000012: SEA's settings through SEA's commands, as FIN's (V2.96) are tested in FIN.

// seaSettingsAuth grants the three permissions that are granted by name to the actors named, as the platform does, and
// every other action of SEA to everyone but the "intruder".
type seaSettingsAuth struct{ grants map[string][]string }

func (a seaSettingsAuth) AuthorizeSeaAction(_ context.Context, _, actor, action, _ string) error {
	switch action {
	case api.ActionConfigureSettings, api.ActionApproveSettings, api.ActionSuperviseCarrierConflicts:
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

// settingsWorld is a tenant with two legal entities, LE and LE2, each with an HBL series, and the module with the
// settings' permissions: ann and eve configure, bob and carl approve, dan does both, sup-1 supervises, and the others
// do none of the three.
type settingsWorld struct {
	*reworkFixture
	n int
}

// newSettingsWorld is the tenant with the HBL series of LE and LE2, so that both are legal entities SEA knows.
func newSettingsWorld(t *testing.T) *settingsWorld { return newSettingsWorldOf(t, "LE", "LE2") }

// newSettingsWorldOf is the same world with the HBL series of the legal entities named only: a day with a movement of
// LE registers LE's own.
func newSettingsWorldOf(t *testing.T, legalEntities ...string) *settingsWorld {
	t.Helper()
	f := newFixture(t)
	rw := &reworkWorld{world: f.w, behind: map[string]bool{}}
	m, err := seamodule.New(f.db, f.w, rw, seaSettingsAuth{grants: map[string][]string{
		"ann": {api.ActionConfigureSettings}, "eve": {api.ActionConfigureSettings},
		"bob": {api.ActionApproveSettings}, "carl": {api.ActionApproveSettings},
		"dan": {api.ActionConfigureSettings, api.ActionApproveSettings}, "frank": {api.ActionConfigureSettings, api.ActionApproveSettings},
		"sup-1": {api.ActionSuperviseCarrierConflicts}}})
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	s := &settingsWorld{reworkFixture: &reworkFixture{fixture: f, rw: rw, t: t}}
	for _, le := range legalEntities {
		if _, err = m.RegisterHblSeries(context.Background(), api.RegisterHblSeries{CommandID: "series-" + le, TenantID: f.tenant, ActorID: "ops-1",
			LegalEntityID: le, Template: "SHAL{seq:7}", FirstNumber: 1, OccurredAt: f.at}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *settingsWorld) command() string {
	s.n++
	return fmt.Sprintf("cmd-%d", s.n)
}

func tenantEntry(value string) api.SeaSettingEntry {
	return api.SeaSettingEntry{ScopeKind: "TENANT", Key: api.SettingS1, Value: value}
}

func leEntry(le, value string) api.SeaSettingEntry {
	return api.SeaSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: le, Key: api.SettingS1, Value: value}
}

func (s *settingsWorld) propose(id, actor string, effective time.Time, entries ...api.SeaSettingEntry) (api.Receipt, error) {
	return s.module.ProposeSeaSettingsChange(context.Background(), api.ProposeSeaSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: actor,
		ChangeID: id, Entries: entries, Reason: "because " + id, EffectiveAt: effective})
}

func (s *settingsWorld) approve(id, actor string) (api.Receipt, error) {
	return s.module.ApproveSeaSettingsChange(context.Background(), api.ApproveSeaSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: actor, ChangeID: id})
}

func (s *settingsWorld) refuse(id, actor string) (api.Receipt, error) {
	return s.module.RefuseSeaSettingsChange(context.Background(), api.RefuseSeaSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: actor,
		ChangeID: id, Reason: "not wanted"})
}

func (s *settingsWorld) cancel(id, actor string) (api.Receipt, error) {
	return s.module.CancelSeaSettingsChange(context.Background(), api.CancelSeaSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: actor,
		ChangeID: id, Reason: "not wanted"})
}

func (s *settingsWorld) must(r api.Receipt, err error) api.Receipt {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
	return r
}

// refusedWith checks that a command was refused with the kind and that the refusal says each of the parts.
func (s *settingsWorld) refusedWith(what string, r api.Receipt, err error, kind error, contains ...string) {
	s.t.Helper()
	if !errors.Is(err, kind) {
		s.t.Fatalf("%s: %+v, %v, want %v", what, r, err, kind)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			s.t.Fatalf("%s: the refusal does not say %q: %v", what, c, err)
		}
	}
}

// s1 answers a scope's S1 as of a time, with the version in force.
func (s *settingsWorld) s1(kind, id string, asOf time.Time) (api.SeaSetting, int) {
	s.t.Helper()
	got, err := s.module.GetSeaSettings(context.Background(), api.GetSeaSettings{TenantID: s.tenant, ScopeKind: kind, ScopeID: id, AsOf: asOf})
	if err != nil || len(got.Items) != 1 || got.Items[0].Key != api.SettingS1 {
		s.t.Fatalf("settings: %+v, %v", got, err)
	}
	return got.Items[0], got.Version
}

func (s *settingsWorld) changes() map[string]api.SeaSettingsChange {
	s.t.Helper()
	list, err := s.module.ListSeaSettingsChanges(context.Background(), api.ListSeaSettingsChanges{TenantID: s.tenant})
	if err != nil {
		s.t.Fatal(err)
	}
	out := map[string]api.SeaSettingsChange{}
	for _, c := range list {
		out[c.ChangeID] = c
	}
	return out
}

func (s *settingsWorld) count(table string) int {
	s.t.Helper()
	var n int
	err := s.within(s.t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM cargo_sea.`+table+` WHERE tenant_id=$1`, s.tenant).Scan(&n)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return n
}

// Propose, approve, refuse, cancel, the history and the settings as of a time, through the real commands: a change that
// only tightens is in force at once and makes a version; one that loosens (SUPERVISOR_ONLY to WITH_REASON) waits for
// another actor; one in the future shows from its time; a change not in force can be cancelled and one in force cannot;
// the value is resolved through the legal entity, the company and the default, with the scope each comes from.
func TestSeaSettingsProposeApproveRefuseCancelOnPostgres(t *testing.T) {
	s := newSettingsWorld(t)
	before := time.Now().UTC().Add(-time.Hour)
	cat, err := s.module.GetSeaSettingsCatalogue(context.Background(), api.GetSeaSettingsCatalogue{})
	if err != nil || len(cat.Items) != 1 || cat.Items[0].Key != "sea.S1" || cat.Items[0].Default != "WITH_REASON" || len(cat.Items[0].Values) != 2 ||
		cat.Items[0].Values[0].ID != "SUPERVISOR_ONLY" || cat.Items[0].Values[1].ID != "WITH_REASON" || !cat.Items[0].InternalControl ||
		!slices.Equal(cat.Items[0].ScopeKinds, []string{"TENANT", "LEGAL_ENTITY"}) || !strings.Contains(cat.Table, "| sea.S1 |") {
		t.Fatalf("the catalogue: %+v, %v", cat, err)
	}
	if got, version := s.s1("LEGAL_ENTITY", "LE", time.Time{}); version != 0 || got.Value != "WITH_REASON" || !got.Default || got.FromScopeKind != "TENANT" {
		t.Fatalf("nothing is set: %+v, version %d", got, version)
	}

	// a tightening in the future is approved on the spot by its proposer and shows from its time
	future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	r := s.must(s.propose("f1", "ann", future, tenantEntry("SUPERVISOR_ONLY")))
	future1 := r.SettingsVersion
	if future1 == 0 || r.NewRevision != 2 || r.ChangeID != "f1" {
		t.Fatalf("a future tightening: %+v", r)
	}
	if got, _ := s.s1("TENANT", "", time.Time{}); got.Value != "WITH_REASON" {
		t.Fatalf("a change before its time: %+v", got)
	}
	if got, v := s.s1("LEGAL_ENTITY", "LE", future.Add(time.Minute)); got.Value != "SUPERVISOR_ONLY" || v != future1 {
		t.Fatalf("as of after its time: %+v, version %d", got, v)
	}
	if c := s.changes()["f1"]; c.Status != "APPROVED" || !c.AutoApproved || c.Loosening || !c.EffectiveAt.Equal(future) || !c.InForceAt.Equal(future) {
		t.Fatalf("history of a future change: %+v", c)
	}
	// cancelled before it is in force, by a configurer, with a reason: its version is not reused and it never counts
	_, err = s.cancel("f1", "zed")
	s.refusedWith("someone who neither configures nor approves cancels", api.Receipt{}, err, api.ErrUnauthorized)
	if r = s.must(s.cancel("f1", "eve")); r.NewRevision != 3 {
		t.Fatalf("the cancellation of an approved change: %+v", r)
	}
	if got, v := s.s1("TENANT", "", future.Add(time.Minute)); got.Value != "WITH_REASON" || v >= future1 {
		t.Fatalf("a cancelled change is in force: %+v, version %d", got, v)
	}
	if c := s.changes()["f1"]; c.Status != "CANCELLED" || c.CancelledBy != "eve" || c.CancelledRole != "CONFIGURER" || c.CancelReason != "not wanted" || c.CancelledAt.IsZero() {
		t.Fatalf("history of f1: %+v", c)
	}
	_, err = s.cancel("f1", "eve")
	s.refusedWith("a second cancellation", api.Receipt{}, err, api.ErrRejected, "SETTINGS_CANCELLED")
	// an approver who does not configure cancels as an approver, and that is recorded
	s.must(s.propose("g1", "ann", future, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.cancel("g1", "bob"))
	if c := s.changes()["g1"]; c.Status != "CANCELLED" || c.CancelledBy != "bob" || c.CancelledRole != "APPROVER" {
		t.Fatalf("a cancellation by an approver: %+v", c)
	}

	// the company tightens S1: approved on the spot by its proposer, the next version, not the cancelled ones'
	r = s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	if r.ChangeID != "c1" || r.NewRevision != 2 || r.SettingsVersion <= future1 {
		t.Fatalf("a tightening: %+v", r)
	}
	v1 := r.SettingsVersion
	got, version := s.s1("LEGAL_ENTITY", "LE", time.Time{})
	if version != v1 || got.Value != "SUPERVISOR_ONLY" || got.FromScopeKind != "TENANT" || got.Default || got.ChangeID != "c1" || got.Exception {
		t.Fatalf("a legal entity takes the company's value: %+v, version %d", got, version)
	}
	// a legal entity looser than the company's value waits for a second person
	r = s.must(s.propose("c2", "ann", time.Time{}, leEntry("LE", "WITH_REASON")))
	if r.NewRevision != 1 || r.SettingsVersion != 0 {
		t.Fatalf("a loosening: %+v", r)
	}
	if got, version = s.s1("LEGAL_ENTITY", "LE", time.Time{}); got.Value != "SUPERVISOR_ONLY" || version != v1 {
		t.Fatalf("a change waiting for approval is in force: %+v, version %d", got, version)
	}
	if c := s.changes()["c2"]; c.Status != "PROPOSED" || !c.Loosening || c.AutoApproved || c.Version != 0 {
		t.Fatalf("a loosening waits: %+v", c)
	}
	_, err = s.approve("c2", "ann")
	s.refusedWith("a proposer without the approve permission approves", api.Receipt{}, err, api.ErrUnauthorized)
	s.must(s.propose("c2b", "dan", time.Time{}, leEntry("LE", "WITH_REASON")))
	_, err = s.approve("c2b", "dan")
	s.refusedWith("the proposer with both permissions approves", api.Receipt{}, err, api.ErrRejected, "SETTINGS_SAME_ACTOR", "another approver")
	_, err = s.approve("c2", "eve")
	s.refusedWith("someone who may not approve", api.Receipt{}, err, api.ErrUnauthorized)
	_, err = s.approve("nope", "bob")
	s.refusedWith("an unknown change set", api.Receipt{}, err, api.ErrNotFound, "ListSeaSettingsChanges")
	r = s.must(s.approve("c2", "bob"))
	if r.SettingsVersion != v1+1 || r.NewRevision != 2 {
		t.Fatalf("the approval: %+v", r)
	}
	got, version = s.s1("LEGAL_ENTITY", "LE", time.Time{})
	if got.Value != "WITH_REASON" || got.FromScopeKind != "LEGAL_ENTITY" || got.FromScopeID != "LE" || got.ChangeID != "c2" || !got.Exception || version != v1+1 {
		t.Fatalf("the approved change, an exception to the company's value: %+v, version %d", got, version)
	}
	if got, _ = s.s1("LEGAL_ENTITY", "LE2", time.Time{}); got.Value != "SUPERVISOR_ONLY" || got.FromScopeKind != "TENANT" {
		t.Fatalf("another legal entity keeps the company's value: %+v", got)
	}
	_, err = s.approve("c2", "carl")
	s.refusedWith("a second approval", api.Receipt{}, err, api.ErrRejected, "SETTINGS_ALREADY_APPROVED")
	// a change set that loosens the company's value: SUPERVISOR_ONLY to WITH_REASON waits, and is refused
	// by an approver
	s.must(s.propose("w1", "ann", time.Time{}, tenantEntry("WITH_REASON")))
	_, err = s.refuse("w1", "ann")
	s.refusedWith("a refusal by someone who may not approve", api.Receipt{}, err, api.ErrUnauthorized)
	if r = s.must(s.refuse("w1", "carl")); r.NewRevision != 2 {
		t.Fatalf("the refusal: %+v", r)
	}
	if c := s.changes()["w1"]; c.Status != "REFUSED" || c.CancelledBy != "carl" || c.CancelledRole != "APPROVER" || c.CancelReason != "not wanted" || c.Version != 0 {
		t.Fatalf("history of a refused change set: %+v", c)
	}
	_, err = s.approve("w1", "bob")
	s.refusedWith("approving a refused change set", api.Receipt{}, err, api.ErrRejected, "propose it again")
	_, err = s.refuse("w1", "carl")
	s.refusedWith("refusing it twice", api.Receipt{}, err, api.ErrRejected, "SETTINGS_CANCELLED")
	_, err = s.refuse("c2", "carl")
	s.refusedWith("refusing a change set approved already", api.Receipt{}, err, api.ErrRejected, "SETTINGS_REFUSED", "cancel it with a reason",
		"CancelSeaSettingsChange")
	if strings.Contains(err.Error(), "Fin") || strings.Contains(err.Error(), "FIN") {
		t.Fatalf("a refusal of SEA names FIN's commands: %v", err)
	}
	// the company tightens again: the notice names the legal entity that keeps its own, looser value
	r = s.must(s.propose("n1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	if !strings.Contains(r.Notice, "LE keeps its own sea.S1 WITH_REASON (change set c2), looser than the company's SUPERVISOR_ONLY") || !strings.Contains(r.Notice, "propose a change set for that legal entity") {
		t.Fatalf("the notice: %q", r.Notice)
	}
	// as of before anything, and the time of the read when none is given
	if g, err := s.module.GetSeaSettings(context.Background(), api.GetSeaSettings{TenantID: s.tenant, ScopeKind: "TENANT", AsOf: before}); err != nil ||
		!g.AsOf.Equal(before.Truncate(time.Microsecond)) || g.Version != 0 || g.Items[0].Value != "WITH_REASON" || !g.Items[0].Default {
		t.Fatalf("as of before any change: %+v, %v", g, err)
	}
	if g, err := s.module.GetSeaSettings(context.Background(), api.GetSeaSettings{TenantID: s.tenant, ScopeKind: "TENANT"}); err != nil || g.AsOf.IsZero() {
		t.Fatalf("the time of a read as of now: %+v, %v", g.AsOf, err)
	}

	// what is refused, each with its way out
	_, err = s.propose("t1", "ann", time.Time{}, leEntry("LE-X", "SUPERVISOR_ONLY"))
	s.refusedWith("a legal entity SEA does not know", api.Receipt{}, err, api.ErrRejected, "LE-X", "RegisterHblSeries")
	_, err = s.propose("t1", "ann", time.Time{}, api.SeaSettingEntry{ScopeKind: "TENANT", Key: "sea.S2", Value: "X"})
	s.refusedWith("an unknown item", api.Receipt{}, err, api.ErrRejected, "sea.S2", "catalogue")
	_, err = s.propose("t1", "ann", time.Time{}, leEntry("LE", "MAYBE"))
	s.refusedWith("an unknown value", api.Receipt{}, err, api.ErrRejected, "SUPERVISOR_ONLY, WITH_REASON")
	_, err = s.propose("t1", "ann", time.Time{}, api.SeaSettingEntry{ScopeKind: "BRANCH", ScopeID: "b", Key: api.SettingS1, Value: "WITH_REASON"})
	s.refusedWith("another kind of scope", api.Receipt{}, err, api.ErrRejected, "choose a scope")
	_, err = s.propose("t1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"), tenantEntry("WITH_REASON"))
	s.refusedWith("one scope and item twice", api.Receipt{}, err, api.ErrRejected, "proposed twice")
	_, err = s.propose("t1", "ann", time.Time{})
	s.refusedWith("no entries", api.Receipt{}, err, api.ErrRejected, "1 to 64 entries")
	_, err = s.propose("t 1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))
	s.refusedWith("an ID that is no ID", api.Receipt{}, err, api.ErrRejected, "choose one")
	_, err = s.module.ProposeSeaSettingsChange(context.Background(), api.ProposeSeaSettingsChange{CommandID: s.command(), TenantID: s.tenant, ActorID: "ann", ChangeID: "t1",
		Entries: []api.SeaSettingEntry{tenantEntry("SUPERVISOR_ONLY")}, Reason: " "})
	s.refusedWith("no reason", api.Receipt{}, err, api.ErrRejected, "reason of 1 to")
	_, err = s.propose("t1", "ann", before, tenantEntry("SUPERVISOR_ONLY"))
	s.refusedWith("a past effective time", api.Receipt{}, err, api.ErrRejected, "SETTINGS_EFFECTIVE_TIME")
	_, err = s.propose("t1", "ann", time.Now().UTC().Add(367*24*time.Hour), tenantEntry("SUPERVISOR_ONLY"))
	s.refusedWith("an effective time more than 366 days ahead", api.Receipt{}, err, api.ErrRejected, "SETTINGS_EFFECTIVE_TIME", "366 days")
	_, err = s.propose("t1", "ann", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), tenantEntry("SUPERVISOR_ONLY"))
	s.refusedWith("an effective time beyond what a command can hold", api.Receipt{}, err, api.ErrRejected, "SETTINGS_EFFECTIVE_TIME")
	_, err = s.propose("t1", "bob", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))
	s.refusedWith("no permission to configure", api.Receipt{}, err, api.ErrUnauthorized)
	_, err = s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))
	s.refusedWith("a change set ID in use", api.Receipt{}, err, api.ErrConflict)
	// one in force cannot be cancelled: a rollback is a new change set
	_, err = s.cancel("c1", "ann")
	s.refusedWith("cancelling a change in force", api.Receipt{}, err, api.ErrRejected, "SETTINGS_IN_FORCE", "new change set")
	// the first change set the commands of the tenant made was unaffected by all this
	if n := len(s.changes()); n != 7 {
		t.Fatalf("%d change sets, want f1, g1, c1, c2, c2b, w1 and n1: a refused proposal writes nothing", n)
	}

	// the history, with who and why and when
	h := s.changes()
	c2 := h["c2"]
	if c2.Status != "IN_FORCE" || c2.ProposedBy != "ann" || c2.ApprovedBy != "bob" || c2.AutoApproved || !c2.Loosening || c2.Version != v1+1 || c2.Reason != "because c2" ||
		c2.ProposedAt.IsZero() || !c2.ApprovedAt.After(c2.ProposedAt) || len(c2.Entries) != 1 || c2.Entries[0].ScopeID != "LE" || c2.Entries[0].Key != "sea.S1" ||
		c2.Entries[0].Value != "WITH_REASON" {
		t.Fatalf("history of c2: %+v", c2)
	}
	if c := h["c1"]; c.Status != "IN_FORCE" || !c.AutoApproved || c.ApprovedBy != "ann" || c.Version != v1 || c.Loosening {
		t.Fatalf("history of c1: %+v", c)
	}
	if c := h["c2b"]; c.Status != "PROPOSED" || c.ApprovedBy != "" || c.Version != 0 {
		t.Fatalf("history of c2b: %+v", c)
	}
	list, _ := s.module.ListSeaSettingsChanges(context.Background(), api.ListSeaSettingsChanges{TenantID: s.tenant})
	if list[0].ChangeID != "f1" || list[2].ChangeID != "c1" {
		t.Fatalf("the order of the history: %v", list)
	}
	// another tenant sees none of it
	other, err := s.module.ListSeaSettingsChanges(context.Background(), api.ListSeaSettingsChanges{TenantID: s.tenant + "-other"})
	if err != nil || len(other) != 0 {
		t.Fatalf("another tenant's history: %+v, %v", other, err)
	}
	if g, err := s.module.GetSeaSettings(context.Background(), api.GetSeaSettings{TenantID: s.tenant, ScopeKind: "LEGAL_ENTITY"}); err == nil {
		t.Fatalf("a legal entity without its ID: %+v", g)
	}
}

// Each command answers its receipt again when it is replayed, writes nothing twice, and refuses the same command id
// with another payload; a proposal, an approval, a refusal and a cancellation each have one event, and a proposal
// approved on the spot has both.
func TestSeaSettingsReplaysAndEventsOnPostgres(t *testing.T) {
	s := newSettingsWorld(t)
	ctx := context.Background()
	baseline := len(s.outboxTypes(t))
	propose := api.ProposeSeaSettingsChange{CommandID: "p-1", TenantID: s.tenant, ActorID: "ann", ChangeID: "c1", Reason: "tighten",
		Entries: []api.SeaSettingEntry{tenantEntry("SUPERVISOR_ONLY"), leEntry("LE2", "SUPERVISOR_ONLY")}}
	r := s.must(s.module.ProposeSeaSettingsChange(ctx, propose))
	if again, err := s.module.ProposeSeaSettingsChange(ctx, propose); err != nil || again != r {
		t.Fatalf("replay of the proposal: %+v, %v", again, err)
	}
	if c := s.changes()["c1"]; len(c.Entries) != 2 || c.Entries[0].ScopeKind != "TENANT" || c.Entries[1].ScopeID != "LE2" {
		t.Fatalf("the entries are not in the order proposed: %+v", c.Entries)
	}
	other := propose
	other.Reason = "something else"
	if _, err := s.module.ProposeSeaSettingsChange(ctx, other); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the command id with another payload: %v", err)
	}
	loosen := api.ProposeSeaSettingsChange{CommandID: "p-2", TenantID: s.tenant, ActorID: "ann", ChangeID: "c2", Reason: "loosen",
		Entries: []api.SeaSettingEntry{leEntry("LE", "WITH_REASON")}}
	s.must(s.module.ProposeSeaSettingsChange(ctx, loosen))
	approve := api.ApproveSeaSettingsChange{CommandID: "a-1", TenantID: s.tenant, ActorID: "bob", ChangeID: "c2"}
	ra := s.must(s.module.ApproveSeaSettingsChange(ctx, approve))
	if again, err := s.module.ApproveSeaSettingsChange(ctx, approve); err != nil || again != ra || ra.SettingsVersion == 0 {
		t.Fatalf("replay of the approval: %+v, %v", again, err)
	}
	s.must(s.module.ProposeSeaSettingsChange(ctx, api.ProposeSeaSettingsChange{CommandID: "p-3", TenantID: s.tenant, ActorID: "ann", ChangeID: "c3",
		Reason: "loosen the company", Entries: []api.SeaSettingEntry{tenantEntry("WITH_REASON")}}))
	refuse := api.RefuseSeaSettingsChange{CommandID: "r-1", TenantID: s.tenant, ActorID: "carl", ChangeID: "c3", Reason: "no"}
	rr := s.must(s.module.RefuseSeaSettingsChange(ctx, refuse))
	if again, err := s.module.RefuseSeaSettingsChange(ctx, refuse); err != nil || again != rr {
		t.Fatalf("replay of the refusal: %+v, %v", again, err)
	}
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	s.must(s.module.ProposeSeaSettingsChange(ctx, api.ProposeSeaSettingsChange{CommandID: "p-4", TenantID: s.tenant, ActorID: "ann", ChangeID: "c4",
		Reason: "later", EffectiveAt: future, Entries: []api.SeaSettingEntry{tenantEntry("SUPERVISOR_ONLY")}}))
	cancel := api.CancelSeaSettingsChange{CommandID: "x-1", TenantID: s.tenant, ActorID: "ann", ChangeID: "c4", Reason: "changed my mind"}
	rc := s.must(s.module.CancelSeaSettingsChange(ctx, cancel))
	if again, err := s.module.CancelSeaSettingsChange(ctx, cancel); err != nil || again != rc {
		t.Fatalf("replay of the cancellation: %+v, %v", again, err)
	}
	cancel.Reason = "another reason"
	if _, err := s.module.CancelSeaSettingsChange(ctx, cancel); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the cancellation with another payload: %v", err)
	}
	// a replay answers an actor still allowed, and no one else
	if _, err := s.module.ApproveSeaSettingsChange(ctx, api.ApproveSeaSettingsChange{CommandID: "a-1", TenantID: s.tenant, ActorID: "ann", ChangeID: "c2"}); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("a replay by an actor who may not approve: %v", err)
	}
	// replays and refusals wrote nothing more: four proposals, c1 and c4 approved on the spot, c2 after, c3 refused
	if n := s.count("settings_change"); n != 4 {
		t.Fatalf("%d proposals", n)
	}
	if n := s.count("settings_approval"); n != 3 {
		t.Fatalf("%d approvals", n)
	}
	if n := s.count("settings_cancellation"); n != 2 {
		t.Fatalf("%d cancellations", n)
	}
	types := s.outboxTypes(t)[baseline:]
	want := []string{"SeaSettingsChangeProposed.v1", "SeaSettingsChangeApproved.v1", "SeaSettingsChangeProposed.v1", "SeaSettingsChangeApproved.v1",
		"SeaSettingsChangeProposed.v1", "SeaSettingsChangeRefused.v1", "SeaSettingsChangeProposed.v1", "SeaSettingsChangeApproved.v1", "SeaSettingsChangeCancelled.v1"}
	// c1 proposed and approved on the spot; c2 proposed, then approved; c3 proposed, then refused; c4 proposed and
	// approved on the spot, then cancelled
	slices.Sort(types)
	slices.Sort(want)
	if !slices.Equal(types, want) {
		t.Fatalf("events %v, want %v", types, want)
	}
	// the events name the change set and the version an approval made, and no actor or reason
	var subject, payload string
	err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT subject, payload->>'settings_version' || ' ' || payload::text FROM bento.outbox WHERE scope=$1
          AND event_type='SeaSettingsChangeApproved.v1' AND subject='c2'`, s.tenant).Scan(&subject, &payload)
	})
	if err != nil || subject != "c2" || !strings.HasPrefix(payload, fmt.Sprintf("%d ", ra.SettingsVersion)) || strings.Contains(payload, "bob") || strings.Contains(payload, "ann") ||
		strings.Contains(payload, "loosen") {
		t.Fatalf("the approval's event: %s, %s, %v", subject, payload, err)
	}
	// a receipt that carries a notice answers it again
	notice := api.ProposeSeaSettingsChange{CommandID: "p-5", TenantID: s.tenant, ActorID: "ann", ChangeID: "c5", Reason: "tighten again",
		Entries: []api.SeaSettingEntry{tenantEntry("SUPERVISOR_ONLY")}}
	rn := s.must(s.module.ProposeSeaSettingsChange(ctx, notice))
	if !strings.Contains(rn.Notice, "LE keeps its own") {
		t.Fatalf("the notice: %q", rn.Notice)
	}
	if again, err := s.module.ProposeSeaSettingsChange(ctx, notice); err != nil || again != rn {
		t.Fatalf("replay of a proposal with a notice: %+v, %v", again, err)
	}
}

// holdSettings holds the tenant's settings lock, as SEA's settings commands take it, on a connection of its own until
// release is called. waiting counts the sessions it blocks, directly or through another waiter.
type heldSettings struct {
	t       *testing.T
	tx      pgx.Tx
	watch   *pgx.Conn
	pid     int
	tenant  string
	release func()
}

func holdSettings(t *testing.T, tenant string) *heldSettings {
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
	h := &heldSettings{t: t, tx: tx, tenant: tenant}
	if _, err = tx.Exec(ctx, seapg.SettingsLockSQL, tenant); err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&h.pid)
	}
	if err != nil {
		t.Fatalf("holding the settings lock: %v", err)
	}
	if h.watch, err = pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN")); err != nil {
		t.Fatal(err)
	}
	released := false
	h.release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
			conn.Close(ctx)
		}
	}
	t.Cleanup(func() { h.release(); h.watch.Close(context.Background()) })
	return h
}

// blocked counts the sessions blocked by the holder, and settingsWaiters those of them that wait in the settings lock
// itself (the lock statement is the session's query while it waits for it).
func (h *heldSettings) blocked() int {
	h.t.Helper()
	var n int
	if err := h.watch.QueryRow(context.Background(), blockedByHolder, h.pid).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *heldSettings) settingsWaiters() int {
	h.t.Helper()
	var n int
	if err := h.watch.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE pid <> $1 AND wait_event_type = 'Lock' AND wait_event = 'advisory'
        AND query LIKE '%cargo_sea.settings/%'`, h.pid).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// start runs a command in the background and returns once one more session is blocked by the holder than before.
func (h *heldSettings) start(name string, run func() error, results map[string]chan error) {
	h.t.Helper()
	before := h.blocked()
	done := make(chan error, 1)
	results[name] = done
	go func() { done <- run() }()
	for i := 0; i < 1200; i++ { // up to 30 seconds
		select {
		case err := <-done:
			h.t.Fatalf("%s did not wait for the lock: %v", name, err)
		case <-time.After(25 * time.Millisecond):
		}
		if h.blocked() > before {
			return
		}
	}
	h.t.Fatalf("%s is not waiting on a lock", name)
}

// queuedSettings holds the tenant's settings lock, starts the commands one after another (each only once the sessions
// before it wait in the lock the holder keeps, which pg_blocking_pids shows), releases it, and answers what each
// returned: the first started is granted the lock first, so the order of the commits is forced.
func (s *settingsWorld) queuedSettings(commands ...func() error) []error {
	s.t.Helper()
	h := holdSettings(s.t, s.tenant)
	results := map[string]chan error{}
	for i, run := range commands {
		h.start(fmt.Sprint(i), run, results)
	}
	if n := h.settingsWaiters(); n < len(commands) { // each waits in the advisory lock of the settings, not in something else
		s.t.Fatalf("%d sessions wait in the settings lock, want %d", n, len(commands))
	}
	h.release()
	errs := make([]error, len(commands))
	for i := range commands {
		errs[i] = result(s.t, fmt.Sprint(i), results[fmt.Sprint(i)])
	}
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

// Two identical commands at once, started while the settings lock is held: the second finds the first's receipt and
// answers it, so the change set is proposed once, with one approval and one event of each kind; for an approval too.
func TestSeaSettingsTwoIdenticalCommandsAtOnce(t *testing.T) {
	s := newSettingsWorld(t)
	ctx := context.Background()
	baseline := len(s.outboxTypes(t))
	propose := api.ProposeSeaSettingsChange{CommandID: "same-1", TenantID: s.tenant, ActorID: "ann", ChangeID: "c1", Reason: "tighten",
		Entries: []api.SeaSettingEntry{tenantEntry("SUPERVISOR_ONLY")}}
	var receipts [2]api.Receipt
	errs := s.queuedSettings(
		func() (err error) { receipts[0], err = s.module.ProposeSeaSettingsChange(ctx, propose); return },
		func() (err error) { receipts[1], err = s.module.ProposeSeaSettingsChange(ctx, propose); return })
	if ok, _ := pairOf(errs); ok != 2 || receipts[0] != receipts[1] || receipts[0].SettingsVersion != 1 {
		t.Fatalf("two identical proposals: %v, %+v", errs, receipts)
	}
	if s.count("settings_change") != 1 || s.count("settings_approval") != 1 || len(s.outboxTypes(t))-baseline != 2 {
		t.Fatalf("a proposal sent twice at once wrote %d change sets, %d approvals, %d events", s.count("settings_change"), s.count("settings_approval"), len(s.outboxTypes(t))-baseline)
	}
	s.must(s.propose("c2", "ann", time.Time{}, leEntry("LE", "WITH_REASON")))
	approve := api.ApproveSeaSettingsChange{CommandID: "same-2", TenantID: s.tenant, ActorID: "bob", ChangeID: "c2"}
	errs = s.queuedSettings(
		func() (err error) { receipts[0], err = s.module.ApproveSeaSettingsChange(ctx, approve); return },
		func() (err error) { receipts[1], err = s.module.ApproveSeaSettingsChange(ctx, approve); return })
	if ok, _ := pairOf(errs); ok != 2 || receipts[0] != receipts[1] || receipts[0].SettingsVersion != 2 || s.count("settings_approval") != 2 {
		t.Fatalf("two identical approvals: %v, %+v, %d approvals", errs, receipts, s.count("settings_approval"))
	}
	// the same command id with another payload, at once, is refused as a reuse of the id
	s.must(s.propose("c3", "ann", time.Now().UTC().Add(time.Hour), tenantEntry("SUPERVISOR_ONLY")))
	cancelOK := func(reason string) func() error {
		return func() error {
			_, err := s.module.CancelSeaSettingsChange(ctx, api.CancelSeaSettingsChange{CommandID: "same-4", TenantID: s.tenant, ActorID: "ann", ChangeID: "c3", Reason: reason})
			return err
		}
	}
	errs = s.queuedSettings(cancelOK("one reason"), cancelOK("another reason"))
	if ok, refused := pairOf(errs); ok != 1 || !errors.Is(refused, api.ErrIdempotencyConflict) || s.count("settings_cancellation") != 1 {
		t.Fatalf("one command id with two payloads: %v", errs)
	}
}

// Two approvals of one change set, started together while the settings lock is held: the first is granted, the second
// is refused as approved already, and the change set has one approval and the next version.
func TestSeaSettingsTwoApprovalsOfOneChangeSet(t *testing.T) {
	s := newSettingsWorld(t)
	s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("c3", "ann", time.Time{}, leEntry("LE", "WITH_REASON")))
	errs := s.queuedSettings(
		func() error { _, err := s.approve("c3", "bob"); return err },
		func() error { _, err := s.approve("c3", "carl"); return err })
	if errs[0] != nil || !errors.Is(errs[1], api.ErrRejected) || !strings.Contains(errs[1].Error(), "SETTINGS_ALREADY_APPROVED") {
		t.Fatalf("two approvals: %v", errs)
	}
	c := s.changes()["c3"]
	if c.ApprovedBy != "bob" || c.Version != 2 || s.count("settings_approval") != 2 {
		t.Fatalf("after two approvals: %+v, %d approvals", c, s.count("settings_approval"))
	}
	// two different change sets approved together are numbered in the order they were granted the lock
	s.must(s.propose("d3", "ann", time.Time{}, leEntry("LE2", "WITH_REASON")))
	s.must(s.propose("d4", "ann", time.Time{}, leEntry("LE2", "WITH_REASON"), tenantEntry("SUPERVISOR_ONLY")))
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
func TestSeaSettingsApprovalRacesCancellation(t *testing.T) {
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
			s.must(s.propose("c0", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
			s.must(s.propose("c1", "ann", c.effective, leEntry("LE", "WITH_REASON")))
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
			case c.approveOK && c.cancelOK && (h.Status != "CANCELLED" || approvals != 2 || cancellations != 1 || h.Version != 2):
				t.Fatalf("approved, then cancelled: %+v, %d, %d", h, approvals, cancellations)
			case c.approveOK && !c.cancelOK && (h.Status != "IN_FORCE" || approvals != 2 || cancellations != 0):
				t.Fatalf("approved and in force: %+v, %d, %d", h, approvals, cancellations)
			case !c.approveOK && (h.Status != "CANCELLED" || approvals != 1 || cancellations != 1 || h.Version != 0):
				t.Fatalf("cancelled: %+v, %d, %d", h, approvals, cancellations)
			}
			if got, _ := s.s1("LEGAL_ENTITY", "LE", future.Add(time.Hour)); got.Value == "WITH_REASON" && h.Status == "CANCELLED" {
				t.Fatalf("a cancelled change set is in force: %+v", got)
			}
		})
	}
}

// A proposal takes the tenant's settings lock too: while it is held, two proposals wait and then commit one after the
// other in the order they started, the first making version 1 and the second version 2.
func TestSeaSettingsProposalsWaitForTheSettingsLock(t *testing.T) {
	s := newSettingsWorld(t)
	errs := s.queuedSettings(
		func() error {
			_, err := s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))
			return err
		},
		func() error {
			_, err := s.propose("c2", "ann", time.Time{}, leEntry("LE", "SUPERVISOR_ONLY"))
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

// The settings are written one tenant at a time: another tenant's settings lock is not the one held, and its command
// does not wait.
func TestSeaSettingsLockIsTheTenants(t *testing.T) {
	s := newSettingsWorld(t)
	h := holdSettings(t, s.tenant+"-other")
	defer h.release()
	done := make(chan error, 1)
	go func() {
		_, err := s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))
		done <- err
	}()
	if err := result(t, "the proposal of another tenant", done); err != nil {
		t.Fatal(err)
	}
}
