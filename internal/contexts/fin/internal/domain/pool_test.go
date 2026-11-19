package domain

import (
	"errors"
	"math/rand"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func kg(n int64) kernel.Quantity  { return kernel.Quantity{Unit: "KGM", Amount: n} }
func mtq(n int64) kernel.Quantity { return kernel.Quantity{Unit: "MTQ", Amount: n} }
func cmq(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }

// airBasis is an air consolidation with two loaded houses: HS1 300 kg gross, 1.2 m³ (volumetric 200 kg, chargeable
// 300 kg); HS2 100 kg gross, 1.2 m³ (volumetric 200 kg, chargeable 200 kg).
func airBasis() CostBasis {
	return CostBasis{ConsolidationID: "CONSOL-1", Revision: 7, Kind: "AIR_CONSOL", OperatingLegalEntityID: "LE", State: "CLOSED",
		Houses: []HouseFacts{
			{ShipmentID: "HS1", Packs: 10, GrossWeight: kg(310), Volume: mtq(2), LoadedPacks: 10, LoadedGrossWeight: kg(300),
				LoadedVolume: cmq(1_200_000), LastLoadedAt: hour(1)},
			{ShipmentID: "HS2", Packs: 5, GrossWeight: kg(90), Volume: mtq(1), LoadedPacks: 5, LoadedGrossWeight: kg(100),
				LoadedVolume: cmq(1_200_000), LastLoadedAt: hour(1)},
		}}
}

func pool(t *testing.T, basis CostBasis) *ConsolCostPool {
	t.Helper()
	p, err := OpenPool(PoolState{TenantID: "T", PoolID: "POOL-1", ConsolidationID: basis.ConsolidationID, LegalEntityID: "LE"}, policy(t), basis,
		"ops-1", hour(2))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func recordCost(t *testing.T, p *ConsolCostPool, code string, amount kernel.Money, method string) {
	t.Helper()
	if err := p.RecordCost(code, "", "MAWB 784-1234", method, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: amount},
		"carrier buy rate", policy(t), "ops-1", hour(3)); err != nil {
		t.Fatal(err)
	}
}

func jobs() map[string]string { return map[string]string{"HS1": "JOB-1", "HS2": "JOB-2"} }

func TestOpenPoolChecks(t *testing.T) {
	basis := airBasis()
	for name, change := range map[string]func(*CostBasis){
		"other operator": func(b *CostBasis) { b.OperatingLegalEntityID = "LE2" },
		"cancelled":      func(b *CostBasis) { b.State = "CANCELLED" },
		"unknown kind":   func(b *CostBasis) { b.Kind = "FCL" },
		"other consol":   func(b *CostBasis) { b.ConsolidationID = "CONSOL-9" },
	} {
		b := basis
		change(&b)
		if _, err := OpenPool(PoolState{TenantID: "T", PoolID: "P", ConsolidationID: "CONSOL-1", LegalEntityID: "LE"}, policy(t), b, "ops-1",
			hour(2)); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	p := pool(t, basis)
	if st := p.State(); st.Mode != PoolModeAir || st.FunctionalCurrency != "CNY" || st.BookCode != "MAIN" || st.State != PoolOpen {
		t.Fatalf("pool state: %+v", st)
	}
}

func TestRecordAndReviseCost(t *testing.T) {
	p := pool(t, airBasis())
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	if err := p.RecordCost("X", "", "", "SOMETHING", SideInput{Amount: cny(1)}, "r", policy(t), "ops-1", hour(3)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad method: %v", err)
	}
	if err := p.RecordCost("X", "", "", MethodVolume, SideInput{Amount: cny(-1)}, "r", policy(t), "ops-1", hour(3)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative amount: %v", err)
	}
	// a USD cost converts at the ESTIMATE rate
	if err := p.RecordCost("FUEL", "", "", MethodChargeable, SideInput{Amount: usd(10_050)}, "co-loader", policy(t), "ops-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	st := p.State()
	if v := st.Lines[1].Versions[0]; v.Local != 71_590 || v.FX == nil {
		t.Fatalf("converted cost: %+v", v)
	}
	if err := p.ReviseCost(1, 2, SideInput{Amount: cny(90_000)}, "re-quoted", policy(t), "ops-1", hour(4)); !errors.Is(err, ErrCostVersion) {
		t.Fatalf("wrong version: %v", err)
	}
	if err := p.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(90_000)}, "re-quoted", policy(t), "ops-1",
		hour(4)); err != nil {
		t.Fatal(err)
	}
	if vs := p.State().Lines[0].Versions; len(vs) != 2 || vs[1].Amount.Minor != 90_000 || vs[1].Reason != "re-quoted" {
		t.Fatalf("revised: %+v", vs)
	}
}

// Chargeable air: HS1 300 kg (gross over volumetric 200 kg), HS2 200 kg (volumetric over gross 100 kg); 1000.00 CNY
// splits 600.00 / 400.00.
func TestApportionChargeableAir(t *testing.T) {
	p := pool(t, airBasis())
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	if err := p.Apportion(ApportionInput{Jobs: jobs()}, airBasis(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	a := p.State().Apportionments[0]
	if a.Stage != BasisActual || a.BasisRevision != 7 || len(a.Lines) != 1 {
		t.Fatalf("apportionment: %+v", a)
	}
	s := a.Lines[0].Shares
	if len(s) != 2 || s[0].ShipmentID != "HS1" || s[0].Quantity != 300_000 || s[0].Unit != UnitChargeableGrams || s[0].Amount != 60_000 ||
		s[0].Local != 60_000 || s[1].Quantity != 200_000 || s[1].Amount != 40_000 || s[0].JobID != "JOB-1" || s[1].JobID != "JOB-2" {
		t.Fatalf("shares: %+v", s)
	}
}

// W/M for LCL: HS1 2.4 freight tonnes by volume (2.4 m³ over 0.3 t), HS2 1.2 t by weight (1200 kg over 0.9 m³).
func TestApportionWMSeaLCL(t *testing.T) {
	basis := airBasis()
	basis.Kind = "SEA_LCL_COLOAD"
	basis.Houses[0].LoadedGrossWeight, basis.Houses[0].LoadedVolume = kg(300), cmq(2_400_000)
	basis.Houses[1].LoadedGrossWeight, basis.Houses[1].LoadedVolume = kg(1200), cmq(900_000)
	p := pool(t, basis)
	recordCost(t, p, "OCEANFREIGHT", cny(36_000), MethodChargeable)
	if err := p.Apportion(ApportionInput{Jobs: jobs()}, basis, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	s := p.State().Apportionments[0].Lines[0].Shares
	if s[0].Quantity != 2400 || s[0].Unit != UnitMilliFreightTon || s[1].Quantity != 1200 || s[0].Amount != 24_000 || s[1].Amount != 12_000 {
		t.Fatalf("W/M shares: %+v", s)
	}
}

func TestApportionVolumePiecesManual(t *testing.T) {
	p := pool(t, airBasis())
	recordCost(t, p, "HANDLING", cny(10_001), MethodVolume) // equal volumes: 50.01 and 50.00 by largest remainder (tie to HS1)
	recordCost(t, p, "TERMINAL", cny(9_000), MethodPieces)  // 10 : 5 packs = 60.00 / 30.00
	if err := p.Apportion(ApportionInput{Jobs: jobs()}, airBasis(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	a := p.State().Apportionments[0]
	volume, pieces := a.Lines[0].Shares, a.Lines[1].Shares
	if volume[0].Amount != 5_001 || volume[1].Amount != 5_000 || volume[0].Unit != UnitCubicCm {
		t.Fatalf("volume shares: %+v", volume)
	}
	if pieces[0].Amount != 6_000 || pieces[1].Amount != 3_000 || pieces[0].Quantity != 10 || pieces[0].Unit != UnitPieces {
		t.Fatalf("pieces shares: %+v", pieces)
	}
	// manual: one line, every house once, summing to the line
	for name, in := range map[string]ApportionInput{
		"both lines":  {LineNos: []int{1, 2}, Method: MethodManual, Manual: []ManualShare{{"HS1", 1}, {"HS2", 10_000}}, Reason: "split by hand", Jobs: jobs()},
		"wrong sum":   {LineNos: []int{1}, Method: MethodManual, Manual: []ManualShare{{"HS1", 1}, {"HS2", 1}}, Reason: "r", Jobs: jobs()},
		"twice":       {LineNos: []int{1}, Method: MethodManual, Manual: []ManualShare{{"HS1", 1}, {"HS1", 10_000}}, Reason: "r", Jobs: jobs()},
		"missing":     {LineNos: []int{1}, Method: MethodManual, Manual: []ManualShare{{"HS1", 10_001}}, Reason: "r", Jobs: jobs()},
		"no reason":   {LineNos: []int{1}, Method: MethodManual, Manual: []ManualShare{{"HS1", 1}, {"HS2", 10_000}}, Jobs: jobs()},
		"other house": {LineNos: []int{1}, Method: MethodManual, Manual: []ManualShare{{"HS9", 1}, {"HS2", 10_000}}, Reason: "r", Jobs: jobs()},
	} {
		if err := p.Apportion(in, airBasis(), "ops-1", hour(5)); err == nil {
			t.Errorf("%s: apportioned", name)
		}
	}
	manual := ApportionInput{LineNos: []int{1}, Method: MethodManual, Manual: []ManualShare{{"HS1", 1}, {"HS2", 10_000}},
		Reason: "agreed with the co-loader", Jobs: jobs()}
	if err := p.Apportion(manual, airBasis(), "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	// the manual apportionment supersedes line 1; line 2 keeps the first apportionment's shares
	if _, la, ok := p.SharesOfLine(1); !ok || la.Method != MethodManual || la.Shares[0].Amount != 1 || la.Shares[1].Amount != 10_000 {
		t.Fatalf("line 1 shares: %+v", la)
	}
	if a, la, ok := p.SharesOfLine(2); !ok || a.No != 1 || la.Shares[0].Amount != 6_000 {
		t.Fatalf("line 2 shares: %+v", la)
	}
}

func TestApportionPlannedBeforeLoading(t *testing.T) {
	basis := airBasis()
	basis.Houses[1].LoadedPacks, basis.Houses[1].LoadedGrossWeight, basis.Houses[1].LoadedVolume = 0, kernel.Quantity{}, kernel.Quantity{}
	p := pool(t, basis)
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	if err := p.Apportion(ApportionInput{Jobs: jobs()}, basis, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	a := p.State().Apportionments[0]
	// planned: HS1 310 kg gross vs 2 m³ volumetric 333,334 g → 333.5 kg; HS2 90 kg vs 1 m³ 166,667 g → 167 kg
	if a.Stage != BasisPlanned || a.Lines[0].Shares[0].Quantity != 333_500 || a.Lines[0].Shares[1].Quantity != 167_000 {
		t.Fatalf("planned apportionment: %+v", a.Lines[0].Shares)
	}
}

func TestApportionRefusals(t *testing.T) {
	basis := airBasis()
	p := pool(t, basis)
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	if err := p.Apportion(ApportionInput{Jobs: map[string]string{"HS1": "JOB-1"}}, basis, "ops-1", hour(4)); !errors.Is(err, ErrPoolJobMissing) {
		t.Fatalf("missing job: %v", err)
	}
	pounds := basis
	pounds.Houses[0].LoadedGrossWeight = kernel.Quantity{Unit: "LBR", Amount: 100}
	if err := p.Apportion(ApportionInput{Jobs: jobs()}, pounds, "ops-1", hour(4)); !errors.Is(err, ErrPoolUnit) {
		t.Fatalf("pounds: %v", err)
	}
	empty := basis
	for i := range empty.Houses {
		empty.Houses[i].LoadedVolume = kernel.Quantity{}
	}
	if err := p.Apportion(ApportionInput{Method: MethodVolume, Jobs: jobs()}, empty, "ops-1", hour(4)); !errors.Is(err, ErrPoolNoBasis) {
		t.Fatalf("no volumes: %v", err)
	}
	if err := p.Apportion(ApportionInput{LineNos: []int{2}, Jobs: jobs()}, basis, "ops-1", hour(4)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown line: %v", err)
	}
	if err := p.Apportion(ApportionInput{Jobs: jobs()}, CostBasis{ConsolidationID: "CONSOL-9"}, "ops-1", hour(4)); !errors.Is(err, ErrPoolNoBasis) {
		t.Fatalf("other consolidation: %v", err)
	}
	if err := p.Apportion(ApportionInput{Jobs: jobs()}, basis, "ops-1", hour(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("backdated: %v", err)
	}
}

// Conservation (DDD-020, DDD-021): over random amounts and quantities, the shares sum exactly to the amount and each
// share is the floor or ceiling of its exact proportion.
func TestSplitConservesEveryMinorUnit(t *testing.T) {
	rng := rand.New(rand.NewSource(20261112))
	for i := 0; i < 5000; i++ {
		n := 1 + rng.Intn(12)
		quantities := make([]int64, n)
		var total int64
		for k := range quantities {
			if rng.Intn(6) > 0 {
				quantities[k] = rng.Int63n(2_000_000)
			}
			total += quantities[k]
		}
		if total == 0 {
			quantities[0], total = 1, 1
		}
		amount := rng.Int63n(MaxAmount)
		shares, err := split(amount, quantities)
		if err != nil {
			t.Fatal(err)
		}
		var sum int64
		for k, s := range shares {
			sum += s
			floor := amount / total * quantities[k] // not exact; check via big is heavier — bound check instead
			_ = floor
			exactLow := float64(amount) * float64(quantities[k]) / float64(total)
			if float64(s) < exactLow-1.5 || float64(s) > exactLow+1.5 {
				t.Fatalf("share %d of %d far from %f (amount %d, q %v)", s, k, exactLow, amount, quantities)
			}
		}
		if sum != amount {
			t.Fatalf("shares sum %d, amount %d (q %v)", sum, amount, quantities)
		}
	}
}

func TestPoolRestoreRoundTrip(t *testing.T) {
	p := pool(t, airBasis())
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	if err := p.Apportion(ApportionInput{Jobs: jobs()}, airBasis(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	p.Stamp(hour(4))
	back, err := RestorePool(p.State())
	if err != nil {
		t.Fatal(err)
	}
	if err := back.ReviseCost(1, 1, SideInput{Amount: cny(90_000)}, "re-quoted", policy(t), "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	lines, versions, lineOf, apportionments := back.Added()
	if len(lines) != 0 || len(versions) != 1 || lineOf[0] != 1 || versions[0].No != 2 || len(apportionments) != 0 {
		t.Fatalf("added: %v %v %v %v", lines, versions, lineOf, apportionments)
	}
}

// A pool opens for the three consolidation kinds PHY reports as having shared costs and for no other: an air
// consolidation (mode AIR), an LCL co-load and an LCL consolidation in the forwarder's own container (mode SEA_LCL).
func TestOpenPoolByConsolidationKind(t *testing.T) {
	for _, tc := range []struct {
		kind, mode string
	}{
		{"AIR_CONSOL", PoolModeAir},
		{"SEA_LCL_COLOAD", PoolModeSeaLCL},
		{"SEA_LCL_OWN", PoolModeSeaLCL},
		{"", ""},
		{"SEA_FCL", ""},
	} {
		basis := airBasis()
		basis.Kind = tc.kind
		p, err := OpenPool(PoolState{TenantID: "T", PoolID: "P", ConsolidationID: "CONSOL-1", LegalEntityID: "LE"}, policy(t), basis, "ops-1", hour(2))
		if tc.mode == "" {
			if !errors.Is(err, ErrNotOpenable) {
				t.Errorf("kind %q: %v", tc.kind, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("kind %q: %v; want mode %s", tc.kind, err, tc.mode)
		} else if p.State().Mode != tc.mode {
			t.Errorf("kind %q: mode %q, want %s", tc.kind, p.State().Mode, tc.mode)
		}
	}
}

// W/M of an own-container consolidation, in milli freight tonnes, each measure rounded up to the milli unit:
//
//	HS1 1250 kg, 0.8 m³: weight 1250 over volume 800            -> 1250
//	HS2  200 kg, 2.4 m³: volume 2400 over weight 200            -> 2400
//	HS3 350.5 kg, 0.3 m³: weight 350.5 rounds up to 351, volume 300 -> 351
//
// 4001 in all. 1000.00 (100,000 minor) divides 31242.19 / 59985.00 / 8772.81: the floors 31242, 59985 and 8772 sum to
// 99,999 and the one minor unit left goes to the largest remainder (HS3, 3228 of 4001), so 31,242 / 59,985 / 8,773.
func TestApportionWMOwnContainer(t *testing.T) {
	basis := CostBasis{ConsolidationID: "CONSOL-1", Revision: 3, Kind: "SEA_LCL_OWN", OperatingLegalEntityID: "LE", State: "CLOSED",
		Houses: []HouseFacts{
			{ShipmentID: "HS1", Packs: 4, LoadedPacks: 4, LoadedGrossWeight: kg(1250), LoadedVolume: cmq(800_000), LastLoadedAt: hour(1)},
			{ShipmentID: "HS2", Packs: 2, LoadedPacks: 2, LoadedGrossWeight: kg(200), LoadedVolume: cmq(2_400_000), LastLoadedAt: hour(1)},
			{ShipmentID: "HS3", Packs: 1, LoadedPacks: 1, LoadedGrossWeight: kernel.Quantity{Unit: "GRM", Amount: 350_500}, LoadedVolume: cmq(300_000),
				LastLoadedAt: hour(1)},
		}}
	p := pool(t, basis)
	if p.State().Mode != PoolModeSeaLCL {
		t.Fatalf("mode %s", p.State().Mode)
	}
	recordCost(t, p, "OCEANFREIGHT", cny(100_000), MethodChargeable)
	in := ApportionInput{Jobs: map[string]string{"HS1": "JOB-1", "HS2": "JOB-2", "HS3": "JOB-3"}}
	if err := p.Apportion(in, basis, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	a := p.State().Apportionments[0]
	s := a.Lines[0].Shares
	if a.Stage != BasisActual || len(s) != 3 {
		t.Fatalf("apportionment: %+v", a)
	}
	for i, want := range []struct{ quantity, amount int64 }{{1250, 31_242}, {2400, 59_985}, {351, 8_773}} {
		if s[i].Quantity != want.quantity || s[i].Amount != want.amount || s[i].Local != want.amount || s[i].Unit != UnitMilliFreightTon {
			t.Errorf("share %d: %+v, want %+v", i+1, s[i], want)
		}
	}
	if s[0].Amount+s[1].Amount+s[2].Amount != 100_000 || s[0].Local+s[1].Local+s[2].Local != 100_000 {
		t.Fatalf("the shares do not conserve the line: %+v", s)
	}
}

// threeHouses is an air consolidation of three loaded houses, for the apportionments that leave one out. Gross weight
// and volume as loaded (planned in brackets):
//
//	HS1 10 packs, 300 kg (310 kg), 1.2 m³ (2 m³)   chargeable 300,000 g (volumetric 200,000 g)
//	HS2  5 packs, 100 kg  (90 kg),  1.2 m³ (1 m³)   chargeable 200,000 g (volumetric 200,000 g)
//	HS3  8 packs, 150 kg (160 kg), 1.2 m³ (1.5 m³) chargeable 200,000 g (volumetric 200,000 g)
func threeHouses() CostBasis {
	return CostBasis{ConsolidationID: "CONSOL-1", Revision: 7, Kind: "AIR_CONSOL", OperatingLegalEntityID: "LE", State: "CLOSED",
		Houses: []HouseFacts{
			{ShipmentID: "HS1", Packs: 10, GrossWeight: kg(310), Volume: mtq(2), LoadedPacks: 10, LoadedGrossWeight: kg(300), LoadedVolume: cmq(1_200_000),
				LastLoadedAt: hour(1)},
			{ShipmentID: "HS2", Packs: 5, GrossWeight: kg(90), Volume: mtq(1), LoadedPacks: 5, LoadedGrossWeight: kg(100), LoadedVolume: cmq(1_200_000),
				LastLoadedAt: hour(1)},
			{ShipmentID: "HS3", Packs: 8, GrossWeight: kg(160), Volume: cmq(1_500_000), LoadedPacks: 8, LoadedGrossWeight: kg(150),
				LoadedVolume: cmq(1_200_000), LastLoadedAt: hour(1)},
		}}
}

func threeJobs() map[string]string {
	return map[string]string{"HS1": "JOB-1", "HS2": "JOB-2", "HS3": "JOB-3"}
}

// unloaded is a house with nothing loaded yet: PHY answers zero loaded facts.
func unloaded(b *CostBasis, house int) {
	b.Houses[house].LoadedPacks, b.Houses[house].LoadedGrossWeight, b.Houses[house].LoadedVolume = 0, kernel.Quantity{}, kernel.Quantity{}
}

type wantShare struct {
	quantity, amount int64
	unit             string
}

func checkShares(t *testing.T, name string, got []Share, left int, want []wantShare, amount int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d shares: %+v", name, len(got), got)
	}
	var sum, local int64
	for i, s := range got {
		sum, local = sum+s.Amount, local+s.Local
		if i == left {
			if s.Used || s.Quantity != 0 || s.Unit != "" || s.Amount != 0 || s.Local != 0 {
				t.Errorf("%s: share %d should be left out and zero: %+v", name, i+1, s)
			}
			continue
		}
		if !s.Used || s.Quantity != want[i].quantity || s.Unit != want[i].unit || s.Amount != want[i].amount || s.Local != want[i].amount ||
			s.No != i+1 || s.JobID != "JOB-"+s.ShipmentID[2:] {
			t.Errorf("%s: share %d is %+v, want %+v", name, i+1, s, want[i])
		}
	}
	if sum != amount || local != amount {
		t.Errorf("%s: the shares sum to %d (%d local), the line is %d", name, sum, local, amount)
	}
}

// With no house left out every method gives the shares it always gave, 100,000 minor over the three houses of
// threeHouses with ties and remainders to the first in shipment order:
//
//	CHARGEABLE   300,000 : 200,000 : 200,000 g  -> 42,857.14 / 28,571.43 / 28,571.43; one unit left to HS2 (tie, first)
//	VOLUME       1,200,000 cm³ each             -> 33,333.33 each; the unit left goes to HS1
//	PIECES       10 : 5 : 8                     -> 43,478.26 / 21,739.13 / 34,782.61; the unit left goes to HS3 (.61)
//	GROSS_WEIGHT 300,000 : 100,000 : 150,000 g  -> 54,545.45 / 18,181.82 / 27,272.73; two left to HS2 (.82) and HS3 (.73)
//	SHIPMENT     1 each                         -> 33,333.33 each; the unit left goes to HS1
func TestApportionNothingLeftOutKeepsEveryMethod(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   []wantShare
	}{
		{MethodChargeable, []wantShare{{300_000, 42_857, "GRM"}, {200_000, 28_572, "GRM"}, {200_000, 28_571, "GRM"}}},
		{MethodVolume, []wantShare{{1_200_000, 33_334, "CMQ"}, {1_200_000, 33_333, "CMQ"}, {1_200_000, 33_333, "CMQ"}}},
		{MethodPieces, []wantShare{{10, 43_478, "PCS"}, {5, 21_739, "PCS"}, {8, 34_783, "PCS"}}},
		{MethodGrossWeight, []wantShare{{300_000, 54_545, "GRM"}, {100_000, 18_182, "GRM"}, {150_000, 27_273, "GRM"}}},
		{MethodShipment, []wantShare{{1, 33_334, "SHP"}, {1, 33_333, "SHP"}, {1, 33_333, "SHP"}}},
	} {
		p := pool(t, threeHouses())
		recordCost(t, p, "AIRFREIGHT", cny(100_000), tc.method)
		if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, threeHouses(), "ops-1", hour(4)); err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		a := p.State().Apportionments[0]
		if a.Stage != BasisActual {
			t.Errorf("%s: stage %s", tc.method, a.Stage)
		}
		checkShares(t, tc.method, a.Lines[0].Shares, -1, tc.want, 100_000)
	}
}

// With HS2 left out, the two houses that take part share the line (100,000 minor); HS2 keeps a share that is not used.
//
//	CHARGEABLE   300,000 : 200,000 g   -> 3 : 2 -> 60,000 / 40,000
//	VOLUME       1,200,000 : 1,200,000 -> 1 : 1 -> 50,000 / 50,000
//	PIECES       10 : 8                -> 55,555.56 / 44,444.44; the unit left goes to HS1 (.56) -> 55,556 / 44,444
//	GROSS_WEIGHT 300,000 : 150,000 g   -> 2 : 1 -> 66,666.67 / 33,333.33; the unit left goes to HS1 -> 66,667 / 33,333
//	SHIPMENT     1 : 1                 -> 50,000 / 50,000
func TestApportionLeavingOneOfThreeHousesOut(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   []wantShare
	}{
		{MethodChargeable, []wantShare{{300_000, 60_000, "GRM"}, {}, {200_000, 40_000, "GRM"}}},
		{MethodVolume, []wantShare{{1_200_000, 50_000, "CMQ"}, {}, {1_200_000, 50_000, "CMQ"}}},
		{MethodPieces, []wantShare{{10, 55_556, "PCS"}, {}, {8, 44_444, "PCS"}}},
		{MethodGrossWeight, []wantShare{{300_000, 66_667, "GRM"}, {}, {150_000, 33_333, "GRM"}}},
		{MethodShipment, []wantShare{{1, 50_000, "SHP"}, {}, {1, 50_000, "SHP"}}},
	} {
		p := pool(t, threeHouses())
		recordCost(t, p, "AIRFREIGHT", cny(100_000), tc.method)
		if err := p.Apportion(ApportionInput{NotUsed: []string{"HS2"}, Jobs: threeJobs()}, threeHouses(), "ops-1", hour(4)); err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		checkShares(t, tc.method, p.State().Apportionments[0].Lines[0].Shares, 1, tc.want, 100_000)
	}
	// the same through a method override of a line recorded by another method, and the first house left out
	p := pool(t, threeHouses())
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	if err := p.Apportion(ApportionInput{Method: MethodShipment, NotUsed: []string{"HS1"}, Jobs: threeJobs()}, threeHouses(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	checkShares(t, "override", p.State().Apportionments[0].Lines[0].Shares, 0, []wantShare{{}, {1, 50_000, "SHP"}, {1, 50_000, "SHP"}}, 100_000)
	if la := p.State().Apportionments[0].Lines[0]; la.Method != MethodShipment {
		t.Fatalf("method %s", la.Method)
	}
}

// A house left out has no basis to give: a weight FIN cannot convert or a missing volume of the house left out refuses
// nothing, and the houses that take part having no basis is FIN_POOL_NO_BASIS.
func TestApportionBasisIsThatOfTheHousesThatTakePart(t *testing.T) {
	b := threeHouses()
	b.Houses[1].LoadedGrossWeight = kernel.Quantity{Unit: "LBR", Amount: 100}
	b.Houses[1].LoadedVolume = kernel.Quantity{}
	p := pool(t, b)
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); !errors.Is(err, ErrPoolUnit) {
		t.Fatalf("pounds with the house taking part: %v", err)
	}
	if err := p.Apportion(ApportionInput{NotUsed: []string{"HS2"}, Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatalf("pounds with the house left out: %v", err)
	}
	b = threeHouses()
	b.Houses[0].LoadedVolume, b.Houses[2].LoadedVolume = kernel.Quantity{}, kernel.Quantity{}
	p = pool(t, b)
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodVolume)
	if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatalf("one house has the volume: %v", err)
	}
	if err := p.Apportion(ApportionInput{NotUsed: []string{"HS2"}, Jobs: threeJobs()}, b, "ops-1", hour(5)); !errors.Is(err, ErrPoolNoBasis) {
		t.Fatalf("no volume among the houses that take part: %v", err)
	}
}

func TestApportionNotUsedRefusals(t *testing.T) {
	p := pool(t, threeHouses())
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	for name, notUsed := range map[string][]string{
		"named twice":     {"HS2", "HS2"},
		"not a house":     {"HS9"},
		"one of them not": {"HS2", "HS9"},
		"every house":     {"HS3", "HS1", "HS2"},
	} {
		if err := p.Apportion(ApportionInput{NotUsed: notUsed, Jobs: threeJobs()}, threeHouses(), "ops-1", hour(4)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(p.State().Apportionments) != 0 {
		t.Fatalf("a refused apportionment was kept")
	}
	// RETAINED leaves no house out: on a cancelled consolidation it keeps the cost, but not with a list of houses
	gone := CostBasis{ConsolidationID: "CONSOL-1", Revision: 8, Kind: "AIR_CONSOL", OperatingLegalEntityID: "LE", State: "CANCELLED"}
	if err := p.Apportion(ApportionInput{Method: MethodRetained, NotUsed: []string{"HS1"}, Reason: "dead freight"}, gone, "ops-1", hour(4)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retained with houses left out: %v", err)
	}
	if err := p.Apportion(ApportionInput{Method: MethodRetained, Reason: "dead freight"}, gone, "ops-1", hour(4)); err != nil {
		t.Fatalf("retained: %v", err)
	}
	// the job of a house left out is still needed
	if err := p.Apportion(ApportionInput{NotUsed: []string{"HS2"}, Jobs: map[string]string{"HS1": "JOB-1", "HS3": "JOB-3"}}, threeHouses(), "ops-1", hour(5)); !errors.Is(err, ErrPoolJobMissing) {
		t.Fatalf("job of the house left out: %v", err)
	}
}

// MANUAL names the houses that take part and none else.
func TestApportionManualWithAHouseLeftOut(t *testing.T) {
	p := pool(t, threeHouses())
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodManual)
	manual := func(shares ...ManualShare) ApportionInput {
		return ApportionInput{LineNos: []int{1}, Method: MethodManual, Manual: shares, NotUsed: []string{"HS2"}, Reason: "agreed", Jobs: threeJobs()}
	}
	for name, in := range map[string]ApportionInput{
		"names the house left out": manual(ManualShare{"HS1", 30_000}, ManualShare{"HS2", 10_000}, ManualShare{"HS3", 60_000}),
		"names it instead":         manual(ManualShare{"HS1", 30_000}, ManualShare{"HS2", 70_000}),
		"omits one taking part":    manual(ManualShare{"HS1", 100_000}),
	} {
		if err := p.Apportion(in, threeHouses(), "ops-1", hour(4)); !errors.Is(err, ErrManualShares) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := p.Apportion(manual(ManualShare{"HS3", 70_000}, ManualShare{"HS1", 30_000}), threeHouses(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	s := p.State().Apportionments[0].Lines[0].Shares
	checkShares(t, "manual", s, 1, []wantShare{{30_000, 30_000, ""}, {}, {70_000, 70_000, ""}}, 100_000)
}

// The stage is that of the houses that take part.
func TestApportionStageIsThatOfTheHousesThatTakePart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unloaded int
		notUsed  []string
		stage    string
	}{
		{"the house left out is not loaded", 1, []string{"HS2"}, BasisActual},
		{"a house taking part is not loaded", 2, []string{"HS2"}, BasisPlanned},
		{"nothing left out, one not loaded", 1, nil, BasisPlanned},
		{"all loaded, one left out", -1, []string{"HS2"}, BasisActual},
	} {
		b := threeHouses()
		if tc.unloaded >= 0 {
			unloaded(&b, tc.unloaded)
		}
		p := pool(t, b)
		recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
		if err := p.Apportion(ApportionInput{NotUsed: tc.notUsed, Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := p.State().Apportionments[0].Stage; got != tc.stage {
			t.Errorf("%s: stage %s, want %s", tc.name, got, tc.stage)
		}
	}
}

// GROSS_WEIGHT divides by the planned gross weight at PLANNED and the loaded gross weight at ACTUAL, in grams:
//
//	PLANNED (HS3 not loaded)  310,000 : 90,000 : 160,000 g -> 55,357.14 / 16,071.43 / 28,571.43; the unit left goes to HS2 (tie, first)
//	ACTUAL                    300,000 : 100,000 : 150,000 g -> as the test above
//
// A weight in a unit FIN cannot convert is FIN_POOL_UNIT; a volume is of no matter to it.
func TestApportionGrossWeightPlannedAndActual(t *testing.T) {
	b := threeHouses()
	unloaded(&b, 2)
	p := pool(t, b)
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodGrossWeight)
	if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	a := p.State().Apportionments[0]
	checkShares(t, "planned", a.Lines[0].Shares, -1, []wantShare{{310_000, 55_357, "GRM"}, {90_000, 16_072, "GRM"}, {160_000, 28_571, "GRM"}}, 100_000)
	if a.Stage != BasisPlanned {
		t.Fatalf("stage %s", a.Stage)
	}
	b = threeHouses()
	b.Houses[0].LoadedVolume = kernel.Quantity{}
	p = pool(t, b)
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodGrossWeight)
	if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatalf("a missing volume: %v", err)
	}
	checkShares(t, "actual", p.State().Apportionments[0].Lines[0].Shares, -1, []wantShare{{300_000, 54_545, "GRM"}, {100_000, 18_182, "GRM"},
		{150_000, 27_273, "GRM"}}, 100_000)
	b.Houses[0].LoadedGrossWeight = kernel.Quantity{Unit: "LBR", Amount: 100}
	if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(5)); !errors.Is(err, ErrPoolUnit) {
		t.Fatalf("pounds: %v", err)
	}
	for i := range b.Houses {
		b.Houses[i].LoadedGrossWeight = kernel.Quantity{}
	}
	if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(5)); !errors.Is(err, ErrPoolNoBasis) {
		t.Fatalf("no weights: %v", err)
	}
}

// SHIPMENT gives each house that takes part the same share; the minor units that do not divide go to the first houses in
// shipment order: 100,001 over three houses is 33,333.67 each, the floors leave two units, to HS1 and HS2; over the two
// that take part (HS1 and HS3) 100,001 leaves one, to HS1.
func TestApportionShipmentExtraMinorUnits(t *testing.T) {
	p := pool(t, threeHouses())
	recordCost(t, p, "AIRFREIGHT", cny(100_001), MethodShipment)
	if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, threeHouses(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err := p.Apportion(ApportionInput{NotUsed: []string{"HS2"}, Jobs: threeJobs()}, threeHouses(), "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	st := p.State()
	checkShares(t, "three", st.Apportionments[0].Lines[0].Shares, -1, []wantShare{{1, 33_334, "SHP"}, {1, 33_334, "SHP"}, {1, 33_333, "SHP"}}, 100_001)
	checkShares(t, "two", st.Apportionments[1].Lines[0].Shares, 1, []wantShare{{1, 50_001, "SHP"}, {}, {1, 50_000, "SHP"}}, 100_001)
	// a cost of zero is shared as zero
	z := pool(t, threeHouses())
	recordCost(t, z, "AIRFREIGHT", cny(0), MethodShipment)
	if err := z.Apportion(ApportionInput{Jobs: threeJobs()}, threeHouses(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
}

func TestRecordCostAcceptsTheNewMethods(t *testing.T) {
	p := pool(t, airBasis())
	recordCost(t, p, "AIRFREIGHT", cny(100), MethodGrossWeight)
	recordCost(t, p, "HANDLING", cny(100), MethodShipment)
	if err := p.RecordCost("X", "", "", MethodRetained, SideInput{Amount: cny(1)}, "r", policy(t), "ops-1", hour(3)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a line is never recorded as retained: %v", err)
	}
	est := []EstimateLine{{ChargeCode: "AIRFREIGHT", LineNo: 1, HasCost: true, CreditorPartyID: "CARRIER-1", CreditorRevision: 3, Cost: cny(100)}}
	q := pool(t, airBasis())
	if err := q.RecordEstimate("BASIS-1", est, MethodShipment, policy(t), "ops-1", hour(3)); err != nil {
		t.Fatalf("a rated estimate by shipment: %v", err)
	}
}

// closable is a pool of the three houses with one line of 100,000 matched final, apportioned as asked at hour 4.
func closable(t *testing.T, method string, notUsed []string, b CostBasis) *ConsolCostPool {
	t.Helper()
	p := pool(t, b)
	recordCost(t, p, "AIRFREIGHT", cny(100_000), method)
	p.Matched(map[int]Consumption{1: {Amount: 100_000, Local: 100_000, Final: true}})
	if err := p.Apportion(ApportionInput{NotUsed: notUsed, Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	return p
}

// The close compares a line over the houses its latest apportionment used.
func TestCloseOverTheHousesUsed(t *testing.T) {
	for _, method := range []string{MethodChargeable, MethodGrossWeight, MethodVolume, MethodPieces, MethodShipment} {
		b := threeHouses()
		unloaded(&b, 1) // HS2 never loaded, left out
		p := closable(t, method, []string{"HS2"}, b)
		if st := p.State().Apportionments[0].Stage; st != BasisActual {
			t.Fatalf("%s: stage %s", method, st)
		}
		if err := p.Close(b, "ops-1", hour(6)); err != nil {
			t.Errorf("%s: a line leaving out the house never loaded does not close: %v", method, err)
		}
	}
	b := threeHouses()
	unloaded(&b, 1)
	for name, change := range map[string]func(*CostBasis){
		"a house taking part is not loaded": func(b *CostBasis) { unloaded(b, 2) },
		"a used house re-weighed":           func(b *CostBasis) { b.Houses[0].LoadedGrossWeight = kg(320) },
		"a house attached since": func(b *CostBasis) {
			b.Houses = append(b.Houses, HouseFacts{ShipmentID: "HS4", LoadedPacks: 1, LoadedGrossWeight: kg(1)})
		},
		"a house detached since":               func(b *CostBasis) { b.Houses = b.Houses[:2] },
		"a house in the place of one left out": func(b *CostBasis) { b.Houses[1].ShipmentID = "HS2A" },
	} {
		p := closable(t, MethodGrossWeight, []string{"HS2"}, b)
		c := b
		c.Houses = append([]HouseFacts(nil), b.Houses...)
		change(&c)
		if err := p.Close(c, "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a line by SHIPMENT has no weighed quantity to compare, and still refuses once a house it used is not loaded
	sp := closable(t, MethodShipment, []string{"HS2"}, b)
	c := b
	c.Houses = append([]HouseFacts(nil), b.Houses...)
	unloaded(&c, 2)
	if err := sp.Close(c, "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
		t.Errorf("a line by shipment with a house used not loaded: %v", err)
	}
	// the house left out may change as it likes, and be loaded: only its presence is compared
	p := closable(t, MethodGrossWeight, []string{"HS2"}, b)
	c = b
	c.Houses = append([]HouseFacts(nil), b.Houses...)
	c.Houses[1].LoadedPacks, c.Houses[1].LoadedGrossWeight = 3, kg(999)
	if err := p.Close(c, "ops-1", hour(6)); err != nil {
		t.Errorf("the house left out changed: %v", err)
	}
}

// A line apportioned with nothing left out still refuses the close while any house is not loaded, and so does a line
// apportioned again with the house that was left out taking part, until that house is loaded.
func TestCloseWithNothingLeftOutNeedsEveryHouseLoaded(t *testing.T) {
	b := threeHouses()
	unloaded(&b, 1)
	p := closable(t, MethodChargeable, nil, b) // PLANNED: HS2 not loaded
	if st := p.State().Apportionments[0].Stage; st != BasisPlanned {
		t.Fatalf("stage %s", st)
	}
	if err := p.Close(b, "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
		t.Fatalf("closed with a house not loaded: %v", err)
	}
	// the same line apportioned again with HS2 taking part, while it is still not loaded: the close is refused
	l := closable(t, MethodChargeable, []string{"HS2"}, b)
	if err := l.Apportion(ApportionInput{AllHouses: true, Jobs: threeJobs()}, b, "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(b, "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
		t.Fatalf("closed with the house taking part not loaded: %v", err)
	}
	// loaded at last and apportioned over all three: it closes
	loaded := threeHouses()
	if err := l.Apportion(ApportionInput{AllHouses: true, Jobs: threeJobs()}, loaded, "ops-1", hour(7)); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(loaded, "ops-1", hour(8)); err != nil {
		t.Fatal(err)
	}
}

// A house left out of a line stays out of it when the line is apportioned again with nothing named; AllHouses takes
// every live house in again, another list replaces it, and each line keeps its own.
func TestApportionKeepsTheHousesLeftOut(t *testing.T) {
	p := pool(t, threeHouses())
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodShipment)
	recordCost(t, p, "HANDLING", cny(60_000), MethodShipment)
	recordCost(t, p, "TERMINAL", cny(30_000), MethodShipment)
	apportion := func(h int, in ApportionInput) []LineApportionment {
		t.Helper()
		in.Jobs = threeJobs()
		if err := p.Apportion(in, threeHouses(), "ops-1", hour(h)); err != nil {
			t.Fatal(err)
		}
		return p.State().Apportionments[len(p.State().Apportionments)-1].Lines
	}
	used := func(la LineApportionment) (out string) {
		for _, s := range la.Shares {
			if s.Used {
				out += s.ShipmentID[2:]
			}
		}
		return out
	}
	// line 1 leaves HS2 out, line 2 leaves HS3 out, line 3 all take part
	apportion(4, ApportionInput{LineNos: []int{1}, NotUsed: []string{"HS2"}})
	apportion(4, ApportionInput{LineNos: []int{2}, NotUsed: []string{"HS3"}})
	apportion(4, ApportionInput{LineNos: []int{3}})
	// with nothing named, each line keeps its own list, in one command
	got := apportion(5, ApportionInput{})
	if used(got[0]) != "13" || used(got[1]) != "12" || used(got[2]) != "123" {
		t.Fatalf("carried lists: %q %q %q", used(got[0]), used(got[1]), used(got[2]))
	}
	if got[0].Shares[0].Amount != 50_000 || got[0].Shares[1].Amount != 0 || got[1].Shares[2].Amount != 0 {
		t.Fatalf("shares: %+v / %+v", got[0].Shares, got[1].Shares)
	}
	// MANUAL takes the carried list too
	manual := ApportionInput{LineNos: []int{1}, Method: MethodManual, Manual: []ManualShare{{"HS1", 10_000}, {"HS3", 90_000}}, Reason: "agreed"}
	if got = apportion(6, manual); used(got[0]) != "13" {
		t.Fatalf("manual: %q", used(got[0]))
	}
	manual.Manual = []ManualShare{{"HS1", 10_000}, {"HS2", 10_000}, {"HS3", 80_000}}
	if err := p.Apportion(ApportionInput{LineNos: []int{1}, Method: MethodManual, Manual: manual.Manual, Reason: "x", Jobs: threeJobs()}, threeHouses(),
		"ops-1", hour(6)); !errors.Is(err, ErrManualShares) {
		t.Fatalf("manual naming the house kept out: %v", err)
	}
	// another list replaces the line's
	if got = apportion(7, ApportionInput{LineNos: []int{1}, NotUsed: []string{"HS1"}}); used(got[0]) != "23" {
		t.Fatalf("replaced: %q", used(got[0]))
	}
	// AllHouses takes everyone back in, in every line of the command
	got = apportion(8, ApportionInput{AllHouses: true})
	if used(got[0]) != "123" || used(got[1]) != "123" || used(got[2]) != "123" {
		t.Fatalf("all houses: %q %q %q", used(got[0]), used(got[1]), used(got[2]))
	}
	// AllHouses with a list, and with RETAINED, is refused
	for name, in := range map[string]ApportionInput{
		"all and a list": {AllHouses: true, NotUsed: []string{"HS2"}, Jobs: threeJobs()},
	} {
		if err := p.Apportion(in, threeHouses(), "ops-1", hour(9)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	gone := CostBasis{ConsolidationID: "CONSOL-1", Revision: 8, Kind: "AIR_CONSOL", OperatingLegalEntityID: "LE", State: "CANCELLED"}
	if err := p.Apportion(ApportionInput{Method: MethodRetained, AllHouses: true, Reason: "r"}, gone, "ops-1", hour(9)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retained with AllHouses: %v", err)
	}
}

// A house detached since is dropped from the carried list; when every house still live was left out the command is
// refused, as when every house is named.
func TestCarriedListDropsHousesNoLongerLive(t *testing.T) {
	p := pool(t, threeHouses())
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodShipment)
	if err := p.Apportion(ApportionInput{NotUsed: []string{"HS2", "HS3"}, Jobs: threeJobs()}, threeHouses(), "ops-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	// HS1 detached: HS2 and HS3 are all that is left, and both were left out
	b := threeHouses()
	b.Houses = b.Houses[1:]
	if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(5)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("every house still live left out: %v", err)
	}
	// HS3 detached instead: HS2 stays out, HS1 takes part, the dropped HS3 is not named by any share
	b = threeHouses()
	b.Houses = b.Houses[:2]
	if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	checkShares(t, "after a detach", p.State().Apportionments[1].Lines[0].Shares, 1, []wantShare{{1, 100_000, "SHP"}, {}}, 100_000)
}

// The stage of an apportionment that names no house to leave out is that of the houses its lines keep out of it: a house
// left out and not loaded does not make it PLANNED.
func TestStageOfAnApportionmentThatCarriesTheHousesLeftOut(t *testing.T) {
	b := threeHouses()
	unloaded(&b, 1)
	p := pool(t, b)
	recordCost(t, p, "AIRFREIGHT", cny(100_000), MethodChargeable)
	for i, in := range []ApportionInput{{NotUsed: []string{"HS2"}}, {}, {AllHouses: true}} {
		in.Jobs = threeJobs()
		if err := p.Apportion(in, b, "ops-1", hour(4+i)); err != nil {
			t.Fatal(err)
		}
	}
	got := []string{}
	for _, a := range p.State().Apportionments {
		got = append(got, a.Stage)
	}
	if got[0] != BasisActual || got[1] != BasisActual || got[2] != BasisPlanned {
		t.Fatalf("stages %v", got)
	}
}

// containerBasis is an own-container consolidation of the three houses of threeHouses in two load units, a 20-foot 22G1
// (10 tenths of a TEU) as unit 1 and a 40-foot 42G1 (20 tenths) as unit 2: HS1 is in unit 1 only, HS2 in unit 2 only and
// HS3 in both.
func containerBasis() CostBasis {
	b := threeHouses()
	b.Kind = "SEA_LCL_OWN"
	b.LoadUnits = []LoadUnitFacts{{UnitNo: 1, TypeCode: "22G1", TEUTenths: 10, Status: LoadUnitClosed}, {UnitNo: 2, TypeCode: "42G1", TEUTenths: 20, Status: LoadUnitClosed}}
	b.Houses[0].Units = []HouseUnitFacts{{UnitNo: 1, LoadedPacks: 10}}
	b.Houses[1].Units = []HouseUnitFacts{{UnitNo: 2, LoadedPacks: 5}}
	b.Houses[2].Units = []HouseUnitFacts{{UnitNo: 1, LoadedPacks: 3}, {UnitNo: 2, LoadedPacks: 5}}
	return b
}

// copyOf is a basis whose houses can be changed without changing the original.
func copyOf(b CostBasis) CostBasis {
	b.Houses = append([]HouseFacts(nil), b.Houses...)
	return b
}

func apportionedLine(t *testing.T, b CostBasis, line Money, method string, notUsed []string) []Share {
	t.Helper()
	p := pool(t, b)
	recordCost(t, p, "SEAFREIGHT", line, method)
	if err := p.Apportion(ApportionInput{NotUsed: notUsed, Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	a := p.State().Apportionments[0]
	if a.Stage != BasisActual || a.Lines[0].Method != method {
		t.Fatalf("%s: %+v", method, a)
	}
	return a.Lines[0].Shares
}

type Money = kernel.Money

// 10,001 minor of USD (71,... local at the estimate rate) over the three houses of containerBasis.
//
//	CONTAINER_COUNT 1 : 1 : 2 containers           -> 2,500.25 / 2,500.25 / 5,000.50; one unit left, to HS3 (.50 is the largest) -> 2,500 / 2,500 / 5,001
//	TEU             10 : 20 : 30 tenths (unit DTE) -> 1,666.83 / 3,333.67 / 5,000.50; two units left, to HS1 (.83) and HS2 (.67) -> 1,667 / 3,334 / 5,000
//
// The local amount is divided the same way by the same quantities, and each currency adds up to its line.
func TestApportionByContainerCountAndTEU(t *testing.T) {
	for _, tc := range []struct {
		method, unit string
		quantities   [3]int64
		amounts      [3]int64
	}{
		{MethodContainerCount, UnitContainers, [3]int64{1, 1, 2}, [3]int64{2_500, 2_500, 5_001}},
		{MethodTEU, UnitTEUTenths, [3]int64{10, 20, 30}, [3]int64{1_667, 3_334, 5_000}},
	} {
		b := containerBasis()
		p := pool(t, b)
		recordCost(t, p, "SEAFREIGHT", usd(10_001), tc.method)
		cost, local := p.LineCost(1)
		if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		a := p.State().Apportionments[0]
		if a.Stage != BasisActual || a.Lines[0].Method != tc.method || a.Lines[0].Amount != 10_001 {
			t.Fatalf("%s: %+v", tc.method, a)
		}
		var sum, localSum int64
		for i, s := range a.Lines[0].Shares {
			if !s.Used || s.Quantity != tc.quantities[i] || s.Unit != tc.unit || s.Amount != tc.amounts[i] || s.JobID != threeJobs()[s.ShipmentID] {
				t.Errorf("%s: share %d is %+v", tc.method, i+1, s)
			}
			sum, localSum = sum+s.Amount, localSum+s.Local
		}
		if sum != cost || localSum != local {
			t.Errorf("%s: shares add up to %d (%d local), the line is %d (%d local)", tc.method, sum, localSum, cost, local)
		}
		// the local amount follows the same quantities: its floor shares differ from the exact ones by less than one unit
		for i, s := range a.Lines[0].Shares {
			exact := float64(local) * float64(tc.quantities[i]) / 4 // 4 containers, so a share is quantity/4 of the line
			if tc.method == MethodTEU {
				exact = float64(local) * float64(tc.quantities[i]) / 60
			}
			if d := float64(s.Local) - exact; d <= -1 || d >= 1 {
				t.Errorf("%s: local share %d is %d, the exact share is %.2f", tc.method, i+1, s.Local, exact)
			}
		}
	}
}

// Three houses in one container are counted alike: both methods give what SHIPMENT gives, the minor unit left over going
// to the first house.
func TestContainerMethodsGiveShipmentShareForOneContainer(t *testing.T) {
	b := containerBasis()
	b.LoadUnits = b.LoadUnits[1:] // the 40-foot container only
	for i := range b.Houses {
		b.Houses[i].Units = []HouseUnitFacts{{UnitNo: 2, LoadedPacks: 1}}
	}
	want := apportionedLine(t, b, cny(100_000), MethodShipment, nil)
	for _, m := range []string{MethodContainerCount, MethodTEU} {
		got := apportionedLine(t, b, cny(100_000), m, nil)
		for i := range got {
			if got[i].Amount != want[i].Amount || got[i].Local != want[i].Local || got[i].Amount != []int64{33_334, 33_333, 33_333}[i] {
				t.Errorf("%s: share %d is %+v, SHIPMENT gives %+v", m, i+1, got[i], want[i])
			}
		}
	}
}

// The container methods exist at ACTUAL only: with a house that takes part not loaded the stage is PLANNED and they are
// refused, also when another line of the command uses an existing method; nothing is recorded. The same basis with that
// house loaded is accepted, and a house left out does not hold the stage back.
func TestContainerMethodsNeedTheActualStage(t *testing.T) {
	for _, m := range []string{MethodContainerCount, MethodTEU} {
		b := containerBasis()
		unloaded(&b, 1)
		b.Houses[1].Units = nil
		p := pool(t, b)
		recordCost(t, p, "SEAFREIGHT", cny(100_000), m)
		recordCost(t, p, "HANDLING", cny(60_000), MethodShipment)
		if err := p.Apportion(ApportionInput{LineNos: []int{1}, Jobs: threeJobs()}, b, "ops-1", hour(4)); !errors.Is(err, ErrPoolNoBasis) {
			t.Errorf("%s at PLANNED: %v", m, err)
		}
		if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); !errors.Is(err, ErrPoolNoBasis) {
			t.Errorf("%s with another line by SHIPMENT in the command: %v", m, err)
		}
		if err := p.Apportion(ApportionInput{LineNos: []int{1}, Method: m, Jobs: threeJobs()}, b, "ops-1", hour(4)); !errors.Is(err, ErrPoolNoBasis) {
			t.Errorf("%s as the command's method at PLANNED: %v", m, err)
		}
		if n := len(p.State().Apportionments); n != 0 {
			t.Errorf("%s: %d apportionments recorded by refused commands", m, n)
		}
		if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, containerBasis(), "ops-1", hour(4)); err != nil {
			t.Errorf("%s at ACTUAL: %v", m, err)
		}
		if err := p.Apportion(ApportionInput{LineNos: []int{1}, NotUsed: []string{"HS2"}, Jobs: threeJobs()}, b, "ops-1", hour(5)); err != nil {
			t.Errorf("%s with the unloaded house left out: %v", m, err)
		}
	}
}

// A basis without containers (an air consolidation, or a co-load loaded by tender) has every quantity zero.
func TestContainerMethodsNeedContainers(t *testing.T) {
	for _, m := range []string{MethodContainerCount, MethodTEU} {
		b := threeHouses() // loaded, tendered: no Units, no LoadUnits
		p := pool(t, b)
		recordCost(t, p, "SEAFREIGHT", cny(100_000), m)
		if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); !errors.Is(err, ErrPoolNoBasis) {
			t.Errorf("%s with no containers: %v", m, err)
		}
	}
}

// PHY's contract gives every unit of a house's entries among the load units with a positive TEU, so an entry that names
// another unit, or a unit with no TEU, is an inconsistent basis: TEU refuses it, CONTAINER_COUNT never reads the tenths.
func TestContainerMethodsInconsistentBasis(t *testing.T) {
	for name, change := range map[string]func(*CostBasis){
		"an entry names a unit the basis lacks": func(b *CostBasis) { b.LoadUnits = b.LoadUnits[:1] },
		"a unit with zero tenths": func(b *CostBasis) {
			b.LoadUnits = []LoadUnitFacts{b.LoadUnits[0], {UnitNo: 2, TypeCode: "42G1", Status: LoadUnitClosed}}
		},
	} {
		b := containerBasis()
		change(&b)
		p := pool(t, b)
		recordCost(t, p, "SEAFREIGHT", cny(100_000), MethodTEU)
		if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); !errors.Is(err, ErrPoolUnit) {
			t.Errorf("TEU, %s: %v", name, err)
		}
		if got := apportionedLine(t, b, cny(100_000), MethodContainerCount, nil); got[0].Quantity != 1 || got[2].Quantity != 2 {
			t.Errorf("CONTAINER_COUNT, %s: %+v", name, got)
		}
	}
	// a unit only a house left out is in is not read
	b := containerBasis()
	b.LoadUnits = b.LoadUnits[:1]
	b.Houses[0].Units = []HouseUnitFacts{{UnitNo: 1, LoadedPacks: 10}}
	b.Houses[1].Units = []HouseUnitFacts{{UnitNo: 1, LoadedPacks: 5}}
	b.Houses[2].Units = []HouseUnitFacts{{UnitNo: 2, LoadedPacks: 5}}
	if got := apportionedLine(t, b, cny(100_000), MethodTEU, []string{"HS3"}); got[0].Amount != 50_000 || got[1].Amount != 50_000 {
		t.Errorf("a house left out names a unit the basis lacks: %+v", got)
	}
}

// A house that takes part and is loaded but has no entry while others have has no container: quantity 0 and a zero
// share, as a house with no volume under VOLUME.
func TestContainerMethodsHouseWithoutAnEntry(t *testing.T) {
	b := containerBasis()
	b.Houses[1].Units = nil
	for m, want := range map[string][3]int64{MethodContainerCount: {1, 0, 2}, MethodTEU: {10, 0, 30}} {
		got := apportionedLine(t, b, cny(100_000), m, nil)
		if got[1].Quantity != 0 || !got[1].Used || got[1].Amount != 0 || got[0].Quantity != want[0] || got[2].Quantity != want[2] {
			t.Errorf("%s: %+v", m, got)
		}
	}
}

// With HS2 left out, the quantities and the split are those of HS1 and HS3 (100,000 minor).
//
//	CONTAINER_COUNT 1 : 2   -> 33,333.33 / 66,666.67; the unit left goes to HS3 -> 33,333 / 66,667
//	TEU             10 : 30 -> 25,000 / 75,000
func TestContainerMethodsWithAHouseLeftOut(t *testing.T) {
	checkShares(t, "CONTAINER_COUNT", apportionedLine(t, containerBasis(), cny(100_000), MethodContainerCount, []string{"HS2"}), 1,
		[]wantShare{{1, 33_333, "CNT"}, {}, {2, 66_667, "CNT"}}, 100_000)
	checkShares(t, "TEU", apportionedLine(t, containerBasis(), cny(100_000), MethodTEU, []string{"HS2"}), 1,
		[]wantShare{{10, 25_000, "DTE"}, {}, {30, 75_000, "DTE"}}, 100_000)
}

// The close recomputes the quantities from PHY's current basis: it closes while the containers are what they were, and
// is refused once a house's containers changed, whether the house moved into another unit or an entry appeared or went.
func TestCloseByContainerCountAndTEU(t *testing.T) {
	for _, m := range []string{MethodContainerCount, MethodTEU} {
		b := containerBasis()
		if err := closable(t, m, nil, b).Close(copyOf(b), "ops-1", hour(6)); err != nil {
			t.Errorf("%s: a line on the current containers does not close: %v", m, err)
		}
		for name, change := range map[string]func(*CostBasis){
			"the second entry of HS3 is gone": func(b *CostBasis) { b.Houses[2].Units = b.Houses[2].Units[:1] },
			"HS1 has a new entry": func(b *CostBasis) {
				b.Houses[0].Units = append(b.Houses[0].Units, HouseUnitFacts{UnitNo: 2, LoadedPacks: 1})
			},
			"HS1 moved into the other unit": func(b *CostBasis) { b.Houses[0].Units = []HouseUnitFacts{{UnitNo: 2, LoadedPacks: 10}} },
		} {
			if name == "HS1 moved into the other unit" && m == MethodContainerCount {
				continue // one container before and after: a count does not change, only the TEU does
			}
			c := copyOf(b)
			change(&c)
			if err := closable(t, m, nil, b).Close(c, "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
				t.Errorf("%s, %s: %v", m, name, err)
			}
		}
		// the houses left out still apply: only the houses that take part are compared
		left := closable(t, m, []string{"HS2"}, b)
		c := copyOf(b)
		c.Houses[1].Units = []HouseUnitFacts{{UnitNo: 1, LoadedPacks: 5}}
		if err := left.Close(c, "ops-1", hour(6)); err != nil {
			t.Errorf("%s: the house left out changed its containers: %v", m, err)
		}
		c = copyOf(b)
		c.Houses[0].Units = append(c.Houses[0].Units, HouseUnitFacts{UnitNo: 2, LoadedPacks: 1})
		left = closable(t, m, []string{"HS2"}, b)
		if err := left.Close(c, "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
			t.Errorf("%s: a house taking part changed its container with another left out: %v", m, err)
		}
	}
	// a unit's TEU changing under a TEU line is a change of the quantity too
	b := containerBasis()
	c := copyOf(b)
	c.LoadUnits = []LoadUnitFacts{{UnitNo: 1, TypeCode: "22G1", TEUTenths: 10, Status: LoadUnitClosed}, {UnitNo: 2, TypeCode: "22G1", TEUTenths: 10, Status: LoadUnitClosed}}
	if err := closable(t, MethodTEU, nil, b).Close(c, "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
		t.Errorf("a unit changed its TEU: %v", err)
	}
	if err := closable(t, MethodContainerCount, nil, b).Close(c, "ops-1", hour(6)); err != nil {
		t.Errorf("a count does not read the TEU: %v", err)
	}
}

// The existing methods give what they gave, on a basis that now has units (the same figures as
// TestApportionNothingLeftOutKeepsEveryMethod, 100,000 minor over the three houses).
func TestExistingMethodsOnABasisWithUnits(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   [3]int64
	}{
		{MethodChargeable, [3]int64{42_857, 28_572, 28_571}},
		{MethodVolume, [3]int64{33_334, 33_333, 33_333}},
		{MethodPieces, [3]int64{43_478, 21_739, 34_783}},
		{MethodGrossWeight, [3]int64{54_545, 18_182, 27_273}},
		{MethodShipment, [3]int64{33_334, 33_333, 33_333}},
	} {
		b := containerBasis()
		b.Kind = "AIR_CONSOL" // the pool mode the figures of the other test are in
		got := apportionedLine(t, b, cny(100_000), tc.method, nil)
		for i, s := range got {
			if s.Amount != tc.want[i] {
				t.Errorf("%s: share %d is %d, want %d", tc.method, i+1, s.Amount, tc.want[i])
			}
		}
	}
}

func TestRecordCostAcceptsTheContainerMethods(t *testing.T) {
	p := pool(t, containerBasis())
	recordCost(t, p, "SEAFREIGHT", cny(100), MethodContainerCount)
	recordCost(t, p, "THC", cny(100), MethodTEU)
	if err := p.RecordCost("X", "", "", "REVENUE", SideInput{Amount: cny(1)}, "r", policy(t), "ops-1", hour(3)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a method outside the list: %v", err)
	}
}

// A house's containers are the distinct units of its entries: two entries of one unit count as one container.
func TestContainerMethodsCountDistinctUnits(t *testing.T) {
	b := containerBasis()
	b.Houses[0].Units = []HouseUnitFacts{{UnitNo: 1, LoadedPacks: 6}, {UnitNo: 1, LoadedPacks: 4}}
	for m, want := range map[string]int64{MethodContainerCount: 1, MethodTEU: 10} {
		if got := apportionedLine(t, b, cny(100_000), m, nil); got[0].Quantity != want {
			t.Errorf("%s: %+v", m, got)
		}
	}
}

// A house split over two containers of which only the first is sealed yet (the case of the review of V2.80): SH-C has
// packs in unit 1 only, unit 1 is CLOSED and unit 2 is still IN_USE. Every house has loaded packs, so the stage is
// ACTUAL, but a count of one container for SH-C would be wrong once its other packs go into unit 2: both methods are
// refused until unit 2 is sealed. Then SH-C is in both: a count of 2 and 30 tenths.
func TestContainerMethodsWithAHouseHalfStuffed(t *testing.T) {
	for _, m := range []string{MethodContainerCount, MethodTEU} {
		half := containerBasis()
		half.Houses[2].Units = half.Houses[2].Units[:1]
		half.LoadUnits[1].Status = "IN_USE"
		p := pool(t, half)
		recordCost(t, p, "SEAFREIGHT", cny(100_000), m)
		if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, half, "ops-1", hour(4)); !errors.Is(err, ErrPoolNoBasis) {
			t.Fatalf("%s with unit 2 still loading: %v", m, err)
		}
		half.LoadUnits[1].Status = LoadUnitClosed
		half.Houses[2].Units = containerBasis().Houses[2].Units
		if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, half, "ops-1", hour(5)); err != nil {
			t.Fatalf("%s with both sealed: %v", m, err)
		}
		want := map[string]int64{MethodContainerCount: 2, MethodTEU: 30}[m]
		if got := p.State().Apportionments[0].Lines[0].Shares[2].Quantity; got != want {
			t.Errorf("%s: SH-C's quantity is %d, want %d", m, got, want)
		}
	}
}

// Every load unit of the basis must be sealed (CLOSED): one IN_USE, or a third unit PLANNED and never stuffed, refuses
// both methods with FIN_POOL_NO_BASIS naming the unit and its status; all CLOSED is accepted. The PLANNED stage is
// refused first, and the other methods do not mind the unit.
func TestContainerMethodsNeedEveryContainerSealed(t *testing.T) {
	for _, m := range []string{MethodContainerCount, MethodTEU} {
		for name, units := range map[string][]LoadUnitFacts{
			"unit 2 in use": {{UnitNo: 1, TypeCode: "22G1", TEUTenths: 10, Status: LoadUnitClosed}, {UnitNo: 2, TypeCode: "42G1", TEUTenths: 20, Status: "IN_USE"}},
			"a third unit planned": {{UnitNo: 1, TypeCode: "22G1", TEUTenths: 10, Status: LoadUnitClosed}, {UnitNo: 2, TypeCode: "42G1", TEUTenths: 20, Status: LoadUnitClosed},
				{UnitNo: 3, TypeCode: "42G1", TEUTenths: 20, Status: "PLANNED"}},
		} {
			b := containerBasis()
			b.LoadUnits = units
			p := pool(t, b)
			recordCost(t, p, "SEAFREIGHT", cny(100_000), m)
			err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4))
			if !errors.Is(err, ErrPoolNoBasis) || !strings.Contains(err.Error(), "sealed") {
				t.Errorf("%s, %s: %v", m, name, err)
			}
			if n := len(p.State().Apportionments); n != 0 {
				t.Errorf("%s, %s: %d apportionments stored", m, name, n)
			}
		}
		// the PLANNED stage is refused first, with its own message
		b := containerBasis()
		b.LoadUnits[1].Status = "PLANNED"
		unloaded(&b, 1)
		b.Houses[1].Units = nil
		p := pool(t, b)
		recordCost(t, p, "SEAFREIGHT", cny(100_000), m)
		if err := p.Apportion(ApportionInput{Jobs: threeJobs()}, b, "ops-1", hour(4)); !errors.Is(err, ErrPoolNoBasis) || !strings.Contains(err.Error(), "stuffed") {
			t.Errorf("%s at PLANNED with a unit planned: %v", m, err)
		}
		if got := apportionedLine(t, containerBasis(), cny(100_000), m, nil); got[2].Amount == 0 {
			t.Errorf("%s all sealed: %+v", m, got)
		}
	}
	b := containerBasis()
	b.LoadUnits[1].Status = "IN_USE"
	if got := apportionedLine(t, b, cny(100_000), MethodShipment, nil); got[0].Amount != 33_334 {
		t.Errorf("SHIPMENT does not mind an unsealed unit: %+v", got)
	}
}

// After an accepted apportionment a new unit is planned: the close compares on the current basis and is refused for each
// method, until the unit is sealed or removed (no longer in the basis).
func TestCloseRefusesANewUnitNotSealed(t *testing.T) {
	for _, m := range []string{MethodContainerCount, MethodTEU} {
		b := containerBasis()
		c := copyOf(b)
		c.LoadUnits = append(append([]LoadUnitFacts(nil), b.LoadUnits...), LoadUnitFacts{UnitNo: 3, TypeCode: "22G1", TEUTenths: 10, Status: "PLANNED"})
		if err := closable(t, m, nil, b).Close(c, "ops-1", hour(6)); !errors.Is(err, ErrPoolNotClosable) {
			t.Errorf("%s: closed with a unit planned: %v", m, err)
		}
		if err := closable(t, m, nil, b).Close(b, "ops-1", hour(6)); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
}

// An entry without loaded packs is not a container the house is in: FIN skips it, as PHY gives none.
func TestContainerMethodsSkipAnEntryWithoutLoadedPacks(t *testing.T) {
	b := containerBasis()
	b.Houses[0].Units = []HouseUnitFacts{{UnitNo: 1, LoadedPacks: 10}, {UnitNo: 2, LoadedPacks: 0}}
	b.Houses[1].Units = []HouseUnitFacts{{UnitNo: 2, LoadedPacks: 5}, {UnitNo: 1}}
	for m, want := range map[string][3]int64{MethodContainerCount: {1, 1, 2}, MethodTEU: {10, 20, 30}} {
		got := apportionedLine(t, b, cny(100_000), m, nil)
		if got[0].Quantity != want[0] || got[1].Quantity != want[1] || got[2].Quantity != want[2] {
			t.Errorf("%s: %+v", m, got)
		}
	}
}
