//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5 with the real ORG and the real location reference data: a co-loader ORG approves as CO_LOADER gets a
// supplier card for Shanghai to Hamburg LCL, entered with the carriers' code CNSHA, which the reference data resolves
// to the port CNSHG for SEA; the company tariff prices the same lane; an evaluation rates both sides.
func TestRateCardsWithTheRealContexts(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-rate-cards-")
	seedORG(t, platform, tenant, "coloader", "party-n")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-coloader", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "coloader", ExpectedRevision: 1, RoleCode: "CO_LOADER", ValidFrom: at, ValidUntil: at.Add(365 * 24 * time.Hour),
		EvidenceRef: "co-loading agreement", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	must := func(r rateapi.Receipt, err error) rateapi.Receipt {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if _, err = rateModule.CreateRateCard(ctx, rateapi.CreateRateCard{CommandID: "card-n", TenantID: tenant, ActorID: "buyer-1", RateCardID: "S-N",
		Side: rateapi.SideBuy, Scope: rateapi.ScopeSupplier, LegalEntityID: "LE", PartyID: "party-n", Name: "not a supplier", OccurredAt: at}); !errors.Is(err, rateapi.ErrRejected) {
		t.Fatalf("a party ORG has not approved as supplier: %v", err)
	}
	lcl := func(rate int64) rateapi.RateEntry {
		return rateapi.RateEntry{Mode: rateapi.ModeSea, ContainerMode: rateapi.ContainerLCL, Origin: "CNSHA", Destination: "DEHAM", ServiceLevel: "STANDARD",
			Commodity: rateapi.CommodityGeneral, Lines: []rateapi.RateLine{{ChargeCode: "OCEAN_FREIGHT", Currency: "USD", Calculator: rateapi.CalcPerUnit,
				Unit: rateapi.UnitFreightTonne, Rate: rate}, {ChargeCode: "CFS_HANDLING", Currency: "CNY", Calculator: rateapi.CalcPerUnit,
				Unit: rateapi.UnitFreightTonne, Rate: 12_000, Minimum: 15_000}}}
	}
	for _, c := range []struct {
		id, side, scope, party, actor string
		rate                          int64
	}{{"S-SHA-HAM", rateapi.SideBuy, rateapi.ScopeSupplier, "coloader", "buyer-1", 3_500}, {"T-SEA", rateapi.SideSell, rateapi.ScopeTariff, "", "pricing-1", 5_000}} {
		must(rateModule.CreateRateCard(ctx, rateapi.CreateRateCard{CommandID: "card-" + c.id, TenantID: tenant, ActorID: c.actor, RateCardID: c.id,
			Side: c.side, Scope: c.scope, LegalEntityID: "LE", PartyID: c.party, Name: c.id, OccurredAt: at}))
		must(rateModule.DraftRateCardVersion(ctx, rateapi.DraftRateCardVersion{CommandID: "draft-" + c.id, TenantID: tenant, ActorID: c.actor,
			RateCardID: c.id, ExpectedRevision: 1, ValidFrom: at, Currency: "USD", SourceRef: "tariff 2026-11", OccurredAt: at}))
		must(rateModule.SetRateCardEntries(ctx, rateapi.SetRateCardEntries{CommandID: "set-" + c.id, TenantID: tenant, ActorID: c.actor, RateCardID: c.id,
			ExpectedRevision: 2, Version: 1, Entries: []rateapi.RateEntry{lcl(c.rate)}, OccurredAt: at}))
		must(rateModule.PublishRateCardVersion(ctx, rateapi.PublishRateCardVersion{CommandID: "publish-" + c.id, TenantID: tenant, ActorID: "pricing-manager",
			RateCardID: c.id, ExpectedRevision: 3, Version: 1, OccurredAt: at.Add(time.Minute)}))
	}
	v, err := rateModule.GetRateCardVersion(ctx, rateapi.GetRateCardVersion{TenantID: tenant, RateCardID: "S-SHA-HAM", Version: 1})
	if err != nil || v.Entries[0].Origin != "CNSHG" || v.Entries[0].Edition == "" {
		t.Fatalf("CNSHA for SEA resolves to the port CNSHG: %+v %v", v.Entries, err)
	}
	// 0.8 t and 2.5 m3: 2.5 W/M; SELL 50.00 USD and CNY 300.00 CFS; BUY 35.00 USD
	must(rateModule.EvaluateRates(ctx, rateapi.EvaluateRates{CommandID: "evaluate", TenantID: tenant, ActorID: "sales-1", SnapshotID: "RS-LCL",
		Purpose: rateapi.PurposeQuoteLine, LegalEntityID: "LE", Sides: []string{rateapi.SideSell, rateapi.SideBuy}, Mode: rateapi.ModeSea,
		ContainerMode: rateapi.ContainerLCL, Origin: "CNSHA", Destination: "DEHAM", ServiceLevel: "STANDARD", Commodity: rateapi.CommodityGeneral,
		Cargo: rateapi.Cargo{Pieces: 4, GrossGrams: 800_000, VolumeCM3: 2_500_000}, OccurredAt: at.Add(time.Hour)}))
	s, err := rateModule.GetRateSnapshot(ctx, rateapi.GetRateSnapshot{TenantID: tenant, SnapshotID: "RS-LCL"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]kernel.Money{"SELL OCEAN_FREIGHT": usd(12_500), "SELL CFS_HANDLING": {Currency: "CNY", Minor: 30_000},
		"BUY OCEAN_FREIGHT": usd(8_750), "BUY CFS_HANDLING": {Currency: "CNY", Minor: 30_000}}
	if len(s.Lines) != 4 || s.Origin != "CNSHG" {
		t.Fatalf("snapshot: %+v", s)
	}
	for _, l := range s.Lines {
		if want[l.Side+" "+l.ChargeCode] != l.Amount || l.Quantity != 2500 {
			t.Errorf("%s %s: %+v", l.Side, l.ChargeCode, l)
		}
	}
}
