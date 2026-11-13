package domain

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var warehouse = Facility{Location: loc("CNPVG"), Code: "WH-1"}
var terminal = Facility{Location: loc("CNPVG"), Code: "PVG-T1"}

func ev(ref string, at time.Time) Evidence {
	return Evidence{Source: "WMS", Reference: ref, OccurredAt: at}
}

func cartons(n int64, c Condition, at time.Time) ReceiptLine {
	return ReceiptLine{Packs: n, PackageType: "CT", Condition: c, Evidence: ev(fmt.Sprintf("SCAN-%d-%s", n, at.Format("150405")), at)}
}

func scale(weight int64, at time.Time) Measurement {
	return Measurement{GrossWeight: kg(weight), Method: MethodScale, Evidence: ev("SCALE-"+at.Format("150405"), at)}
}

// received returns an AIR lot of SH-1 with 6 cartons received, weighed at 300 kg and completed.
func received(t *testing.T) *CargoLot {
	t.Helper()
	l, err := ReceiveCargo("T", "L-1", warehouse, intent("SH-1", 1), cartons(6, ConditionGood, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(l.Measure(scale(300, t0.Add(2*time.Hour)), "ops", t0.Add(2*time.Hour)),
		l.Complete(intent("SH-1", 1), "ops", t0.Add(3*time.Hour))); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestCargoLotReceipt(t *testing.T) {
	at := t0.Add(time.Hour)
	draft := intent("SH-1", 1)
	draft.Status = IntentDraft
	if _, err := ReceiveCargo("T", "L-1", warehouse, draft, cartons(6, ConditionGood, at), "ops", at); !errors.Is(err, ErrIntentNotPlanned) {
		t.Fatalf("a draft shipment is not received: %v", err)
	}
	if _, err := ReceiveCargo("T", "L-1", Facility{Location: loc("CNPVG"), Code: "wh 1"}, intent("SH-1", 1), cartons(6, ConditionGood, at), "ops", at); !errors.Is(err, ErrInvalidLot) {
		t.Fatalf("a facility code is capitals, digits and . / -: %v", err)
	}
	if _, err := ReceiveCargo("T", "L-1", warehouse, intent("SH-1", 1), cartons(6, ConditionGood, at.Add(time.Hour)), "ops", at); !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("evidence does not occur after the business time: %v", err)
	}
	l, err := ReceiveCargo("T", "L-1", warehouse, intent("SH-1", 1), cartons(4, ConditionGood, at), "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Receive(cartons(3, ConditionDamaged, at.Add(time.Minute)), "ops", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = l.Complete(intent("SH-1", 1), "ops", at.Add(2*time.Minute)); !errors.Is(err, ErrNoMeasurement) {
		t.Fatalf("receipt completes once measured: %v", err)
	}
	if err = l.Measure(Measurement{GrossWeight: kernel.Quantity{Unit: "LBR", Amount: 700}, Method: MethodScale, Evidence: ev("S", at)}, "ops", at.Add(2*time.Minute)); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("weight in the plan's unit: %v", err)
	}
	if err = l.Screen(Screening{Outcome: ScreeningPassed, Screener: "PVG security", Method: "XRAY", Reference: "SC-1", Evidence: ev("SC-1", at)}, "ops", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	m := scale(340, at.Add(3*time.Minute))
	m.Volume = kernel.Quantity{Unit: "MTQ", Amount: 2}
	m.Groups = []PackageGroup{{Count: 7, PackageType: "CT", LengthMM: 600, WidthMM: 400, HeightMM: 400}}
	if err = l.Measure(m, "ops", at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = l.Complete(intent("SH-1", 1), "ops", at.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	kinds := map[DiscrepancyKind]Discrepancy{}
	for _, d := range l.State().Discrepancies {
		kinds[d.Kind] = d
	}
	if len(kinds) != 3 || kinds[DiscrepancyOver].Observed != 7 || kinds[DiscrepancyDamaged].Observed != 3 || kinds[DiscrepancyWeightVariance].Observed != 340 {
		t.Fatalf("7 of 6 packs, 3 damaged, 340 of 300 kg: %+v", kinds)
	}
	if err = l.Receive(cartons(1, ConditionGood, at.Add(5*time.Minute)), "ops", at.Add(5*time.Minute)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("no receipt after completion: %v", err)
	}
	if err = l.Resolve(1, ResolvedByReplan, "", "", intent("SH-1", 1), "ops", at.Add(5*time.Minute)); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("FUL_REPLANNED needs FUL to plan the 7 packs received: %v", err)
	}
	replanned := intent("SH-1", 2)
	replanned.Scope.Packs = 7
	if err = errors.Join(l.Resolve(1, ResolvedByReplan, "", "", replanned, "ops", at.Add(5*time.Minute)),
		l.Resolve(2, ResolvedByClaim, "CLM-9", "", LoadIntent{}, "ops", at.Add(5*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(7, 7, replanned, "ops", at.Add(6*time.Minute)); !errors.Is(err, ErrOpenDiscrepancy) {
		t.Fatalf("the weight variance is open: %v", err)
	}
	if err = l.Resolve(3, ResolvedByAccepted, "", "", LoadIntent{}, "ops", at.Add(6*time.Minute)); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("ACCEPTED needs a reason: %v", err)
	}
	if err = l.Resolve(3, ResolvedByAccepted, "", "customer confirmed 340 kg", LoadIntent{}, "ops", at.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(5, 5, replanned, "ops", at.Add(7*time.Minute)); !errors.Is(err, ErrUnaccounted) {
		t.Fatalf("2 packs are neither loaded nor re-planned: %v", err)
	}
	cancelled := replanned
	cancelled.Status = IntentCancelled
	if err = l.Close(0, 0, cancelled, "ops", at.Add(7*time.Minute)); err != nil {
		t.Fatalf("a cancelled shipment's lot closes with nothing loaded: %v", err)
	}
	restored, err := RestoreCargoLot(l.State())
	if err != nil || restored.Received() != 7 || restored.Screened() != ScreeningPassed || restored.OpenDiscrepancies() != 0 {
		t.Fatalf("restore: %v", err)
	}
	if err = restored.Measure(scale(300, at.Add(8*time.Minute)), "ops", at.Add(8*time.Minute)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a closed lot does not change: %v", err)
	}
	sea, _ := ReceiveCargo("T", "L-2", Facility{Location: loc("CNSHG"), Code: "CFS-1"}, LoadIntent{ShipmentID: "SH-2", Revision: 1, Status: IntentPlanned,
		PlanVersion: 1, Mode: ModeSea, Scope: CargoScope{Packs: 1, GrossWeight: kg(10)}}, cartons(1, ConditionGood, at), "ops", at)
	if err = sea.Screen(Screening{Outcome: ScreeningPassed, Screener: "x", Method: "XRAY", Reference: "1", Evidence: ev("1", at)}, "ops", at); !errors.Is(err, ErrScreeningForAir) {
		t.Fatalf("screening is for AIR cargo: %v", err)
	}
}

// tender returns a closed air consolidation with SH-1 attached, its live entry and a loading tender.
func tender(t *testing.T) (*Consolidation, Entry, *LoadExecution) {
	t.Helper()
	c := airConsol(t, "C-1")
	m, _ := NewMembership("T", "SH-1")
	if err := errors.Join(m.Attach(c, intent("SH-1", 1), "ops", t0.Add(time.Hour)), c.Close(scopes(1), t0.Add(time.Hour), "", "ops", t0.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	x, err := OpenLoadExecution("T", "X-1", c, ExecutionTender, terminal, "ops", t0.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = x.Start("ops", t0.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	live, _ := m.Live()
	return c, live, x
}

func TestLoadExecutionTender(t *testing.T) {
	c, entry, x := tender(t)
	lot := received(t)
	at := t0.Add(5 * time.Hour)
	sea, _ := CreateConsolidation("T", "C-S", ModeSea, KindSeaLCLOwn, "LE", shgHAM, t0.Add(72*time.Hour), t0.Add(60*time.Hour), "ops", t0)
	if _, err := OpenLoadExecution("T", "X-S", sea, ExecutionTender, Facility{Location: loc("CNSHG"), Code: "CFS"}, "ops", at); !errors.Is(err, ErrKindNotTendered) {
		t.Fatalf("an own LCL container is stuffed, not tendered: %v", err)
	}
	if _, err := OpenLoadExecution("T", "X-2", c, "STUFFING", terminal, "ops", at); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("stuffing comes with load units: %v", err)
	}
	if _, err := OpenLoadExecution("T", "X-2", c, ExecutionTender, Facility{Location: loc("USJFK"), Code: "T"}, "ops", at); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("a tender is at the origin: %v", err)
	}
	if err := x.Load(LoadLine{Packs: 7, Evidence: ev("TD-1", at)}, c, lot, entry, LotLoad{}, 0, "ops", at); !errors.Is(err, ErrConservation) {
		t.Fatalf("7 of 6 received: %v", err)
	}
	if err := x.Load(LoadLine{Packs: 2, Evidence: ev("TD-1", at)}, c, lot, entry, LotLoad{Packs: 5}, 0, "ops", at); !errors.Is(err, ErrConservation) {
		t.Fatalf("5 loaded elsewhere and 2 more: %v", err)
	}
	if err := x.Load(LoadLine{Packs: 2, Evidence: ev("TD-1", at)}, c, lot, entry, LotLoad{}, 5, "ops", at); !errors.Is(err, ErrConservation) {
		t.Fatalf("the house plans 6, 5 are loaded: %v", err)
	}
	for i, packs := range []int64{2, 2, 2} {
		done := LotLoad{Packs: int64(2 * i), GrossWeight: int64(100 * i)}
		if err := x.Load(LoadLine{Packs: packs, Evidence: ev(fmt.Sprint("TD-", i), at)}, c, lot, entry, done, int64(2*i), "ops", at); err != nil {
			t.Fatal(err)
		}
	}
	var sum int64
	for _, l := range x.Lines() {
		if l.Basis != BasisApportioned || l.MeasurementNo != 1 {
			t.Fatalf("apportioned from measurement 1: %+v", l)
		}
		sum += l.GrossWeight.Amount
	}
	if sum != 300 || x.Tendered() != 6 {
		t.Fatalf("lines loading every pack add up to the lot's 300 kg: %d, %d packs", sum, x.Tendered())
	}
	if err := x.Load(LoadLine{Packs: 1, Volume: kernel.Quantity{Unit: "MTQ", Amount: 1}, Evidence: ev("TD-V", at)}, c, lot, entry, LotLoad{}, 0, "ops", at); !errors.Is(err, ErrInvalidLoad) {
		t.Fatalf("a volume measured for the line comes with its weight: %v", err)
	}
	if err := x.Unload(3, 1, ReversalMisloaded, ev("UN-1", at), lot, "ops", at); err != nil {
		t.Fatal(err)
	}
	if packs, weight, _ := x.LineLive(3); packs != 1 || weight != 50 {
		t.Fatalf("1 of line 3's 2 packs and 100 kg is live: %d, %d", packs, weight)
	}
	if err := x.Unload(3, 2, ReversalMisloaded, ev("UN-2", at), lot, "ops", at); !errors.Is(err, ErrInvalidLoad) {
		t.Fatalf("only 1 pack of line 3 is live: %v", err)
	}
	if err := x.PlaceHold(HoldScreeningFailed, "", ev("H", at), "ops", at); !errors.Is(err, ErrInvalidLoad) {
		t.Fatalf("a screening hold is PHY's: %v", err)
	}
	if err := x.PlaceHold(HoldDGQuery, "UN3481 query", ev("DG-1", at), "ops", at); err != nil || x.Current() != ExecutionOnHold {
		t.Fatalf("hold: %v", err)
	}
	if err := x.Close(ev("TR-1", at), "ops", at); !errors.Is(err, ErrOnHold) {
		t.Fatalf("a held tender does not close: %v", err)
	}
	if err := x.ReleaseHold(1, ev("DG-OK", at), "", "ops", at); err != nil || x.Current() != ExecutionLoading {
		t.Fatalf("the last release resumes loading: %v", err)
	}
	if _, _, err := x.RecordCarrierEvidence(CarrierEvidence{Fact: CarrierDeparted, Pieces: 5, Transport: "MU587", Evidence: ev("FSU-1", at)}, "ops", at); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("carrier evidence follows the tender: %v", err)
	}
	if err := x.Close(ev("TR-1", at), "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := c.Reopen("late cargo", "ops", at, x.Tendered(), Stuffed{}); !errors.Is(err, ErrExecutionsClosed) {
		t.Fatalf("no reopen after the tender: %v", err)
	}
	departed := CarrierEvidence{Fact: CarrierDeparted, Pieces: 4, Transport: "MU587", Evidence: Evidence{Source: "CARGO-IMP", Reference: "FSU-DEP-1", OccurredAt: at}}
	no, added, err := x.RecordCarrierEvidence(departed, "ops", at)
	if err != nil || no != 1 || !added {
		t.Fatalf("departed: %d %v %v", no, added, err)
	}
	if no, added, err = x.RecordCarrierEvidence(departed, "ops", at); err != nil || no != 1 || added {
		t.Fatalf("the same message again changes nothing: %d %v %v", no, added, err)
	}
	other := departed
	other.Pieces = 3
	if _, _, err = x.RecordCarrierEvidence(other, "ops", at); !errors.Is(err, ErrEvidenceReused) {
		t.Fatalf("the same reference with other content: %v", err)
	}
	offloaded := CarrierEvidence{Fact: CarrierOffloaded, Pieces: 2, Transport: "MU587", Evidence: Evidence{Source: "CARGO-IMP", Reference: "FSU-DIS-1", OccurredAt: at}}
	if _, _, err = x.RecordCarrierEvidence(offloaded, "ops", at); err != nil {
		t.Fatal(err)
	}
	kinds := map[LoadingDiscrepancyKind]LoadingDiscrepancy{}
	for _, d := range x.State().Discrepancies {
		kinds[d.Kind] = d
	}
	if len(kinds) != 2 || kinds[LoadConflict].Observed != 6 || kinds[LoadConflict].Expected != 5 || kinds[LoadOffloaded].Observed != 2 {
		t.Fatalf("4 departed and 2 offloaded of 5 tendered: %+v", kinds)
	}
	if err = x.ResolveDiscrepancy(1, ResolvedByClaim, "", "", "ops", at); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("a claim has a reference: %v", err)
	}
	restored, err := RestoreLoadExecution(x.State())
	if err != nil || restored.OpenDiscrepancies() != 2 || restored.Tendered() != 5 {
		t.Fatalf("restore: %v", err)
	}
}

func TestScreeningFailureHoldsTheTender(t *testing.T) {
	c, entry, x := tender(t)
	lot := received(t)
	at := t0.Add(5 * time.Hour)
	failed := Screening{Outcome: ScreeningFailed, Screener: "PVG security", Method: "XRAY", Reference: "SC-9", Evidence: ev("SC-9", at)}
	if err := lot.Screen(failed, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := x.Load(LoadLine{Packs: 6, GrossWeight: kg(310), Evidence: ev("TD-1", at)}, c, lot, entry, LotLoad{}, 0, "ops", at); err != nil {
		t.Fatalf("a failed lot's packs are recorded: %v", err)
	}
	if x.Current() != ExecutionOnHold || x.Lines()[0].Basis != BasisMeasured || x.Placed() != 1 {
		t.Fatalf("and hold the tender: %s", x.Current())
	}
	if x.HoldForScreening(lot.ID(), failed.Evidence, "ops", at) {
		t.Fatal("one screening hold per lot")
	}
	if err := x.ReleaseHold(1, ev("REL", at), lot.Screened(), "ops", at); !errors.Is(err, ErrHoldNotReleasable) {
		t.Fatalf("not while the lot failed and is loaded: %v", err)
	}
	if err := x.Unload(1, 6, ReversalOffloadedByForwarder, ev("UN-1", at), lot, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := x.ReleaseHold(1, ev("REL", at), lot.Screened(), "ops", at); err != nil || x.Current() != ExecutionLoading {
		t.Fatalf("released once the lot's packs are out: %v", err)
	}
}

func TestReconcileConsolidation(t *testing.T) {
	c, entry, x := tender(t)
	at := t0.Add(6 * time.Hour)
	allocated := intent("SH-1", 1)
	allocated.ConsolidationID = c.ID()
	if err := c.Reconcile([]MemberCheck{{Entry: entry, Intent: allocated, Loaded: 6}}, 1, 0, "ops", at); !errors.Is(err, ErrNotReconciled) {
		t.Fatalf("a tender is open: %v", err)
	}
	if err := c.Reconcile([]MemberCheck{{Entry: entry, Intent: allocated, Loaded: 5}}, 0, 0, "ops", at); !errors.Is(err, ErrNotReconciled) {
		t.Fatalf("5 of 6 loaded: %v", err)
	}
	if err := c.Reconcile([]MemberCheck{{Entry: entry, Intent: intent("SH-1", 1), Loaded: 6}}, 0, 0, "ops", at); !errors.Is(err, ErrMemberNotAllocated) {
		t.Fatalf("FUL has not allocated the house: %v", err)
	}
	if err := c.Reconcile([]MemberCheck{{Entry: entry, Intent: allocated, Loaded: 6}}, 0, 1, "ops", at); !errors.Is(err, ErrNotReconciled) {
		t.Fatalf("a discrepancy is open: %v", err)
	}
	if err := c.Reconcile([]MemberCheck{{Entry: entry, Intent: allocated, Loaded: 6}}, 0, 0, "ops", at); err != nil || c.Current().State != StateReconciled {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := RestoreConsolidation(c.State()); err != nil {
		t.Fatal(err)
	}
	if err := c.Amend(Route{}, t0.Add(80*time.Hour), t0.Add(60*time.Hour), "late", "ops", at, 1); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a reconciled consolidation never changes: %v", err)
	}
	if _, err := OpenLoadExecution("T", "X-9", c, ExecutionTender, terminal, "ops", at); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("nor is it tendered: %v", err)
	}
	_ = x
	if err := Leavable(1); !errors.Is(err, ErrLiveLoadLines) {
		t.Fatalf("a loaded house does not leave: %v", err)
	}
	if err := Replannable(intent("SH-1", 2), 7); !errors.Is(err, ErrConservation) {
		t.Fatalf("a plan below the loaded packs: %v", err)
	}
}

// TestRandomLoadsConserveTheLot loads and, for even seeds, unloads random packs of one lot over three tenders.
// Whatever is accepted, live loaded packs never exceed the 9 received, no line is reversed beyond its packs,
// every tender restores, and whenever all 9 packs are live, reversals or not, the apportioned weights add up to
// the lot's.
func TestRandomLoadsConserveTheLot(t *testing.T) {
	complete := 0
	for seed := uint64(1); seed <= 200; seed++ {
		r := rand.New(rand.NewPCG(seed, 11))
		c := airConsol(t, "C-1")
		m, _ := NewMembership("T", "SH-1")
		nine := intent("SH-1", 1)
		nine.Scope, nine.Parts[0].Packs, nine.Parts[0].GrossWeight = CargoScope{Packs: 9, GrossWeight: kg(1001)}, 9, kg(1001)
		_ = m.Attach(c, nine, "ops", t0.Add(time.Hour))
		entry, _ := m.Live()
		lot, _ := ReceiveCargo("T", "L-1", warehouse, nine, cartons(9, ConditionGood, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
		_ = lot.Measure(scale(1001, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
		var xs []*LoadExecution
		for i := range 3 {
			x, _ := OpenLoadExecution("T", LoadExecutionID(fmt.Sprint("X-", i)), c, ExecutionTender, terminal, "ops", t0.Add(2*time.Hour))
			_ = x.Start("ops", t0.Add(2*time.Hour))
			xs = append(xs, x)
		}
		load := func() LotLoad {
			var l LotLoad
			for _, x := range xs {
				for _, line := range x.Lines() {
					packs, weight, _ := x.LineLive(line.No)
					l.Packs, l.GrossWeight = l.Packs+packs, l.GrossWeight+weight
				}
			}
			return l
		}
		live := func() int64 { return load().Packs }
		at := t0.Add(3 * time.Hour)
		for step := 0; step < 30; step++ {
			x := xs[r.IntN(3)]
			if seed%2 == 0 && r.IntN(4) == 0 && len(x.Lines()) > 0 { // odd seeds never reverse
				line := 1 + r.IntN(len(x.Lines()))
				_ = x.Unload(line, 1+int64(r.IntN(3)), ReversalMisloaded, ev("UN", at), lot, "ops", at)
			} else {
				_ = x.Load(LoadLine{Packs: 1 + int64(r.IntN(4)), Evidence: ev("TD", at)}, c, lot, entry, load(), live(), "ops", at)
			}
			if n := live(); n < 0 || n > 9 {
				t.Fatalf("seed %d: %d packs live of 9", seed, n)
			}
		}
		var weight int64
		for _, x := range xs {
			if _, err := RestoreLoadExecution(x.State()); err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
			for _, l := range x.Lines() {
				_, w, _ := x.LineLive(l.No)
				weight += w
			}
		}
		if live() == 9 {
			complete++
			if weight != 1001 {
				t.Fatalf("seed %d: all 9 packs loaded weigh %d of 1001 kg", seed, weight)
			}
		}
	}
	if complete < 50 {
		t.Fatalf("only %d of 200 runs end with the whole lot loaded", complete)
	}
}

// TestPhysicalDeadEndsAreOpen covers what the review of V0.2 found: packs taken back from the carrier after the
// tender, a closed lot's packs staying loaded, a cancelled shipment's lot closing while receiving, resolution
// texts checked, and one departure reported by two sources being one departure.
func TestPhysicalDeadEndsAreOpen(t *testing.T) {
	c, entry, x := tender(t)
	lot := received(t)
	at := t0.Add(5 * time.Hour)
	if err := errors.Join(x.Load(LoadLine{Packs: 6, Evidence: ev("TD-1", at)}, c, lot, entry, LotLoad{}, 0, "ops", at), x.Close(ev("TR-1", at), "ops", at)); err != nil {
		t.Fatal(err)
	}
	if got := x.Lines()[0].GrossWeight; got != kg(300) {
		t.Fatalf("the packs completing the lot take its measure: %+v", got)
	}
	for _, src := range []string{"MU-CARGO", "PVG-HANDLER"} {
		e := CarrierEvidence{Fact: CarrierDeparted, Pieces: 6, Transport: "MU587", Evidence: Evidence{Source: src, Reference: "DEP-1", OccurredAt: at}}
		if _, _, err := x.RecordCarrierEvidence(e, "ops", at); err != nil {
			t.Fatal(err)
		}
	}
	if n := x.OpenDiscrepancies(); n != 0 {
		t.Fatalf("one departure reported twice is no conflict: %d open", n)
	}
	if err := x.Unload(1, 2, ReversalMisloaded, ev("UN-1", at), lot, "ops", at); !errors.Is(err, ErrNotLoading) {
		t.Fatalf("after the tender only packs taken back from the carrier: %v", err)
	}
	if err := x.Unload(1, 2, ReversalOffloadedByForwarder, ev("UN-1", at), lot, "ops", at); err != nil || x.Tendered() != 4 {
		t.Fatalf("2 packs retrieved from the terminal: %v, %d tendered", err, x.Tendered())
	}
	if err := lot.Close(4, 4, intent("SH-1", 1), "ops", at); !errors.Is(err, ErrUnaccounted) {
		t.Fatalf("2 retrieved packs are not accounted for: %v", err)
	}
	other, _ := ReceiveCargo("T", "L-9", warehouse, intent("SH-1", 1), cartons(6, ConditionGood, at), "ops", at)
	if err := x.Unload(1, 4, ReversalOffloadedByForwarder, ev("UN-2", at), other, "ops", at); !errors.Is(err, ErrInvalidLoad) {
		t.Fatalf("a line's own lot: %v", err)
	}
	closedLot := received(t)
	_ = closedLot.Close(6, 6, intent("SH-1", 1), "ops", at)
	if err := x.Unload(1, 4, ReversalOffloadedByForwarder, ev("UN-2", at), closedLot, "ops", at); !errors.Is(err, ErrLotClosed) {
		t.Fatalf("a closed lot's packs stay loaded: %v", err)
	}

	open, _ := ReceiveCargo("T", "L-2", warehouse, intent("SH-2", 1), cartons(3, ConditionGood, at), "ops", at)
	cancelled := intent("SH-2", 1)
	cancelled.Status = IntentCancelled
	if err := open.Close(0, 0, intent("SH-2", 1), "ops", at); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a planned shipment's lot completes receipt first: %v", err)
	}
	if err := open.Close(0, 0, cancelled, "ops", at); err != nil {
		t.Fatalf("a cancelled shipment's lot closes while receiving: %v", err)
	}
	if _, err := RestoreCargoLot(open.State()); err != nil {
		t.Fatalf("restore a lot closed without completion: %v", err)
	}

	short, _ := ReceiveCargo("T", "L-3", warehouse, intent("SH-3", 1), cartons(5, ConditionGood, at), "ops", at)
	_ = errors.Join(short.Measure(scale(250, at), "ops", at), short.Complete(intent("SH-3", 1), "ops", at))
	for _, tc := range []struct {
		r                 Resolution
		reference, reason string
	}{{ResolvedByAccepted, "   ", "customer agrees"}, {ResolvedByClaim, "CLM-1", "   "}, {ResolvedByAccepted, "", string(make([]rune, 501))}} {
		if err := short.Resolve(1, tc.r, tc.reference, tc.reason, LoadIntent{}, "ops", at); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("%s with %q and %q: %v", tc.r, tc.reference, tc.reason, err)
		}
	}
}

// TestEvidenceForFUL covers what PhysicalLoadEvidence v1 tells FUL of a house in a tender: the pieces the
// carrier's evidence shows departed (the house's own reports summed over flights, else the master's when they
// cover the tender), and the measures at the lot's latest measurement, so a terminal weight after the tender
// counts.
func TestEvidenceForFUL(t *testing.T) {
	at := t0.Add(5 * time.Hour)
	tendered := func(t *testing.T) *LoadExecution {
		t.Helper()
		c, entry, x := tender(t)
		if err := errors.Join(x.Load(LoadLine{Packs: 6, Evidence: ev("TD", at)}, c, received(t), entry, LotLoad{}, 0, "ops", at),
			x.Close(ev("TR", at), "ops", at)); err != nil {
			t.Fatal(err)
		}
		return x
	}
	report := func(x *LoadExecution, fact CarrierFact, shipment ShipmentID, pieces int64, flight, ref string) {
		t.Helper()
		if _, _, err := x.RecordCarrierEvidence(CarrierEvidence{Fact: fact, ShipmentID: shipment, Pieces: pieces, Transport: flight,
			Evidence: Evidence{Source: "MU", Reference: ref, OccurredAt: at}}, "ops", at); err != nil {
			t.Fatal(err)
		}
	}
	x := tendered(t)
	if p := x.HousePieces("SH-1"); p != (HousePieces{}) {
		t.Fatalf("nothing reported: %+v", p)
	}
	report(x, CarrierDeparted, "", 4, "MU587", "M-1")
	if p := x.HousePieces("SH-1"); p.Departed != 0 {
		t.Fatalf("4 of 6 departed, not attributable: %+v", p)
	}
	report(x, CarrierOffloaded, "SH-1", 2, "MU587", "H-1")
	if p := x.HousePieces("SH-1"); p.Departed != 4 || p.Offloaded != 2 {
		t.Fatalf("the master covers the tender less the offloaded: %+v", p)
	}
	report(x, CarrierDeparted, "", 2, "MU589", "M-2")
	report(x, CarrierArrived, "", 6, "MU587", "M-3")
	if p := x.HousePieces("SH-1"); p.Departed != 6 || p.Arrived != 6 {
		t.Fatalf("the offloaded pieces flew on MU589: %+v", p)
	}
	own := tendered(t)
	report(own, CarrierDeparted, "SH-1", 5, "MU587", "H-5")
	report(own, CarrierDeparted, "SH-1", 5, "MU587", "H-5B") // the same departure from a second source
	report(own, CarrierDeparted, "SH-1", 1, "MU589", "H-6")
	if p := own.HousePieces("SH-1"); p.Departed != 6 {
		t.Fatalf("the house's own reports, largest per flight, summed: %+v", p)
	}

	c, entry, y := tender(t)
	lot := received(t)
	if err := y.Load(LoadLine{Packs: 4, Evidence: ev("TD-1", at)}, c, lot, entry, LotLoad{}, 0, "ops", at); err != nil {
		t.Fatal(err)
	}
	lots := map[CargoLotID]*CargoLot{lot.ID(): lot}
	if packs, w, _ := y.HouseMeasure("SH-1", lots); packs != 4 || w != kg(200) {
		t.Fatalf("4 of 6 at 300 kg: %d %+v", packs, w)
	}
	if err := lot.Measure(Measurement{GrossWeight: kg(312), Method: MethodTerminalWeight, Evidence: ev("PVG-SCALE", at)}, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := y.Load(LoadLine{Packs: 2, Evidence: ev("TD-2", at)}, c, lot, entry, LotLoad{Packs: 4, GrossWeight: 200}, 4, "ops", at); err != nil {
		t.Fatal(err)
	}
	if packs, w, _ := y.HouseMeasure("SH-1", lots); packs != 6 || w != kg(312) {
		t.Fatalf("the whole lot at its terminal weight: %d %+v", packs, w)
	}
	if err := y.Unload(2, 2, ReversalMisloaded, ev("UN", at), lot, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := y.Load(LoadLine{Packs: 2, GrossWeight: kg(110), Evidence: ev("TD-3", at)}, c, lot, entry, LotLoad{Packs: 4, GrossWeight: 200}, 4, "ops", at); err != nil {
		t.Fatal(err)
	}
	if packs, w, _ := y.HouseMeasure("SH-1", lots); packs != 6 || w != kg(312) {
		t.Fatalf("a line weighed at 110 kg and the other 4 packs taking the rest of the lot's 312 kg: %d %+v", packs, w)
	}
	masterOnly := tendered(t)
	report(masterOnly, CarrierDeparted, "", 4, "MU587", "MO-1")
	report(masterOnly, CarrierOffloaded, "", 2, "MU587", "MO-2")
	if p := masterOnly.HousePieces("SH-1"); p.Departed != 0 {
		t.Fatalf("an offload reported for the master names no house: %+v", p)
	}
}
