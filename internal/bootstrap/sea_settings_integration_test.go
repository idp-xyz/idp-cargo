//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// V2.100: S1, a carrier report that contradicts SEA's records, with the real ORG, RATE, COM, FUL, PHY, BOOK and SEA and
// the platform's own permissions (permissions{}): the conflict of the day of reworkDay (K1 of the V2.89 tests: the
// carrier reports at 21 a loading of the master bill, naming no container; PHY reworks and seals again at 25), released
// under each value of S1. ops-9 holds "sea:*", which grants the release of a conflict and neither of the two names;
// sup-1 holds "sea:*" and "sea:SuperviseCarrierConflicts"; ann configures and bob approves, each by name.

// conflictDayUnderPermissions builds the V2.89 conflict day and puts SEA, over the same database, under the platform's
// permissions. Every call the test makes then names the principal it acts as.
func conflictDayUnderPermissions(t *testing.T, prefix string) *reworkDay {
	t.Helper()
	d := newReworkDay(t, prefix)
	d.stuffedWithTheFirstSeal()
	_, err := d.sea.RecordOceanCarrierFact(d.ctx, seaapi.RecordOceanCarrierFact{CommandID: "carrier-load", TenantID: d.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: d.movement().Revision, Kind: seaapi.CarrierLoaded, BillNumber: "LINU2611000457", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line",
		Reference: "LD-1", SHA256: "9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c", HappenedAt: d.hours(21), OccurredAt: d.hours(21)})
	d.must("the carrier's report", err)
	d.openRework()
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01", "SH-H02", "SH-H03"))
	d.reworkedAndRead()
	d.visit = d.cv().Revision
	d.must("VGM v2", d.vgm("vgm-2", 2950, 31))
	d.wantPending("the conflict", "GATED_IN_BEFORE_RESEAL")
	secured, err := d.platform.SEA(d.book, d.ful, permissions{})
	d.must("SEA under the platform's permissions", err)
	d.sea = secured
	return d
}

func (d *reworkDay) as(actor string, permissions ...string) context.Context {
	return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: d.tenant, ActorID: actor, Permissions: permissions})
}

func (d *reworkDay) release(actor string, perms []string, command string, hour int) (seaapi.Receipt, error) {
	return d.sea.AcceptCarrierLoadingConflict(d.as(actor, perms...), seaapi.AcceptCarrierLoadingConflict{CommandID: command, TenantID: d.tenant, ActorID: actor, VisitID: "CV-1",
		ExpectedRevision: d.cv().Revision, Reason: "the line's agent confirmed that the box was still at the CFS", OccurredAt: d.hours(hour)})
}

func (d *reworkDay) finalizeAs(command string, hour int) error {
	_, err := d.sea.FinalizeOceanDocFacts(d.as("ops-2", "sea:*"), seaapi.FinalizeOceanDocFacts{CommandID: command, TenantID: d.tenant, ActorID: "ops-2", MovementID: "OM-1",
		ExpectedRevision: d.movement().Revision, OccurredAt: d.hours(hour)})
	return err
}

func (d *reworkDay) s1(le string) (string, int) {
	d.t.Helper()
	got, err := d.sea.GetSeaSettings(d.ctx, seaapi.GetSeaSettings{TenantID: d.tenant, ScopeKind: "LEGAL_ENTITY", ScopeID: le})
	d.must("SEA's settings", err)
	return got.Items[0].Value, got.Version
}

var (
	ops  = []string{"sea:*"}
	sup  = []string{"sea:*", "sea:SuperviseCarrierConflicts"}
	conf = []string{"sea:ConfigureSettings"}
	appr = []string{"sea:ApproveSettings"}
)

// With S1 as it comes (WITH_REASON, the standard), the release is what it was before V2.100: an operator with "sea:*"
// releases the conflict with a reason, the release records the settings it was decided under (version 0 and
// WITH_REASON), and finalize then publishes.
func TestAConflictIsReleasedUnderTheStandardValueOfS1(t *testing.T) {
	d := conflictDayUnderPermissions(t, "cargo-ocean-s1-standard-")
	if value, version := d.s1("LE"); value != "WITH_REASON" || version != 0 {
		t.Fatalf("S1: %s, version %d", value, version)
	}
	item := d.pendingItem("GATED_IN_BEFORE_RESEAL")
	if !strings.Contains(item.Message, "S1 is WITH_REASON for this legal entity") || !strings.Contains(item.Message, "release the conflict with a reason (AcceptCarrierLoadingConflict)") {
		t.Fatalf("the item: %+v", item)
	}
	refusedWith(t, "finalize", d.finalizeAs("fin-1", 32), "the carrier's report of a loading", "under WITH_REASON whoever holds AcceptCarrierLoadingConflict, under SUPERVISOR_ONLY only a supervisor")
	r, err := d.release("ops-9", ops, "acc-1", 33)
	d.must("an operator's release", err)
	if r.ReleaseNo != 1 || r.VisitID != "CV-1" {
		t.Fatalf("the receipt: %+v", r)
	}
	rel := d.cv().Releases[0]
	if rel.RecordedBy != "ops-9" || rel.SettingsVersion != 0 || rel.S1 != "WITH_REASON" || rel.PHYSeal != "SEAL-X-1-NEW" {
		t.Fatalf("the release: %+v", rel)
	}
	d.wantPending("released", "CARRIER_LOADING_CONFLICT_RELEASED (warning)")
	d.must("finalize", d.finalizeAs("fin-2", 34))
	if facts := d.facts(); facts.Version != 1 || facts.Containers[0].SealNumber != "SEAL-X-1-NEW" {
		t.Fatalf("the published facts: %+v", facts)
	}
}

