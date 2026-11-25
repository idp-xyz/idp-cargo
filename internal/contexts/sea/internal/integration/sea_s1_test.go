//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// V2.100 on PostgreSQL: S1, "a carrier report that contradicts SEA's records", read by the release of a carrier
// conflict. The day is the conflict day of sea_conflict_release_test.go: the carrier reports at hour 4 that
// CSQU3054383 was loaded, PHY sealed it again at 5, and the container never gated in. ops-9 may release a conflict
// (every actor may but the intruder), sup-1 holds the permission SuperviseCarrierConflicts, and the settings' actors
// are those of settingsWorld.

const releaseReason = "the line confirmed that the box was not loaded"

// conflictWorld is the settings world with the conflict day built: the movement OM-1 of the legal entity LE, open,
// whose container CV-1 has an unreleased conflict with the carrier's report.
func conflictWorld(t *testing.T) *settingsWorld {
	t.Helper()
	s := newSettingsWorldOf(t, "LE2")
	s.conflictDay(t)
	if got := pendingKinds(s.pending(t)); got != "GATED_IN_BEFORE_RESEAL" {
		t.Fatalf("the day has no conflict: %s", got)
	}
	return s
}

func (s *settingsWorld) acceptAs(actor, command string, hours int) (api.Receipt, error) {
	return s.module.AcceptCarrierLoadingConflict(context.Background(), api.AcceptCarrierLoadingConflict{CommandID: command, TenantID: s.tenant, ActorID: actor,
		VisitID: "CV-1", ExpectedRevision: s.visit(s.t, "CV-1").Revision, Reason: releaseReason, OccurredAt: s.hours(hours)})
}

// release answers the only release of CV-1 as the visit reads it.
func (s *settingsWorld) release(t *testing.T) api.CarrierConflictRelease {
	t.Helper()
	v := s.visit(t, "CV-1")
	if len(v.Releases) != 1 {
		t.Fatalf("%d releases, want one: %+v", len(v.Releases), v.Releases)
	}
	return v.Releases[0]
}

// times answers the time the release was entered at, and the times the change set was proposed and approved at.
func (s *settingsWorld) times(t *testing.T, change string) (entered, proposed, approved time.Time) {
	t.Helper()
	err := s.within(t, func(ctx context.Context, e bentopg.Executor) error {
		if err := e.QueryRow(ctx, `SELECT entered_at FROM cargo_sea.carrier_conflict_release WHERE tenant_id=$1 AND visit_id='CV-1' AND release_no=1`, s.tenant).Scan(&entered); err != nil {
			return err
		}
		return e.QueryRow(ctx, `SELECT c.proposed_at, a.approved_at FROM cargo_sea.settings_change c JOIN cargo_sea.settings_approval a
          ON a.tenant_id=c.tenant_id AND a.change_id=c.change_id WHERE c.tenant_id=$1 AND c.change_id=$2`, s.tenant, change).Scan(&proposed, &approved)
	})
	if err != nil {
		t.Fatal(err)
	}
	return entered, proposed, approved
}

func (s *settingsWorld) wantRefusedAsNoSupervisor(what string, err error) {
	s.t.Helper()
	if !errors.Is(err, api.ErrUnauthorized) || !strings.Contains(err.Error(), "SUPERVISOR_ONLY") || !strings.Contains(err.Error(), "only a supervisor") ||
		!strings.Contains(err.Error(), "ask a holder of SuperviseCarrierConflicts") {
		s.t.Fatalf("%s: %v", what, err)
	}
}

