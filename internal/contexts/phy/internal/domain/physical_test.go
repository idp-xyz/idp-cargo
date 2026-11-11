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
	if err := errors.Join(m.Attach(c, intent("SH-1", 1), "ops", t0.Add(time.Hour)), c.Close(1, t0.Add(time.Hour), "ops", t0.Add(2*time.Hour))); err != nil {
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
	if err := x.Load(LoadLine{Packs: 7, Evidence: ev("TD-1", at)}, c, lot, entry, 0, 0, "ops", at); !errors.Is(err, ErrConservation) {
		t.Fatalf("7 of 6 received: %v", err)
	}
	if err := x.Load(LoadLine{Packs: 2, Evidence: ev("TD-1", at)}, c, lot, entry, 5, 0, "ops", at); !errors.Is(err, ErrConservation) {
		t.Fatalf("5 loaded elsewhere and 2 more: %v", err)
	}
	if err := x.Load(LoadLine{Packs: 2, Evidence: ev("TD-1", at)}, c, lot, entry, 0, 5, "ops", at); !errors.Is(err, ErrConservation) {
		t.Fatalf("the house plans 6, 5 are loaded: %v", err)
	}
	for i, packs := range []int64{2, 2, 2} {
		if err := x.Load(LoadLine{Packs: packs, Evidence: ev(fmt.Sprint("TD-", i), at)}, c, lot, entry, int64(2*i), int64(2*i), "ops", at); err != nil {
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
	if err := x.Unload(3, 1, ReversalMisloaded, ev("UN-1", at), "ops", at); err != nil {
		t.Fatal(err)
	}
	if packs, weight, _ := x.LineLive(3); packs != 1 || weight != 50 {
		t.Fatalf("1 of line 3's 2 packs and 100 kg is live: %d, %d", packs, weight)
	}
	if err := x.Unload(3, 2, ReversalMisloaded, ev("UN-2", at), "ops", at); !errors.Is(err, ErrInvalidLoad) {
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
	if err := c.Reopen("late cargo", "ops", at, x.Tendered()); !errors.Is(err, ErrExecutionsClosed) {
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
	if err := x.Load(LoadLine{Packs: 6, GrossWeight: kg(310), Evidence: ev("TD-1", at)}, c, lot, entry, 0, 0, "ops", at); err != nil {
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
	if err := x.Unload(1, 6, ReversalOffloadedByForwarder, ev("UN-1", at), "ops", at); err != nil {
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
// every tender restores, and once all 9 packs are loaded without reversals the apportioned weights add up to
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
		live := func() int64 {
			var n int64
			for _, x := range xs {
				n += x.LiveFromLot("L-1")
			}
			return n
		}
		reversed := false
		at := t0.Add(3 * time.Hour)
		for step := 0; step < 30; step++ {
			x := xs[r.IntN(3)]
			if seed%2 == 0 && r.IntN(4) == 0 && len(x.Lines()) > 0 { // odd seeds never reverse
				line := 1 + r.IntN(len(x.Lines()))
				if x.Unload(line, 1+int64(r.IntN(3)), ReversalMisloaded, ev("UN", at), "ops", at) == nil {
					reversed = true
				}
			} else {
				_ = x.Load(LoadLine{Packs: 1 + int64(r.IntN(4)), Evidence: ev("TD", at)}, c, lot, entry, live(), live(), "ops", at)
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
		if live() == 9 && !reversed {
			complete++
			if weight != 1001 {
				t.Fatalf("seed %d: all 9 packs loaded weigh %d of 1001 kg", seed, weight)
			}
		}
	}
	if complete < 50 {
		t.Fatalf("only %d of 200 runs loaded the whole lot without reversals", complete)
	}
}
