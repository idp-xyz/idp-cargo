package ful

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type stub struct {
	plan fulapi.OceanOperationPlan
	err  error
}

func (s stub) GetOceanOperationPlan(context.Context, fulapi.GetShipmentPlanSnapshot) (fulapi.OceanOperationPlan, error) {
	return s.plan, s.err
}

func (s stub) GetOceanEvidenceCurrency(context.Context, fulapi.GetShipmentPlanSnapshot) (fulapi.OceanEvidenceCurrency, error) {
	return fulapi.OceanEvidenceCurrency{}, nil
}

func unlocode(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

var sealed = time.Date(2026, 11, 20, 9, 0, 0, 123456789, time.FixedZone("CST", 8*3600))

// stuffed is a SEA LCL shipment as FUL answers it once PHY's sealed container is relayed, with times finer than a
// microsecond in another zone.
func stuffed() fulapi.OceanOperationPlan {
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	return fulapi.OceanOperationPlan{TenantID: "T", ShipmentID: "SH-1", Revision: 5, Status: fulapi.StatusPlanned, PlanVersion: 2, CustomerPartyID: "party-1",
		LegalEntityID: "LE", TransportMode: fulapi.ModeSea, ContainerMode: fulapi.ContainerLCL, Origin: unlocode("CNSHG"), Destination: unlocode("DEHAM"),
		LocationEdition: "E1", DepartureWindow: kernel.ValidityInterval{From: sealed, Until: sealed.Add(48 * time.Hour)},
		AgreedWindow: kernel.ValidityInterval{From: sealed, Until: sealed.Add(48 * time.Hour)}, Packs: 6, GrossWeight: kg(300),
		Volume: kernel.Quantity{Unit: "DMQ", Amount: 2400}, Stage: fulapi.StageMain, ConsolidationID: "C-1", MembershipEntryNo: 1, MembershipRevision: 1,
		Parts: []fulapi.LoadIntentPart{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 6, PackageType: "CT", GrossWeight: kg(300),
			GoodsDescription: "Furniture parts"}},
		EvidenceVersion: 3, ExecutionStatus: fulapi.ExecutionNotStarted, MeasuredBasis: fulapi.MeasuredTendered, MeasuredPacks: 6, MeasuredGrossWeight: kg(312),
		Stuffings: []fulapi.ContainerStuffing{{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 6, GrossWeight: kg(312), LineNos: []int{1, 3},
			Seal: "CN-SEAL-1", SealSHA256: "c4", SealedAt: sealed}}}
}

func TestPlanAsSEAFreezesIt(t *testing.T) {
	ctx := context.Background()
	answer := stuffed()
	got, err := Plans{FUL: stub{plan: answer}}.Plan(ctx, "T", "SH-1")
	if err != nil {
		t.Fatal(err)
	}
	stored := sealed.UTC().Truncate(time.Microsecond)
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	want := sea.Plan{ShipmentID: "SH-1", Revision: 5, Status: "PLANNED", PlanVersion: 2, LegalEntityID: "LE", Mode: "SEA", ContainerMode: "LCL",
		Origin: unlocode("CNSHG"), Destination: unlocode("DEHAM"), Window: kernel.ValidityInterval{From: stored, Until: stored.Add(48 * time.Hour)},
		Packs: 6, GrossWeight: kg(300), Volume: kernel.Quantity{Unit: "DMQ", Amount: 2400}, ConsolidationID: "C-1", EvidenceVersion: 3,
		MeasuredBasis: "TENDERED", MeasuredPacks: 6, MeasuredWeight: kg(312),
		Parts: []sea.Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 6, PackageType: "CT", GrossWeight: kg(300),
			GoodsDescription: "Furniture parts"}},
		Stuffings: []sea.Stuffing{{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 6, GrossWeight: kg(312), LineNos: []int{1, 3},
			Seal: "CN-SEAL-1", SealSHA256: "c4", SealedAt: stored}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan:\n got %+v\nwant %+v", got, want)
	}
	// the plan SEA keeps shares nothing with FUL's answer
	answer.Stuffings[0].LineNos[0] = 9
	if got.Stuffings[0].LineNos[0] != 1 {
		t.Fatal("the load lines are FUL's slice")
	}
	// an open-ended departure window stays open-ended
	open := stuffed()
	open.DepartureWindow.Until, open.AgreedWindow.Until = time.Time{}, time.Time{}
	if got, err = (Plans{FUL: stub{plan: open}}).Plan(ctx, "T", "SH-1"); err != nil || !got.Window.Until.IsZero() {
		t.Fatalf("an open-ended window: %+v %v", got.Window, err)
	}
	// the house is held to the window the customer agreed to (V2.110), not the plan's
	agreed := stuffed()
	agreed.AgreedWindow.Until = sealed.Add(96 * time.Hour)
	if got, err = (Plans{FUL: stub{plan: agreed}}).Plan(ctx, "T", "SH-1"); err != nil || !got.Window.Until.Equal(sealed.Add(96*time.Hour).Truncate(time.Microsecond)) {
		t.Fatalf("the agreed window: %+v %v", got.Window, err)
	}
}

// What FUL answers for another tenant, shipment or stage, and FUL's errors: SEA's own kinds, never FUL's.
func TestPlanAnswersSEADoesNotTake(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(*fulapi.OceanOperationPlan){
		"another tenant":   func(p *fulapi.OceanOperationPlan) { p.TenantID = "T-2" },
		"another shipment": func(p *fulapi.OceanOperationPlan) { p.ShipmentID = "SH-2" },
		"another stage":    func(p *fulapi.OceanOperationPlan) { p.Stage = "PRE_CARRIAGE" },
	} {
		p := stuffed()
		change(&p)
		if _, err := (Plans{FUL: stub{plan: p}}).Plan(ctx, "T", "SH-1"); !errors.Is(err, app.ErrUnknownShipment) {
			t.Errorf("%s: %v", name, err)
		}
	}
	kinds := []error{fulapi.ErrUnauthenticated, fulapi.ErrUnauthorized, fulapi.ErrNotFound, fulapi.ErrConflict, fulapi.ErrIdempotencyConflict, fulapi.ErrRejected}
	for _, kind := range kinds {
		_, err := Plans{FUL: stub{err: fmt.Errorf("%w: stub", kind)}}.Plan(ctx, "T", "SH-1")
		for _, k := range kinds {
			if errors.Is(err, k) {
				t.Errorf("%v reaches SEA's caller as FUL's kind: %v", kind, err)
			}
		}
		unknown := kind == fulapi.ErrNotFound || kind == fulapi.ErrRejected
		if err == nil || errors.Is(err, app.ErrUnknownShipment) != unknown {
			t.Errorf("%v: unknown shipment %v, want %v: %v", kind, !unknown, unknown, err)
		}
	}
	if _, err := (Plans{FUL: stub{err: context.DeadlineExceeded}}).Plan(ctx, "T", "SH-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a deadline passes unchanged: %v", err)
	}
}