// Each value of S1 through the release: WITH_REASON, the standard value and today's behaviour, lets whoever may release
// a conflict do so, and records the settings it was decided under; SUPERVISOR_ONLY needs the permission as well, and
// refuses without it with the way out; the release records the version and the value; the item says who may release it
// under the value in force.
func TestS1EachValueOnTheRelease(t *testing.T) {
	s := conflictWorld(t)
	standard := func(it api.PendingItem) bool {
		return strings.Contains(it.Message, "S1 is WITH_REASON for this legal entity") && strings.Contains(it.Message, "whoever holds AcceptCarrierLoadingConflict may release it")
	}
	if items := s.pending(t); len(items) != 1 || !standard(items[0]) || strings.Contains(items[0].Message, "ask a supervisor") {
		t.Fatalf("with no settings set the item says WITH_REASON: %+v", items)
	}
	// the company tightens S1: the item says that only a supervisor may release it
	r := s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	if r.SettingsVersion != 1 {
		t.Fatalf("the change set: %+v", r)
	}
	items := s.pending(t)
	if len(items) != 1 || !strings.Contains(items[0].Message, "S1 is SUPERVISOR_ONLY for this legal entity") || !strings.Contains(items[0].Message, "only a supervisor") ||
		!strings.Contains(items[0].Message, "SuperviseCarrierConflicts") || !strings.Contains(items[0].Message, "ask a supervisor") ||
		!strings.Contains(items[0].Message, "release the conflict with a reason (AcceptCarrierLoadingConflict)") || strings.Contains(items[0].Message, "whoever holds") {
		t.Fatalf("under SUPERVISOR_ONLY the item says who may release it: %+v", items)
	}
	// the final declaration is held back under both values, and says both ways out: it does not read the settings
	if _, err := s.finalize("fin-1", 8); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "the carrier's report of a loading") ||
		!strings.Contains(err.Error(), "under WITH_REASON whoever holds AcceptCarrierLoadingConflict, under SUPERVISOR_ONLY only a supervisor") {
		t.Fatalf("finalize under SUPERVISOR_ONLY: %v", err)
	}
	before := len(s.outbox(t))
	revision := s.visit(t, "CV-1").Revision
	// whoever may release a conflict, without the permission: refused, with the way out, and nothing is written
	_, err := s.acceptAs("ops-9", "acc-1", 8)
	s.wantRefusedAsNoSupervisor("an operator", err)
	// it is not a stale revision, a missing reason or another refusal in disguise: the same release by
	// a supervisor passes
	if got := s.visit(t, "CV-1"); got.Revision != revision || len(got.Releases) != 0 || len(s.outbox(t)) != before {
		t.Fatalf("a refused release wrote: %+v", got)
	}
	if _, err = s.acceptAs("intruder", "acc-1", 8); !errors.Is(err, api.ErrUnauthorized) || strings.Contains(err.Error(), "SUPERVISOR_ONLY") {
		t.Fatalf("an actor who may not release a conflict at all: %v", err)
	}
	// the supervisor, who also may release a conflict
	got, err := s.acceptAs("sup-1", "acc-1", 8)
	if err != nil || got.ReleaseNo != 1 || got.VisitID != "CV-1" {
		t.Fatalf("a supervisor's release: %+v, %v", got, err)
	}
	rel := s.release(t)
	if rel.SettingsVersion != 1 || rel.S1 != "SUPERVISOR_ONLY" || rel.RecordedBy != "sup-1" || rel.Reason != releaseReason {
		t.Fatalf("the release records what it was decided under: %+v", rel)
	}
	items = s.pending(t)
	if pendingKinds(items) != "CARRIER_LOADING_CONFLICT_RELEASED (warning)" || !strings.Contains(items[0].Message, "sup-1") {
		t.Fatalf("after the release: %+v", items)
	}
	// the final declaration passes for anyone who may finalize, with no permission of the supervisor's, and the
	// published facts say nothing of the settings
	if _, err = s.finalize("fin-2", 9); err != nil {
		t.Fatalf("finalize after the release: %v", err)
	}
	facts, err := s.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: s.tenant, MovementID: "OM-1"})
	if err != nil || facts.Version != 1 || strings.Contains(strings.ToLower(string(facts.Content)), "setting") {
		t.Fatalf("the published facts: %+v, %v", facts, err)
	}
	// the event names the release and carries nothing of the settings or the reason
	for _, e := range s.outbox(t) {
		if e.Type == api.CarrierLoadingConflictReleased && (e.Payload.ReleaseNo != 1 || e.Payload.SettingsVersion != 0 || strings.Contains(e.Raw, "line confirmed") ||
			strings.Contains(e.Raw, "SUPERVISOR")) {
			t.Fatalf("the event: %s", e.Raw)
		}
	}
}

