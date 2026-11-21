package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// sealedBasis is airBasis as a sea consolidation of one sealed container with its packing list, so that a basis carries
// what a fingerprint reads of a unit.
func sealedBasis() CostBasis {
	b := airBasis()
	b.Kind = "SEA_LCL_OWN"
	b.Houses[0].Units = []HouseUnitFacts{{UnitNo: 1, LoadedPacks: 10, LoadedGrossWeight: kg(300), LoadedVolume: cmq(1_200_000)}}
	b.Houses[1].Units = []HouseUnitFacts{{UnitNo: 1, LoadedPacks: 5, LoadedGrossWeight: kg(100), LoadedVolume: cmq(1_200_000)}}
	b.LoadUnits = []LoadUnitFacts{{UnitNo: 1, TypeCode: "22G1", TEUTenths: 10, Status: LoadUnitClosed, ExecutionID: "X-1", ExecutionState: "CLOSED", ExecutionRevision: 4}}
	b.PackingLists = []UnitPackingListFacts{{UnitNo: 1, ExecutionID: "X-1", ExecutionState: "CLOSED", Digest: "digest-1"}}
	return b
}

// reworked is a sealed basis whose container was reworked once, asked by who, and sealed again (or given up).
func reworked(who, end string) CostBasis {
	b := sealedBasis()
	b.PackingLists[0].Digest = "digest-2"
	b.PackingLists[0].Reworks = []ReworkFacts{{No: 1, AskedBy: who, End: end, EndedAt: hour(6)}}
	return b
}

func inRework(b CostBasis) CostBasis {
	b.LoadUnits[0].ExecutionState, b.PackingLists[0].ExecutionState = ExecutionRework, ExecutionRework
	b.PackingLists[0].Reworks = []ReworkFacts{{No: 1, AskedBy: "ops-asker"}}
	return b
}

func previewed(t *testing.T, basis CostBasis, methods ...string) *ConsolCostPool {
	t.Helper()
	p := pool(t, basis)
	for i, m := range methods {
		recordCost(t, p, []string{"FREIGHT", "HANDLING"}[i], cny(100_000), m)
	}
	return p
}

func preview(t *testing.T, p *ConsolCostPool, basis CostBasis, by string, h int) int {
	t.Helper()
	if err := p.Preview(ApportionInput{Jobs: jobs()}, basis, by, hour(h)); err != nil {
		t.Fatal(err)
	}
	return len(p.State().Previews)
}

func submit(p *ConsolCostPool, no int, basis CostBasis, by string, h int) error {
	return p.Submit(no, SubmitInput{Basis: basis, Jobs: jobs()}, by, hour(h))
}

func refused(t *testing.T, what string, err error, kind error, parts ...string) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("%s: %v, want %v", what, err, kind)
	}
	for _, part := range parts {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("%s: %q does not say %q", what, err, part)
		}
	}
}

// A preview stores the figures and the fingerprint and changes nothing else: no apportionment, no event, no cost.
func TestAPreviewChangesNothingButTheListOfPreviews(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable)
	events, costs := len(p.Pending()), p.State().Lines
	no := preview(t, p, sealedBasis(), "ops-1", 4)
	st := p.State()
	if no != 1 || len(st.Apportionments) != 0 || len(st.Previews) != 1 || len(p.Pending()) != events || p.State().Lines[0].Versions[0] != costs[0].Versions[0] {
		t.Fatalf("a preview changed the pool: %+v", st)
	}
	v := st.Previews[0]
	s := v.Lines[0].Shares
	if v.By != "ops-1" || v.Stage != BasisActual || v.BasisRevision != 7 || len(s) != 2 || s[0].Amount != 50_000 || s[1].Amount != 50_000 || s[0].Unit != UnitMilliFreightTon || s[0].JobID != "JOB-1" {
		t.Fatalf("the preview: %+v", v)
	}
	if len(v.FingerprintSHA256) != 64 || !strings.HasPrefix(v.Fingerprint, "basis\trevision 7\n") {
		t.Fatalf("the fingerprint: %q %s", v.Fingerprint, v.FingerprintSHA256)
	}
	added := p.AddedPreviews()
	if len(added) != 1 {
		t.Fatalf("added previews: %+v", added)
	}
	p.Acknowledge()
	if len(p.AddedPreviews()) != 0 {
		t.Fatal("an acknowledged preview is added again")
	}
}

