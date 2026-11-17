//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// TestAGivenNumberDoesNotStopTheSeries registers a series whose first number is 41 and keys numbers of the series by
// hand before the sequence reaches them: each draw passes over a number that is taken and gives the next free one.
func TestAGivenNumberDoesNotStopTheSeries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	register := func(command string, first int64) error {
		_, err := f.module.RegisterHblSeries(ctx, api.RegisterHblSeries{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", LegalEntityID: "LE",
			Template: "SHAL{seq:7}", FirstNumber: first, OccurredAt: f.at})
		return err
	}
	if err := register("series-0", 0); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a first number of zero: %v", err)
	}
	if err := register("series-wide", 10_000_000); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a first number wider than the sequence: %v", err)
	}
	if err := register("series", 41); err != nil {
		t.Fatal(err)
	}
	if err := register("series-again", 1); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a second series for the legal entity: %v", err)
	}
	stands := func() int64 {
		t.Helper()
		s, err := f.module.GetHblSeries(ctx, api.GetHblSeries{TenantID: f.tenant, LegalEntityID: "LE"})
		if err != nil || s.FirstNumber != 41 || len(s.Last) != 1 || s.Last[0].Period != "" {
			t.Fatalf("series: %+v %v", s, err)
		}
		return s.Last[0].Last
	}
	if at := stands(); at != 40 {
		t.Fatalf("before a number is taken the sequence stands at the one before the first: %d", at)
	}
	for i, tc := range []struct{ given, want string }{
		{"SHAL0000041", "SHAL0000041"}, // keyed by hand: the number the series would draw first
		{"", "SHAL0000042"},            // drawn: 41 is taken and passed over
		{"SHAL0000044", "SHAL0000044"}, // keyed by hand, ahead of the sequence
		{"", "SHAL0000043"},            // drawn: the next free number
		{"", "SHAL0000045"},            // drawn: 44 is taken and passed over
	} {
		shipment := fmt.Sprintf("SH-%d", i+1)
		f.w.plan(shipment, "C-1", 2)
		r, err := f.associate("assoc-"+shipment, "OM-1", shipment, tc.given, int64(1+i))
		if err != nil || r.HBL != tc.want {
			t.Fatalf("%s given %q: %+v %v, want %s", shipment, tc.given, r, err, tc.want)
		}
	}
	if at := stands(); at != 45 {
		t.Fatalf("the sequence stands at %d, want 45", at)
	}
	for hbl, shipment := range map[string]string{"SHAL0000041": "SH-1", "SHAL0000042": "SH-2", "SHAL0000043": "SH-4", "SHAL0000044": "SH-3", "SHAL0000045": "SH-5"} {
		if ref, err := f.module.FindHouseBill(ctx, api.FindHouseBill{TenantID: f.tenant, LegalEntityID: "LE", HBL: hbl}); err != nil ||
			ref.ShipmentID != shipment || ref.MovementID != "OM-1" {
			t.Fatalf("%s: %+v %v", hbl, ref, err)
		}
	}
}

// TestOneCommandIDIsOneCommand sends a refresh and an end of one house with the same command id and the same fields:
// the end is not answered with the refresh's receipt.
func TestOneCommandIDIsOneCommand(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 2)
	if _, err := f.associate("assoc", "OM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}
	f.w.stuffed("SH-1", 1, "CN-SEAL-1")
	refresh := api.RefreshHouseBillFacts{CommandID: "X", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1", ExpectedRevision: 2, ShipmentID: "SH-1",
		OccurredAt: f.hours(2)}
	refreshed, err := f.module.RefreshHouseBillFacts(ctx, refresh)
	if err != nil || refreshed.NewRevision != 3 || refreshed.EntryNo != 2 {
		t.Fatalf("refresh: %+v %v", refreshed, err)
	}
	f.w.mu.Lock()
	p := f.w.plans["SH-1"]
	p.Status, p.ConsolidationID = fulapi.StatusCancelled, ""
	f.w.plans["SH-1"] = p
	f.w.mu.Unlock()
	end := api.EndHouseBillAssociation{CommandID: "X", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1", ExpectedRevision: 2, ShipmentID: "SH-1",
		OccurredAt: f.hours(2)}
	if r, err := f.module.EndHouseBillAssociation(ctx, end); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("an end under a refresh's command id: %+v %v", r, err)
	}
	if m := f.get(t, "OM-1"); m.Revision != 3 || len(m.Houses) != 1 {
		t.Fatalf("the house is still live: %+v", m)
	}
	if again, err := f.module.RefreshHouseBillFacts(ctx, refresh); err != nil || again != refreshed {
		t.Fatalf("the refresh replayed: %+v %v", again, err)
	}
	end.CommandID, end.ExpectedRevision = "Y", 3
	if r, err := f.module.EndHouseBillAssociation(ctx, end); err != nil || r.EntryNo != 2 || r.NewRevision != 4 {
		t.Fatalf("the end under its own command id: %+v %v", r, err)
	}
	if m := f.get(t, "OM-1"); len(m.Houses) != 0 {
		t.Fatalf("the house is ended: %+v", m)
	}
}
