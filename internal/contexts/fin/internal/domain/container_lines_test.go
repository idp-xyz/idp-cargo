package domain

import (
	"errors"
	"testing"
)

// A line of one container (V2.104) over containerBasis: unit 1 holds HS1 (10 packs) and HS3 (3 packs), unit 2 holds HS2
// (5 packs) and HS3 (5 packs).

func recordUnitCost(t *testing.T, p *ConsolCostPool, unitNo int, amount Money, method string) {
	t.Helper()
	if err := p.RecordCostInUnit("SEAFREIGHT", "", "MBL 1", method, unitNo, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: amount},
		"carrier buy rate", policy(t), "ops-1", hour(3)); err != nil {
		t.Fatal(err)
	}
}

// sharesOf is a line's shares in the latest apportionment, by shipment.
func sharesOf(t *testing.T, p *ConsolCostPool, lineNo int) map[string]Share {
	t.Helper()
	_, la, ok := p.SharesOfLine(lineNo)
	if !ok {
		t.Fatalf("line %d is not apportioned", lineNo)
	}
	out := map[string]Share{}
	for _, s := range la.Shares {
		out[s.ShipmentID] = s
	}
	return out
}

func inUnit(s Share) string {
	switch {
	case s.InUnit == nil:
		return "nil"
	case *s.InUnit:
		return "in"
	}
	return "out"
}

func TestALineOfOneContainerIsDividedOverTheHousesLoadedInItByWhatEachHasInIt(t *testing.T) {
	b := containerBasis()
	p := pool(t, b)
	recordUnitCost(t, p, 1, usd(13_000), MethodPieces) // HS1 10 packs, HS3 3 packs in unit 1
	recordUnitCost(t, p, 2, usd(10_000), MethodPieces) // HS2 5, HS3 5 in unit 2
	recordCost(t, p, "DOCFEE", usd(300), MethodShipment)
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		line    int
		amounts map[string]int64
		quant   map[string]int64
		in      map[string]string
	}{
		{1, map[string]int64{"HS1": 10_000, "HS2": 0, "HS3": 3_000}, map[string]int64{"HS1": 10, "HS2": 0, "HS3": 3}, map[string]string{"HS1": "in", "HS2": "out", "HS3": "in"}},
		{2, map[string]int64{"HS1": 0, "HS2": 5_000, "HS3": 5_000}, map[string]int64{"HS1": 0, "HS2": 5, "HS3": 5}, map[string]string{"HS1": "out", "HS2": "in", "HS3": "in"}},
		{3, map[string]int64{"HS1": 100, "HS2": 100, "HS3": 100}, map[string]int64{"HS1": 1, "HS2": 1, "HS3": 1}, map[string]string{"HS1": "nil", "HS2": "nil", "HS3": "nil"}},
	} {
		for ship, s := range sharesOf(t, p, tc.line) {
			if s.Amount != tc.amounts[ship] || s.Quantity != tc.quant[ship] || inUnit(s) != tc.in[ship] || s.Used != (tc.amounts[ship] > 0) {
				t.Errorf("line %d, %s: %+v (in the container: %s)", tc.line, ship, s, inUnit(s))
			}
		}
	}
	if got := p.State().Lines[0].LoadUnitNo(); got != 1 {
		t.Errorf("line 1 belongs to %d", got)
	}
}

