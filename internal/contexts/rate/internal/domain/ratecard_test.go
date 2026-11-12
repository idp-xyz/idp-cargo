package domain

import (
	"errors"
	"testing"
	"time"
)

func codeInput() ChargeCodeInput {
	return ChargeCodeInput{NameZH: "仓储费", NameEN: "Storage", Group: GroupOrigin, Calculator: CalcPerUnit, Unit: UnitKgGross, Modes: []Mode{ModeSea, ModeAir, ModeSea}}
}

func TestChargeCodeCatalogue(t *testing.T) {
	c := RestoreChargeCode("t", "STORAGE", ChargeCodeVersion{}, false)
	if c.Defined() {
		t.Fatal("STORAGE is not a starter code")
	}
	if err := c.Revise(0, codeInput(), "md", t0); !errors.Is(err, ErrChargeCodeUnknown) {
		t.Fatalf("revise undefined: %v", err)
	}
	if err := c.Define(codeInput(), "md", t0); err != nil {
		t.Fatal(err)
	}
	if v := c.Current(); v.Version != 1 || !v.Active || len(v.Modes) != 2 || v.Modes[0] != ModeAir || c.Pending()[0].Type != EventChargeCodeDefined {
		t.Fatalf("defined: %+v %+v", v, c.Pending())
	}
	// stored: deactivate, revise refused while inactive, reactivate
	c = RestoreChargeCode("t", "STORAGE", c.Current(), true)
	if err := c.Define(codeInput(), "md", t0); !errors.Is(err, ErrChargeCodeExists) {
		t.Fatalf("define twice: %v", err)
	}
	if err := c.Deactivate(0, "md", t0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if err := c.Deactivate(1, "md", t0); err != nil || c.Current().Active || c.Current().Version != 2 {
		t.Fatalf("deactivated: %+v %v", c.Current(), err)
	}
	c = RestoreChargeCode("t", "STORAGE", c.Current(), true)
	if err := c.Revise(2, codeInput(), "md", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("revise inactive: %v", err)
	}
	if err := c.Deactivate(2, "md", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("deactivate twice: %v", err)
	}
	if err := c.Reactivate(2, "md", t0); err != nil || !c.Current().Active || c.Pending()[0].Type != EventChargeCodeReactivated {
		t.Fatalf("reactivated: %+v %v", c.Current(), err)
	}
	// a starter code stands at version 0 until the tenant changes it
	s := RestoreChargeCode("t", "AIR_FREIGHT", ChargeCodeVersion{}, false)
	if !s.Defined() || s.Version() != 0 || !s.Current().AppliesTo(ModeAir) || s.Current().AppliesTo(ModeSea) {
		t.Fatalf("starter: %+v", s.Current())
	}
	in := codeInput()
	in.Group, in.Calculator, in.Unit, in.Modes = GroupFreight, CalcPerUnit, UnitKgChargeable, []Mode{ModeAir}
	if err := s.Revise(0, in, "md", t0); err != nil || s.Current().Version != 1 || s.Current().NameEN != "Storage" {
		t.Fatalf("starter revised: %+v %v", s.Current(), err)
	}
	for name, mutate := range map[string]func(*ChargeCodeInput){
		"no name":          func(in *ChargeCodeInput) { in.NameEN = " " },
		"group":            func(in *ChargeCodeInput) { in.Group = "OTHER" },
		"per unit no unit": func(in *ChargeCodeInput) { in.Unit = UnitNone },
		"flat with unit":   func(in *ChargeCodeInput) { in.Calculator = CalcFlat },
		"no mode":          func(in *ChargeCodeInput) { in.Modes = nil },
		"road":             func(in *ChargeCodeInput) { in.Modes = []Mode{"ROAD"} },
	} {
		in := codeInput()
		mutate(&in)
		if err := RestoreChargeCode("t", "X_CODE", ChargeCodeVersion{}, false).Define(in, "md", t0); !errors.Is(err, ErrInvalidChargeCode) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, code := range []ChargeCode{"x", "A", "1ABC", "ABCDEFGHIJKLMNOPQ", "A-B"} {
		if err := RestoreChargeCode("t", code, ChargeCodeVersion{}, false).Define(codeInput(), "md", t0); !errors.Is(err, ErrInvalidChargeCode) {
			t.Errorf("code %q: %v", code, err)
		}
	}
}

type catalogue map[ChargeCode]ChargeCodeVersion

func (c catalogue) ChargeCode(code ChargeCode) (ChargeCodeVersion, bool) {
	if v, ok := c[code]; ok {
		return v, true
	}
	v, ok := StarterChargeCodes[code]
	return v, ok
}
func (catalogue) ServiceLevelListed(code string) bool { _, ok := ListedServiceLevel(code); return ok }

func tariffCard(t *testing.T) *RateCard {
	t.Helper()
	c, err := NewRateCard(RateCardDraft{TenantID: "t", RateCardID: "T1", Side: SideSell, Scope: ScopeTariff, LegalEntityID: "LE", Name: "Export air tariff"}, "pricing-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRateCardLifecycle(t *testing.T) {
	c := tariffCard(t)
	month := VersionValidity{From: t0, Until: t0.AddDate(0, 1, 0)}
	no, err := c.DraftVersion(VersionInput{Validity: month, Currency: "CNY", SourceRef: "Tariff 2026-11"}, nil, "pricing-1", t0)
	if err != nil || no != 1 {
		t.Fatal(no, err)
	}
	if _, err = c.DraftVersion(VersionInput{Validity: month, Currency: "CNY"}, nil, "pricing-1", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a second draft: %v", err)
	}
	if err = c.Publish(1, catalogue{}, Publication{By: "pm", At: t0, ApprovedBy: "pm"}); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("no entry: %v", err)
	}
	if err = c.SetEntries(1, []RateEntry{airEntry(1, airFreight(1), flat(2, "DOC_FEE", "CNY", 15_000))}, "pricing-2", t0); err != nil {
		t.Fatal(err)
	}
	// the tariff is approved by the pricing manager; the one who prepared it approves it only as a self-approval
	if err = c.Publish(1, catalogue{}, Publication{By: "pricing-2", At: t0}); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("unapproved: %v", err)
	}
	if err = c.Publish(1, catalogue{}, Publication{By: "pricing-2", At: t0, ApprovedBy: "pricing-2"}); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("self-approval unflagged: %v", err)
	}
	inactive := StarterChargeCodes["DOC_FEE"]
	inactive.Active = false
	if err = c.Publish(1, catalogue{"DOC_FEE": inactive}, Publication{By: "pm", At: t0, ApprovedBy: "pm"}); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("an inactive code: %v", err)
	}
	if err = c.Publish(1, catalogue{}, Publication{By: "pm", At: t0, ApprovedBy: "pm"}); err != nil {
		t.Fatal(err)
	}
	if v := c.State().Versions[0]; v.Status != VersionPublished || v.ApprovedBy != "pm" || v.SelfApproved || len(c.Pending()) != 1 {
		t.Fatalf("published: %+v", v)
	}
	if err = c.SetEntries(1, nil, "pricing-1", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a published version changed: %v", err)
	}
	// the next version may not overlap the published one; once that one is withdrawn it may
	entries := c.State().Versions[0].Entries
	no, _ = c.DraftVersion(VersionInput{Validity: VersionValidity{From: t0.AddDate(0, 0, 10)}, Currency: "CNY"}, entries, "pricing-1", t0)
	if err = c.Publish(no, catalogue{}, Publication{By: "pricing-1", At: t0, ApprovedBy: "pricing-1", SelfApprove: true}); !errors.Is(err, ErrVersionOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	if err = c.Withdraw(1, "", "pm", t0); !errors.Is(err, ErrInvalidRateCard) {
		t.Fatalf("no reason: %v", err)
	}
	if err = c.Withdraw(1, "fuel index changed", "pm", t0.AddDate(0, 0, 10)); err != nil {
		t.Fatal(err)
	}
	if err = c.Publish(no, catalogue{}, Publication{By: "pricing-1", At: t0, ApprovedBy: "pricing-1", SelfApprove: true}); err != nil {
		t.Fatal(err)
	}
	if v := c.State().Versions[1]; !v.SelfApproved || len(v.Entries) != 1 || !c.HeaderChanged(1) || !c.ContentChanged(2) {
		t.Fatalf("second version: %+v", v)
	}
	if err = c.Withdraw(1, "again", "pm", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("withdrawn twice: %v", err)
	}
}