// A submission writes the previewed figures as an apportionment that names the preview, by the submitter, with the
// event of an apportionment; the figures are those the one-step command wrote.
func TestASubmissionWritesThePreviewedApportionment(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, p, sealedBasis(), "ops-1", 4)
	if err := submit(p, no, sealedBasis(), "fin-2", 5); err != nil {
		t.Fatal(err)
	}
	st := p.State()
	a := st.Apportionments[0]
	if len(st.Apportionments) != 1 || a.No != 1 || a.PreviewNo != 1 || a.By != "fin-2" || !a.At.Equal(hour(5)) || a.Stage != BasisActual || a.BasisRevision != 7 {
		t.Fatalf("the apportionment: %+v", a)
	}
	once := previewed(t, sealedBasis(), MethodChargeable)
	if err := apportion(once, ApportionInput{Jobs: jobs()}, sealedBasis(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if got, want := a.Lines[0].Shares, once.State().Apportionments[0].Lines[0].Shares; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("shares %+v, want %+v", got, want)
	}
	var types []string
	for _, e := range p.Pending() {
		types = append(types, e.Type)
	}
	if strings.Join(types, " ") != "ConsolCostPoolOpened.v1 ConsolCostRecorded.v1 ConsolCostsApportioned.v1" {
		t.Fatalf("events: %v", types)
	}
	for _, no := range []int{0, -1, 2} {
		if err := submit(p, no, sealedBasis(), "fin-2", 5); !errors.Is(err, ErrInvalid) {
			t.Fatalf("preview %d, which the pool does not have: %v", no, err)
		}
	}
	if err := p.Submit(1, SubmitInput{Basis: CostBasis{ConsolidationID: "CONSOL-9"}}, "fin-2", hour(5)); !errors.Is(err, ErrPreviewSubmitted) {
		t.Fatalf("a preview submitted twice is refused before anything else is read: %v", err)
	}
}

func TestAPreviewIsSubmittedOnce(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, p, sealedBasis(), "ops-1", 4)
	if err := submit(p, no, sealedBasis(), "fin-1", 5); err != nil {
		t.Fatal(err)
	}
	refused(t, "the same preview again", submit(p, no, sealedBasis(), "fin-1", 6), ErrPreviewSubmitted, "preview 1 was submitted as apportionment 1", "preview again")
	// a later preview is submitted as the second apportionment
	second := preview(t, p, sealedBasis(), "ops-1", 6)
	if err := submit(p, second, sealedBasis(), "fin-1", 6); err != nil || p.State().Apportionments[1].PreviewNo != 2 {
		t.Fatalf("the second preview: %v %+v", err, p.State().Apportionments)
	}
	// the submission is not made before its preview
	third := preview(t, p, sealedBasis(), "ops-1", 8)
	refused(t, "a submission dated before its preview", submit(p, third, sealedBasis(), "fin-1", 7), ErrInvalid, "before the preview")
}

