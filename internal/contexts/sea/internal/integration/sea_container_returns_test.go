//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// V2.89 on PostgreSQL, migration 000011: a container taken back out of the terminal. The commands are SEA's real ones,
// with FUL's plans as the stand-in of sea_rework_test.go sets them (the day of that file: CV-1 stuffed at hour 3 with a
// VGM at hour 4, PHY's seal S1 at hour 1 and S2 at hour 5).

func (f *reworkFixture) event(command, visit string, revision int64, kind, reference string, happened time.Time, recorded int) (api.Receipt, error) {
	return f.module.RecordContainerEvent(context.Background(), api.RecordContainerEvent{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", VisitID: visit,
		ExpectedRevision: revision, Kind: kind, Location: unlocode("CNSHG"), HappenedAt: happened, Source: "terminal", Reference: reference, OccurredAt: f.hours(recorded)})
}

// ok fails the test on an error and returns the receipt: ok(f.event(...)).
func ok(t *testing.T) func(api.Receipt, error) api.Receipt {
	return func(r api.Receipt, err error) api.Receipt {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
}

func eventKinds(v api.ContainerVisit) string {
	var out []string
	for i := len(v.Events) - 1; i >= 0; i-- { // latest first
		out = append(out, fmt.Sprintf("%d:%s", v.Events[i].EventNo, v.Events[i].Kind))
	}
	return strings.Join(out, " ")
}

// A container is taken back out of the terminal while it is gated in, on an open or a final movement; the movement
// stays FINAL and its published facts are as they were; the same message again is a replay; the event is the container
// event with the new kind; a new VGM after it, like any change of the published facts, reopens the movement; a loaded
// container is not taken back.
func TestAContainerIsTakenBackOutOfTheTerminalOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newReworkFixture(t)
	f.day(t)
	v := f.visit(t, "CV-1")
	if _, err := f.event("out-early", "CV-1", v.Revision, api.EventGateOut, "GOUT-0", f.hours(5), 5); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a return on a VGM_SUBMITTED container that never gated in: %v", err)
	}
	if got := f.visit(t, "CV-1"); got.Revision != v.Revision || got.Status != api.VisitVGM || len(got.Events) != 0 {
		t.Fatalf("a refused return wrote: %+v", got)
	}
	r := ok(t)(f.event("gate-1", "CV-1", v.Revision, api.EventGateIn, "GIN-1", f.hours(5), 5))
	if got := f.visit(t, "CV-1"); got.Status != api.VisitGatedIn {
		t.Fatalf("gated in: %s", got.Status)
	}
	ok(t)(f.finalize("fin-1", 6))
	final := f.get(t, "OM-1")
	before, err := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil || final.State != api.StateFinal || before.Version != 1 || before.Status != api.DocFactsFinal {
		t.Fatalf("final: %+v %+v %v", final, before, err)
	}
	// the return on the FINAL movement: recorded, the movement and its facts stand
	r = ok(t)(f.event("out-1", "CV-1", r.NewRevision, api.EventGateOut, "GOUT-1", f.hours(6), 7))
	got := f.visit(t, "CV-1")
	after, err := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil || got.Status != api.VisitVGM || r.EventNo != 2 || eventKinds(got) != "1:GATE_IN_TERMINAL 2:GATE_OUT_TERMINAL" || got.Revision != r.NewRevision {
		t.Fatalf("the return: %+v %+v %v", r, got, err)
	}
	if m := f.get(t, "OM-1"); m.State != api.StateFinal || m.Revision != final.Revision || m.DocFactsVersion != 1 || !reflect.DeepEqual(before, after) {
		t.Fatalf("the movement stays FINAL with its facts: %+v", m)
	}
	// the event is the container event with the new kind as its reason, and nothing else of the movement was published
	published := 0
	for _, e := range f.outbox(t) {
		if e.Type == api.ContainerEventRecorded && e.Payload.Reason == api.EventGateOut {
			published++
			if e.Subject != "CV-1" || e.Payload.EventNo != 2 || e.Payload.MovementID != "OM-1" || strings.Contains(e.Raw, "GOUT-1") || strings.Contains(e.Raw, "CNSHG") {
				t.Fatalf("the return's event: %+v %s", e.Payload, e.Raw)
			}
		}
	}
	if published != 1 {
		t.Fatalf("%d return events published", published)
	}
	outboxBefore := len(f.outbox(t))
	// the replay: the same command, and the same source and reference under another command, change nothing
	if again, err := f.event("out-1", "CV-1", got.Revision-1, api.EventGateOut, "GOUT-1", f.hours(6), 7); err != nil || again != r {
		t.Fatalf("the same command again: %+v %v", again, err)
	}
	if same, err := f.event("out-again", "CV-1", got.Revision, api.EventGateOut, "GOUT-1", f.hours(6), 8); err != nil || same.EventNo != 2 || same.NewRevision != got.Revision {
		t.Fatalf("the same reference again: %+v %v", same, err)
	}
	if _, err = f.event("out-other", "CV-1", got.Revision, api.EventGateOut, "GOUT-1", f.hours(7), 8); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("the same reference with another time: %v", err)
	}
	if got2 := f.visit(t, "CV-1"); got2.Revision != got.Revision || len(f.outbox(t)) != outboxBefore {
		t.Fatalf("a replay wrote: %+v", got2)
	}
	if _, err = f.event("out-2", "CV-1", got.Revision, api.EventGateOut, "GOUT-2", f.hours(7), 8); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a second return without a gate-in: %v", err)
	}
	// a second round: in, out, in; the movement is still FINAL
	r = ok(t)(f.event("gate-2", "CV-1", got.Revision, api.EventGateIn, "GIN-2", f.hours(8), 8))
	r = ok(t)(f.event("out-3", "CV-1", r.NewRevision, api.EventGateOut, "GOUT-3", f.hours(9), 9))
	if got = f.visit(t, "CV-1"); got.Status != api.VisitVGM || eventKinds(got) != "1:GATE_IN_TERMINAL 2:GATE_OUT_TERMINAL 3:GATE_IN_TERMINAL 4:GATE_OUT_TERMINAL" ||
		f.get(t, "OM-1").State != api.StateFinal {
		t.Fatalf("a second round: %s %s", got.Status, eventKinds(got))
	}
	// what follows: a new VGM changes the published facts and reopens the movement, as on any VGM_SUBMITTED container
	r = ok(t)(f.vgm("vgm-2", r.NewRevision, 10))
	if m := f.get(t, "OM-1"); m.State != api.StateOpen || m.DocFactsVersion != 1 {
		t.Fatalf("a new VGM reopens: %+v", m)
	}
	r = ok(t)(f.event("gate-3", "CV-1", r.NewRevision, api.EventGateIn, "GIN-3", f.hours(11), 11))
	// a loaded container is not taken back, and the refusal writes nothing
	r = ok(t)(f.event("load-1", "CV-1", r.NewRevision, api.EventLoaded, "LOA-1", f.hours(12), 12))
	if _, err = f.event("out-4", "CV-1", r.NewRevision, api.EventGateOut, "GOUT-4", f.hours(13), 13); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "outside the system") {
		t.Fatalf("a return of a loaded container: %v", err)
	}
	if got = f.visit(t, "CV-1"); got.Status != api.VisitLoaded || got.Revision != r.NewRevision || !strings.HasSuffix(eventKinds(got), "6:LOADED_ON_BOARD") {
		t.Fatalf("the loaded container: %s rev %d %s", got.Status, got.Revision, eventKinds(got))
	}
}

