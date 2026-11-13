//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	ratemodule "go.idp.xyz/idp-cargo/internal/contexts/rate"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

// RATE V0.5 on PostgreSQL: the charge-code catalogue, rate cards and rating, with stand-ins for ORG and for the
// location reference data (the composition root runs the real ones).

const edition = "UNLOCODE-2026-11-01-S1"

// locationStub resolves the codes the tests use as the S1 subset does: airports for AIR, ports for SEA, and the
// carriers' alias CNSHA for the port of Shanghai (CNSHG).
type locationStub struct{}

func (locationStub) ResolveLocation(_ context.Context, q refapi.ResolveLocation) (refapi.Location, error) {
	known := map[string]map[string]string{
		refapi.ModeAir: {"CNPVG": "CNPVG", "DEFRA": "DEFRA", "AEDXB": "AEDXB", "USLAX": "USLAX", "CNSHA": "CNSHA"},
		refapi.ModeSea: {"CNSHG": "CNSHG", "CNSHA": "CNSHG", "DEHAM": "DEHAM", "NLRTM": "NLRTM"},
	}
	code, ok := known[q.Mode][q.Code]
	if !ok {
		return refapi.Location{}, fmt.Errorf("%w: stub %s %s", refapi.ErrUnknownLocation, q.Mode, q.Code)
	}
	return refapi.Location{UNLocode: code, Edition: edition}, nil
}

// partyStub confirms parties in roles: suppliers as CO_LOADER, customers as CUSTOMER, at revision 3.
type partyStub struct {
	mu    sync.Mutex
	roles map[string]string
}

func (p *partyStub) ResolvePartyRole(_ context.Context, q orgapi.ResolvePartyRole) (orgapi.PartyRole, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.roles[q.PartyID] != q.RoleCode {
		return orgapi.PartyRole{}, fmt.Errorf("%w: stub: %s is not %s", orgapi.ErrRejected, q.PartyID, q.RoleCode)
	}
	return orgapi.PartyRole{Ref: orgapi.PartyRef{TenantID: q.TenantID, PartyID: q.PartyID, PartyRevision: 3, IdentitySource: orgapi.IdentitySource,
		LegalEntityScope: q.LegalEntityID}, RoleCode: q.RoleCode}, nil
}

// cardPolicy: "intruder" may do nothing; the pricing manager and the pricing lead approve tariffs; only the pricing
// lead approves a tariff version it prepared (R3).
type cardPolicy struct{ quotePolicy }

func (cardPolicy) AuthorizeQuoteAction(_ context.Context, _, actor, action, _ string) error {
	switch {
	case actor == "intruder",
		action == api.ActionApproveTariff && actor != "pricing-manager" && actor != "pricing-lead",
		action == api.ActionApproveOwnTariff && actor != "pricing-lead":
		return errors.New("test policy: forbidden")
	}
	return nil
}

type cards struct {
	*fixture
	t   *testing.T
	ctx context.Context
	org *partyStub
}

func newCards(t *testing.T) cards {
	t.Helper()
	f := newFixture(t)
	org := &partyStub{roles: map[string]string{"coloader-a": "CO_LOADER", "coloader-b": "CO_LOADER", "acme": "CUSTOMER"}}
	m, err := ratemodule.New(f.db, org, cardPolicy{}, quotePolicy{}, ratemodule.WithLocations(locationStub{}))
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	return cards{fixture: f, t: t, ctx: context.Background(), org: org}
}

func (c cards) must(r api.Receipt, err error) api.Receipt {
	c.t.Helper()
	if err != nil {
		c.t.Fatal(err)
	}
	return r
}

func (c cards) rejected(name string, err error, want error, text string) {
	c.t.Helper()
	if !errors.Is(err, want) || !strings.Contains(err.Error(), text) {
		c.t.Fatalf("%s: %v, want %v containing %q", name, err, want, text)
	}
}

func (c cards) card(id string) api.RateCard {
	c.t.Helper()
	r, err := c.module.GetRateCard(c.ctx, api.GetRateCard{TenantID: c.tenant, RateCardID: id})
	if err != nil {
		c.t.Fatal(err)
	}
	return r
}

func (c cards) h(n int) time.Time { return c.at.Add(time.Duration(n) * time.Hour) }

func (c cards) create(id, side, scope, party, actor string) api.CreateRateCard {
	return api.CreateRateCard{CommandID: "create-" + id, TenantID: c.tenant, ActorID: actor, RateCardID: id, Side: side, Scope: scope,
		LegalEntityID: "LE", PartyID: party, Name: id + " card", OccurredAt: c.h(0)}
}

func (c cards) draft(id string, from, until time.Time, copyFrom int, actor string) api.DraftRateCardVersion {
	return api.DraftRateCardVersion{CommandID: fmt.Sprintf("draft-%s-%d", id, c.card(id).Revision), TenantID: c.tenant, ActorID: actor, RateCardID: id,
		ExpectedRevision: c.card(id).Revision, ValidFrom: from, ValidUntil: until, Currency: "CNY", SourceRef: "tariff " + id, CopyFrom: copyFrom,
		OccurredAt: c.h(1)}
}

func (c cards) set(id string, version int, actor string, entries ...api.RateEntry) api.SetRateCardEntries {
	return api.SetRateCardEntries{CommandID: fmt.Sprintf("set-%s-%d", id, c.card(id).Revision), TenantID: c.tenant, ActorID: actor, RateCardID: id,
		ExpectedRevision: c.card(id).Revision, Version: version, Entries: entries, OccurredAt: c.h(1)}
}

func (c cards) publish(id string, version int, actor string) api.PublishRateCardVersion {
	return api.PublishRateCardVersion{CommandID: fmt.Sprintf("publish-%s-%d-%s", id, c.card(id).Revision, actor), TenantID: c.tenant, ActorID: actor,
		RateCardID: id, ExpectedRevision: c.card(id).Revision, Version: version, OccurredAt: c.h(2)}
}

// freight is CNY 30.00/kg below 45 kg, 25.00 from 45 kg and 20.00 from 100 kg, at least 300.00.
func freight(rate int64) api.RateLine {
	return api.RateLine{ChargeCode: "AIR_FREIGHT", Currency: "CNY", Calculator: api.CalcPerUnit, Unit: api.UnitKgChargeable, Rate: rate, Minimum: 30_000,
		Breaks: []api.RateBreak{{From: 45_000, Rate: rate - 500}, {From: 100_000, Rate: rate - 1000}}}
}