func TestRateCardScopesAndPublication(t *testing.T) {
	for name, d := range map[string]RateCardDraft{
		"buy tariff":           {Side: SideBuy, Scope: ScopeTariff},
		"sell supplier":        {Side: SideSell, Scope: ScopeSupplier, PartyID: "mu", PartyRevision: 1},
		"tariff with party":    {Side: SideSell, Scope: ScopeTariff, PartyID: "acme", PartyRevision: 1},
		"customer unconfirmed": {Side: SideSell, Scope: ScopeCustomer, PartyID: "acme"},
	} {
		d.TenantID, d.RateCardID, d.LegalEntityID, d.Name = "t", "C1", "LE", "card"
		if _, err := NewRateCard(d, "p", t0); !errors.Is(err, ErrInvalidRateCard) {
			t.Errorf("%s: %v", name, err)
		}
	}
	customer, _ := NewRateCard(RateCardDraft{TenantID: "t", RateCardID: "C1", Side: SideSell, Scope: ScopeCustomer, LegalEntityID: "LE",
		PartyID: "acme", PartyRevision: 2, Name: "ACME"}, "p", t0)
	customer.DraftVersion(VersionInput{Validity: VersionValidity{From: t0}, Currency: "CNY"}, []RateEntry{airEntry(1, airFreight(1))}, "p", t0)
	if err := customer.Publish(1, catalogue{}, Publication{By: "pm", At: t0, ApprovedBy: "pm"}); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("a customer card published directly: %v", err)
	}
	supplier, _ := NewRateCard(RateCardDraft{TenantID: "t", RateCardID: "S1", Side: SideBuy, Scope: ScopeSupplier, LegalEntityID: "LE",
		PartyID: "airline-mu", PartyRevision: 4, Name: "MU PVG"}, "p", t0)
	if _, err := supplier.DraftVersion(VersionInput{Validity: VersionValidity{From: t0}, Currency: "CNY"}, nil, "p", t0); !errors.Is(err, ErrInvalidRateCard) {
		t.Fatalf("a supplier card without its source: %v", err)
	}
	supplier.DraftVersion(VersionInput{Validity: VersionValidity{From: t0}, Currency: "CNY", SourceRef: "MU contract 2026/88"}, nil, "p", t0)
	sea := airEntry(1, flat(1, "AIR_FREIGHT", "CNY", 1))
	sea.Mode, sea.ContainerMode = ModeSea, ContainerLCL
	supplier.SetEntries(1, []RateEntry{sea}, "p", t0)
	if err := supplier.Publish(1, catalogue{}, Publication{By: "p", At: t0}); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("an AIR code on a SEA entry: %v", err)
	}
	unknown := airEntry(1, flat(1, "NOT_LISTED", "CNY", 1))
	supplier.SetEntries(1, []RateEntry{unknown}, "p", t0)
	if err := supplier.Publish(1, catalogue{}, Publication{By: "p", At: t0}); !errors.Is(err, ErrChargeCodeUnknown) {
		t.Fatalf("an unlisted code: %v", err)
	}
	supplier.SetEntries(1, []RateEntry{airEntry(1, airFreight(1))}, "p", t0)
	if err := supplier.Publish(1, catalogue{}, Publication{By: "p", At: t0}); err != nil {
		t.Fatalf("a supplier card needs no tariff approval: %v", err)
	}
}