// The standard value through the release: with nothing set, and with WITH_REASON set explicitly, an operator releases
// the conflict with a reason, as in V2.99, and the release records the version in force and WITH_REASON.
func TestS1WithReasonIsTheBehaviourOfV299(t *testing.T) {
	for _, setIt := range []bool{false, true} {
		s := conflictWorld(t)
		version := 0
		if setIt {
			// the company keeps the standard value, which only keeps: approved on the spot
			version = s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("WITH_REASON"))).SettingsVersion
		}
		if _, err := s.acceptAs("ops-9", "acc-0", 8); err != nil {
			t.Fatalf("set %v: an operator releases it: %v", setIt, err)
		}
		rel := s.release(t)
		if rel.SettingsVersion != version || rel.S1 != "WITH_REASON" || rel.RecordedBy != "ops-9" {
			t.Fatalf("set %v: %+v", setIt, rel)
		}
	}
}

// S1 is read for the legal entity of the movement the container belongs to: its own value, else the company's, else the
// standard; another legal entity's value does not apply; the version is the tenant's.
func TestS1IsReadForTheLegalEntityOfTheMovement(t *testing.T) {
	for _, c := range []struct {
		name       string
		changes    func(*settingsWorld)
		supervisor bool // the release needs a supervisor
		version    int  // the version recorded
	}{
		{"the company's value", func(s *settingsWorld) { s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))) }, true, 1},
		{"the legal entity's own value", func(s *settingsWorld) { s.must(s.propose("c1", "ann", time.Time{}, leEntry("LE", "SUPERVISOR_ONLY"))) }, true, 1},
		{"another legal entity's value", func(s *settingsWorld) { s.must(s.propose("c1", "ann", time.Time{}, leEntry("LE2", "SUPERVISOR_ONLY"))) }, false, 1},
		{"the legal entity's own looser value under a stricter company", func(s *settingsWorld) {
			s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
			s.must(s.propose("c2", "ann", time.Time{}, leEntry("LE", "WITH_REASON")))
			s.must(s.approve("c2", "bob"))
		}, false, 2},
		{"the company's later tightening under the legal entity's own looser value", func(s *settingsWorld) {
			s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
			s.must(s.propose("c2", "ann", time.Time{}, leEntry("LE", "WITH_REASON")))
			s.must(s.approve("c2", "bob"))
			s.must(s.propose("c3", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))) // the company keeps its value: the exception stays
		}, false, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := conflictWorld(t)
			c.changes(s)
			_, err := s.acceptAs("ops-9", "acc-ops", 8)
			if c.supervisor {
				s.wantRefusedAsNoSupervisor("an operator", err)
				if _, err = s.acceptAs("sup-1", "acc-sup", 8); err != nil {
					t.Fatalf("a supervisor: %v", err)
				}
			} else if err != nil {
				t.Fatalf("an operator: %v", err)
			}
			want := "WITH_REASON"
			if c.supervisor {
				want = "SUPERVISOR_ONLY"
			}
			if rel := s.release(t); rel.S1 != want || rel.SettingsVersion != c.version {
				t.Fatalf("the release records %s at version %d, got %+v", want, c.version, rel)
			}
		})
	}
}

