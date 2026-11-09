package com

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type comStub struct {
	snapshot comapi.ObligationSnapshotV3
	err      error
}

func (c comStub) GetObligationSnapshot(context.Context, comapi.GetObligationSnapshot) (comapi.ObligationSnapshot, error) {
	return comapi.ObligationSnapshot{}, errors.New("FUL reads ObligationSnapshot v3 only")
}

func (c comStub) GetObligationSnapshotV2(context.Context, comapi.GetObligationSnapshot) (comapi.ObligationSnapshotV2, error) {
	return comapi.ObligationSnapshotV2{}, errors.New("FUL reads ObligationSnapshot v3 only")
}

func (c comStub) GetObligationSnapshotV3(context.Context, comapi.GetObligationSnapshot) (comapi.ObligationSnapshotV3, error) {
	return c.snapshot, c.err
}

func reserved() comapi.ObligationSnapshotV3 {
	return comapi.ObligationSnapshotV3{TenantID: "T", ObligationID: "OB-1", ObligationVersion: 1, LatestVersion: 1,
		Status: comapi.ObligationActive, OrderID: "O-1", OrderLineNo: 1, Description: "Pallets", Packs: 10, PackageType: "PX",
		GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 500}, Volume: kernel.Quantity{Unit: "DMQ", Amount: 12_000}, LegalEntityID: "LE",
		Customer: comapi.CustomerRef{PartyID: "P", PartyRevision: 2}, QuoteID: "Q-1", QuoteRevision: 3,
		AllocatedAt: time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC), HeldByShipmentIDs: []string{"SH-1"}}
}

func TestPlannableTakesTheReservedVersionFromCOM(t *testing.T) {
	got, err := Obligations{COM: comStub{snapshot: reserved()}}.Plannable(context.Background(), "T", "OB-1", 1, "SH-1")
	want := ful.ObligationRef{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", CustomerPartyID: "P", LegalEntityID: "LE",
		Packs: 10, PackageType: "PX", GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 500}, Volume: kernel.Quantity{Unit: "DMQ", Amount: 12_000}}
	if err != nil || got != want {
		t.Fatalf("Plannable = %+v, %v", got, err)
	}
	shared := reserved()
	shared.HeldByShipmentIDs = []string{"SH-0", "SH-1"}
	if got, err = (Obligations{COM: comStub{snapshot: shared}}).Plannable(context.Background(), "T", "OB-1", 1, "SH-1"); err != nil || got != want {
		t.Fatalf("held by SH-1 among others (F1): %+v, %v", got, err)
	}
}

func TestPlannableRefusesWhatCOMDoesNotHoldForTheShipment(t *testing.T) {
	edit := func(f func(*comapi.ObligationSnapshotV3)) comStub {
		s := reserved()
		f(&s)
		return comStub{snapshot: s}
	}
	for name, stub := range map[string]comStub{
		"revoked version":          edit(func(s *comapi.ObligationSnapshotV3) { s.Status = comapi.ObligationRevoked }),
		"superseded version":       edit(func(s *comapi.ObligationSnapshotV3) { s.LatestVersion = 2 }),
		"reserved for another":     edit(func(s *comapi.ObligationSnapshotV3) { s.HeldByShipmentIDs = []string{"SH-0", "SH-2"} }),
		"not reserved":             edit(func(s *comapi.ObligationSnapshotV3) { s.HeldByShipmentIDs = nil }),
		"answer for another":       edit(func(s *comapi.ObligationSnapshotV3) { s.ObligationID = "OB-2" }),
		"unknown to COM":           {err: fmt.Errorf("%w: no such obligation", comapi.ErrNotFound)},
		"rejected by COM's checks": {err: fmt.Errorf("%w: invalid query", comapi.ErrRejected)},
	} {
		_, err := Obligations{COM: stub}.Plannable(context.Background(), "T", "OB-1", 1, "SH-1")
		if !errors.Is(err, app.ErrNotPlannable) || errors.Is(err, comapi.ErrNotFound) || errors.Is(err, comapi.ErrRejected) {
			t.Errorf("%s: err = %v, want ErrNotPlannable without COM's error kinds", name, err)
		}
	}
	down := errors.New("connection reset")
	if _, err := (Obligations{COM: comStub{err: down}}).Plannable(context.Background(), "T", "OB-1", 1, "SH-1"); err != down {
		t.Fatalf("infrastructure errors pass through: %v", err)
	}
}
