//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// carrierFact records a carrier report of vessel EVER GIVEN, voyage 0123W, from the source "line". It names a
// container or a bill.
func (f *fixture) carrierFact(command, movement string, revision int64, kind, container, bill, reference string, happened, hours int) (api.Receipt, error) {
	return f.module.RecordOceanCarrierFact(context.Background(), api.RecordOceanCarrierFact{CommandID: command, TenantID: f.tenant, ActorID: "line-1",
		MovementID: movement, ExpectedRevision: revision, Kind: kind, ContainerNumber: container, BillNumber: bill, Vessel: "EVER GIVEN", Voyage: "0123W",
		Source: "line", Reference: reference, SHA256: strings.Repeat("cd", 32), HappenedAt: f.hours(happened), OccurredAt: f.hours(hours)})
}

// TestOceanCarrierFactsOnPostgres records the carrier's reports on a movement whose facts are final. A departure, a
// discharge and a roll leave it FINAL at its revision. A loading returns it to OPEN, because the published facts
// state no on-board date.
func TestOceanCarrierFactsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.publishOwnFacts(t)
	const master, box = "MAEU123456789", "CSQU3054383"
	final := func(what string, got api.Receipt, err error, fact int) {
		t.Helper()
		facts, factsErr := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
		if m := f.get(t, "OM-1"); err != nil || factsErr != nil || got.FactNo != fact || got.NewRevision != 5 || m.State != api.StateFinal ||
			m.Revision != 5 || facts.Status != api.DocFactsFinal || len(m.CarrierFacts) < fact {
			t.Fatalf("%s leaves the facts final: %+v %s rev %d %s %v %v", what, got, m.State, m.Revision, facts.Status, err, factsErr)
		}
	}
	got, err := f.carrierFact("dep-1", "OM-1", 5, api.CarrierDeparted, "", master, "DEP-1", 6, 6)
	final("a departure", got, err, 1)
	again, err := f.carrierFact("dep-1", "OM-1", 5, api.CarrierDeparted, "", master, "DEP-1", 6, 6)
	if err != nil || again != got {
		t.Fatalf("replay: %+v %v", again, err)
	}
	same, err := f.carrierFact("dep-2", "OM-1", 5, api.CarrierDeparted, "", master, "DEP-1", 6, 7)
	final("the same report under another command", same, err, 1)
	if len(f.get(t, "OM-1").CarrierFacts) != 1 {
		t.Fatal("the same report was recorded twice")
	}
	for name, try := range map[string]func() (api.Receipt, error){
		"the same reference with another kind": func() (api.Receipt, error) {
			return f.carrierFact("dep-other", "OM-1", 5, api.CarrierRolled, "", master, "DEP-1", 7, 7)
		},
		"a bill that is not the movement's": func() (api.Receipt, error) {
			return f.carrierFact("bill-other", "OM-1", 5, api.CarrierDeparted, "", "MAEU999999999", "DEP-9", 7, 7)
		},
		"a container that is not the movement's": func() (api.Receipt, error) {
			return f.carrierFact("box-other", "OM-1", 5, api.CarrierRolled, "MSKU0000080", "", "ROL-9", 7, 7)
		},
		"a container and a bill": func() (api.Receipt, error) {
			return f.carrierFact("both", "OM-1", 5, api.CarrierDeparted, box, master, "DEP-8", 7, 7)
		},
		"a kind SEA does not know": func() (api.Receipt, error) {
			return f.carrierFact("kind", "OM-1", 5, "ARRIVED", "", master, "ARR-1", 7, 7)
		},
		"a report dated after it is recorded": func() (api.Receipt, error) {
			return f.carrierFact("early", "OM-1", 5, api.CarrierDeparted, "", master, "DEP-7", 8, 7)
		},
	} {
		if _, err := try(); !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a house bill is named by its HBL; a container by its number
	hbl := f.get(t, "OM-1").Houses[0].HBL
	discharged, err := f.carrierFact("dis-1", "OM-1", 5, api.CarrierDischarged, "", hbl, "DIS-1", 7, 7)
	final("a discharge naming the house bill", discharged, err, 2)
	rolled, err := f.carrierFact("rol-1", "OM-1", 5, api.CarrierRolled, box, "", "ROL-1", 7, 7)
	final("a roll naming the container", rolled, err, 3)

	// the same source reference belongs to one movement of the tenant
	f.w.booking("BK-2", "C-2", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err = f.open("OM-2", "C-2", "BK-2"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.masterBill("bill-2", "OM-2", "MAEU222222222", api.MasterBillCarrierMBL, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = f.carrierFact("dep-om2", "OM-2", 2, api.CarrierDeparted, "", "MAEU222222222", "DEP-1", 2, 2); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a reference recorded on another movement: %v", err)
	}
	// a cancelled movement takes no report, through SEA or past it
	f.w.booking("BK-3", "C-3", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err = f.open("OM-3", "C-3", "BK-3"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.cancel("cancel-3", "OM-3", 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = f.carrierFact("dep-om3", "OM-3", 2, api.CarrierDeparted, "", "MAEU333333333", "DEP-3", 3, 3); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a report on a cancelled movement: %v", err)
	}

	// written past SEA: each refused at commit by the rule it breaks
	insert := `INSERT INTO cargo_sea.ocean_carrier_fact (tenant_id,movement_id,fact_no,kind,container_number,bill_number,vessel,voyage,source,reference,
        content_sha256,occurred_at,recorded_by,recorded_at,entered_at)
        VALUES ($1,'%s',%d,'%s',%s,%s,'EVER GIVEN','0123W','line','%s','` + strings.Repeat("cd", 32) + `',now(),'x',now(),now())`
	for name, c := range map[string]struct{ sql, reason string }{
		"a loading that leaves the movement final": {fmt.Sprintf(insert, "OM-1", 4, "LOADED_ON_BOARD", "'"+box+"'", "NULL", "PAST-1"), "on-board date"},
		"a container of another movement":          {fmt.Sprintf(insert, "OM-1", 4, "ROLLED", "'MSKU0000080'", "NULL", "PAST-2"), "live container"},
		"a bill of another movement":               {fmt.Sprintf(insert, "OM-1", 4, "DEPARTED", "NULL", "'MAEU222222222'", "PAST-3"), "current master bill"},
		"a number skipped":                         {fmt.Sprintf(insert, "OM-1", 9, "DEPARTED", "NULL", "'"+master+"'", "PAST-4"), "without gaps"},
		"a report on a cancelled movement":         {fmt.Sprintf(insert, "OM-3", 1, "DEPARTED", "NULL", "'MAEU333333333'", "PAST-5"), "open or final"},
	} {
		if err := f.past(t, c.sql); sqlState(err) != "23514" || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: %v (state %s), want 23514 with %q", name, err, sqlState(err), c.reason)
		}
	}

	// the loading: the published facts state no on-board date, so the movement returns to OPEN
	loaded, err := f.carrierFact("load-fact", "OM-1", 5, api.CarrierLoaded, box, "", "LD-9", 6, 7)
	mv := f.get(t, "OM-1")
	first, firstErr := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1", Version: 1})
	if err != nil || firstErr != nil || loaded.FactNo != 4 || loaded.NewRevision != 6 || mv.State != api.StateOpen || mv.Revision != 6 ||
		len(mv.CarrierFacts) != 4 || mv.CarrierFacts[3].ContainerNumber != box || first.Status != api.DocFactsSuperseded {
		t.Fatalf("a loading the published facts do not state: %+v %s %+v %v %v", loaded, mv.State, mv.CarrierFacts, err, firstErr)
	}
	var departed, superseded bool
	for _, e := range f.outbox(t) {
		if e.Type == api.OceanCarrierFactRecorded && e.Payload.FactNo == 1 && e.Payload.Reason == api.CarrierDeparted {
			departed = true
		}
		if e.Type == api.OceanDocFactsSuperseded && e.Payload.FactsVersion == 1 {
			superseded = true
		}
		if strings.Contains(e.Raw, "MAEU") || strings.Contains(e.Raw, "EVER") || strings.Contains(e.Raw, box) || strings.Contains(e.Raw, hbl) {
			t.Fatalf("an event carries a bill, a container number or the vessel: %s", e.Raw)
		}
	}
	if !departed || !superseded {
		t.Fatalf("the departure (%v) and the supersession (%v) were not both published", departed, superseded)
	}
}

// TestACoLoadTakesItsOnBoardDateFromTheCarrier finalizes a co-load, which has no container of its own, records the
// co-loader's report of the loading, and finalizes again: the second version states the on-board date.
func TestACoLoadTakesItsOnBoardDateFromTheCarrier(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	const bill = "SHA-HAM/2611-0042"
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	f.coLoad("BK-1")
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 1)
	if _, err := f.associate("assoc-1", "OM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.masterBill("coload-bill", "OM-1", bill, api.MasterBillCoLoad, 2, 1); err != nil {
		t.Fatal(err)
	}
	finalize := func(command string, revision int64, hours int) api.Receipt {
		t.Helper()
		r, err := f.module.FinalizeOceanDocFacts(ctx, api.FinalizeOceanDocFacts{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
			ExpectedRevision: revision, OccurredAt: f.hours(hours)})
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		return r
	}
	if r := finalize("fin-1", 3, 2); r.FactsVersion != 1 || r.NewRevision != 4 || strings.Contains(f.publishedContent(t, "OM-1", 1), "on_board_at") {
		t.Fatalf("the first version: %+v %s", r, f.publishedContent(t, "OM-1", 1))
	}
	loaded, err := f.carrierFact("load-1", "OM-1", 4, api.CarrierLoaded, "", bill, "LD-1", 3, 3)
	if m := f.get(t, "OM-1"); err != nil || loaded.FactNo != 1 || loaded.NewRevision != 5 || m.State != api.StateOpen {
		t.Fatalf("the co-loader's loading: %+v %s %v", loaded, m.State, err)
	}
	r := finalize("fin-2", 5, 4)
	facts, err := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil || r.FactsVersion != 2 || facts.Status != api.DocFactsFinal || !facts.OnBoardAt.Equal(f.hours(3)) || len(facts.Containers) != 0 ||
		!strings.Contains(f.publishedContent(t, "OM-1", 2), `"on_board_at":"`) {
		t.Fatalf("the second version: %+v %+v %v", r, facts, err)
	}
}
