//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.90 on PostgreSQL, through SEA's real commands: each container of the published facts states its own on-board date,
// and a loading that changes what the latest facts state for a container returns a FINAL movement to OPEN (the
// database's own rule, migration 000008, is the narrower one: it refuses a loading written to a FINAL movement only
// when it changes the movement's date, so it lets the wider reopening through).

// twoContainersReady leaves OM-1 OPEN with SH-1 in CV-1 and SH-2 in CV-2, both stuffed with a VGM, ready to finalize.
func (f *fixture) twoContainersReady(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	f.stuffOwnContainer(t)
	f.w.plan("SH-2", "C-1", 1)
	f.stuffing("SH-2", "X-2", 2, "22G1", "CN-SEAL-2")
	if _, err := f.associateAt("assoc-2", "OM-1", "SH-2", "", f.get(t, "OM-1").Revision, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assign("CV-2", "OM-1", 2, "TEMU1234565", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := f.seal("seal-2", "CV-2", 1, "CN-SEAL-2", api.SealShipper, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := f.stuffingDone("done-2", "CV-2", 2, "", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AssertVgm(ctx, api.AssertVgm{CommandID: "vgm-cv2", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-2", ExpectedRevision: 3,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 2200}, Method: api.VgmMethod1, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
		DeterminedAt: f.hours(4), Signatory: "Ada Lovelace", OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) finalizeAt(t *testing.T, command string, hours int) api.OceanDocFacts {
	t.Helper()
	if _, err := f.module.FinalizeOceanDocFacts(context.Background(), api.FinalizeOceanDocFacts{CommandID: command, TenantID: f.tenant, ActorID: "ops-2", MovementID: "OM-1",
		ExpectedRevision: f.get(t, "OM-1").Revision, OccurredAt: f.hours(hours)}); err != nil {
		t.Fatalf("%s: %v", command, err)
	}
	facts, err := f.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil || facts.Status != api.DocFactsFinal {
		t.Fatalf("%s: %+v %v", command, facts.Status, err)
	}
	return facts
}

// loadAt records the container's gate-in an hour before and its loading at the hour, as the terminal reports them.
func (f *fixture) loadAt(t *testing.T, visit string, hour int) {
	t.Helper()
	for _, e := range []struct {
		kind string
		at   int
	}{{api.EventGateIn, hour - 1}, {api.EventLoaded, hour}} {
		if _, err := f.module.RecordContainerEvent(context.Background(), api.RecordContainerEvent{CommandID: "ev-" + visit + "-" + e.kind, TenantID: f.tenant, ActorID: "ops-1",
			VisitID: visit, ExpectedRevision: f.visit(t, visit).Revision, Kind: e.kind, Location: unlocode("CNSHG"), HappenedAt: f.hours(e.at), Source: "terminal",
			Reference: "T-" + visit + "-" + e.kind, OccurredAt: f.hours(e.at)}); err != nil {
			t.Fatalf("%s %s: %v", visit, e.kind, err)
		}
	}
}

func boxDates(facts api.OceanDocFacts) map[string]time.Time {
	out := map[string]time.Time{}
	for _, c := range facts.Containers {
		out[c.VisitID] = c.OnBoardAt
	}
	return out
}

func TestEachContainerStatesItsOwnLoadingOnPostgres(t *testing.T) {
	f := newFixture(t)
	f.twoContainersReady(t)
	first := f.finalizeAt(t, "fin-1", 5)
	if d := boxDates(first); !first.ContainerDates || !d["CV-1"].IsZero() || !d["CV-2"].IsZero() || !first.OnBoardAt.IsZero() ||
		!strings.Contains(f.publishedContent(t, "OM-1", 1), `"container_dates":true`) || strings.Contains(f.publishedContent(t, "OM-1", 1), `"on_board_at"`) {
		t.Fatalf("version 1, nothing loaded: %+v %s", boxDates(first), f.publishedContent(t, "OM-1", 1))
	}
	// CV-1 loads at hour 8: the facts state none for it, the movement is reopened
	f.loadAt(t, "CV-1", 8)
	if m := f.get(t, "OM-1"); m.State != api.StateOpen {
		t.Fatalf("the first loading: %s", m.State)
	}
	second := f.finalizeAt(t, "fin-2", 9)
	if d := boxDates(second); !d["CV-1"].Equal(f.hours(8)) || !d["CV-2"].IsZero() || !second.OnBoardAt.Equal(f.hours(8)) || second.Version != 2 {
		t.Fatalf("version 2: %+v movement %s", boxDates(second), second.OnBoardAt)
	}
	// CV-2 loads at hour 11, after CV-1: the movement's date does not change, CV-2's does, so the movement is reopened
	f.loadAt(t, "CV-2", 11)
	if m := f.get(t, "OM-1"); m.State != api.StateOpen {
		t.Fatalf("the loading of the second container left the movement %s", m.State)
	}
	third := f.finalizeAt(t, "fin-3", 12)
	if d := boxDates(third); !d["CV-1"].Equal(f.hours(8)) || !d["CV-2"].Equal(f.hours(11)) || !third.OnBoardAt.Equal(f.hours(8)) || third.Version != 3 {
		t.Fatalf("version 3: %+v movement %s", boxDates(third), third.OnBoardAt)
	}
	// the earlier versions are as they were published
	if again, err := f.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1", Version: 2}); err != nil ||
		again.Status != api.DocFactsSuperseded || !boxDates(again)["CV-2"].IsZero() || again.SHA256 != second.SHA256 {
		t.Fatalf("version 2 read again: %+v %v", boxDates(again), err)
	}
}

func TestACarrierReportGivesAContainerItsDateOnPostgres(t *testing.T) {
	// a report that names no container, with two live containers: the movement's date and no container's
	f := newFixture(t)
	f.twoContainersReady(t)
	f.finalizeAt(t, "fin-1", 5)
	if r, err := f.carrierFact("load-bill", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "", "MAEU123456789", "LD-1", 7, 7); err != nil || f.get(t, "OM-1").State != api.StateOpen {
		t.Fatalf("a nameless report: %+v %v", r, err)
	}
	second := f.finalizeAt(t, "fin-2", 8)
	if d := boxDates(second); !d["CV-1"].IsZero() || !d["CV-2"].IsZero() || !second.OnBoardAt.Equal(f.hours(7)) {
		t.Fatalf("nameless report, two containers: %+v movement %s", boxDates(second), second.OnBoardAt)
	}
	// a report that names CV-2 gives it a date, though the movement's date stands
	if _, err := f.carrierFact("load-cv2", "OM-1", f.get(t, "OM-1").Revision, api.CarrierLoaded, "TEMU1234565", "", "LD-2", 9, 9); err != nil || f.get(t, "OM-1").State != api.StateOpen {
		t.Fatalf("a report naming CV-2: %v %s", err, f.get(t, "OM-1").State)
	}
	third := f.finalizeAt(t, "fin-3", 10)
	if d := boxDates(third); !d["CV-1"].IsZero() || !d["CV-2"].Equal(f.hours(9)) || !third.OnBoardAt.Equal(f.hours(7)) {
		t.Fatalf("named report: %+v movement %s", boxDates(third), third.OnBoardAt)
	}

	// with one live container, a report that names none is about it
	g := newFixture(t)
	g.publishOwnFacts(t)
	if _, err := g.carrierFact("load-bill", "OM-1", g.get(t, "OM-1").Revision, api.CarrierLoaded, "", "MAEU123456789", "LD-1", 6, 6); err != nil || g.get(t, "OM-1").State != api.StateOpen {
		t.Fatalf("a nameless report, one container: %v %s", err, g.get(t, "OM-1").State)
	}
	one := g.finalizeAt(t, "fin-2", 7)
	if d := boxDates(one); !d["CV-1"].Equal(g.hours(6)) || !one.OnBoardAt.Equal(g.hours(6)) {
		t.Fatalf("nameless report, one container: %+v movement %s", boxDates(one), one.OnBoardAt)
	}
}
