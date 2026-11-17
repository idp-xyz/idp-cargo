//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/air/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
)

// TestAGivenNumberDoesNotStopTheSeries keys HAWB numbers of the series by hand before the sequence reaches them: each
// draw passes over a number that is taken and gives the next free one.
func TestAGivenNumberDoesNotStopTheSeries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "781-12345675", 1, f.at.Add(72*time.Hour))
	if _, err := f.open("AM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	for i, tc := range []struct{ given, want string }{
		{"SHA0000001", "SHA0000001"}, // keyed by hand: the number the series would draw first
		{"", "SHA0000002"},           // drawn: 1 is taken and passed over
		{"SHA0000004", "SHA0000004"}, // keyed by hand, ahead of the sequence
		{"", "SHA0000003"},           // drawn: the next free number
		{"", "SHA0000005"},           // drawn: 4 is taken and passed over
	} {
		shipment := fmt.Sprintf("SH-%d", i+1)
		f.w.plan(shipment, "C-1", 2)
		r, err := f.associate("assoc-"+shipment, "AM-1", shipment, tc.given, int64(1+i))
		if err != nil || r.HAWB != tc.want {
			t.Fatalf("%s given %q: %+v %v, want %s", shipment, tc.given, r, err, tc.want)
		}
	}
	if s, err := f.module.GetHawbSeries(ctx, api.GetHawbSeries{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil || len(s.Last) != 1 ||
		s.Last[0] != (api.NumberPeriod{Period: "", Last: 5}) {
		t.Fatalf("series: %+v %v", s, err)
	}
}

// TestOneCommandIDIsOneCommand sends a refresh and an end of one house with the same command id and the same fields:
// the end is not answered with the refresh's receipt. A command recorded before the hash covered the command's name is
// still replayed.
func TestOneCommandIDIsOneCommand(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "781-12345675", 1, f.at.Add(72*time.Hour))
	if _, err := f.open("AM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 2)
	if _, err := f.associate("assoc", "AM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}
	f.w.measured("SH-1", 1, "XR-0001")
	refresh := api.RefreshHouseWaybillFacts{CommandID: "X", TenantID: f.tenant, ActorID: "ops-1", MovementID: "AM-1", ExpectedRevision: 2, ShipmentID: "SH-1",
		OccurredAt: f.at.Add(2 * time.Hour)}
	refreshed, err := f.module.RefreshHouseWaybillFacts(ctx, refresh)
	if err != nil || refreshed.NewRevision != 3 || refreshed.EntryNo != 2 {
		t.Fatalf("refresh: %+v %v", refreshed, err)
	}
	f.w.mu.Lock()
	p := f.w.plans["SH-1"]
	p.Status, p.ConsolidationID = fulapi.StatusCancelled, ""
	f.w.plans["SH-1"] = p
	f.w.mu.Unlock()
	end := api.EndHouseWaybillAssociation{CommandID: "X", TenantID: f.tenant, ActorID: "ops-1", MovementID: "AM-1", ExpectedRevision: 2, ShipmentID: "SH-1",
		OccurredAt: f.at.Add(2 * time.Hour)}
	if r, err := f.module.EndHouseWaybillAssociation(ctx, end); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("an end under a refresh's command id: %+v %v", r, err)
	}
	if m := f.get(t, "AM-1"); m.Revision != 3 || len(m.Houses) != 1 {
		t.Fatalf("the house is still live: %+v", m)
	}
	if again, err := f.module.RefreshHouseWaybillFacts(ctx, refresh); err != nil || again != refreshed {
		t.Fatalf("the refresh replayed: %+v %v", again, err)
	}
	end.CommandID, end.ExpectedRevision = "Y", 3
	if r, err := f.module.EndHouseWaybillAssociation(ctx, end); err != nil || r.EntryNo != 2 || r.NewRevision != 4 {
		t.Fatalf("the end under its own command id: %+v %v", r, err)
	}

	// A receipt as AIR wrote it before V2.62, with the hash of the command's fields alone: the same command is replayed
	// from it, and the id with other fields is still a conflict.
	old := api.OpenAirMovement{CommandID: "open-before-v2.62", TenantID: f.tenant, ActorID: "ops-1", MovementID: "AM-OLD", ConsolidationID: "C-OLD",
		BookingID: "BK-OLD", OccurredAt: f.at}
	blob, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	if err = f.within(t, func(txCtx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(txCtx, `INSERT INTO cargo_air.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision)
            VALUES ($1,$2,$3,'AirMovement','AM-OLD',1)`, f.tenant, old.CommandID, hex.EncodeToString(sum[:]))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r, err := f.module.OpenAirMovement(ctx, old); err != nil || r != (api.Receipt{CommandID: old.CommandID, MovementID: "AM-OLD", NewRevision: 1}) {
		t.Fatalf("a command recorded before V2.62, replayed: %+v %v", r, err)
	}
	old.BookingID = "BK-OTHER"
	if _, err = f.module.OpenAirMovement(ctx, old); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("its command id with other fields: %v", err)
	}
}