// Every part of what a preview reads changes the fingerprint; what it does not read does not.
func TestEachPartOfTheFingerprintChangesIt(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable, MethodRevenue)
	lines := []LineApportionment{{LineNo: 2, Method: MethodRevenue, Shares: []Share{{ShipmentID: "HS1", Used: true}, {ShipmentID: "HS2", Used: true}}}}
	base := p.fingerprint(sealedBasis(), jobs(), map[string]map[string]int64{"HS1": {"HANDLING": 10}, "HS2": {"HANDLING": 20}}, lines)
	if again := p.fingerprint(sealedBasis(), jobs(), map[string]map[string]int64{"HS1": {"HANDLING": 10}, "HS2": {"HANDLING": 20}}, lines); again != base {
		t.Fatal("the same facts give another fingerprint")
	}
	revenue := func(a, b int64) map[string]map[string]int64 {
		return map[string]map[string]int64{"HS1": {"HANDLING": a}, "HS2": {"HANDLING": b}}
	}
	for name, change := range map[string]func(b *CostBasis){
		"basis revision":      func(b *CostBasis) { b.Revision++ },
		"unit type":           func(b *CostBasis) { b.LoadUnits[0].TypeCode = "42G1" },
		"unit TEU":            func(b *CostBasis) { b.LoadUnits[0].TEUTenths = 20 },
		"unit status":         func(b *CostBasis) { b.LoadUnits[0].Status = "IN_USE" },
		"unit stuffing":       func(b *CostBasis) { b.LoadUnits[0].ExecutionID = "X-2" },
		"unit stuffing state": func(b *CostBasis) { b.LoadUnits[0].ExecutionState = "LOADING" },
		"a new unit": func(b *CostBasis) {
			b.LoadUnits = append(b.LoadUnits, LoadUnitFacts{UnitNo: 2, TypeCode: "22G1", TEUTenths: 10, Status: "PLANNED"})
		},
		"packing execution": func(b *CostBasis) { b.PackingLists[0].ExecutionID = "X-2" },
		"packing state":     func(b *CostBasis) { b.PackingLists[0].ExecutionState = "ON_HOLD" },
		"packing digest":    func(b *CostBasis) { b.PackingLists[0].Digest = "other" },
		"a rework": func(b *CostBasis) {
			b.PackingLists[0].Reworks = []ReworkFacts{{No: 1, End: ReworkSealed, EndedAt: hour(6)}}
		},
		"a rework end": func(b *CostBasis) {
			b.PackingLists[0].Reworks = []ReworkFacts{{No: 1, End: ReworkGivenUp, EndedAt: hour(6)}}
		},
		"a rework end time": func(b *CostBasis) {
			b.PackingLists[0].Reworks = []ReworkFacts{{No: 1, End: ReworkSealed, EndedAt: hour(7)}}
		},
		"a house leaving":       func(b *CostBasis) { b.Houses = b.Houses[:1] },
		"a house plan packs":    func(b *CostBasis) { b.Houses[0].Packs++ },
		"a house plan weight":   func(b *CostBasis) { b.Houses[0].GrossWeight = kg(311) },
		"a house plan volume":   func(b *CostBasis) { b.Houses[0].Volume = mtq(3) },
		"a house loaded packs":  func(b *CostBasis) { b.Houses[1].LoadedPacks-- },
		"a house loaded weight": func(b *CostBasis) { b.Houses[1].LoadedGrossWeight = kg(101) },
		"a house loaded volume": func(b *CostBasis) { b.Houses[1].LoadedVolume = cmq(1_200_001) },
		"a house in a unit":     func(b *CostBasis) { b.Houses[0].Units[0].LoadedPacks-- },
	} {
		b := sealedBasis()
		change(&b)
		if got := p.fingerprint(b, jobs(), revenue(10, 20), lines); got == base {
			t.Errorf("%s: the fingerprint did not change", name)
		}
	}
	if p.fingerprint(sealedBasis(), map[string]string{"HS1": "JOB-1", "HS2": "JOB-9"}, revenue(10, 20), lines) == base {
		t.Error("a house's job: the fingerprint did not change")
	}
	if p.fingerprint(sealedBasis(), jobs(), revenue(11, 20), lines) == base || p.fingerprint(sealedBasis(), jobs(), revenue(10, 21), lines) == base {
		t.Error("a house's revenue under a line by REVENUE: the fingerprint did not change")
	}
	// the revenue is read for the lines by REVENUE and the houses that take part only
	if p.fingerprint(sealedBasis(), jobs(), revenue(10, 20), nil) == base {
		t.Error("without a line by REVENUE the revenue is not part of the fingerprint")
	}
	left := []LineApportionment{{LineNo: 2, Method: MethodRevenue, Shares: []Share{{ShipmentID: "HS1", Used: true}, {ShipmentID: "HS2"}}}}
	if p.fingerprint(sealedBasis(), jobs(), revenue(10, 21), left) != p.fingerprint(sealedBasis(), jobs(), revenue(10, 20), left) {
		t.Error("the revenue of a house left out is part of the fingerprint")
	}
	// a rework is read for how it ended and when, not for who asked for it
	a, b := reworked("ops-asker", ReworkSealed), reworked("someone-else", ReworkSealed)
	if p.fingerprint(a, jobs(), nil, nil) != p.fingerprint(b, jobs(), nil, nil) {
		t.Error("the person who asked for a rework is part of the fingerprint")
	}
	for name, change := range map[string]func(b *CostBasis){
		"how a rework ended":     func(b *CostBasis) { b.PackingLists[0].Reworks[0].End = ReworkGivenUp },
		"when a rework ended":    func(b *CostBasis) { b.PackingLists[0].Reworks[0].EndedAt = hour(7) },
		"the number of a rework": func(b *CostBasis) { b.PackingLists[0].Reworks[0].No = 2 },
		"a second rework": func(b *CostBasis) {
			b.PackingLists[0].Reworks = append(b.PackingLists[0].Reworks, ReworkFacts{No: 2, AskedBy: "ops-asker", End: ReworkSealed, EndedAt: hour(8)})
		},
	} {
		changed := reworked("ops-asker", ReworkSealed)
		change(&changed)
		if p.fingerprint(changed, jobs(), nil, nil) == p.fingerprint(a, jobs(), nil, nil) {
			t.Errorf("%s: the fingerprint of a basis with a rework did not change", name)
		}
	}
}

// The cost lines are part of it: a new line, a new version, a lowered cost, what an invoice matched, a release.
func TestTheCostLinesArePartOfTheFingerprint(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable)
	one := []LineApportionment{{LineNo: 1}}
	base := p.fingerprint(sealedBasis(), jobs(), nil, one)
	check := func(name string, change func(p *ConsolCostPool)) {
		t.Helper()
		q := previewed(t, sealedBasis(), MethodChargeable)
		change(q)
		if q.fingerprint(sealedBasis(), jobs(), nil, one) == base {
			t.Errorf("%s: the fingerprint did not change", name)
		}
	}
	check("a new version of the line", func(q *ConsolCostPool) {
		if err := q.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(100_000)}, "re-confirmed", policy(t), "ops-1", hour(4)); err != nil {
			t.Fatal(err)
		}
	})
	check("a lower cost", func(q *ConsolCostPool) {
		if err := q.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(90_000)}, "re-quoted", policy(t), "ops-1", hour(4)); err != nil {
			t.Fatal(err)
		}
	})
	check("what an invoice matched", func(q *ConsolCostPool) {
		q.Matched(map[int]Consumption{1: {Amount: 95_000, Local: 95_000, Final: true}})
	})
	check("another apportionment of the line", func(q *ConsolCostPool) {
		if err := apportion(q, ApportionInput{Jobs: jobs()}, sealedBasis(), "ops-1", hour(4)); err != nil {
			t.Fatal(err)
		}
	})
	// the other lines are not part of it: a new line and a revision of another line leave the fingerprint as it was
	for name, change := range map[string]func(q *ConsolCostPool){
		"a new line": func(q *ConsolCostPool) { recordCost(t, q, "HANDLING", cny(5_000), MethodShipment) },
		"a revision of another line": func(q *ConsolCostPool) {
			recordCost(t, q, "HANDLING", cny(5_000), MethodShipment)
			if err := q.ReviseCost(2, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(6_000)}, "re-quoted", policy(t), "ops-1", hour(4)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		q := previewed(t, sealedBasis(), MethodChargeable)
		change(q)
		if q.fingerprint(sealedBasis(), jobs(), nil, one) != base {
			t.Errorf("%s: the fingerprint of line 1 changed", name)
		}
	}
	check("a release", func(q *ConsolCostPool) {
		if err := q.ReleaseCost(1, 1, "no invoice will come", "ops-1", hour(4)); err != nil {
			t.Fatal(err)
		}
	})
}