// A change set that takes effect in the future does not apply before: a release made now is decided under what is in
// force now and records that version, and the settings as of the later time say the change applies from then.
func TestS1AChangeInTheFutureDoesNotApplyBefore(t *testing.T) {
	s := conflictWorld(t)
	future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	r := s.must(s.propose("f1", "ann", future, tenantEntry("SUPERVISOR_ONLY")))
	if r.SettingsVersion != 1 {
		t.Fatalf("the change: %+v", r)
	}
	if items := s.pending(t); len(items) != 1 || !strings.Contains(items[0].Message, "S1 is WITH_REASON") {
		t.Fatalf("the item before the change takes effect: %+v", items)
	}
	if _, err := s.acceptAs("ops-9", "acc-0", 8); err != nil {
		t.Fatalf("an operator releases it before the change takes effect: %v", err)
	}
	if rel := s.release(t); rel.SettingsVersion != 0 || rel.S1 != "WITH_REASON" {
		t.Fatalf("the release records what was in force: %+v", rel)
	}
	if got, v := s.s1("LEGAL_ENTITY", "LE", future.Add(time.Minute)); got.Value != "SUPERVISOR_ONLY" || v != 1 {
		t.Fatalf("the change applies from its time: %+v, version %d", got, v)
	}
}

// A decision made is not reopened: a release made before S1 was tightened keeps its validity, and the conflict it
// released stays released; the same command again answers its receipt though its actor would now be refused.
func TestS1AReleaseBeforeAChangeKeepsItsValidity(t *testing.T) {
	s := conflictWorld(t)
	first, err := s.acceptAs("ops-9", "acc-1", 8)
	if err != nil {
		t.Fatal(err)
	}
	released := s.release(t)
	s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	if got := s.release(t); got != released || got.SettingsVersion != 0 || got.S1 != "WITH_REASON" {
		t.Fatalf("the release changed: %+v, was %+v", got, released)
	}
	if got := pendingKinds(s.pending(t)); got != "CARRIER_LOADING_CONFLICT_RELEASED (warning)" {
		t.Fatalf("the conflict is open again: %s", got)
	}
	again, err := s.module.AcceptCarrierLoadingConflict(context.Background(), api.AcceptCarrierLoadingConflict{CommandID: "acc-1", TenantID: s.tenant, ActorID: "ops-9",
		VisitID: "CV-1", ExpectedRevision: first.NewRevision - 1, Reason: releaseReason, OccurredAt: s.hours(8)})
	if err != nil || again != first {
		t.Fatalf("the same command again: %+v, %v, want %+v", again, err, first)
	}
	if _, err = s.finalize("fin-1", 9); err != nil {
		t.Fatalf("finalize with a release made before the change: %v", err)
	}
	if n := len(s.visit(t, "CV-1").Releases); n != 1 {
		t.Fatalf("%d releases", n)
	}
}

// The release reads the settings under the tenant's settings lock in shared mode, and reads the clock after it:
// deterministically, with the lock held by the test, the commands queued behind it and the order in which they are
// granted it forced. Behind a settings change that holds the lock, the release reads the new version (and is refused
// for an operator when the change tightens S1); granted the lock before the change, it reads the old one, and the
// change waits for the release to commit. The version a release records is the version the ledger says was in force at
// the time it is entered at.
func TestS1TheReleaseWaitsForASettingsChangeHoldingTheLock(t *testing.T) {
	t.Run("behind the change: the new version", func(t *testing.T) {
		s := conflictWorld(t)
		h := holdSettings(t, s.tenant)
		results := map[string]chan error{}
		h.start("change", func() error {
			_, err := s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))
			return err
		}, results)
		h.start("operator's release", func() error { _, err := s.acceptAs("ops-9", "acc-ops", 8); return err }, results)
		if n := h.settingsWaiters(); n != 2 {
			t.Fatalf("%d sessions wait in the settings lock, want the change and the release", n)
		}
		h.release()
		if err := result(t, "change", results["change"]); err != nil {
			t.Fatal(err)
		}
		s.wantRefusedAsNoSupervisor("the operator's release, which read the settings after the change", result(t, "operator's release", results["operator's release"]))
		if n := len(s.visit(t, "CV-1").Releases); n != 0 {
			t.Fatalf("a refused release wrote %d", n)
		}
		// the supervisor's, granted the lock after the change, reads the new version and is entered after it
		if _, err := s.acceptAs("sup-1", "acc-sup", 8); err != nil {
			t.Fatal(err)
		}
		rel := s.release(t)
		entered, _, approved := s.times(t, "c1")
		if rel.SettingsVersion != 1 || rel.S1 != "SUPERVISOR_ONLY" || !entered.After(approved) {
			t.Fatalf("the release %+v entered at %s, the change approved at %s", rel, entered, approved)
		}
	})
	t.Run("before the change: the old version, and the change waits", func(t *testing.T) {
		s := conflictWorld(t)
		h := holdSettings(t, s.tenant)
		results := map[string]chan error{}
		h.start("release", func() error { _, err := s.acceptAs("ops-9", "acc-ops", 8); return err }, results)
		h.start("change", func() error {
			_, err := s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))
			return err
		}, results)
		if n := h.settingsWaiters(); n != 2 {
			t.Fatalf("%d sessions wait in the settings lock, want the release and the change", n)
		}
		h.release()
		if err := result(t, "release", results["release"]); err != nil {
			t.Fatalf("the release, granted the lock first: %v", err)
		}
		if err := result(t, "change", results["change"]); err != nil {
			t.Fatal(err)
		}
		rel := s.release(t)
		entered, proposed, _ := s.times(t, "c1")
		if rel.SettingsVersion != 0 || rel.S1 != "WITH_REASON" || !entered.Before(proposed) {
			t.Fatalf("the release %+v entered at %s, the change proposed at %s", rel, entered, proposed)
		}
	})
}