// With S1 SUPERVISOR_ONLY, set through SEA's settings commands under the permissions of the platform: "sea:*" grants
// the release of a conflict but not the supervisor's name, so the operator is refused with the way out and the
// supervisor, who holds the name, releases it; the release records the version and the value; the item says who may
// release it; and finalize holds back the conflict under this value too.
func TestAConflictIsReleasedUnderSupervisorOnlyByASupervisor(t *testing.T) {
	d := conflictDayUnderPermissions(t, "cargo-ocean-s1-supervisor-")
	propose := seaapi.ProposeSeaSettingsChange{CommandID: "cfg-1", TenantID: d.tenant, ActorID: "ann", ChangeID: "S1-STRICT", Reason: "audit asked for a supervisor",
		Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: seaapi.SettingS1, Value: seaapi.S1SupervisorOnly}}}
	// "sea:*" grants neither name of the settings; the names do
	for name, perms := range map[string][]string{"sea:*": {"sea:*"}, "*": {"*"}, "the approver's name": appr, "the supervisor's name": {"sea:SuperviseCarrierConflicts"}} {
		if _, err := d.sea.ProposeSeaSettingsChange(d.as("ann", perms...), propose); !errors.Is(err, seaapi.ErrUnauthorized) {
			t.Fatalf("a proposal by someone with %s: %v", name, err)
		}
	}
	r, err := d.sea.ProposeSeaSettingsChange(d.as("ann", conf...), propose)
	d.must("the proposal", err)
	if r.SettingsVersion != 1 || r.ChangeID != "S1-STRICT" {
		t.Fatalf("tightening S1 is approved at once: %+v", r)
	}
	if value, version := d.s1("LE"); value != "SUPERVISOR_ONLY" || version != 1 {
		t.Fatalf("S1: %s, version %d", value, version)
	}
	item := d.pendingItem("GATED_IN_BEFORE_RESEAL")
	if !strings.Contains(item.Message, "S1 is SUPERVISOR_ONLY for this legal entity") || !strings.Contains(item.Message, "ask a supervisor") {
		t.Fatalf("the item: %+v", item)
	}
	refusedWith(t, "finalize", d.finalizeAs("fin-1", 32), "the carrier's report of a loading", "under SUPERVISOR_ONLY only a supervisor", "SuperviseCarrierConflicts")
	// the operator: "sea:*" is no supervisor; the way out names the permission and what to do
	_, err = d.release("ops-9", ops, "acc-1", 33)
	if !errors.Is(err, seaapi.ErrUnauthorized) {
		t.Fatalf("an operator's release: %v", err)
	}
	refusedWith(t, "the operator", err, "SUPERVISOR_ONLY", "only a supervisor", "ask a holder of SuperviseCarrierConflicts")
	// nobody who holds only the supervisor's name releases a conflict either: the release needs its own permission
	if _, err = d.release("sup-0", []string{"sea:SuperviseCarrierConflicts"}, "acc-0", 33); !errors.Is(err, seaapi.ErrUnauthorized) {
		t.Fatalf("a release by someone who holds only the supervisor's name: %v", err)
	}
	if len(d.cv().Releases) != 0 {
		t.Fatal("a refused release wrote")
	}
	got, err := d.release("sup-1", sup, "acc-2", 33)
	d.must("the supervisor's release", err)
	if got.ReleaseNo != 1 {
		t.Fatalf("the receipt: %+v", got)
	}
	rel := d.cv().Releases[0]
	if rel.RecordedBy != "sup-1" || rel.SettingsVersion != 1 || rel.S1 != "SUPERVISOR_ONLY" || rel.PHYSeal != "SEAL-X-1-NEW" {
		t.Fatalf("the release: %+v", rel)
	}
	d.wantPending("released", "CARRIER_LOADING_CONFLICT_RELEASED (warning)")
	d.must("finalize", d.finalizeAs("fin-2", 34))
	if facts := d.facts(); facts.Version != 1 || facts.Containers[0].SealNumber != "SEAL-X-1-NEW" || strings.Contains(strings.ToLower(string(facts.Content)), "setting") {
		t.Fatalf("the published facts: %+v", facts)
	}
	// the impact, asked before the loosening takes effect: the conflict is released, so there is none to tell
	_, err = d.sea.ProposeSeaSettingsChange(d.as("ann", conf...), seaapi.ProposeSeaSettingsChange{CommandID: "cfg-2", TenantID: d.tenant, ActorID: "ann", ChangeID: "S1-LOOSE",
		Reason: "back to the standard", Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: seaapi.SettingS1, Value: seaapi.S1WithReason}}})
	d.must("the loosening waits", err)
	impact, err := d.sea.GetSeaSettingsChangeImpact(d.ctx, seaapi.GetSeaSettingsChangeImpact{TenantID: d.tenant, ChangeID: "S1-LOOSE"})
	d.must("the impact", err)
	if len(impact.Entities) != 1 || impact.Entities[0].LegalEntityID != "LE" || impact.Entities[0].From != "SUPERVISOR_ONLY" || impact.Entities[0].To != "WITH_REASON" ||
		len(impact.Entities[0].Conflicts) != 0 {
		t.Fatalf("the impact: %+v", impact)
	}
}