func air(lines ...api.RateLine) api.RateEntry {
	return api.RateEntry{Mode: api.ModeAir, ContainerMode: api.ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD",
		Commodity: api.CommodityGeneral, TransitDays: 2, Frequency: "daily", Lines: lines}
}

func TestChargeCodesOnPostgres(t *testing.T) {
	c := newCards(t)
	codes, err := c.module.ListChargeCodes(c.ctx, api.ListChargeCodes{TenantID: c.tenant})
	if err != nil || len(codes) != 12 || codes[0].Code != "AIR_FREIGHT" || codes[0].Version != 0 || codes[0].Unit != api.UnitKgChargeable {
		t.Fatalf("starter list: %+v %v", codes, err)
	}
	define := api.DefineChargeCode{CommandID: "define-storage", TenantID: c.tenant, ActorID: "md-1", Code: "STORAGE", NameZH: "仓储费", NameEN: "Storage",
		Group: api.GroupOrigin, Calculator: api.CalcPerUnit, Unit: api.UnitCBM, Modes: []string{api.ModeSea}, OccurredAt: c.h(0)}
	r := c.must(c.module.DefineChargeCode(c.ctx, define))
	if r.SubjectID != "STORAGE" || r.NewRevision != 1 {
		t.Fatalf("defined: %+v", r)
	}
	if again, err := c.module.DefineChargeCode(c.ctx, define); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	define.CommandID = "define-storage-again"
	_, err = c.module.DefineChargeCode(c.ctx, define)
	c.rejected("defined twice", err, api.ErrRejected, "RATE_CHARGE_CODE_DEFINED")
	define.CommandID, define.Code = "define-air", "AIR_FREIGHT"
	_, err = c.module.DefineChargeCode(c.ctx, define)
	c.rejected("a starter code defined", err, api.ErrRejected, "RATE_CHARGE_CODE_DEFINED")
	define.CommandID, define.Code = "define-bad", "storage"
	_, err = c.module.DefineChargeCode(c.ctx, define)
	c.rejected("a code of the wrong form", err, api.ErrRejected, "upper-case")
	deactivate := api.DeactivateChargeCode{CommandID: "deactivate-storage", TenantID: c.tenant, ActorID: "md-1", Code: "STORAGE", ExpectedVersion: 0,
		OccurredAt: c.h(1)}
	_, err = c.module.DeactivateChargeCode(c.ctx, deactivate)
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	deactivate.CommandID, deactivate.ExpectedVersion = "deactivate-storage-1", 1
	c.must(c.module.DeactivateChargeCode(c.ctx, deactivate))
	if got, _ := c.module.ListChargeCodes(c.ctx, api.ListChargeCodes{TenantID: c.tenant}); len(got) != 12 {
		t.Fatalf("an inactive code listed: %d", len(got))
	}
	if got, _ := c.module.ListChargeCodes(c.ctx, api.ListChargeCodes{TenantID: c.tenant, IncludeInactive: true}); len(got) != 13 {
		t.Fatalf("inactive codes asked for: %d", len(got))
	}
	c.must(c.module.ReactivateChargeCode(c.ctx, api.ReactivateChargeCode{CommandID: "reactivate-storage", TenantID: c.tenant, ActorID: "md-1", Code: "STORAGE",
		ExpectedVersion: 2, OccurredAt: c.h(2)}))
	// a starter code revised by the tenant: version 1 of its own
	revise := api.ReviseChargeCode{CommandID: "revise-doc", TenantID: c.tenant, ActorID: "md-1", Code: "DOC_FEE", ExpectedVersion: 0, NameZH: "单证费",
		NameEN: "Document fee", Group: api.GroupDocumentation, Calculator: api.CalcFlat, Modes: []string{api.ModeAir, api.ModeSea}, OccurredAt: c.h(2)}
	c.must(c.module.ReviseChargeCode(c.ctx, revise))
	if doc, err := c.module.GetChargeCode(c.ctx, api.GetChargeCode{TenantID: c.tenant, Code: "DOC_FEE"}); err != nil || doc.Version != 1 || doc.NameZH != "单证费" {
		t.Fatalf("revised starter: %+v %v", doc, err)
	}
	if _, err = c.module.GetChargeCode(c.ctx, api.GetChargeCode{TenantID: c.tenant, Code: "NOT_LISTED"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown code: %v", err)
	}
	// another tenant sees only the starter list
	if other, _ := c.module.GetChargeCode(c.ctx, api.GetChargeCode{TenantID: c.tenant + "-other", Code: "DOC_FEE"}); other.Version != 0 {
		t.Fatalf("another tenant: %+v", other)
	}
	_, err = c.module.DefineChargeCode(c.ctx, api.DefineChargeCode{CommandID: "define-intruder", TenantID: c.tenant, ActorID: "intruder", Code: "X_CODE",
		NameZH: "x", NameEN: "x", Group: api.GroupOrigin, Calculator: api.CalcFlat, Modes: []string{api.ModeAir}, OccurredAt: c.h(0)})
	if !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("intruder: %v", err)
	}
	counts := map[string]int{}
	for _, kind := range c.outboxTypes(t) {
		counts[kind]++
	}
	if counts[api.ChargeCodeDefined] != 1 || counts[api.ChargeCodeDeactivated] != 1 || counts[api.ChargeCodeReactivated] != 1 || counts[api.ChargeCodeRevised] != 1 {
		t.Fatalf("outbox: %v", counts)
	}
	for name, tc := range map[string]struct{ sql, state, constraint string }{
		"a version changed": {`UPDATE cargo_rate.charge_code_version SET name_en='x' WHERE tenant_id=$1`, "42501|23001", ""},
		"a version deleted": {`DELETE FROM cargo_rate.charge_code_version WHERE tenant_id=$1`, "42501|23001", ""},
		"a version skipped": {`INSERT INTO cargo_rate.charge_code_version (tenant_id,code,version,name_zh,name_en,charge_group,calculator,modes,active,set_by,
            set_at) VALUES ($1,'STORAGE',9,'x','x','ORIGIN','FLAT','{AIR}',true,'x',now())`, "23514", "charge_code_version_sequence"},
	} {
		err := c.exec(tc.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || !strings.Contains(tc.state, pgErr.Code) || (tc.constraint != "" && pgErr.ConstraintName != tc.constraint) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// publishedTariff is the tariff T-AIR with version 1 (from h0, a month) of one air entry: freight, a document fee,
// a fuel surcharge of 10% of the freight and customs clearance priced by a person.
func publishedTariff(c cards) {
	c.t.Helper()
	c.must(c.module.CreateRateCard(c.ctx, c.create("T-AIR", api.SideSell, api.ScopeTariff, "", "pricing-1")))
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("T-AIR", c.h(0), c.h(24*30), 0, "pricing-1")))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set("T-AIR", 1, "pricing-1", air(freight(3000),
		api.RateLine{ChargeCode: "DOC_FEE", Currency: "CNY", Calculator: api.CalcFlat, Rate: 15_000},
		api.RateLine{ChargeCode: "FUEL_SURCHARGE", Currency: "CNY", Calculator: api.CalcPercentOf, PercentBP: 1000, BaseCode: "AIR_FREIGHT"},
		api.RateLine{ChargeCode: "CUSTOMS_CLEARING", Currency: "CNY", Calculator: api.CalcFlat, CallForPricing: true}))))
	c.must(c.module.PublishRateCardVersion(c.ctx, c.publish("T-AIR", 1, "pricing-manager")))
}