// The final declaration reads no settings: it takes no lock of the settings and records no version. With the
// settings lock held by someone else it is refused for the open conflict, and passes once the conflict is
// released, without waiting.
func TestS1TheFinalDeclarationReadsNoSettings(t *testing.T) {
	s := conflictWorld(t)
	h := holdSettings(t, s.tenant)
	refusedAt := make(chan error, 1)
	go func() { _, err := s.finalize("fin-1", 8); refusedAt <- err }()
	if err := result(t, "finalize with an open conflict, while the settings lock is held", refusedAt); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("finalize with an open conflict: %v", err)
	}
	h.release()
	// the release needs the lock (it reads the settings), so it is made with the lock free; finalize is then run with
	// the lock held again
	if _, err := s.acceptAs("ops-9", "acc-1", 8); err != nil {
		t.Fatal(err)
	}
	h = holdSettings(t, s.tenant)
	passed := make(chan error, 1)
	go func() { _, err := s.finalize("fin-2", 9); passed <- err }()
	if err := result(t, "finalize with the conflict released, while the settings lock is held", passed); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	facts, err := s.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: s.tenant, MovementID: "OM-1"})
	if err != nil || facts.Version != 1 || facts.Status != api.DocFactsFinal {
		t.Fatalf("the published facts: %+v, %v", facts, err)
	}
	h.release()
}

// The release of a conflict reads the settings under the lock, so while a settings command holds it the release waits
// (the other tests show in which order); a release that has nothing to release is refused before it matters.
func TestS1AReleaseWithNothingToReleaseIsRefusedAsBefore(t *testing.T) {
	s := newSettingsWorldOf(t, "LE2")
	s.day(t) // no carrier report: no conflict
	s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	for _, actor := range []string{"ops-9", "sup-1"} {
		if _, err := s.acceptAs(actor, "acc-"+actor, 8); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "no unreleased conflict") || strings.Contains(err.Error(), "SUPERVISOR_ONLY") {
			t.Fatalf("%s: %v", actor, err)
		}
	}
}