// F3 with returns, through SEA's commands, the pending list at each stage: PHY seals S2 at hour 5, after the container
// gated in at hour 4.
func TestF3WithReturnsOnPostgres(t *testing.T) {
	t.Run("returned before the reseal", func(t *testing.T) {
		f := newReworkFixture(t)
		f.day(t)
		v := f.visit(t, "CV-1")
		r := ok(t)(f.event("gate-1", "CV-1", v.Revision, api.EventGateIn, "GIN-1", f.hours(4), 4))
		f.reseal()
		for _, s := range []string{"SH-1", "SH-2"} {
			ok(t)(f.refresh("refresh-"+s, s, 6))
		}
		r.NewRevision = f.visit(t, "CV-1").Revision
		if got := pendingKinds(f.pending(t)); got != "GATED_IN_BEFORE_RESEAL, VGM_BEFORE_RESEAL" {
			t.Fatalf("gated in, resealed: %s", got)
		}
		_, err := f.finalize("fin-f3", 7)
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "was at the terminal when PHY sealed it again") || !strings.Contains(err.Error(), "GATE_OUT_TERMINAL") ||
			!strings.Contains(err.Error(), "assert a new VGM") || !strings.Contains(err.Error(), "gate it in again") {
			t.Fatalf("F3: %v", err)
		}
		// the return, dated before the reseal, recorded afterwards
		r = ok(t)(f.event("out-1", "CV-1", r.NewRevision, api.EventGateOut, "GOUT-1", f.hours(4).Add(30*time.Minute), 7))
		if got := f.visit(t, "CV-1").Status; got != api.VisitVGM {
			t.Fatalf("returned: %s", got)
		}
		if got := pendingKinds(f.pending(t)); got != "VGM_BEFORE_RESEAL" {
			t.Fatalf("returned: %s", got)
		}
		if _, err = f.finalize("fin-f4", 7); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "no VGM submitted after PHY sealed it") {
			t.Fatalf("F4: %v", err)
		}
		r = ok(t)(f.vgm("vgm-2", r.NewRevision, 8))
		if got := pendingKinds(f.pending(t)); got != "" {
			t.Fatalf("a new VGM: %s", got)
		}
		r = ok(t)(f.event("gate-2", "CV-1", r.NewRevision, api.EventGateIn, "GIN-2", f.hours(9), 9))
		if got := pendingKinds(f.pending(t)); got != "" {
			t.Fatalf("gated in again: %s", got)
		}
		ok(t)(f.finalize("fin-ok", 10))
		facts, err := f.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
		if err != nil || facts.Containers[0].SealNumber != "S2" || facts.Containers[0].VGMAssertionID != "CV-1:2" || f.visit(t, "CV-1").Status != api.VisitGatedIn {
			t.Fatalf("published: %+v %v", facts.Containers, err)
		}
	})
	t.Run("returned after the reseal", func(t *testing.T) {
		f := newReworkFixture(t)
		f.day(t)
		v := f.visit(t, "CV-1")
		r := ok(t)(f.event("gate-1", "CV-1", v.Revision, api.EventGateIn, "GIN-1", f.hours(4), 4))
		f.reseal()
		for _, s := range []string{"SH-1", "SH-2"} {
			ok(t)(f.refresh("refresh-"+s, s, 6))
		}
		r.NewRevision = f.visit(t, "CV-1").Revision
		r = ok(t)(f.event("out-1", "CV-1", r.NewRevision, api.EventGateOut, "GOUT-1", f.hours(5).Add(30*time.Minute), 7))
		r = ok(t)(f.vgm("vgm-2", r.NewRevision, 8))
		if got := pendingKinds(f.pending(t)); got != "RETURN_DATED_AFTER_RESEAL (warning)" {
			t.Fatalf("returned after the reseal: %s", got)
		}
		ok(t)(f.finalize("fin-ok", 9))
	})
	t.Run("loaded", func(t *testing.T) {
		f := newReworkFixture(t)
		f.day(t)
		v := f.visit(t, "CV-1")
		r := ok(t)(f.event("gate-1", "CV-1", v.Revision, api.EventGateIn, "GIN-1", f.hours(4), 4))
		r = ok(t)(f.event("load-1", "CV-1", r.NewRevision, api.EventLoaded, "LOA-1", f.hours(4).Add(30*time.Minute), 5))
		f.reseal()
		for _, s := range []string{"SH-1", "SH-2"} {
			ok(t)(f.refresh("refresh-"+s, s, 6))
		}
		if got := pendingKinds(f.pending(t)); got != "GATED_IN_BEFORE_RESEAL, VGM_BEFORE_RESEAL" {
			t.Fatalf("loaded, resealed: %s", got)
		}
		_, err := f.finalize("fin-f3", 7)
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "was loaded before PHY sealed it again") || !strings.Contains(err.Error(), "handled outside the system") ||
			strings.Contains(err.Error(), "GATE_OUT_TERMINAL") {
			t.Fatalf("F3: %v", err)
		}
		r.NewRevision = f.visit(t, "CV-1").Revision
		if _, err = f.event("out-1", "CV-1", r.NewRevision, api.EventGateOut, "GOUT-1", f.hours(5), 7); !errors.Is(err, api.ErrRejected) {
			t.Fatalf("a loaded container is not taken back: %v", err)
		}
	})
}