func TestALineOfOneContainerWaitsForItToBeSealedAndIsRefusedWhatItCannotBe(t *testing.T) {
	open := containerBasis()
	open.LoadUnits[0].Status = "IN_USE"
	p := pool(t, open)
	recordUnitCost(t, p, 1, usd(1_000), MethodPieces)
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, open, "ops-1", hour(4)); !errors.Is(err, ErrPoolNoBasis) {
		t.Fatalf("container not sealed: %v", err)
	}
	missing := containerBasis()
	missing.LoadUnits = missing.LoadUnits[1:]
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, missing, "ops-1", hour(4)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no container 1: %v", err)
	}
	coload := containerBasis()
	coload.Kind = "SEA_LCL_COLOAD"
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, coload, "ops-1", hour(4)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a co-load has no container of its own: %v", err)
	}
	for _, m := range []string{MethodContainerCount, MethodTEU} {
		if err := p.RecordCostInUnit("SEAFREIGHT", "", "", m, 1, SideInput{Amount: usd(1)}, "r", policy(t), "ops-1", hour(3)); !errors.Is(err, ErrInvalid) {
			t.Errorf("record by %s: %v", m, err)
		}
		if err := apportion(p, ApportionInput{Method: m, Jobs: threeJobs()}, containerBasis(), "ops-1", hour(4)); !errors.Is(err, ErrInvalid) {
			t.Errorf("apportion by %s: %v", m, err)
		}
	}
	for _, n := range []int{-1, MaxLoadUnitNo + 1} {
		if err := p.RecordCostInUnit("SEAFREIGHT", "", "", MethodPieces, n, SideInput{Amount: usd(1)}, "r", policy(t), "ops-1", hour(3)); !errors.Is(err, ErrInvalid) {
			t.Errorf("container %d: %v", n, err)
		}
	}
	air := pool(t, airBasis())
	if err := air.RecordCostInUnit("AIRFREIGHT", "", "", MethodChargeable, 1, SideInput{Amount: usd(1)}, "r", policy(t), "ops-1", hour(3)); !errors.Is(err, ErrInvalid) {
		t.Errorf("air: %v", err)
	}
}

func TestFinanceLeavesOutOnlyHousesOfTheContainer(t *testing.T) {
	b := containerBasis()
	p := pool(t, b)
	recordUnitCost(t, p, 1, usd(13_000), MethodPieces)
	// HS2 is not in unit 1: naming it changes nothing on that line; HS3 is, and is left out
	if err := apportion(p, ApportionInput{NotUsed: []string{"HS2", "HS3"}, Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	s := sharesOf(t, p, 1)
	if s["HS1"].Amount != 13_000 || s["HS3"].Used || inUnit(s["HS3"]) != "in" || inUnit(s["HS2"]) != "out" {
		t.Fatalf("left out: %+v", s)
	}
	// with none named, the house left out stays out; AllHouses takes it in again
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if s = sharesOf(t, p, 1); s["HS3"].Used {
		t.Fatalf("carried: %+v", s)
	}
	if err := apportion(p, ApportionInput{AllHouses: true, Jobs: threeJobs()}, b, "ops-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	if s = sharesOf(t, p, 1); s["HS1"].Amount != 10_000 || s["HS3"].Amount != 3_000 {
		t.Fatalf("all: %+v", s)
	}
	// every house of the container left out is refused
	if err := apportion(p, ApportionInput{NotUsed: []string{"HS1", "HS3"}, Jobs: threeJobs()}, b, "ops-1", hour(7)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("all of unit 1 left out: %v", err)
	}
}

func TestALineMovedToAnotherContainerIsARevisionAndIsDividedThere(t *testing.T) {
	b := containerBasis()
	p := pool(t, b)
	recordUnitCost(t, p, 1, usd(13_000), MethodPieces)
	if err := p.ReviseCostInUnit(1, 1, 2, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: usd(10_000)}, "it is container 2's freight", policy(t),
		"ops-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	line := p.State().Lines[0]
	if line.LoadUnitNo() != 2 || len(line.Versions) != 2 || line.Versions[0].LoadUnitNo != 1 {
		t.Fatalf("versions %+v", line.Versions)
	}
	// a revision of the amount alone keeps the container
	if err := p.ReviseCost(1, 2, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: usd(10_000)}, "same", policy(t), "ops-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	if p.State().Lines[0].LoadUnitNo() != 2 {
		t.Fatal("the revision moved the line")
	}
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if s := sharesOf(t, p, 1); s["HS1"].Used || s["HS2"].Amount != 5_000 || s["HS3"].Amount != 5_000 {
		t.Fatalf("shares %+v", s)
	}
	// back to the whole consolidation
	if err := p.ReviseCostInUnit(1, 3, 0, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: usd(10_000)}, "the whole consolidation's", policy(t),
		"ops-1", hour(5)); err != nil || p.State().Lines[0].LoadUnitNo() != 0 {
		t.Fatalf("to the whole consolidation: %v", err)
	}
}