// A submission whose basis or pool is not what the preview read is refused as stale, naming what changed and the way
// out, for each kind of change.
func TestASubmissionIsRefusedWhenWhatThePreviewReadHasChanged(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(b *CostBasis, jobs map[string]string)
		part   string
	}{
		"the basis revision": {func(b *CostBasis, _ map[string]string) { b.Revision = 8 }, "basis changed"},
		"a unit's digest":    {func(b *CostBasis, _ map[string]string) { b.PackingLists[0].Digest = "digest-9" }, "packing.1 changed"},
		"a unit's status":    {func(b *CostBasis, _ map[string]string) { b.LoadUnits[0].Status = "IN_USE" }, "unit.1 changed"},
		"a rework that left no trace": {func(b *CostBasis, _ map[string]string) {
			b.PackingLists[0].Reworks = []ReworkFacts{{No: 1, End: ReworkSealed, EndedAt: hour(5)}}
		}, "is new"},
		"a house's loaded figures": {func(b *CostBasis, _ map[string]string) { b.Houses[1].LoadedPacks = 4 }, `house."HS2" changed`},
		"a house gone":             {func(b *CostBasis, _ map[string]string) { b.Houses = b.Houses[:1] }, `house."HS2" [`},
		"a house's job":            {func(_ *CostBasis, j map[string]string) { j["HS2"] = "JOB-7" }, `house."HS2" changed`},
	} {
		p := previewed(t, sealedBasis(), MethodChargeable)
		no := preview(t, p, sealedBasis(), "ops-1", 4)
		b, j := sealedBasis(), jobs()
		tc.change(&b, j)
		err := p.Submit(no, SubmitInput{Basis: b, Jobs: j}, "fin-1", hour(5))
		refused(t, name, err, ErrPreviewStale, tc.part, "preview again")
		if len(p.State().Apportionments) != 0 {
			t.Errorf("%s: a stale preview was submitted", name)
		}
	}
	for name, tc := range map[string]struct {
		change func(p *ConsolCostPool)
		part   string
	}{
		"another apportionment of the line": {func(p *ConsolCostPool) {
			if err := apportion(p, ApportionInput{Jobs: jobs()}, sealedBasis(), "ops-2", hour(4)); err != nil {
				t.Fatal(err)
			}
		}, "line.1 changed"},
		"a revision": {func(p *ConsolCostPool) {
			if err := p.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(90_000)}, "re-quoted", policy(t), "ops-1", hour(4)); err != nil {
				t.Fatal(err)
			}
		}, "line.1 changed"},
		"a match": {func(p *ConsolCostPool) {
			p.Matched(map[int]Consumption{1: {Amount: 95_000, Local: 95_000, Final: true}})
		}, "line.1 changed"},
		"a release": {func(p *ConsolCostPool) {
			if err := p.ReleaseCost(1, 1, "no invoice will come", "ops-1", hour(4)); err != nil {
				t.Fatal(err)
			}
		}, "line.1 changed"},
	} {
		p := previewed(t, sealedBasis(), MethodChargeable)
		no := preview(t, p, sealedBasis(), "ops-1", 4)
		tc.change(p)
		refused(t, name, submit(p, no, sealedBasis(), "fin-1", 5), ErrPreviewStale, tc.part, "preview again")
	}
	// the revenue a line by REVENUE read
	p := previewed(t, sealedBasis(), MethodRevenue)
	rev := map[string]map[string]int64{"HS1": {"FREIGHT": 10}, "HS2": {"FREIGHT": 20}}
	if err := p.Preview(ApportionInput{Jobs: jobs(), Revenue: rev}, sealedBasis(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	rev["HS2"]["FREIGHT"] = 25
	refused(t, "the revenue", p.Submit(1, SubmitInput{Basis: sealedBasis(), Jobs: jobs(), Revenue: rev}, "fin-1", hour(5)), ErrPreviewStale, `revenue.1."HS2" changed from [20] to [25]`)
	rev["HS2"]["FREIGHT"] = 20
	if err := p.Submit(1, SubmitInput{Basis: sealedBasis(), Jobs: jobs(), Revenue: rev}, "fin-1", hour(5)); err != nil {
		t.Fatalf("the revenue as it was: %v", err)
	}
	// a long list of changes is cut
	long := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, long, sealedBasis(), "ops-1", 4)
	b := sealedBasis()
	b.Revision, b.PackingLists[0].Digest, b.LoadUnits[0].Status, b.Houses[0].Packs, b.Houses[1].Packs, b.LoadUnits[0].TEUTenths, b.LoadUnits[0].TypeCode = 9, "x", "x", 99, 99, 99, "x"
	b.LoadUnits = append(b.LoadUnits, LoadUnitFacts{UnitNo: 2, Status: "PLANNED"})
	b.PackingLists = append(b.PackingLists, UnitPackingListFacts{UnitNo: 2, ExecutionID: "X-2", ExecutionState: "OPEN"})
	refused(t, "many changes", long.Submit(no, SubmitInput{Basis: b, Jobs: jobs()}, "fin-1", hour(5)), ErrPreviewStale, "and 2 more")
}