func TestRateCardsOnPostgres(t *testing.T) {
	c := newCards(t)
	c.must(c.module.CreateRateCard(c.ctx, c.create("T-AIR", api.SideSell, api.ScopeTariff, "", "pricing-1")))
	_, err := c.module.CreateRateCard(c.ctx, c.create("S-X", api.SideBuy, api.ScopeSupplier, "acme", "pricing-1"))
	c.rejected("a customer as supplier", err, api.ErrRejected, "RATE_PARTY_ROLE")
	// an actor without rights learns nothing of ORG's roles: refused before ORG is asked
	_, err = c.module.CreateRateCard(c.ctx, c.create("S-Y", api.SideBuy, api.ScopeSupplier, "acme", "intruder"))
	if !errors.Is(err, api.ErrUnauthorized) || strings.Contains(err.Error(), "acme") {
		t.Fatalf("an intruder: %v", err)
	}
	r := c.must(c.module.CreateRateCard(c.ctx, c.create("S-A", api.SideBuy, api.ScopeSupplier, "coloader-a", "buyer-1")))
	if r.SubjectID != "S-A" || r.NewRevision != 1 || c.card("S-A").PartyRevision != 3 {
		t.Fatalf("supplier card: %+v %+v", r, c.card("S-A"))
	}
	c.must(c.module.CreateRateCard(c.ctx, c.create("C-ACME", api.SideSell, api.ScopeCustomer, "acme", "sales-1")))

	month := c.h(24 * 30)
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("T-AIR", c.h(0), month, 0, "pricing-1")))
	_, err = c.module.DraftRateCardVersion(c.ctx, c.draft("T-AIR", c.h(0), month, 0, "pricing-1"))
	c.rejected("a second draft", err, api.ErrRejected, "still a draft")
	via := air(freight(2800))
	via.Via = "AEDXB"
	sea := api.RateEntry{Mode: api.ModeSea, ContainerMode: api.ContainerLCL, Origin: "CNSHA", Destination: "DEHAM", ServiceLevel: "ECONOMY",
		Commodity: api.CommodityGeneral, Lines: []api.RateLine{{ChargeCode: "OCEAN_FREIGHT", Currency: "USD", Calculator: api.CalcPerUnit,
			Unit: api.UnitFreightTonne, Rate: 4500}, {ChargeCode: "THC", Currency: "CNY", Calculator: api.CalcPerUnit, Unit: api.UnitKgChargeable, Rate: 100}}}
	via.ValidFrom = c.h(24 * 20) // an entry valid from day 20 on, within version 1 and the version 2 copying it
	set := c.set("T-AIR", 1, "pricing-1", air(freight(3000), api.RateLine{ChargeCode: "DOC_FEE", Currency: "CNY", Calculator: api.CalcFlat, Rate: 15_000}),
		via, sea)
	unknown := set
	unknown.CommandID, unknown.Entries = "set-unknown", []api.RateEntry{{Mode: api.ModeAir, ContainerMode: api.ContainerLoose, Origin: "CNPVG",
		Destination: "ZZZZZ", ServiceLevel: "STANDARD", Commodity: api.CommodityGeneral, Lines: []api.RateLine{freight(3000)}}}
	_, err = c.module.SetRateCardEntries(c.ctx, unknown)
	c.rejected("an unknown location", err, api.ErrRejected, "RATE_LOCATION")
	c.must(c.module.SetRateCardEntries(c.ctx, set))
	v, err := c.module.GetRateCardVersion(c.ctx, api.GetRateCardVersion{TenantID: c.tenant, RateCardID: "T-AIR", Version: 1})
	if err != nil || len(v.Entries) != 3 || v.Entries[2].Origin != "CNSHG" || v.Entries[2].Edition != edition || v.Entries[2].Lines[0].MinQuantity != 1000 ||
		v.Entries[2].Lines[0].VolumeFactor != 1_000_000 || v.Entries[0].Lines[0].Step != 500 || v.Entries[0].Lines[0].VolumeFactor != 6000 ||
		len(v.Entries[0].Lines[0].Breaks) != 2 || v.Status != api.VersionDraft || len(v.Preparers) != 1 || v.Preparers[0] != "pricing-1" ||
		v.Entries[2].Lines[1].VolumeFactor != 1000 || v.Entries[2].Lines[1].Step != 1 || !v.Entries[1].ValidUntil.IsZero() || !v.Entries[1].ValidFrom.Equal(c.h(24*20)) {
		t.Fatalf("version 1 as set (CNSHA resolved to the port CNSHG; OD-8 defaults): %+v %v", v, err)
	}
	// publication: the pricing manager approves the tariff; the one who prepared it only as the pricing lead (flagged)
	_, err = c.module.PublishRateCardVersion(c.ctx, c.publish("T-AIR", 1, "pricing-1"))
	if !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("published by its preparer without approval rights: %v", err)
	}
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("C-ACME", c.h(0), month, 0, "sales-1")))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set("C-ACME", 1, "sales-1", air(freight(1800)))))
	_, err = c.module.PublishRateCardVersion(c.ctx, c.publish("C-ACME", 1, "pricing-manager"))
	c.rejected("a customer card published directly", err, api.ErrRejected, "AGREEMENT quote")
	c.must(c.module.PublishRateCardVersion(c.ctx, c.publish("T-AIR", 1, "pricing-manager")))
	v, _ = c.module.GetRateCardVersion(c.ctx, api.GetRateCardVersion{TenantID: c.tenant, RateCardID: "T-AIR", Version: 1})
	if v.Status != api.VersionPublished || v.ApprovedBy != "pricing-manager" || v.SelfApproved {
		t.Fatalf("published: %+v", v.RateCardVersionHeader)
	}
	_, err = c.module.SetRateCardEntries(c.ctx, c.set("T-AIR", 1, "pricing-1", air(freight(1))))
	c.rejected("a published version changed", err, api.ErrRejected, "new version")

	nowhere := c.draft("T-AIR", c.h(24*14), time.Time{}, 7, "pricing-lead")
	nowhere.CommandID = "draft-from-nowhere"
	_, err = c.module.DraftRateCardVersion(c.ctx, nowhere)
	c.rejected("copying a version that does not exist", err, api.ErrRejected, "no version 7")
	// version 2 copies version 1 and starts in two weeks: it overlaps until version 1 is withdrawn
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("T-AIR", c.h(24*14), time.Time{}, 1, "pricing-lead")))
	_, err = c.module.PublishRateCardVersion(c.ctx, c.publish("T-AIR", 2, "pricing-lead"))
	c.rejected("overlap", err, api.ErrRejected, "RATE_VERSION_OVERLAP")
	c.must(c.module.WithdrawRateCardVersion(c.ctx, api.WithdrawRateCardVersion{CommandID: "withdraw-1", TenantID: c.tenant, ActorID: "pricing-manager",
		RateCardID: "T-AIR", ExpectedRevision: c.card("T-AIR").Revision, Version: 1, Reason: "fuel index changed", OccurredAt: c.h(3)}))
	c.must(c.module.PublishRateCardVersion(c.ctx, c.publish("T-AIR", 2, "pricing-lead")))
	card := c.card("T-AIR")
	if len(card.Versions) != 2 || card.Versions[0].Status != api.VersionWithdrawn || card.Versions[0].WithdrawReason != "fuel index changed" ||
		!card.Versions[1].SelfApproved || card.Versions[1].ApprovedBy != "pricing-lead" || !card.Versions[1].ValidUntil.IsZero() {
		t.Fatalf("card after withdrawal: %+v", card)
	}
	if v2, _ := c.module.GetRateCardVersion(c.ctx, api.GetRateCardVersion{TenantID: c.tenant, RateCardID: "T-AIR", Version: 2}); len(v2.Entries) != 3 {
		t.Fatalf("version 2 copied: %+v", v2)
	}
	// a supplier version starts no earlier than the record window allows
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("S-A", c.h(-24*30), time.Time{}, 0, "buyer-1")))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set("S-A", 1, "buyer-1", air(freight(2000)))))
	_, err = c.module.PublishRateCardVersion(c.ctx, c.publish("S-A", 1, "buyer-1"))
	c.rejected("starting before the record window", err, api.ErrRejected, "record window")
	listed, err := c.module.ListRateCards(c.ctx, api.ListRateCards{TenantID: c.tenant, Side: api.SideSell, ValidAt: c.h(24 * 20)})
	if err != nil || len(listed) != 1 || listed[0].RateCardID != "T-AIR" {
		t.Fatalf("sell cards valid in three weeks: %+v %v", listed, err)
	}
	counts := map[string]int{}
	for _, kind := range c.outboxTypes(t) {
		counts[kind]++
	}
	if counts[api.RateCardVersionPublished] != 2 || counts[api.RateCardVersionWithdrawn] != 1 {
		t.Fatalf("outbox: %v", counts)
	}

	draftVersion := `INSERT INTO cargo_rate.rate_card_version (tenant_id,rate_card_id,version_no,status,valid_from,currency,preparers,prepared_at)
        VALUES ($1,'S-A',9,'DRAFT',now(),'CNY','{x}',now())`
	for name, tc := range map[string]struct{ sql, state, constraint string }{
		"a published version's validity changed": {`UPDATE cargo_rate.rate_card_version SET valid_until=now() WHERE tenant_id=$1 AND rate_card_id='T-AIR'
            AND version_no=2`, "42501|23001", ""},
		"an entry added to a published version": {`INSERT INTO cargo_rate.rate_entry (tenant_id,rate_card_id,version_no,entry_no,mode,container_mode,origin,
            destination,edition,service_level,commodity,transit_days,frequency) VALUES ($1,'T-AIR',2,9,'AIR','LOOSE','CNPVG','USLAX','x','STANDARD','GENERAL',
            0,'')`, "23001", ""},
		"a line deleted from a published version": {`DELETE FROM cargo_rate.rate_break WHERE tenant_id=$1 AND rate_card_id='T-AIR' AND version_no=2`, "23001", ""},
		"a version published again":               {`UPDATE cargo_rate.rate_card_version SET status='DRAFT' WHERE tenant_id=$1 AND rate_card_id='T-AIR' AND version_no=1`, "42501|23001|23514", ""},
		"a customer version published past RATE": {`UPDATE cargo_rate.rate_card_version SET status='PUBLISHED',published_by='x',published_at=now()
            WHERE tenant_id=$1 AND rate_card_id='C-ACME' AND version_no=1`, "23514", "rate_version_publishable"},
		"a second draft": {strings.Replace(draftVersion, "9,", "2,", 1) + `;
INSERT INTO cargo_rate.rate_card_version (tenant_id,rate_card_id,version_no,status,valid_from,currency,preparers,prepared_at)
        VALUES ($1,'S-A',3,'DRAFT',now(),'CNY','{x}',now())`, "23505", "rate_card_one_draft"},
		"an empty version published past RATE": {`INSERT INTO cargo_rate.rate_card (tenant_id,rate_card_id,side,scope,legal_entity_id,party_id,
            party_revision,name,created_by,created_at) VALUES ($1,'S-EMPTY','BUY','SUPPLIER','LE','coloader-b',3,'x','x',now());
INSERT INTO cargo_rate.rate_card_version (tenant_id,rate_card_id,version_no,status,valid_from,currency,preparers,prepared_at)
        VALUES ($1,'S-EMPTY',1,'DRAFT',now(),'CNY','{x}',now());
UPDATE cargo_rate.rate_card_version SET status='PUBLISHED',published_by='x',published_at=now() WHERE tenant_id=$1 AND rate_card_id='S-EMPTY'`,
			"23514", "rate_version_publishable"},
		"a card renamed":            {`UPDATE cargo_rate.rate_card SET name='x',revision=revision+1 WHERE tenant_id=$1 AND rate_card_id='T-AIR'`, "42501|23001", ""},
		"a version out of sequence": {strings.Replace(draftVersion, "9,", "5,", 1), "23514", "rate_card_version_sequence"},
		"preparers rewritten on publication": {`UPDATE cargo_rate.rate_card_version SET status='PUBLISHED',published_by='pricing-1',published_at=now(),
            preparers='{ghost}',approved_by='pricing-1',self_approved=false WHERE tenant_id=$1 AND rate_card_id='C-ACME'`, "23001", ""},
		"approved by another than the publisher": {`UPDATE cargo_rate.rate_card SET revision=revision+1 WHERE tenant_id=$1 AND rate_card_id='S-A';
UPDATE cargo_rate.rate_card_version SET status='PUBLISHED',published_by='clerk',published_at=now(),approved_by='pricing-manager',self_approved=false
            WHERE tenant_id=$1 AND rate_card_id='S-A'`, "23514", "rate_card_version_state"},
	} {
		err := c.statements(tc.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || !strings.Contains(tc.state, pgErr.Code) || (tc.constraint != "" && pgErr.ConstraintName != tc.constraint) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Review findings of RATE V0.5 part 1 on PostgreSQL: a self-approval is not laundered by another preparer; the
// database's starter list equals the application's.
func TestTariffApprovalAndStarterListOnPostgres(t *testing.T) {
	c := newCards(t)
	c.must(c.module.CreateRateCard(c.ctx, c.create("T-AIR", api.SideSell, api.ScopeTariff, "", "pricing-manager")))
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("T-AIR", c.h(0), time.Time{}, 0, "pricing-manager")))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set("T-AIR", 1, "pricing-1", air(freight(3000)))))
	_, err := c.module.PublishRateCardVersion(c.ctx, c.publish("T-AIR", 1, "pricing-manager"))
	if !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("the manager who drafted it, after a colleague set the entries: %v", err)
	}
	c.must(c.module.PublishRateCardVersion(c.ctx, c.publish("T-AIR", 1, "pricing-lead")))
	if v := c.card("T-AIR").Versions[0]; v.SelfApproved || v.ApprovedBy != "pricing-lead" || len(v.Preparers) != 2 {
		t.Fatalf("published: %+v", v)
	}
	type starter struct{ code, row string }
	var stored []starter
	err = (scope.TenantTransactor{DB: c.db, Tenant: c.tenant}).WithinTransaction(c.ctx, func(txCtx context.Context) error {
		q, err := c.db.ReadExecutor(txCtx)
		if err != nil {
			return err
		}
		rows, err := q.Query(txCtx, `SELECT code, concat_ws('|',name_zh,name_en,charge_group,calculator,coalesce(rate_unit,''),array_to_string(modes,','))
          FROM cargo_rate.charge_code_starter ORDER BY code`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x starter
			if err = rows.Scan(&x.code, &x.row); err != nil {
				return err
			}
			stored = append(stored, x)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range stored {
		v, ok := rate.StarterChargeCodes[rate.ChargeCode(x.code)]
		modes := make([]string, len(v.Modes))
		for i, m := range v.Modes {
			modes[i] = string(m)
		}
		want := strings.Join([]string{v.NameZH, v.NameEN, string(v.Group), string(v.Calculator), string(v.Unit), strings.Join(modes, ",")}, "|")
		if !ok || x.row != want {
			t.Errorf("starter %s: database %q, application %q", x.code, x.row, want)
		}
	}
	if len(stored) != len(rate.StarterChargeCodes) {
		t.Errorf("%d starter codes in the database, %d in the application", len(stored), len(rate.StarterChargeCodes))
	}
}

// statements runs statements separated by ";\n" in one transaction of the fixture's tenant; $1 is the tenant.
func (c cards) statements(sql string) error {
	return (scope.TenantTransactor{DB: c.db, Tenant: c.tenant}).WithinTransaction(c.ctx, func(txCtx context.Context) error {
		e, err := c.db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		for _, s := range strings.Split(sql, ";\n") {
			var args []any
			if strings.Contains(s, "$1") {
				args = append(args, c.tenant)
			}
			if _, err = e.Exec(txCtx, s, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestEvaluateRatesOnPostgres(t *testing.T) {
	c := newCards(t)
	publishedTariff(c)
	for _, s := range []struct {
		id, party string
		rate      int64
	}{{"S-A", "coloader-a", 2200}, {"S-B", "coloader-b", 2000}} {
		c.must(c.module.CreateRateCard(c.ctx, c.create(s.id, api.SideBuy, api.ScopeSupplier, s.party, "buyer-1")))
		c.must(c.module.DraftRateCardVersion(c.ctx, c.draft(s.id, c.h(0), time.Time{}, 0, "buyer-1")))
		c.must(c.module.SetRateCardEntries(c.ctx, c.set(s.id, 1, "buyer-1", air(freight(s.rate)))))
		c.must(c.module.PublishRateCardVersion(c.ctx, c.publish(s.id, 1, "buyer-1")))
	}
	evaluate := api.EvaluateRates{CommandID: "evaluate-1", TenantID: c.tenant, ActorID: "sales-1", SnapshotID: "RS-1", Purpose: api.PurposeQuoteLine,
		LegalEntityID: "LE", Sides: []string{api.SideBuy, api.SideSell}, CustomerPartyID: "acme", Mode: api.ModeAir, ContainerMode: api.ContainerLoose,
		Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD", Commodity: api.CommodityGeneral,
		Cargo: api.Cargo{Pieces: 3, GrossGrams: 100_000, VolumeCM3: 900_000, Documents: 1}, SubjectKind: "QUOTE", SubjectID: "Q-9", OccurredAt: c.h(4)}
	r := c.must(c.module.EvaluateRates(c.ctx, evaluate))
	if r.SubjectID != "RS-1" || r.NewRevision != 1 {
		t.Fatalf("evaluated: %+v", r)
	}
	if again, err := c.module.EvaluateRates(c.ctx, evaluate); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	s, err := c.module.GetRateSnapshot(c.ctx, api.GetRateSnapshot{TenantID: c.tenant, SnapshotID: "RS-1"})
	if err != nil {
		t.Fatal(err)
	}
	line := func(side, code string) api.RateSnapshotLine {
		for _, l := range s.Lines {
			if l.Side == side && l.ChargeCode == code {
				return l
			}
		}
		t.Fatalf("no %s %s in %+v", side, code, s.Lines)
		return api.RateSnapshotLine{}
	}
	// 150 kg chargeable (0.9 m3 at 6000 cm3/kg) in the 100 kg band: SELL 20.00 = 3,000.00; fuel 10% = 300.00
	if l := line(api.SideSell, "AIR_FREIGHT"); l.Amount != cny(300_000) || l.Quantity != 150_000 || l.BandFrom != 100_000 || l.RateCardID != "T-AIR" {
		t.Errorf("sell freight: %+v", l)
	}
	if l := line(api.SideSell, "FUEL_SURCHARGE"); l.Amount != cny(30_000) || l.BaseAmount != 300_000 {
		t.Errorf("fuel: %+v", l)
	}
	if l := line(api.SideSell, "CUSTOMS_CLEARING"); l.Gap != api.GapCallForPricing || l.Amount.Minor != 0 {
		t.Errorf("call for pricing: %+v", l)
	}
	// BUY: the lowest total, S-B at 10.00/kg from 100 kg = 1,500.00
	if l := line(api.SideBuy, "AIR_FREIGHT"); l.RateCardID != "S-B" || l.Amount != cny(150_000) {
		t.Errorf("buy freight: %+v", l)
	}
	if s.Customer.PartyRevision != 3 || s.EngineVersion != "RATE-ENGINE-1" || s.Sides[0] != api.SideSell || s.Edition != edition || len(s.Candidates) != 3 ||
		!s.At.Equal(c.h(4)) {
		t.Errorf("snapshot: %+v", s)
	}
	// a withdrawal ends the tariff when recorded: business times before then still rate on it
	c.must(c.module.WithdrawRateCardVersion(c.ctx, api.WithdrawRateCardVersion{CommandID: "withdraw-tariff", TenantID: c.tenant, ActorID: "pricing-manager",
		RateCardID: "T-AIR", ExpectedRevision: c.card("T-AIR").Revision, Version: 1, Reason: "replaced", OccurredAt: c.h(6)}))
	before := evaluate
	before.CommandID, before.SnapshotID, before.Sides, before.OccurredAt = "evaluate-before", "RS-B", []string{api.SideSell}, c.h(5)
	c.must(c.module.EvaluateRates(c.ctx, before))
	if sb, _ := c.module.GetRateSnapshot(c.ctx, api.GetRateSnapshot{TenantID: c.tenant, SnapshotID: "RS-B"}); len(sb.Lines) != 4 || sb.Lines[0].RateCardID != "T-AIR" {
		t.Errorf("rated before the withdrawal: %+v", sb.Lines)
	}
	// the consolidation's carrier is given: only its card
	given := evaluate
	given.CommandID, given.SnapshotID, given.Sides, given.Carrier, given.CustomerPartyID = "evaluate-2", "RS-2", []string{api.SideBuy}, "coloader-a", ""
	c.must(c.module.EvaluateRates(c.ctx, given))
	if s2, _ := c.module.GetRateSnapshot(c.ctx, api.GetRateSnapshot{TenantID: c.tenant, SnapshotID: "RS-2"}); s2.Lines[0].RateCardID != "S-A" ||
		s2.Lines[0].Amount != cny(180_000) || s2.Candidates[0].Reason != "CARRIER_GIVEN" {
		t.Errorf("carrier given: %+v", s2)
	}
	// a lane no card serves is a gap, never zero; a customer ORG does not confirm is refused
	gap := evaluate
	gap.CommandID, gap.SnapshotID, gap.Destination, gap.CustomerPartyID = "evaluate-3", "RS-3", "USLAX", ""
	c.must(c.module.EvaluateRates(c.ctx, gap))
	if s3, _ := c.module.GetRateSnapshot(c.ctx, api.GetRateSnapshot{TenantID: c.tenant, SnapshotID: "RS-3"}); len(s3.Lines) != 2 || s3.Lines[0].Gap != api.GapNoRate ||
		s3.Lines[1].Gap != api.GapNoRate {
		t.Errorf("no rate: %+v", s3.Lines)
	}
	stranger := evaluate
	stranger.CommandID, stranger.SnapshotID, stranger.CustomerPartyID = "evaluate-4", "RS-4", "coloader-a"
	_, err = c.module.EvaluateRates(c.ctx, stranger)
	c.rejected("a customer ORG does not confirm", err, api.ErrRejected, "RATE_PARTY_ROLE")
	estimate := evaluate
	estimate.CommandID, estimate.SnapshotID, estimate.Purpose = "evaluate-5", "RS-5", api.PurposeShipmentEstimate
	_, err = c.module.EvaluateRates(c.ctx, estimate)
	c.rejected("a shipment estimate before PricingBasis", err, api.ErrRejected, "quote lines")
	if counts := c.outboxTypes(t); strings.Count(strings.Join(counts, ","), api.RatesEvaluated) != 4 {
		t.Errorf("outbox: %v", counts)
	}
	for name, tc := range map[string]struct{ sql, state, constraint string }{
		"a snapshot line changed": {`UPDATE cargo_rate.rate_snapshot_line SET amount_minor=1 WHERE tenant_id=$1`, "42501|23001", ""},
		"a recorded snapshot extended": {`INSERT INTO cargo_rate.rate_snapshot_line (tenant_id,snapshot_id,line_no,side,charge_code,rate_card_id,version_no,entry_no,
            rate_line_no,calculator,quantity,charged,band_from,rate_minor,rate_scale,percent_bp,base_amount,minimum_applied,maximum_applied,currency,amount_minor)
            VALUES ($1,'RS-1',99,'BUY','AIR_FREIGHT','S-FAKE',1,1,1,'FLAT',0,0,0,1,0,0,0,false,false,'CNY',1)`, "23514", "rate_snapshot_written_once"},
		"a candidate added later": {`INSERT INTO cargo_rate.rate_snapshot_candidate (tenant_id,snapshot_id,candidate_no,side,rate_card_id,version_no,entry_no,taken,reason)
            VALUES ($1,'RS-B',99,'BUY','S-FAKE',1,1,true,'LOWEST_TOTAL')`, "23514", "rate_snapshot_written_once"},
		"a snapshot deleted": {`DELETE FROM cargo_rate.rate_snapshot_line WHERE tenant_id=$1`, "42501|23001", ""},
		"a gap priced at zero": {`INSERT INTO cargo_rate.rate_snapshot_line (tenant_id,snapshot_id,line_no,side,quantity,charged,band_from,rate_minor,rate_scale,
            percent_bp,base_amount,minimum_applied,maximum_applied,amount_minor,gap) VALUES ($1,'RS-3',9,'SELL',0,0,0,0,0,0,0,false,false,0,NULL)`, "23514", ""},
		"a row committed once the transaction moved to another tenant": {`INSERT INTO cargo_rate.rate_card (tenant_id,rate_card_id,side,scope,legal_entity_id,name,
            created_by,created_at) VALUES ($1,'T-X','SELL','TARIFF','LE','x','x',now());
SELECT set_config('app.tenant_id', 'another-tenant', true)`, "23514", "rate_tenant_scope"},
	} {
		err := c.statements(tc.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || !strings.Contains(tc.state, pgErr.Code) || (tc.constraint != "" && pgErr.ConstraintName != tc.constraint) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// raceCards runs first and second at once over a fresh world, alternating which starts a little later, and expects
// exactly one to commit; it logs which lost.
func raceCards(t *testing.T, rounds int, setup func(t *testing.T) cards, first, second func(c cards) error, lost func(error) bool) {
	t.Helper()
	var losses [2]int
	for round := range rounds {
		c := setup(t)
		var errs [2]error
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, fn := range []func(cards) error{first, second} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if i == round%2 {
					time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
				}
				errs[i] = fn(c)
			}()
		}
		close(start)
		wg.Wait()
		switch {
		case (errs[0] == nil) == (errs[1] == nil):
			t.Fatalf("round %d: %v / %v, want exactly one to commit", round, errs[0], errs[1])
		case errs[0] != nil && !lost(errs[0]), errs[1] != nil && !lost(errs[1]):
			t.Fatalf("round %d: lost with %v / %v", round, errs[0], errs[1])
		case errs[0] != nil:
			losses[0]++
		default:
			losses[1]++
		}
	}
	t.Logf("the first lost %d rounds, the second %d", losses[0], losses[1])
}

// Two people define the same code at once: one version 1 is written, the other hears a conflict.
func TestCodeDefinitionsRace(t *testing.T) {
	define := func(actor string) func(c cards) error {
		return func(c cards) error {
			_, err := c.module.DefineChargeCode(c.ctx, api.DefineChargeCode{CommandID: "define-" + actor, TenantID: c.tenant, ActorID: actor, Code: "STORAGE",
				NameZH: "仓储费", NameEN: "Storage " + actor, Group: api.GroupOrigin, Calculator: api.CalcFlat, Modes: []string{api.ModeSea}, OccurredAt: c.h(0)})
			return err
		}
	}
	raceCards(t, 6, newCards, define("md-1"), define("md-2"), func(err error) bool {
		return errors.Is(err, api.ErrConflict) || errors.Is(err, api.ErrRejected)
	})
}

// Two drafts of the same tariff at once, at the same revision: one is written, the other hears a conflict.
func TestDraftsRaceForOneCard(t *testing.T) {
	setup := func(t *testing.T) cards {
		c := newCards(t)
		c.must(c.module.CreateRateCard(c.ctx, c.create("T-AIR", api.SideSell, api.ScopeTariff, "", "pricing-1")))
		return c
	}
	draft := func(actor string) func(c cards) error {
		return func(c cards) error {
			d := api.DraftRateCardVersion{CommandID: "draft-" + actor, TenantID: c.tenant, ActorID: actor, RateCardID: "T-AIR", ExpectedRevision: 1,
				ValidFrom: c.h(0), Currency: "CNY", OccurredAt: c.h(1)}
			_, err := c.module.DraftRateCardVersion(c.ctx, d)
			return err
		}
	}
	raceCards(t, 6, setup, draft("pricing-1"), draft("pricing-2"), func(err error) bool { return errors.Is(err, api.ErrConflict) })
}

// repeatable runs statements at REPEATABLE READ in the fixture's tenant, with a connection of its own.
func (c cards) repeatable(sql string) error {
	pool, err := pgxpool.New(c.ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		return err
	}
	defer pool.Close()
	tx, err := pool.BeginTx(c.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback(c.ctx)
	if _, err = tx.Exec(c.ctx, `SELECT set_config('app.tenant_id', $1, true)`, c.tenant); err != nil {
		return err
	}
	for _, s := range strings.Split(sql, ";\n") {
		if _, err = tx.Exec(c.ctx, s, c.tenant); err != nil {
			return err
		}
	}
	return tx.Commit(c.ctx)
}

// A withdrawal written past RATE at REPEATABLE READ is refused at commit: the commit check reads what committed.
func TestRepeatableReadRefusedOnPostgres(t *testing.T) {
	c := newCards(t)
	publishedTariff(c)
	err := c.repeatable(`UPDATE cargo_rate.rate_card SET revision=revision+1 WHERE tenant_id=$1 AND rate_card_id='T-AIR';
UPDATE cargo_rate.rate_card_version SET status='WITHDRAWN',withdrawn_by='x',withdrawn_at=now(),withdraw_reason='x' WHERE tenant_id=$1
  AND rate_card_id='T-AIR'`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "rate_read_committed" {
		t.Fatalf("REPEATABLE READ: %v", err)
	}
	if card := c.card("T-AIR"); card.Versions[0].Status != api.VersionPublished {
		t.Fatalf("withdrawn anyway: %+v", card)
	}
}

// PricingBasis v1 rated for FIN (RATE V0.5 part 2): a shipment rated on both sides for its customer, a consolidation on
// the BUY side with its carrier, each basis equal to its snapshot, and what the database refuses past RATE.
func TestRatedBasesOnPostgres(t *testing.T) {
	c := newCards(t)
	publishedTariff(c)
	c.must(c.module.CreateRateCard(c.ctx, c.create("S-A", api.SideBuy, api.ScopeSupplier, "coloader-a", "buyer-1")))
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("S-A", c.h(0), time.Time{}, 0, "buyer-1")))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set("S-A", 1, "buyer-1", air(freight(2200), api.RateLine{ChargeCode: "THC", Currency: "CNY",
		Calculator: api.CalcFlat, Rate: 8_000}))))
	c.must(c.module.PublishRateCardVersion(c.ctx, c.publish("S-A", 1, "buyer-1")))
	ship := api.RateShipmentEstimate{CommandID: "fin/rate-B", TenantID: c.tenant, ActorID: "fin-1", BasisID: "FIN-1", LegalEntityID: "LE",
		CustomerPartyID: "acme", ShipmentID: "SH-B", PlanVersion: 2, Mode: api.ModeAir, ContainerMode: api.ContainerLoose, Origin: "CNPVG",
		Destination: "DEFRA", ServiceLevel: "STANDARD", Cargo: api.Cargo{Pieces: 6, GrossGrams: 100_000, VolumeCM3: 900_000}, OccurredAt: c.h(4)}
	r := c.must(c.module.RateShipmentEstimate(c.ctx, ship))
	if r.SubjectID != "FIN-1" || r.NewRevision != 1 {
		t.Fatalf("issued: %+v", r)
	}
	if again, err := c.module.RateShipmentEstimate(c.ctx, ship); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	b, err := c.module.GetPricingBasis(c.ctx, api.GetPricingBasis{TenantID: c.tenant, BasisID: "FIN-1"})
	if err != nil {
		t.Fatal(err)
	}
	// 150 kg: SELL freight 3,000.00 and fuel 300.00 (10%), a document fee; BUY freight at 12.00 = 1,800.00 and THC 80.00
	want := map[string][2]int64{"AIR_FREIGHT": {300_000, 180_000}, "DOC_FEE": {15_000, -1}, "FUEL_SURCHARGE": {30_000, -1}, "THC": {-1, 8_000}}
	if b.Purpose != api.BasisShipmentEstimate || b.Customer.PartyID != "acme" || b.Customer.PartyRevision != 3 || b.SubjectKind != "SHIPMENT" ||
		b.SubjectID != "SH-B" || b.SubjectVersion != 2 || b.SnapshotID != "FIN-1" || len(b.Lines) != 4 || len(b.Gaps) != 1 ||
		b.Gaps[0].Gap != api.GapCallForPricing || b.Gaps[0].ChargeCode != "CUSTOMS_CLEARING" {
		t.Fatalf("basis: %+v", b)
	}
	for _, l := range b.Lines {
		w := want[l.ChargeCode]
		if l.Source != api.SourceRated || l.HasSell != (w[0] >= 0) || l.HasCost != (w[1] >= 0) || (l.HasSell && l.Sell != cny(w[0])) ||
			(l.HasCost && (l.Cost != cny(w[1]) || l.CreditorPartyID != "coloader-a")) {
			t.Errorf("line %+v, want %v", l, w)
		}
	}
	if s, _ := c.module.GetRateSnapshot(c.ctx, api.GetRateSnapshot{TenantID: c.tenant, SnapshotID: "FIN-1"}); s.Purpose != api.PurposeShipmentEstimate ||
		len(s.Lines) != 6 {
		t.Errorf("its snapshot: %+v", s)
	}
	consol := api.RateConsolCost{CommandID: "fin/rate-P", TenantID: c.tenant, ActorID: "fin-1", BasisID: "FIN-2", LegalEntityID: "LE",
		ConsolidationID: "CONSOL-1", ConsolidationRevision: 7, Mode: api.ModeAir, ContainerMode: api.ContainerLoose, Origin: "CNPVG", Destination: "DEFRA",
		ServiceLevel: "STANDARD", Carrier: "coloader-a", Cargo: api.Cargo{Pieces: 12, GrossGrams: 400_000, VolumeCM3: 2_400_000}, OccurredAt: c.h(4)}
	c.must(c.module.RateConsolCost(c.ctx, consol))
	if b, err = c.module.GetPricingBasis(c.ctx, api.GetPricingBasis{TenantID: c.tenant, BasisID: "FIN-2"}); err != nil ||
		b.Purpose != api.BasisConsolCostEstimate || b.Customer.PartyID != "" || len(b.Lines) != 2 || b.Lines[0].HasSell || b.Lines[0].Cost != cny(480_000) {
		t.Fatalf("consolidation basis (400 kg at 12.00): %+v %v", b, err)
	}
	if _, err = c.module.GetPricingBasis(c.ctx, api.GetPricingBasis{TenantID: c.tenant, BasisID: "FIN-NONE"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown basis: %v", err)
	}
	stranger := ship
	stranger.CommandID, stranger.BasisID, stranger.CustomerPartyID = "fin/rate-X", "FIN-3", "coloader-b"
	_, err = c.module.RateShipmentEstimate(c.ctx, stranger)
	c.rejected("a shipment for a party ORG does not confirm as recipient", err, api.ErrRejected, "RATE_PARTY_ROLE")
	if counts := strings.Join(c.outboxTypes(t), ","); strings.Count(counts, api.PricingBasisIssued) != 2 {
		t.Errorf("outbox: %v", counts)
	}
	basisLine := `INSERT INTO cargo_rate.pricing_basis_line (tenant_id,basis_id,line_no,charge_code,sell_currency,sell_minor,sell_line_no)
            VALUES ($1,'%s',9,'PICKUP','CNY',1,1)`
	for name, tc := range map[string]struct{ sql, state, constraint string }{
		"a basis changed":         {`UPDATE cargo_rate.pricing_basis_line SET sell_minor=1 WHERE tenant_id=$1`, "42501|23001", ""},
		"a line added to a basis": {strings.Replace(basisLine, "%s", "FIN-1", 1), "23514", "pricing_basis_consistent"},
		"a basis unlike its snapshot": {`INSERT INTO cargo_rate.rate_snapshot (tenant_id,snapshot_id,purpose,legal_entity_id,sides,mode,container_mode,origin,
            destination,edition,service_level,commodity,business_at,pieces,gross_grams,volume_cm3,documents,subject_kind,subject_id,subject_version,
            engine_version,evaluated_by,recorded_at) VALUES ($1,'S-X','CONSOL_COST_ESTIMATE','LE','{BUY}','AIR','LOOSE','CNPVG','DEFRA','x','STANDARD',
            'GENERAL',now(),0,0,0,0,'CONSOLIDATION','C-X',1,'RATE-ENGINE-1','x',now());
INSERT INTO cargo_rate.rate_snapshot_line (tenant_id,snapshot_id,line_no,side,charge_code,rate_card_id,version_no,entry_no,rate_line_no,calculator,
            quantity,charged,band_from,rate_minor,rate_scale,percent_bp,base_amount,minimum_applied,maximum_applied,currency,amount_minor)
            VALUES ($1,'S-X',1,'BUY','THC','S-A',1,1,2,'FLAT',0,0,0,8000,0,0,0,false,false,'CNY',8000);
INSERT INTO cargo_rate.pricing_basis (tenant_id,basis_id,purpose,legal_entity_id,subject_kind,subject_id,subject_version,snapshot_id,issued_by,issued_at)
            VALUES ($1,'B-X','CONSOL_COST_ESTIMATE','LE','CONSOLIDATION','C-X',1,'S-X','x',now());
INSERT INTO cargo_rate.pricing_basis_line (tenant_id,basis_id,line_no,charge_code,cost_currency,cost_minor,cost_line_no)
            VALUES ($1,'B-X',1,'THC','CNY',1,1)`, "23514", "pricing_basis_consistent"},
		"a basis of a snapshot recorded before": {`INSERT INTO cargo_rate.pricing_basis (tenant_id,basis_id,purpose,legal_entity_id,subject_kind,subject_id,
            subject_version,snapshot_id,issued_by,issued_at) VALUES ($1,'B-Y','CONSOL_COST_ESTIMATE','LE','CONSOLIDATION','CONSOL-1',7,'FIN-2','x',now())`,
			"23505|23514", ""},
	} {
		err := c.statements(tc.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || !strings.Contains(tc.state, pgErr.Code) || (tc.constraint != "" && pgErr.ConstraintName != tc.constraint) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