// movedBasis is containerBasis after HS3 left unit 1: all its 8 packs are in unit 2.
func movedBasis() CostBasis {
	b := copyOf(containerBasis())
	b.Revision++
	b.Houses[2].Units = []HouseUnitFacts{{UnitNo: 2, LoadedPacks: 8}}
	return b
}

func TestAHouseThatChangedContainerFollowsItsCargoByDefault(t *testing.T) {
	b := containerBasis()
	p := pool(t, b)
	recordUnitCost(t, p, 1, usd(13_000), MethodPieces)
	recordUnitCost(t, p, 2, usd(13_000), MethodPieces)
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	after := movedBasis()
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, after, "ops-1", hour(5)); err != nil {
		t.Fatalf("no attribution needed: %v", err)
	}
	one, two := sharesOf(t, p, 1), sharesOf(t, p, 2)
	// unit 1: HS1 bears the whole line; HS3 was in it and is not now, which the shares record
	if one["HS1"].Amount != 13_000 || one["HS3"].Used || inUnit(one["HS3"]) != "out" {
		t.Fatalf("unit 1: %+v", one)
	}
	// unit 2: HS2 5 packs, HS3 8
	if two["HS2"].Amount != 5_000 || two["HS3"].Amount != 8_000 || inUnit(two["HS3"]) != "in" {
		t.Fatalf("unit 2: %+v", two)
	}
	if _, la, _ := p.SharesOfLine(1); len(la.Attributions) != 0 {
		t.Fatalf("attributions %+v", la.Attributions)
	}
}

func TestFinanceMayAttributeTheShareOfAHouseThatChangedContainer(t *testing.T) {
	b := containerBasis()
	p := pool(t, b)
	recordUnitCost(t, p, 1, usd(13_000), MethodPieces)
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	after := movedBasis()
	// HS1 stayed in the container: it cannot be attributed
	in := ApportionInput{Jobs: threeJobs(), Attributions: []AttributionInput{{LineNo: 1, ShipmentID: "HS1", Loss: 100, Voucher: "V-1"}}}
	if err := p.Preview(in, after, "fin-1", hour(5)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a house still in the container: %v", err)
	}
	// HS3 bears its former share of 3,000 as its own loss: HS1 bears the rest
	in.Attributions = []AttributionInput{{LineNo: 1, ShipmentID: "HS3", Loss: 3_000, Voucher: "V-1", Reason: "the shipper asked for the change"}}
	if err := apportion(p, in, after, "fin-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	_, la, _ := p.SharesOfLine(1)
	s := sharesOf(t, p, 1)
	if len(la.Attributions) != 1 || la.Attributions[0].ShipmentID != "HS3" || la.Attributions[0].Loss != 3_000 || la.Attributions[0].JobID != "JOB-3" ||
		s["HS1"].Amount != 10_000 || s["HS3"].Used {
		t.Fatalf("attributed: %+v %+v", la.Attributions, s)
	}
	// stated nowhere again, it is carried while HS3 is out of the container
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, after, "fin-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	if _, la, _ = p.SharesOfLine(1); len(la.Attributions) != 1 || !la.Attributions[0].Carried || la.Attributions[0].Loss != 3_000 {
		t.Fatalf("carried: %+v", la.Attributions)
	}
	// back in the container, the attribution is not carried and HS3 takes its share again
	if err := apportion(p, ApportionInput{Jobs: threeJobs()}, b, "fin-1", hour(7)); err != nil {
		t.Fatal(err)
	}
	if _, la, _ = p.SharesOfLine(1); len(la.Attributions) != 0 || sharesOf(t, p, 1)["HS3"].Amount != 3_000 {
		t.Fatalf("back: %+v %+v", la.Attributions, sharesOf(t, p, 1))
	}
}