func names(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	return err.Error()
}

// While PHY reads a container in REWORK, a preview, a submission and the close are refused, naming the container and
// the way out; the pool's costs go on.
func TestAPoolIsFrozenWhileAContainerIsReworked(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable)
	old := preview(t, p, sealedBasis(), "ops-1", 4)
	open := inRework(sealedBasis())
	err := p.Preview(ApportionInput{Jobs: jobs()}, open, "ops-1", hour(5))
	refused(t, "a preview", err, ErrPoolInWork, "load unit 1 (stuffing X-1) in rework", "wait for PHY to seal the container again or to give its stuffing up")
	refused(t, "a submission", submit(p, old, open, "fin-1", 5), ErrPoolInWork, "load unit 1 (stuffing X-1)")
	refused(t, "the close", p.Close(open, "fin-1", hour(5)), ErrPoolInWork, "load unit 1 (stuffing X-1)")
	if len(p.State().Previews) != 1 || len(p.State().Apportionments) != 0 || p.State().State != PoolOpen {
		t.Fatalf("a refusal changed the pool: %+v", p.State())
	}
	// the state of the stuffing is read from the load unit or from its packing list
	for name, change := range map[string]func(b *CostBasis){
		"the load unit alone":    func(b *CostBasis) { b.PackingLists[0].ExecutionState = "CLOSED" },
		"the packing list alone": func(b *CostBasis) { b.LoadUnits[0].ExecutionState = "CLOSED" },
	} {
		b := inRework(sealedBasis())
		change(&b)
		if err := p.Preview(ApportionInput{Jobs: jobs()}, b, "ops-1", hour(5)); !errors.Is(err, ErrPoolInWork) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// two containers are both named
	two := inRework(sealedBasis())
	two.LoadUnits = append(two.LoadUnits, LoadUnitFacts{UnitNo: 2, Status: "IN_USE", ExecutionID: "X-2", ExecutionState: ExecutionRework})
	two.PackingLists = append(two.PackingLists, UnitPackingListFacts{UnitNo: 2, ExecutionID: "X-2", ExecutionState: ExecutionRework})
	if err := p.Preview(ApportionInput{Jobs: jobs()}, two, "ops-1", hour(5)); !strings.Contains(names(t, err), "load unit 1 (stuffing X-1), load unit 2 (stuffing X-2)") {
		t.Errorf("two containers: %v", err)
	}
	// costs are recorded, revised and released while the pool is frozen
	recordCost(t, p, "HANDLING", cny(5_000), MethodShipment)
	if err := p.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(90_000)}, "re-quoted", policy(t), "ops-1", hour(5)); err != nil {
		t.Fatalf("a revision while the pool is in work: %v", err)
	}
	if err := p.ReleaseCost(2, 1, "no invoice will come", "ops-1", hour(5)); err != nil {
		t.Fatalf("a release while the pool is in work: %v", err)
	}
	// the freeze lifts with the reseal, and the preview made before it is stale
	sealed := reworked("ops-asker", ReworkSealed)
	refused(t, "the old preview after the reseal", submit(p, old, sealed, "fin-1", 8), ErrPreviewStale, "packing.1 changed")
	fresh := preview(t, p, sealed, "ops-1", 8)
	if err := submit(p, fresh, sealed, "fin-1", 8); err != nil {
		t.Fatalf("after the reseal: %v", err)
	}
}