// returnsFixture is four houses SH-1 to SH-4 in four containers CV-1 to CV-4 (units 1 to 4), each stuffed and with a
// VGM: CV-1 gated in; CV-2 only VGM_SUBMITTED; CV-3 gated in and loaded; CV-4 gated in and taken back. The raw writes
// of the migration's rules are made on them.
func newReturnsFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	numbers := []string{"CSQU3054383", "TEMU1234565", "HLXU1234561", "MSKU0000080"}
	for i, s := range []string{"SH-1", "SH-2", "SH-3", "SH-4"} {
		f.w.plan(s, "C-1", 1)
		f.stuffing(s, fmt.Sprintf("X-%d", i+1), i+1, "22G1", fmt.Sprintf("CN-SEAL-%d", i+1))
		if _, err := f.associate("assoc-"+s, "OM-1", s, "", f.get(t, "OM-1").Revision); err != nil {
			t.Fatal(err)
		}
		visit := fmt.Sprintf("CV-%d", i+1)
		r, err := f.assign(visit, "OM-1", i+1, numbers[i], 1)
		if err != nil {
			t.Fatal(err)
		}
		if r, err = f.seal("seal-"+visit, visit, r.NewRevision, fmt.Sprintf("CN-SEAL-%d", i+1), api.SealShipper, 2); err != nil {
			t.Fatal(err)
		}
		if r, err = f.stuffingDone("done-"+visit, visit, r.NewRevision, "", 3); err != nil {
			t.Fatal(err)
		}
		if _, err = f.module.AssertVgm(ctx, api.AssertVgm{CommandID: "vgm-" + visit, TenantID: f.tenant, ActorID: "ops-1", VisitID: visit, ExpectedRevision: r.NewRevision,
			Mass: kg(3700), Method: api.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: f.hours(4), Signatory: "Ada", OccurredAt: f.hours(4)}); err != nil {
			t.Fatal(err)
		}
	}
	rf := &reworkFixture{fixture: f, t: t}
	ev := func(visit, kind, ref string, hour int) {
		t.Helper()
		r, err := rf.event("e-"+ref, visit, f.visit(t, visit).Revision, kind, ref, f.hours(hour), hour)
		ok(t)(r, err)
	}
	ev("CV-1", api.EventGateIn, "GIN-1", 5)
	ev("CV-3", api.EventGateIn, "GIN-3", 5)
	ev("CV-3", api.EventLoaded, "LOA-3", 6)
	ev("CV-4", api.EventGateIn, "GIN-4", 5)
	ev("CV-4", api.EventGateOut, "GOUT-4", 6)
	for visit, want := range map[string]string{"CV-1": api.VisitGatedIn, "CV-2": api.VisitVGM, "CV-3": api.VisitLoaded, "CV-4": api.VisitVGM} {
		if got := f.visit(t, visit).Status; got != want {
			t.Fatalf("%s is %s, want %s", visit, got, want)
		}
	}
	return f
}

