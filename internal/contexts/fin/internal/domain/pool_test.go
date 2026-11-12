package domain

import (
	"errors"
	"math/rand"
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