// The apportionment after a rework is submitted by someone other than the person who asked for it, once: reworks that
// an earlier submission's fingerprint carried as ended are not asked again.
func TestTheConfirmerOfTheApportionmentAfterAReworkIsNotWhoAskedForIt(t *testing.T) {
	sealed := reworked("ops-asker", ReworkSealed)

	// an ended rework not yet carried
	p := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, p, sealed, "ops-1", 8)
	refused(t, "the asker", submit(p, no, sealed, "ops-asker", 8), ErrConfirmer, "ops-asker asked for rework 1 of load unit 1 (stuffing X-1), which ended SEALED",
		"another finance user submits")
	if len(p.State().Apportionments) != 0 {
		t.Fatal("the asker's submission was written")
	}
	if err := submit(p, no, sealed, "fin-2", 8); err != nil {
		t.Fatalf("another finance user: %v", err)
	}

	// carried by an earlier submission: the asker submits the next one
	next := preview(t, p, sealed, "ops-1", 9)
	if err := submit(p, next, sealed, "ops-asker", 9); err != nil {
		t.Fatalf("a rework carried by an earlier submission is asked again: %v", err)
	}

	// a preview that was not submitted carries nothing
	q := previewed(t, sealedBasis(), MethodChargeable)
	first := preview(t, q, sealed, "ops-1", 8)
	if err := submit(q, first, sealed, "fin-2", 8); err != nil {
		t.Fatal(err)
	}
	second := preview(t, q, sealed, "ops-1", 8)
	if err := submit(q, second, sealed, "ops-asker", 8); err != nil {
		t.Fatalf("carried by the first submission: %v", err)
	}
	r := previewed(t, sealedBasis(), MethodChargeable)
	preview(t, r, sealed, "ops-1", 8) // never submitted
	later := preview(t, r, sealed, "ops-1", 8)
	refused(t, "a preview not submitted carries nothing", submit(r, later, sealed, "ops-asker", 8), ErrConfirmer)

	// an open rework is not an ended one: nobody is named for it
	if asked := previewed(t, sealedBasis(), MethodChargeable).reworksAskedBy(inRework(sealedBasis()), "ops-asker", []LineApportionment{{LineNo: 1}}); asked != nil {
		t.Fatalf("an open rework was named: %v", asked)
	}
	// and a fingerprint that had it open does not carry it as ended: a basis that lists a rework open in a container
	// that reads sealed (PHY gives none) is previewed and submitted, and the rework, ended later, is still asked of
	// its asker
	odd := sealedBasis()
	odd.PackingLists[0].Reworks = []ReworkFacts{{No: 1, AskedBy: "ops-asker"}}
	o := previewed(t, sealedBasis(), MethodChargeable)
	if err := submit(o, preview(t, o, odd, "ops-1", 8), odd, "fin-2", 8); err != nil {
		t.Fatalf("a preview with an open rework in a sealed container: %v", err)
	}
	ended := reworked("ops-asker", ReworkSealed)
	refused(t, "a rework that an earlier fingerprint had open", submit(o, preview(t, o, ended, "ops-1", 9), ended, "ops-asker", 9), ErrConfirmer)

	// an apportionment submitted before V2.91 (no preview) carried nothing
	s := previewed(t, sealedBasis(), MethodChargeable)
	if err := apportion(s, ApportionInput{Jobs: jobs()}, sealedBasis(), "fin-2", hour(4)); err != nil {
		t.Fatal(err)
	}
	no = preview(t, s, sealed, "ops-1", 8)
	s.state.Apportionments[0].PreviewNo = 0 // as one written before the previews were
	refused(t, "after an apportionment with no fingerprint", submit(s, no, sealed, "ops-asker", 8), ErrConfirmer)

	// two reworks, the first carried: only the second is named
	two := sealedBasis()
	two.PackingLists[0].Digest = "digest-3"
	two.PackingLists[0].Reworks = []ReworkFacts{{No: 1, AskedBy: "ops-asker", End: ReworkSealed, EndedAt: hour(6)}, {No: 2, AskedBy: "ops-asker", End: ReworkSealed, EndedAt: hour(9)}}
	u := previewed(t, sealedBasis(), MethodChargeable)
	one := preview(t, u, sealed, "ops-1", 8)
	if err := submit(u, one, sealed, "fin-2", 8); err != nil {
		t.Fatal(err)
	}
	both := preview(t, u, two, "ops-1", 10)
	err := submit(u, both, two, "ops-asker", 10)
	refused(t, "the second rework", err, ErrConfirmer, "rework 2 of load unit 1")
	if strings.Contains(err.Error(), "rework 1 of") {
		t.Fatalf("the first rework was carried: %v", err)
	}
	if err = submit(u, both, two, "fin-3", 10); err != nil {
		t.Fatalf("another finance user: %v", err)
	}

	// two reworks of different containers by different people: each asker is refused, the third person accepted
	mixed := sealedBasis()
	mixed.LoadUnits = append(mixed.LoadUnits, LoadUnitFacts{UnitNo: 2, Status: LoadUnitClosed, TEUTenths: 10, ExecutionID: "X-2", ExecutionState: "CLOSED"})
	mixed.PackingLists[0].Reworks = []ReworkFacts{{No: 1, AskedBy: "ops-a", End: ReworkSealed, EndedAt: hour(6)}}
	mixed.PackingLists = append(mixed.PackingLists, UnitPackingListFacts{UnitNo: 2, ExecutionID: "X-2", ExecutionState: "CLOSED", Digest: "d2",
		Reworks: []ReworkFacts{{No: 1, AskedBy: "ops-b", End: ReworkSealed, EndedAt: hour(7)}}})
	w := previewed(t, mixed, MethodChargeable)
	for _, who := range []string{"ops-a", "ops-b"} {
		id := preview(t, w, mixed, "ops-1", 8)
		if err := submit(w, id, mixed, who, 8); !errors.Is(err, ErrConfirmer) {
			t.Errorf("%s asked for a rework: %v", who, err)
		}
	}

	// a rework given up: PHY drops a cancelled stuffing from its basis (checked in the cross-context test), but a basis
	// that lists one is read as any ended rework
	given := reworked("ops-asker", ReworkGivenUp)
	g := previewed(t, sealedBasis(), MethodChargeable)
	id := preview(t, g, given, "ops-1", 8)
	refused(t, "a given up rework", submit(g, id, given, "ops-asker", 8), ErrConfirmer, "which ended GIVEN_UP")
	if err := submit(g, id, given, "fin-2", 8); err != nil {
		t.Fatal(err)
	}
}