// rawEvent is an event written past SEA: now() for its times, so that it is not before the visit was assigned.
func rawEvent(visit string, no int, kind string) string {
	return fmt.Sprintf(`INSERT INTO cargo_sea.container_event (tenant_id,visit_id,event_no,kind,location_code,occurred_at,source,reference,recorded_by,recorded_at,entered_at)
        VALUES ($1,'%s',%d,'%s','CNSHG',now(),'raw','RAW-%s-%d','x',now(),now())`, visit, no, kind, visit, no)
}

func rawStep(visit, status string) string {
	return fmt.Sprintf(`UPDATE cargo_sea.container_visit SET status='%s', revision=revision+1 WHERE tenant_id=$1 AND visit_id='%s'`, status, visit)
}

// rawTx runs the statements in one transaction past SEA, in the tenant's scope, and commits.
func (f *fixture) rawTx(t *testing.T, statements ...string) error {
	t.Helper()
	return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, s := range statements {
			if _, err := e.Exec(ctx, s, f.tenant); err != nil {
				return err
			}
		}
		return nil
	})
}

// The database's rules of V2.89, written past SEA as the application role: each refuses by name, the refused write
// leaves the visit as it was, and the writes SEA itself makes are accepted. What only the domain keeps is written too.
func TestTheDatabaseRulesOfTheReturnOnPostgres(t *testing.T) {
	f := newReturnsFixture(t)
	state := func(visit string) string {
		v := f.visit(t, visit)
		return fmt.Sprintf("%s rev %d %s", v.Status, v.Revision, eventKinds(v))
	}
	before := map[string]string{}
	for _, v := range []string{"CV-1", "CV-2", "CV-3", "CV-4"} {
		before[v] = state(v)
	}
	const (
		changeMessage = "leaves ASSIGNED once"
		gateMessage   = "latest gate-in comes after its latest gate-out"
		returnMessage = "a gate-out follows a gate-in of the same container with no gate-out between them"
	)
	for name, tc := range map[string]struct {
		statements []string
		state      string
		in         string
	}{
		// check_visit_change: GATED_IN to VGM_SUBMITTED changes only the status, by one revision; no other move is new
		"the return step changes another column too": {[]string{rawEvent("CV-1", 2, "GATE_OUT_TERMINAL"), `UPDATE cargo_sea.container_visit SET status='VGM_SUBMITTED',
          release_reason='x', revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`}, "23001", changeMessage},
		"the return step moves the revision by two": {[]string{rawEvent("CV-1", 2, "GATE_OUT_TERMINAL"), `UPDATE cargo_sea.container_visit SET status='VGM_SUBMITTED',
          revision=revision+2 WHERE tenant_id=$1 AND visit_id='CV-1'`}, "23001", changeMessage},
		"gated in back to stuffed":      {[]string{rawStep("CV-1", "STUFFED")}, "23001", changeMessage},
		"gated in back to assigned":     {[]string{rawStep("CV-1", "ASSIGNED")}, "23001", changeMessage},
		"loaded back to VGM_SUBMITTED":  {[]string{rawEvent("CV-3", 3, "GATE_OUT_TERMINAL"), rawStep("CV-3", "VGM_SUBMITTED")}, "23001", changeMessage},
		"loaded back to gated in":       {[]string{rawStep("CV-3", "GATED_IN")}, "23001", changeMessage},
		"VGM_SUBMITTED back to stuffed": {[]string{rawStep("CV-2", "STUFFED")}, "23001", changeMessage},
		"returned back to stuffed":      {[]string{rawStep("CV-4", "STUFFED")}, "23001", changeMessage},
		// visit_inconsistency: gated in or loaded exactly when the latest gate-in comes after the latest gate-out
		"returned status without the return": {[]string{rawStep("CV-1", "VGM_SUBMITTED")}, "23514", gateMessage},
		"the return without the status":      {[]string{rawEvent("CV-1", 2, "GATE_OUT_TERMINAL"), `UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`}, "23514", gateMessage},
		"gated in status after the return":   {[]string{rawStep("CV-4", "GATED_IN")}, "23514", gateMessage},
		"a gate-in without the status":       {[]string{rawEvent("CV-4", 3, "GATE_IN_TERMINAL"), `UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-4'`}, "23514", gateMessage},
		"loaded and returned":                {[]string{rawEvent("CV-3", 3, "GATE_OUT_TERMINAL"), `UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-3'`}, "23514", gateMessage},
		// no gate-out after a loading, though the latest gate-in comes after the latest gate-out
		"a gate-out after a loading, gated in again": {[]string{rawEvent("CV-3", 3, "GATE_OUT_TERMINAL"), rawEvent("CV-3", 4, "GATE_IN_TERMINAL"),
			`UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-3'`}, "23514", "no gate-out comes after a loading"},
		// loaded exactly when a loading is recorded
		"loaded without a loading": {[]string{rawStep("CV-4", "GATED_IN"), rawEvent("CV-4", 3, "GATE_IN_TERMINAL"), `UPDATE cargo_sea.container_visit SET status='LOADED', revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-4'`}, "23514", "loaded exactly when"},
		// a gate-out follows a gate-in of the same visit with no gate-out between them
		"a return that follows no gate-in": {[]string{rawEvent("CV-2", 1, "GATE_OUT_TERMINAL"), `UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-2'`}, "23514", returnMessage},
		"a second return in a row":         {[]string{rawEvent("CV-4", 3, "GATE_OUT_TERMINAL"), `UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-4'`}, "23514", returnMessage},
		"two returns after one gate-in":    {[]string{rawEvent("CV-1", 2, "GATE_OUT_TERMINAL"), rawEvent("CV-1", 3, "GATE_OUT_TERMINAL"), rawStep("CV-1", "VGM_SUBMITTED")}, "23514", returnMessage},
		// the kind check
		"another kind than the list": {[]string{rawEvent("CV-1", 2, "GATE_OUT"), `UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`}, "23514", "container_event_kind_check"},
	} {
		err := f.rawTx(t, tc.statements...)
		if state := sqlState(err); state != tc.state || !strings.Contains(refusal(err), tc.in) {
			t.Errorf("%s: %v (state %s, %s), want %s %s", name, err, state, refusal(err), tc.state, tc.in)
		}
	}
	for v, was := range before {
		if got := state(v); got != was {
			t.Fatalf("a refused write changed %s: %s, was %s", v, got, was)
		}
	}
	// what SEA writes: the gate-out with the status step, in one transaction
	if err := f.rawTx(t, rawEvent("CV-1", 2, "GATE_OUT_TERMINAL"), rawStep("CV-1", "VGM_SUBMITTED")); err != nil {
		t.Fatalf("the return as SEA writes it: %v", err)
	}
	if got := state("CV-1"); !strings.HasPrefix(got, "VGM_SUBMITTED") || !strings.HasSuffix(got, "1:GATE_IN_TERMINAL 2:GATE_OUT_TERMINAL") {
		t.Fatalf("CV-1: %s", got)
	}
	// and a second round of the returned container, written past SEA: in, then loaded
	if err := f.rawTx(t, rawEvent("CV-4", 3, "GATE_IN_TERMINAL"), rawStep("CV-4", "GATED_IN")); err != nil {
		t.Fatalf("gated in again: %v", err)
	}
	if err := f.rawTx(t, rawEvent("CV-4", 4, "LOADED_ON_BOARD"), rawStep("CV-4", "LOADED")); err != nil {
		t.Fatalf("loaded after a return: %v", err)
	}
	// what only the domain keeps: the dates (a return dated before the gate-in it follows is accepted)
	if err := f.rawTx(t, rawEvent("CV-2", 1, "GATE_IN_TERMINAL"), rawStep("CV-2", "GATED_IN"), `INSERT INTO cargo_sea.container_event (tenant_id,visit_id,event_no,kind,location_code,occurred_at,source,reference,recorded_by,recorded_at,entered_at)
        VALUES ($1,'CV-2',2,'GATE_OUT_TERMINAL','CNSHG',now() - interval '1 hour','raw','RAW-CV-2-2','x',now(),now())`, rawStep("CV-2", "VGM_SUBMITTED")); err != nil {
		t.Fatalf("the database accepts a return dated before its gate-in: %v", err)
	}
}