// What a change of S1 would change, before it takes effect: for each legal entity whose S1 would differ, from
// what to what, with the open conflicts of its open movements and who could release them before and after; a
// query that writes nothing.
func TestSeaSettingsChangeImpactOnPostgres(t *testing.T) {
	s := conflictWorld(t)
	impact := func(change string, entries ...api.SeaSettingEntry) (api.SeaSettingsChangeImpact, error) {
		return s.module.GetSeaSettingsChangeImpact(context.Background(), api.GetSeaSettingsChangeImpact{TenantID: s.tenant, ChangeID: change, Entries: entries})
	}
	must := func(got api.SeaSettingsChangeImpact, err error) api.SeaSettingsChangeImpact {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	counts := func() [4]int {
		return [4]int{s.count("settings_change"), s.count("settings_approval"), s.count("settings_cancellation"), len(s.outbox(t))}
	}
	// the entries someone thinks of proposing, taking effect now: both legal entities would change
	got := must(impact("", tenantEntry("SUPERVISOR_ONLY")))
	if got.ChangeID != "" || got.TakesEffect.Before(time.Now().UTC().Add(-time.Minute)) || got.TakesEffect.After(time.Now().UTC().Add(time.Minute)) || len(got.Entities) != 2 {
		t.Fatalf("the impact of entries: %+v", got)
	}
	le, le2 := got.Entities[0], got.Entities[1]
	if le.LegalEntityID != "LE" || le.From != "WITH_REASON" || le.To != "SUPERVISOR_ONLY" || len(le.Conflicts) != 1 {
		t.Fatalf("LE: %+v", le)
	}
	if c := le.Conflicts[0]; c.MovementID != "OM-1" || c.VisitID != "CV-1" || c.ContainerNumber != "CSQU3054383" || c.FactNo != 1 ||
		c.ReleaseBefore != api.ReleaseByAnyOperator || c.ReleaseAfter != api.ReleaseBySupervisor {
		t.Fatalf("the conflict: %+v", c)
	}
	if le2.LegalEntityID != "LE2" || le2.From != "WITH_REASON" || le2.To != "SUPERVISOR_ONLY" || le2.Conflicts == nil || len(le2.Conflicts) != 0 {
		t.Fatalf("LE2 has no open movement: %+v", le2)
	}
	// a change that changes nothing has no impact; a change of one legal entity changes that one
	if got = must(impact("", tenantEntry("WITH_REASON"))); len(got.Entities) != 0 {
		t.Fatalf("a change of nothing: %+v", got.Entities)
	}
	if got = must(impact("", leEntry("LE", "SUPERVISOR_ONLY"))); len(got.Entities) != 1 || got.Entities[0].LegalEntityID != "LE" || len(got.Entities[0].Conflicts) != 1 {
		t.Fatalf("a change of one legal entity: %+v", got.Entities)
	}
	// LE2 keeps its own value (the standard, set on purpose) when the company tightens: only LE would change. The
	// company will tighten in an hour: a change set approved, not in force.
	s.must(s.propose("e1", "ann", time.Time{}, leEntry("LE2", "WITH_REASON")))
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	s.must(s.propose("f1", "ann", future, tenantEntry("SUPERVISOR_ONLY")))
	written := counts()
	got = must(impact("f1"))
	if got.ChangeID != "f1" || !got.TakesEffect.Equal(future) || len(got.Entities) != 1 || got.Entities[0].LegalEntityID != "LE" || got.Entities[0].From != "WITH_REASON" ||
		got.Entities[0].To != "SUPERVISOR_ONLY" || len(got.Entities[0].Conflicts) != 1 {
		t.Fatalf("the impact of a change set in the future: %+v", got)
	}
	// entries that would take effect before a change set approved for later are refused as a proposal would be
	if _, err := impact("", tenantEntry("SUPERVISOR_ONLY")); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "SETTINGS_ORDER") {
		t.Fatalf("entries before a change set that is to come: %v", err)
	}
	// the other direction: the company is SUPERVISOR_ONLY now (a change set in force), and a loosening that waits for
	// approval would let any operator release the conflict
	s.must(s.cancel("f1", "eve"))
	s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("w1", "ann", time.Time{}, tenantEntry("WITH_REASON")))
	got = must(impact("w1"))
	if len(got.Entities) != 1 || got.Entities[0].LegalEntityID != "LE" || got.Entities[0].From != "SUPERVISOR_ONLY" || got.Entities[0].To != "WITH_REASON" ||
		len(got.Entities[0].Conflicts) != 1 || got.Entities[0].Conflicts[0].ReleaseBefore != api.ReleaseBySupervisor || got.Entities[0].Conflicts[0].ReleaseAfter != api.ReleaseByAnyOperator ||
		got.TakesEffect.Before(time.Now().UTC().Add(-time.Minute)) {
		t.Fatalf("the impact of a loosening that waits: %+v", got)
	}
	// a change set that is not to come has no impact to tell, and the way out is named
	_, err := impact("c1")
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "IN_FORCE") || !strings.Contains(err.Error(), "ask about a change set that is proposed or approved") {
		t.Fatalf("a change set in force: %v", err)
	}
	s.must(s.refuse("w1", "carl"))
	if _, err = impact("w1"); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "REFUSED") {
		t.Fatalf("a refused change set: %v", err)
	}
	if _, err = impact("f1"); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "CANCELLED") {
		t.Fatalf("a cancelled change set: %v", err)
	}
	if _, err = impact("nope"); !errors.Is(err, api.ErrNotFound) || !strings.Contains(err.Error(), "ListSeaSettingsChanges") {
		t.Fatalf("an unknown change set: %v", err)
	}
	if _, err = impact("c1", tenantEntry("WITH_REASON")); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "one of the two") {
		t.Fatalf("a change set and entries: %v", err)
	}
	if _, err = impact(""); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "one of the two") {
		t.Fatalf("neither: %v", err)
	}
	if _, err = impact("", leEntry("LE-X", "SUPERVISOR_ONLY")); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "RegisterHblSeries") {
		t.Fatalf("entries for a legal entity SEA does not know: %v", err)
	}
	if _, err = impact("", leEntry("LE", "MAYBE")); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "SUPERVISOR_ONLY, WITH_REASON") {
		t.Fatalf("entries with a value there is not: %v", err)
	}
	// the conflict once released is no conflict to tell
	if _, err = s.acceptAs("sup-1", "acc-1", 8); err != nil {
		t.Fatal(err)
	}
	if got = must(impact("", leEntry("LE", "WITH_REASON"))); len(got.Entities) != 1 || got.Entities[0].Conflicts == nil || len(got.Entities[0].Conflicts) != 0 {
		t.Fatalf("after the release: %+v", got.Entities)
	}
	// none of the queries wrote anything: the counts moved only by the commands this test sent
	want := counts()
	for i := 0; i < 3; i++ {
		must(impact("", leEntry("LE", "WITH_REASON")))
	}
	if counts() != want {
		t.Fatalf("an impact query wrote: %v, was %v", counts(), want)
	}
	if written == want {
		t.Fatal("the counts did not move at all")
	}
}