// A closed pool takes no preview and no submission; a preview or submission by nobody or dated before the pool's last
// change is refused as any change of the pool.
func TestAPreviewAndASubmissionAreChangesOfTheOpenPool(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, p, sealedBasis(), "ops-1", 4)
	if err := p.Preview(ApportionInput{Jobs: jobs()}, sealedBasis(), "", hour(5)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a preview by nobody: %v", err)
	}
	if err := p.Preview(ApportionInput{Jobs: jobs()}, sealedBasis(), "ops-1", hour(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a preview before the pool's last change: %v", err)
	}
	if err := submit(p, no, sealedBasis(), "", 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a submission by nobody: %v", err)
	}
	p.state.State = PoolClosed
	if err := p.Preview(ApportionInput{Jobs: jobs()}, sealedBasis(), "ops-1", hour(5)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a preview of a closed pool: %v", err)
	}
	if err := submit(p, no, sealedBasis(), "fin-1", 5); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a submission to a closed pool: %v", err)
	}
	other := sealedBasis()
	other.ConsolidationID = "CONSOL-9"
	p.state.State = PoolOpen
	if err := submit(p, no, other, "fin-1", 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another consolidation's basis: %v", err)
	}
}

// A stored pool keeps its previews and the apportionments that name them.
func TestAPoolWithPreviewsIsRestored(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, p, sealedBasis(), "ops-1", 4)
	preview(t, p, sealedBasis(), "ops-1", 4)
	if err := submit(p, no, sealedBasis(), "fin-1", 5); err != nil {
		t.Fatal(err)
	}
	st := p.State()
	back, err := RestorePool(st)
	if err != nil {
		t.Fatal(err)
	}
	got := back.State()
	if len(got.Previews) != 2 || got.Apportionments[0].PreviewNo != 1 || got.Previews[0].Fingerprint != st.Previews[0].Fingerprint || len(back.AddedPreviews()) != 0 {
		t.Fatalf("restored: %+v", got)
	}
	st.Previews[1].No = 5
	if _, err = RestorePool(st); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a gap in the previews: %v", err)
	}
	st = p.State()
	st.Apportionments[0].PreviewNo = 3
	if _, err = RestorePool(st); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an apportionment of a preview the pool lacks: %v", err)
	}
	var stamped ApportionmentPreview
	p.Stamp(time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC))
	if stamped = p.State().Previews[0]; stamped.EnteredAt.IsZero() {
		t.Fatal("a preview is not stamped")
	}
}

// The confirmer is per line: a rework is carried for a line only by an apportionment of that line made from a preview
// that read the rework as ended.
func TestTheConfirmerIsPerLine(t *testing.T) {
	sealed := reworked("ops-asker", ReworkSealed)
	lines := func(p *ConsolCostPool, by string, h int, nos ...int) int {
		t.Helper()
		if err := p.Preview(ApportionInput{LineNos: nos, Jobs: jobs()}, sealed, by, hour(h)); err != nil {
			t.Fatal(err)
		}
		return len(p.State().Previews)
	}
	p := previewed(t, sealedBasis(), MethodChargeable, MethodChargeable)

	// another person apportions line 2 only: it does not confirm line 1 for the asker
	if err := submit(p, lines(p, "ops-1", 8, 2), sealed, "fin-2", 8); err != nil {
		t.Fatal(err)
	}
	err := submit(p, lines(p, "ops-1", 9, 1), sealed, "ops-asker", 9)
	refused(t, "line 1 by the asker", err, ErrConfirmer, "which ended SEALED, for line 1", "another finance user submits")
	if strings.Contains(err.Error(), "line 1, 2") || strings.Contains(err.Error(), "line 2") {
		t.Fatalf("line 2 was carried: %v", err)
	}
	// a preview of both lines: only the line not carried is named
	both := lines(p, "ops-1", 9, 1, 2)
	err = submit(p, both, sealed, "ops-asker", 9)
	refused(t, "both lines by the asker", err, ErrConfirmer, "for line 1")
	if strings.Contains(err.Error(), "line 1, 2") {
		t.Fatalf("line 2 was named although carried: %v", err)
	}
	// line 2 alone is carried: the asker may submit it again
	if err = submit(p, lines(p, "ops-1", 10, 2), sealed, "ops-asker", 10); err != nil {
		t.Fatalf("the asker's second submission of the carried line: %v", err)
	}
	// another person submits line 1; then the asker's next submission of line 1 is accepted
	if err = submit(p, lines(p, "ops-1", 11, 1), sealed, "fin-3", 11); err != nil {
		t.Fatal(err)
	}
	if err = submit(p, lines(p, "ops-1", 12, 1), sealed, "ops-asker", 12); err != nil {
		t.Fatalf("the asker's next submission of line 1: %v", err)
	}
	// a line recorded after the rework ended is carried by nothing: its first apportionment is by someone else
	if err = p.RecordCost("CUSTOMS", "", "", MethodShipment, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(5_000)}, "after the rework", policy(t), "ops-1", hour(12)); err != nil {
		t.Fatal(err)
	}
	err = submit(p, lines(p, "ops-1", 13, 3), sealed, "ops-asker", 13)
	refused(t, "a line recorded after the rework", err, ErrConfirmer, "for line 3")
	if err = submit(p, lines(p, "ops-1", 14, 3), sealed, "fin-2", 14); err != nil {
		t.Fatal(err)
	}

	// an apportionment from before the migration carries no rework, for any line
	o := previewed(t, sealedBasis(), MethodChargeable, MethodChargeable)
	if err = apportion(o, ApportionInput{Jobs: jobs()}, sealedBasis(), "fin-2", hour(4)); err != nil {
		t.Fatal(err)
	}
	refused(t, "after an apportionment with no preview", submit(o, lines(o, "ops-1", 8, 1, 2), sealed, "ops-asker", 8), ErrConfirmer, "for line 1, 2")
}