// bothReturnedAndLoaded asks the database whether the visit has a gate-out and a loading recorded.
func (f *fixture) bothReturnedAndLoaded(t *testing.T, visit string) bool {
	t.Helper()
	var both bool
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT coalesce(bool_or(kind='GATE_OUT_TERMINAL'), false) AND coalesce(bool_or(kind='LOADED_ON_BOARD'), false)
          FROM cargo_sea.container_event WHERE tenant_id=$1 AND visit_id=$2`, f.tenant, visit).Scan(&both)
	}); err != nil {
		t.Fatal(err)
	}
	return both
}

// A return and a loading of the same visit race, deterministically: a second connection holds the movement's lock, so
// that the first command waits for it holding the visit and the second waits for the visit (blockedByHolder counts
// only the sessions the test's own holder blocks, directly or through a waiter). Both name the revision the visit had;
// the one that took the visit first is accepted and the other is a conflict, which with the new revision is refused by
// the rules: whichever commits first, the visit is never returned and loaded.
func TestAReturnRacingALoadingOfTheSameVisitOnPostgres(t *testing.T) {
	for _, order := range [][]string{{"return", "load"}, {"load", "return"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			f := newReworkFixture(t)
			f.day(t)
			v := f.visit(t, "CV-1")
			r := ok(t)(f.event("gate-1", "CV-1", v.Revision, api.EventGateIn, "GIN-1", f.hours(5), 5))
			hold := f.holdMovement(t)
			commands := map[string]func(int64, string) error{
				"return": func(revision int64, command string) error {
					_, err := f.event(command, "CV-1", revision, api.EventGateOut, "GOUT-R", f.hours(6), 7)
					return err
				},
				"load": func(revision int64, command string) error {
					_, err := f.event(command, "CV-1", revision, api.EventLoaded, "LOA-R", f.hours(6), 7)
					return err
				},
			}
			results := map[string]chan error{}
			for _, name := range order {
				name := name
				hold.start(name, func() error { return commands[name](r.NewRevision, "race-"+name) }, results)
			}
			hold.release()
			outcome := map[string]error{}
			for _, name := range order {
				outcome[name] = result(t, name, results[name])
			}
			first, second := order[0], order[1]
			if outcome[first] != nil {
				t.Fatalf("%s went first: %v", first, outcome[first])
			}
			if !errors.Is(outcome[second], api.ErrConflict) {
				t.Fatalf("%s after %s, with the old revision: %v", second, first, outcome[second])
			}
			after := f.visit(t, "CV-1")
			if err := commands[second](after.Revision, "race-"+second+"-again"); !errors.Is(err, api.ErrRejected) {
				t.Fatalf("%s after %s, with the revision now: %v", second, first, err)
			}
			final := f.visit(t, "CV-1")
			want, kinds := api.VisitVGM, "1:GATE_IN_TERMINAL 2:GATE_OUT_TERMINAL"
			if first == "load" {
				want, kinds = api.VisitLoaded, "1:GATE_IN_TERMINAL 2:LOADED_ON_BOARD"
			}
			if final.Status != want || eventKinds(final) != kinds || final.Revision != after.Revision || f.bothReturnedAndLoaded(t, "CV-1") {
				t.Fatalf("after %s then %s: %s %s", first, second, final.Status, eventKinds(final))
			}
		})
	}
}

// holdWrite is a transaction on a second connection that has made its writes and not committed; waiting counts the
// sessions it blocks, directly or through another waiter.
type heldWrite struct {
	tx      pgx.Tx
	waiting func() int
}

func (f *fixture) holdWrite(t *testing.T, statements ...string) *heldWrite {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenant); err != nil {
		t.Fatal(err)
	}
	for _, s := range statements {
		if _, err = tx.Exec(ctx, s, f.tenant); err != nil {
			t.Fatal(err)
		}
	}
	watch, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { watch.Close(context.Background()) })
	return &heldWrite{tx: tx, waiting: func() int {
		var n int
		if err := watch.QueryRow(ctx, blockedByHolder, tx.Conn().PgConn().PID()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}}
}

// The same race written past SEA: one transaction has written the return and not committed, a second writes the
// loading of the same visit and waits for the visit's row; once the first commits, the second finds the visit returned
// and is refused by the database (and the other way round), so that no visit is ever both.
func TestAReturnRacingALoadingPastSEAOnPostgres(t *testing.T) {
	for _, order := range []string{"return-first", "load-first"} {
		t.Run(order, func(t *testing.T) {
			f := newReworkFixture(t)
			f.day(t)
			v := f.visit(t, "CV-1")
			ok(t)(f.event("gate-1", "CV-1", v.Revision, api.EventGateIn, "GIN-1", f.hours(5), 5))
			returning := []string{rawStep("CV-1", "VGM_SUBMITTED"), rawEvent("CV-1", 2, "GATE_OUT_TERMINAL")}
			loading := []string{rawStep("CV-1", "LOADED"), rawEvent("CV-1", 2, "LOADED_ON_BOARD")}
			firstWrites, secondWrites, kind, status := returning, loading, "GATE_OUT_TERMINAL", api.VisitVGM
			if order == "load-first" {
				firstWrites, secondWrites, kind, status = loading, returning, "LOADED_ON_BOARD", api.VisitLoaded
			}
			held := f.holdWrite(t, firstWrites...)
			done := make(chan error, 1)
			go func() { done <- f.rawTx(t, secondWrites...) }()
			waited := false
			for i := 0; i < 1200 && !waited; i++ { // up to 30 seconds
				select {
				case err := <-done:
					t.Fatalf("the second write did not wait for the first: %v", err)
				case <-time.After(25 * time.Millisecond):
				}
				waited = held.waiting() > 0
			}
			if !waited {
				t.Fatal("the second write is not waiting on a lock")
			}
			if err := held.tx.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := result(t, "the second write", done)
			if sqlState(err) != "23001" || !strings.Contains(refusal(err), "leaves ASSIGNED once") {
				t.Fatalf("the second write: %v", err)
			}
			final := f.visit(t, "CV-1")
			if final.Status != status || !strings.HasSuffix(eventKinds(final), "2:"+kind) || f.bothReturnedAndLoaded(t, "CV-1") {
				t.Fatalf("%s: %s %s", order, final.Status, eventKinds(final))
			}
		})
	}
}

// A container gated in before PHY's reseal and loaded after it is told it is handled outside the system, and a return
// dated before its gate-in is refused (by the model only: the database accepts it).
func TestALoadingAfterTheResealAndAnEarlyReturnOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	v := f.visit(t, "CV-1")
	r := ok(t)(f.event("gate-1", "CV-1", v.Revision, api.EventGateIn, "GIN-1", f.hours(4), 4))
	if _, err := f.event("out-early", "CV-1", r.NewRevision, api.EventGateOut, "GOUT-0", f.hours(3), 4); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "give the time it was taken back out") {
		t.Fatalf("a return dated before the gate-in: %v", err)
	}
	f.reseal()
	for _, s := range []string{"SH-1", "SH-2"} {
		ok(t)(f.refresh("refresh-"+s, s, 6))
	}
	r.NewRevision = f.visit(t, "CV-1").Revision
	ok(t)(f.event("load-1", "CV-1", r.NewRevision, api.EventLoaded, "LOA-1", f.hours(6), 7))
	_, err := f.finalize("fin-f3", 8)
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "loaded since") || !strings.Contains(err.Error(), "handled outside the system") || strings.Contains(err.Error(), "GATE_OUT_TERMINAL") {
		t.Fatalf("F3: %v", err)
	}
}

// A carrier's loading report that names no container, dated before PHY's reseal: with one live container it can only be
// about that container and blocks as a named report does; with a second live container it only warns (the second
// container assigned later changes the answer from then on); a released second visit is not live.
func TestACarrierReportNamingNoContainerOnlyWarnsOnPostgres(t *testing.T) {
	const master = "MAEU123456789"
	prepare := func(t *testing.T) *reworkFixture {
		f := newReworkFixture(t)
		f.day(t)
		ok(t)(f.carrierFact("load-fact", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "", master, "LD-9", 4, 4))
		f.reseal()
		for _, s := range []string{"SH-1", "SH-2"} {
			ok(t)(f.refresh("refresh-"+s, s, 6))
		}
		return f
	}
	t.Run("one live container", func(t *testing.T) {
		f := prepare(t)
		if got := pendingKinds(f.pending(t)); got != "GATED_IN_BEFORE_RESEAL, VGM_BEFORE_RESEAL" {
			t.Fatalf("pending: %s", got)
		}
		_, err := f.finalize("fin-f3", 7)
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "the carrier's report of a loading") || !strings.Contains(err.Error(), "naming no container, while this is the movement's only live container") ||
			!strings.Contains(err.Error(), "handled outside the system") {
			t.Fatalf("F3: %v", err)
		}
	})
	t.Run("a second live container assigned later", func(t *testing.T) {
		f := prepare(t)
		ok(t)(f.assign("CV-2", "OM-1", 2, "TEMU1234565", 7))
		got := pendingKinds(f.pending(t))
		if !strings.Contains(got, "CARRIER_LOADING_BEFORE_RESEAL (warning)") || strings.Contains(got, "GATED_IN_BEFORE_RESEAL") {
			t.Fatalf("pending: %s", got)
		}
	})
	t.Run("a released second visit", func(t *testing.T) {
		f := prepare(t)
		r := ok(t)(f.assign("CV-2", "OM-1", 2, "TEMU1234565", 7))
		ok(t)(f.release("release-2", "CV-2", r.NewRevision, "not needed", 8))
		if got := pendingKinds(f.pending(t)); got != "GATED_IN_BEFORE_RESEAL, VGM_BEFORE_RESEAL" {
			t.Fatalf("pending: %s", got)
		}
	})
	t.Run("one live container at the terminal (its own events first)", func(t *testing.T) {
		f := newReworkFixture(t)
		f.day(t)
		v := f.visit(t, "CV-1")
		ok(t)(f.event("gate-1", "CV-1", v.Revision, api.EventGateIn, "GIN-1", f.hours(4), 4))
		ok(t)(f.carrierFact("load-fact", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "", master, "LD-9", 4, 4))
		f.reseal()
		for _, s := range []string{"SH-1", "SH-2"} {
			ok(t)(f.refresh("refresh-"+s, s, 6))
		}
		_, err := f.finalize("fin-f3", 7)
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "was at the terminal when PHY sealed it again") || !strings.Contains(err.Error(), "GATE_OUT_TERMINAL") {
			t.Fatalf("F3: %v", err)
		}
	})
}