// The impact is against what would be in force when the change set takes effect, not against what is in force now: with
// the company's tightening to come in half an hour, a change of one legal entity to the same value an hour from now
// changes nothing for it.
func TestSeaSettingsImpactIsAgainstWhatWillBeInForceThen(t *testing.T) {
	s := conflictWorld(t)
	s.must(s.propose("a1", "ann", time.Now().UTC().Add(30*time.Minute), tenantEntry("SUPERVISOR_ONLY")))
	s.must(s.propose("a2", "ann", time.Now().UTC().Add(time.Hour), leEntry("LE", "SUPERVISOR_ONLY")))
	got, err := s.module.GetSeaSettingsChangeImpact(context.Background(), api.GetSeaSettingsChangeImpact{TenantID: s.tenant, ChangeID: "a2"})
	if err != nil || len(got.Entities) != 0 {
		t.Fatalf("a change to what the company's change will have made already: %+v, %v", got, err)
	}
	got, err = s.module.GetSeaSettingsChangeImpact(context.Background(), api.GetSeaSettingsChangeImpact{TenantID: s.tenant, ChangeID: "a1"})
	if err != nil || len(got.Entities) != 2 || got.Entities[0].From != "WITH_REASON" || got.Entities[0].To != "SUPERVISOR_ONLY" {
		t.Fatalf("the company's change: %+v, %v", got, err)
	}
}