// A preview that carries the houses left out of the latest apportionment is stale once another apportionment of its
// line was submitted after it; a preview of another line is not.
func TestAPreviewIsStaleOnceAnotherApportionmentOfItsLineWasSubmitted(t *testing.T) {
	p := previewed(t, sealedBasis(), MethodChargeable, MethodChargeable)
	older := preview(t, p, sealedBasis(), "ops-1", 4)
	other := func() int {
		if err := p.Preview(ApportionInput{LineNos: []int{2}, Jobs: jobs()}, sealedBasis(), "ops-1", hour(4)); err != nil {
			t.Fatal(err)
		}
		return len(p.State().Previews)
	}()
	newer := preview(t, p, sealedBasis(), "ops-1", 4)
	if err := submit(p, newer, sealedBasis(), "fin-2", 5); err != nil {
		t.Fatal(err)
	}
	refused(t, "the older preview of the same lines", submit(p, older, sealedBasis(), "fin-2", 6), ErrPreviewStale, "line.1 changed", "apportioned 0] to [", "apportioned 1]")
	// the preview of line 2 was made before an apportionment of both lines: it is stale for line 2 as well
	refused(t, "a preview of line 2", submit(p, other, sealedBasis(), "fin-2", 6), ErrPreviewStale, "line.2 changed")
	// the other way round: an apportionment of line 2 alone leaves a preview of line 1 as it was
	q := previewed(t, sealedBasis(), MethodChargeable, MethodChargeable)
	one := func() int {
		if err := q.Preview(ApportionInput{LineNos: []int{1}, Jobs: jobs()}, sealedBasis(), "ops-1", hour(4)); err != nil {
			t.Fatal(err)
		}
		return len(q.State().Previews)
	}()
	if err := q.Preview(ApportionInput{LineNos: []int{2}, Jobs: jobs()}, sealedBasis(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err := submit(q, 2, sealedBasis(), "fin-2", 5); err != nil {
		t.Fatal(err)
	}
	if err := submit(q, one, sealedBasis(), "fin-2", 6); err != nil {
		t.Fatalf("a preview of line 1 after an apportionment of line 2: %v", err)
	}
}

// When two people each asked for an ended rework that is not carried, neither may submit and a third person is needed:
// the refusal says who may.
func TestWhenTwoPeopleAskedForReworksATheThirdSubmits(t *testing.T) {
	mixed := sealedBasis()
	mixed.LoadUnits = append(mixed.LoadUnits, LoadUnitFacts{UnitNo: 2, Status: LoadUnitClosed, TEUTenths: 10, ExecutionID: "X-2", ExecutionState: "CLOSED"})
	mixed.PackingLists[0].Reworks = []ReworkFacts{{No: 1, AskedBy: "ops-a", End: ReworkSealed, EndedAt: hour(6)}}
	mixed.PackingLists = append(mixed.PackingLists, UnitPackingListFacts{UnitNo: 2, ExecutionID: "X-2", ExecutionState: "CLOSED", Digest: "d2",
		Reworks: []ReworkFacts{{No: 1, AskedBy: "ops-b", End: ReworkSealed, EndedAt: hour(7)}}})
	p := previewed(t, mixed, MethodChargeable)
	no := preview(t, p, mixed, "ops-1", 8)
	for _, who := range []string{"ops-a", "ops-b"} {
		err := submit(p, no, mixed, who, 8)
		refused(t, who, err, ErrConfirmer, "another finance user submits it, one who did not ask for an ended rework of these lines that is not yet carried")
	}
	if err := submit(p, no, mixed, "fin-3", 8); err != nil {
		t.Fatalf("a third person: %v", err)
	}
}