// A loosening of S1 is approved by another person, who holds the approver's name: a proposer cannot approve its own,
// "sea:*" does not approve, and once it is approved an operator releases the conflict again, the release recording the
// new version.
func TestALooseningOfS1IsApprovedByAnotherPersonAndTheOperatorReleases(t *testing.T) {
	d := conflictDayUnderPermissions(t, "cargo-ocean-s1-loosen-")
	_, err := d.sea.ProposeSeaSettingsChange(d.as("ann", conf...), seaapi.ProposeSeaSettingsChange{CommandID: "cfg-1", TenantID: d.tenant, ActorID: "ann", ChangeID: "S1-STRICT",
		Reason: "audit", Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: seaapi.SettingS1, Value: seaapi.S1SupervisorOnly}}})
	d.must("tighten", err)
	_, err = d.sea.ProposeSeaSettingsChange(d.as("ann", conf...), seaapi.ProposeSeaSettingsChange{CommandID: "cfg-2", TenantID: d.tenant, ActorID: "ann", ChangeID: "S1-LOOSE",
		Reason: "the audit is over", Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: seaapi.SettingS1, Value: seaapi.S1WithReason}}})
	d.must("loosen", err)
	if value, _ := d.s1("LE"); value != "SUPERVISOR_ONLY" {
		t.Fatalf("a loosening that waits is in force: %s", value)
	}
	approve := func(actor string, perms []string, command string) error {
		_, err := d.sea.ApproveSeaSettingsChange(d.as(actor, perms...), seaapi.ApproveSeaSettingsChange{CommandID: command, TenantID: d.tenant, ActorID: actor, ChangeID: "S1-LOOSE"})
		return err
	}
	if err = approve("bob", []string{"sea:*"}, "app-0"); !errors.Is(err, seaapi.ErrUnauthorized) {
		t.Fatalf("sea:* approves: %v", err)
	}
	if err = approve("ann", append(append([]string{}, conf...), appr...), "app-1"); !errors.Is(err, seaapi.ErrRejected) || !strings.Contains(err.Error(), "SETTINGS_SAME_ACTOR") {
		t.Fatalf("the proposer approves: %v", err)
	}
	if _, err = d.release("ops-9", ops, "acc-1", 33); !errors.Is(err, seaapi.ErrUnauthorized) {
		t.Fatalf("an operator under SUPERVISOR_ONLY: %v", err)
	}
	d.must("bob approves", approve("bob", appr, "app-2"))
	if value, version := d.s1("LE"); value != "WITH_REASON" || version != 2 {
		t.Fatalf("S1: %s, version %d", value, version)
	}
	if _, err = d.release("ops-9", ops, "acc-2", 33); err != nil {
		t.Fatalf("an operator under WITH_REASON: %v", err)
	}
	if rel := d.cv().Releases[0]; rel.SettingsVersion != 2 || rel.S1 != "WITH_REASON" {
		t.Fatalf("the release: %+v", rel)
	}
	d.must("finalize", d.finalizeAs("fin-1", 34))
}

// The settings of a legal entity that SEA does not know are refused, with the way out, through the real contexts too.
func TestSeaSettingsRefuseALegalEntityOfNoMovementAndNoSeries(t *testing.T) {
	d := conflictDayUnderPermissions(t, "cargo-ocean-s1-unknown-")
	_, err := d.sea.ProposeSeaSettingsChange(d.as("ann", conf...), seaapi.ProposeSeaSettingsChange{CommandID: "cfg-1", TenantID: d.tenant, ActorID: "ann", ChangeID: "S1-X",
		Reason: "x", EffectiveAt: time.Time{}, Entries: []seaapi.SeaSettingEntry{{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE-ELSEWHERE", Key: seaapi.SettingS1, Value: seaapi.S1SupervisorOnly}}})
	refusedWith(t, "an unknown legal entity", err, "LE-ELSEWHERE", "RegisterHblSeries")
	if !errors.Is(err, seaapi.ErrRejected) {
		t.Fatalf("the kind: %v", err)
	}
}