func TestRateEntryAndLineChecks(t *testing.T) {
	c := tariffCard(t)
	c.DraftVersion(VersionInput{Validity: VersionValidity{From: t0, Until: t0.AddDate(0, 1, 0)}, Currency: "CNY"}, nil, "p", t0)
	ok := func() RateEntry { return airEntry(1, airFreight(1), flat(2, "DOC_FEE", "CNY", 1)) }
	for name, mutate := range map[string]func(*RateEntry){
		"numbering":             func(e *RateEntry) { e.No = 2 },
		"line numbering":        func(e *RateEntry) { e.Lines[1].No = 3 },
		"code twice":            func(e *RateEntry) { e.Lines[1].ChargeCode = "AIR_FREIGHT" },
		"FCL":                   func(e *RateEntry) { e.Mode, e.ContainerMode = ModeSea, "FCL" },
		"same ends":             func(e *RateEntry) { e.Destination = e.Origin },
		"no edition":            func(e *RateEntry) { e.Edition = "" },
		"commodity":             func(e *RateEntry) { e.Commodity = "DG" },
		"no lines":              func(e *RateEntry) { e.Lines = nil },
		"breaks descending":     func(e *RateEntry) { e.Lines[0].Breaks[1].From = 10_000 },
		"no step":               func(e *RateEntry) { e.Lines[0].Step = 0 },
		"no volume factor":      func(e *RateEntry) { e.Lines[0].VolumeFactor = 0 },
		"maximum below minimum": func(e *RateEntry) { e.Lines[0].Maximum = 1 },
		"flat with breaks":      func(e *RateEntry) { e.Lines[1].Breaks = []RateBreak{{From: 1, Rate: 1}} },
		"percent of a missing": func(e *RateEntry) {
			e.Lines[1] = RateLine{No: 2, ChargeCode: "FUEL_SURCHARGE", Currency: "CNY", Calculator: CalcPercentOf, PercentBP: 100, BaseCode: "THC"}
		},
		"percent of itself": func(e *RateEntry) {
			e.Lines[1] = RateLine{No: 2, ChargeCode: "FUEL_SURCHARGE", Currency: "CNY", Calculator: CalcPercentOf, PercentBP: 100, BaseCode: "FUEL_SURCHARGE"}
		},
		"call for pricing rate": func(e *RateEntry) { e.Lines[1].CallForPricing = true },
		"validity outside":      func(e *RateEntry) { e.Validity.From, e.Validity.Until = t0.Add(-time.Hour), t0.Add(time.Hour) },
		"scale 7":               func(e *RateEntry) { e.Lines[0].Rate.Scale = 7 },
	} {
		e := ok()
		e.Lines[0].Breaks = append([]RateBreak(nil), e.Lines[0].Breaks...)
		mutate(&e)
		if err := c.SetEntries(1, []RateEntry{e}, "p", t0); !errors.Is(err, ErrInvalidRateCard) {
			t.Errorf("%s: %v", name, err)
		}
	}
	twice := ok()
	twice.No = 2
	if err := c.SetEntries(1, []RateEntry{ok(), twice}, "p", t0); !errors.Is(err, ErrInvalidRateCard) {
		t.Errorf("the same lane twice: %v", err)
	}
	// the same lane in two halves of the month is fine
	first, second := ok(), ok()
	second.No = 2
	first.Validity.From, first.Validity.Until = t0, t0.AddDate(0, 0, 15)
	second.Validity.From, second.Validity.Until = t0.AddDate(0, 0, 15), t0.AddDate(0, 1, 0)
	if err := c.SetEntries(1, []RateEntry{first, second}, "p", t0); err != nil {
		t.Errorf("two halves: %v", err)
	}
}

func TestStarterCodesAreValid(t *testing.T) {
	for code, v := range StarterChargeCodes {
		in := ChargeCodeInput{NameZH: v.NameZH, NameEN: v.NameEN, Group: v.Group, Calculator: v.Calculator, Unit: v.Unit, Modes: v.Modes}
		if !ValidChargeCode(code) || in.check() != nil || !v.Active || v.Version != 0 {
			t.Errorf("starter code %s: %+v %v", code, v, in.check())
		}
	}
}
