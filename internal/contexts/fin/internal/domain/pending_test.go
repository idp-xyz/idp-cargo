package domain

import (
	"slices"
	"strings"
	"testing"
)

// FIN V2.94: a pool's pending items. Each kind is tested in the list and out of it, on the owner's box A (C-1 with H01,
// H02 and H03 of 10, 15 and 5 W/M, freight CNY 3,000.00 apportioned 1,000.00, 1,500.00 and 500.00; H03 transferred by
// ops-9 after a rework asked by ops-1).

func kindsOf(items []PendingItem) (out []string) {
	for _, it := range items {
		out = append(out, it.Kind)
	}
	return out
}

func onlyKind(t *testing.T, what string, items []PendingItem, kind string) PendingItem {
	t.Helper()
	if len(items) != 1 || items[0].Kind != kind {
		t.Fatalf("%s: %+v, want only %s", what, items, kind)
	}
	return items[0]
}

func mustSay(t *testing.T, what, message string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(message, part) {
			t.Fatalf("%s: %q does not say %q", what, message, part)
		}
	}
}

// settle matches every line of the pool final at its accrued cost, so that no line is listed for its accrual.
func settle(p *ConsolCostPool) {
	consumed := map[int]Consumption{}
	for _, l := range p.state.Lines {
		v := l.Versions[len(l.Versions)-1]
		consumed[l.No] = Consumption{Amount: v.Amount.Minor, Local: v.Local, Final: true}
	}
	p.Matched(consumed)
}

// pendingOf lists the pending items of the pool once its lines are matched final.
func pendingOf(p *ConsolCostPool, basis CostBasis) []PendingItem {
	settle(p)
	return p.PendingItems(basis)
}

// boxAGone is box A once every house left.
func boxAGone() CostBasis {
	b := boxAAfter()
	b.Houses = nil
	b.FormerHouses = append([]FormerHouseFacts{{ShipmentID: "H01", End: "DETACHED", EndedBy: "ops-8", EndedAt: hour(25)},
		{ShipmentID: "H02", End: "DETACHED", EndedBy: "ops-8", EndedAt: hour(25)}}, b.FormerHouses...)
	return b
}

// A container of the consolidation in rework is the first item: operations seal it again or give it up. A pool with
// nothing pending lists nothing.
func TestPendingItemsListAContainerInRework(t *testing.T) {
	p := boxAPool(t)
	if got := pendingOf(p, boxA()); len(got) != 0 {
		t.Fatalf("a pool with nothing pending: %+v", got)
	}
	b := boxA()
	b.LoadUnits = []LoadUnitFacts{{UnitNo: 1, Status: "IN_USE", ExecutionID: "X-1", ExecutionState: ExecutionRework}}
	b.PackingLists = []UnitPackingListFacts{{UnitNo: 1, ExecutionID: "X-1", ExecutionState: ExecutionRework}}
	it := onlyKind(t, "in rework", pendingOf(p, b), pendingInWork)
	if it.UnitNo != 1 || it.Actor != ActorOperations || it.LineNo != 0 {
		t.Fatalf("the item: %+v", it)
	}
	mustSay(t, "in rework", it.Message, "load unit 1 (stuffing X-1) is in rework", "seal the container again", "give its stuffing up", "OPERATIONS")
}

// While a container is in rework the lines are not listed as not on the basis (nothing is apportioned before it is
// sealed again), and once it is they are.
func TestPendingItemsKeepTheLinesAsideWhileAContainerIsInRework(t *testing.T) {
	p := boxAPool(t)
	unloaded := boxA()
	unloaded.Houses = slices.Clone(unloaded.Houses)
	unloaded.Houses[2].LoadedPacks = 0
	onlyKind(t, "unloaded, sealed again", pendingOf(p, unloaded), pendingLineNotOnBasis)
	unloaded.LoadUnits = []LoadUnitFacts{{UnitNo: 1, ExecutionState: ExecutionRework}}
	onlyKind(t, "unloaded, in rework", pendingOf(p, unloaded), pendingInWork)
}

// A former share not attributed names the line, the house, the amount (the default loss), the way out, who acts and who
// cannot confirm it. Attributed, or before the house left, it is not listed.
func TestPendingItemsListAFormerShare(t *testing.T) {
	p := boxAPool(t)
	if got := pendingOf(p, boxA()); len(got) != 0 {
		t.Fatalf("before H03 left: %+v", got)
	}
	it := onlyKind(t, "after H03 left", pendingOf(p, boxAAfter()), pendingFormerShare)
	if it.LineNo != 1 || it.ShipmentID != "H03" || it.Amount != 50_000 || it.Local != 50_000 || it.Currency != "CNY" || it.Actor != ActorFinance {
		t.Fatalf("the item: %+v", it)
	}
	mustSay(t, "former share", it.Message, "house H03", "CNY 500.00", "line 1", "loss it bears", "attribute it", "voucher", "FINANCE", "cannot submit it", "ops-1", "ops-9")
	if err := attributed(p, "fin-1", 32, attribution(1, "H03", 10_000, 20_000, PartyCarrier)); err != nil {
		t.Fatal(err)
	}
	if got := pendingOf(p, boxAAfter()); len(got) != 0 {
		t.Fatalf("attributed: %+v", got)
	}
	// a house that left with no rework names only who ended its place
	b := boxAAfter()
	b.FormerHouses = slices.Clone(b.FormerHouses)
	b.FormerHouses[0].ReworkAskedBy, b.FormerHouses[0].ReworkNo = "", 0
	q := boxAPool(t)
	it = onlyKind(t, "no rework", pendingOf(q, b), pendingFormerShare)
	mustSay(t, "no rework", it.Message, "Whoever ended its place in the consolidation (ops-9) cannot submit it")
	if strings.Contains(it.Message, "ops-1") {
		t.Fatalf("names a rework there was none of: %q", it.Message)
	}
}

// With no live house the remainder of a line is to be allocated: add cargo, attribute it, or cancel and retain.
func TestPendingItemsListAnAmountToBeAllocated(t *testing.T) {
	p := boxAPool(t)
	gone := boxAGone()
	if err := apportion(p, ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""),
		attribution(1, "H03", 10_000, 20_000, PartyCarrier)}}, gone, "fin-1", hour(30)); err != nil {
		t.Fatal(err)
	}
	it := onlyKind(t, "to be allocated", pendingOf(p, gone), pendingToBeAllocated)
	if it.LineNo != 1 || it.Amount != 270_000 || it.Local != 270_000 || it.Actor != ActorFinance || it.ShipmentID != "" {
		t.Fatalf("the item: %+v", it)
	}
	mustSay(t, "to be allocated", it.Message, "line 1", "CNY 2,700.00 to be allocated", "add cargo", "attribute it as a loss or an exception cost", "cancel the consolidation and retain",
		"FINANCE")
	// all of it attributed: nothing is left to allocate
	all := []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""), attribution(1, "H03", 10_000, 290_000, PartyCarrier)}
	for i := range all {
		all[i].Reason = "the rest is the consolidation's"
	}
	if err := apportion(p, ApportionInput{Jobs: boxAJobs(), Attributions: all}, gone, "fin-1", hour(32)); err != nil {
		t.Fatal(err)
	}
	if got := pendingOf(p, gone); len(got) != 0 {
		t.Fatalf("nothing to allocate: %+v", got)
	}
}

// An exception cost in force whose party is pending is listed, with the way to name the party; it does not hold back
// the close. A party that is established is not listed.
func TestPendingItemsListAnExceptionCostWhoseCauseIsPending(t *testing.T) {
	p := boxAPool(t)
	if err := attributed(p, "fin-1", 32, attribution(1, "H03", 10_000, 20_000, PartyPending)); err != nil {
		t.Fatal(err)
	}
	it := onlyKind(t, "cause pending", pendingOf(p, boxAAfter()), pendingExceptionCause)
	if it.LineNo != 1 || it.ShipmentID != "H03" || it.Amount != 20_000 || it.Actor != ActorFinance {
		t.Fatalf("the item: %+v", it)
	}
	mustSay(t, "cause pending", it.Message, "CNY 200.00", "house H03", "party is not established", "correction", "reason", "voucher CLAIM-1", "does not hold back the close", "FINANCE")
	if strings.Contains(it.Message, "reopen") {
		t.Fatalf("an open pool needs no reopening: %q", it.Message)
	}
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	if err := p.Close(boxAAfter(), "fin-1", hour(33)); err != nil {
		t.Fatalf("the pending party does not stop the close: %v", err)
	}
	it = onlyKind(t, "closed", pendingOf(p, boxAAfter()), pendingExceptionCause)
	mustSay(t, "closed", it.Message, "reopen the pool with a reason (ReopenConsolCostPool)")
	q := boxAPool(t)
	if err := attributed(q, "fin-1", 32, attribution(1, "H03", 10_000, 20_000, PartyWarehouse)); err != nil {
		t.Fatal(err)
	}
	if got := pendingOf(q, boxAAfter()); len(got) != 0 {
		t.Fatalf("an established party: %+v", got)
	}
}

// A pool closed before the house left holds a former share that only a reopening can attribute: REOPEN_TO_ATTRIBUTE
// replaces FORMER_SHARE_PENDING and says so. Nothing reopens the pool.
func TestPendingItemsSayThatAClosedPoolIsReopenedToAttribute(t *testing.T) {
	p := boxAPool(t)
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	if err := p.Close(boxA(), "fin-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if got := pendingOf(p, boxA()); len(got) != 0 {
		t.Fatalf("closed, nobody left: %+v", got)
	}
	it := onlyKind(t, "closed, H03 left", pendingOf(p, boxAAfter()), pendingReopenToAttribute)
	if it.LineNo != 1 || it.ShipmentID != "H03" || it.Amount != 50_000 || it.Actor != ActorFinance {
		t.Fatalf("the item: %+v", it)
	}
	mustSay(t, "reopen", it.Message, "reopen the pool with a reason (ReopenConsolCostPool)", "then preview the line again", "CNY 500.00", "ops-1", "ops-9", "FINANCE")
	if p.State().State != PoolClosed {
		t.Fatal("listing reopened the pool")
	}
	if err := p.Reopen("H03 left", "fin-1", hour(40)); err != nil {
		t.Fatal(err)
	}
	onlyKind(t, "reopened", pendingOf(p, boxAAfter()), pendingFormerShare)
}

// A line the close would refuse as not apportioned, or apportioned at another cost, or not on the loaded basis, is
// listed with the way out, which is the close's own; a line that costs nothing is not.
func TestPendingItemsListTheLinesTheCloseWouldRefuse(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	recordCost(t, p, "BAF", cny(0), MethodChargeable)
	it := onlyKind(t, "not apportioned", pendingOf(p, boxA()), pendingLineNotApportioned)
	if it.LineNo != 1 || it.Actor != ActorFinance {
		t.Fatalf("the item: %+v", it)
	}
	mustSay(t, "not apportioned", it.Message, "cost line 1 is not apportioned", "preview the line again and submit it", "FINANCE")
	if err := apportion(p, ApportionInput{Jobs: boxAJobs(), LineNos: []int{1}}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if got := pendingOf(p, boxA()); len(got) != 0 {
		t.Fatalf("apportioned, the other costs nothing: %+v", got)
	}
	// apportioned at another cost
	p.Matched(nil) // the accrual is open again for the revision
	if err := p.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(310_000)}, "more", policy(t), "ops-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	it = onlyKind(t, "another cost", pendingOf(p, boxA()), pendingLineNotApportioned)
	mustSay(t, "another cost", it.Message, "cost line 1 was apportioned at CNY 3,000.00 (CNY 3,000.00 local), and its final cost is now CNY 3,100.00 (CNY 3,100.00 local)")
	// on another loaded basis: a house unloaded, and a house measured again
	q := boxAPool(t)
	unloaded := boxA()
	unloaded.Houses = slices.Clone(unloaded.Houses)
	unloaded.Houses[2].LoadedPacks = 0
	it = onlyKind(t, "unloaded", pendingOf(q, unloaded), pendingLineNotOnBasis)
	mustSay(t, "unloaded", it.Message, "cost line 1 is not apportioned on the current loaded basis", "preview the line again and submit it", "FINANCE")
	if it.LineNo != 1 || it.Actor != ActorFinance {
		t.Fatalf("the item: %+v", it)
	}
	measured := boxA()
	measured.Houses = slices.Clone(measured.Houses)
	measured.Houses[0].Volume, measured.Houses[0].LoadedVolume = cmq(11_000_000), cmq(11_000_000)
	onlyKind(t, "measured again", pendingOf(q, measured), pendingLineNotOnBasis)
}

// The close does not list a line of a closed pool: it is reopened first.
func TestPendingItemsDoNotListLinesOfAClosedPool(t *testing.T) {
	p := boxAPool(t)
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	if err := p.Close(boxA(), "fin-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	unloaded := boxA()
	unloaded.Houses = slices.Clone(unloaded.Houses)
	unloaded.Houses[2].LoadedPacks = 0
	if got := pendingOf(p, unloaded); len(got) != 0 {
		t.Fatalf("a closed pool: %+v", got)
	}
}

// The list is in a fixed order: by kind, then line, then house.
func TestPendingItemsAreInAFixedOrder(t *testing.T) {
	// open: former shares (by line, then house), an amount to be allocated, an exception cost, a line not apportioned,
	// a line not on the basis
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	recordCost(t, p, "THC", cny(90_000), MethodChargeable)
	recordCost(t, p, "DOC", cny(30_000), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs(), LineNos: []int{1, 2}}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	gone := boxAGone()
	// line 2 is divided with every house gone: all of it to be allocated, with an exception cost pending on it
	in := ApportionInput{Jobs: boxAJobs(), LineNos: []int{2}, Attributions: []AttributionInput{attribution(2, "H01", 0, 0, ""), attribution(2, "H02", 0, 0, ""),
		attribution(2, "H03", 0, 10_000, PartyPending)}}
	if err := apportion(p, in, gone, "fin-1", hour(30)); err != nil {
		t.Fatal(err)
	}
	got := pendingOf(p, gone)
	type row struct {
		kind  string
		line  int
		house string
	}
	var rows []row
	for _, it := range got {
		rows = append(rows, row{it.Kind, it.LineNo, it.ShipmentID})
	}
	want := []row{{pendingFormerShare, 1, "H01"}, {pendingFormerShare, 1, "H02"}, {pendingFormerShare, 1, "H03"}, {pendingToBeAllocated, 2, ""},
		{pendingExceptionCause, 2, "H03"}, {pendingLineNotApportioned, 3, ""}}
	if !slices.Equal(rows, want) {
		t.Fatalf("the order: %v, want %v", rows, want)
	}
	// a container in rework comes first, and the lines are left aside
	working := gone
	working.LoadUnits = []LoadUnitFacts{{UnitNo: 2, ExecutionState: ExecutionRework}}
	got = pendingOf(p, working)
	if k := kindsOf(got); len(k) != 6 || k[0] != pendingInWork || k[1] != pendingFormerShare || slices.Contains(k, pendingLineNotApportioned) {
		t.Fatalf("in rework: %v", k)
	}
	// not apportioned before not on the basis
	q := pool(t, boxA())
	recordCost(t, q, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	recordCost(t, q, "THC", cny(90_000), MethodChargeable)
	if err := apportion(q, ApportionInput{Jobs: boxAJobs(), LineNos: []int{1}}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	unloaded := boxA()
	unloaded.Houses = slices.Clone(unloaded.Houses)
	unloaded.Houses[2].LoadedPacks = 0
	if k := kindsOf(pendingOf(q, unloaded)); !slices.Equal(k, []string{pendingLineNotApportioned, pendingLineNotOnBasis}) {
		t.Fatalf("lines: %v", k)
	}
	// closed: an exception cost, then a house that left after the close
	r := boxAPool(t)
	if err := attributed(r, "fin-1", 32, attribution(1, "H03", 10_000, 20_000, PartyPending)); err != nil {
		t.Fatal(err)
	}
	r.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	if err := r.Close(boxAAfter(), "fin-1", hour(33)); err != nil {
		t.Fatal(err)
	}
	later := boxAAfter()
	later.Houses = later.Houses[:1]
	later.FormerHouses = append(slices.Clone(later.FormerHouses), FormerHouseFacts{ShipmentID: "H02", End: "DETACHED", EndedBy: "ops-8", EndedAt: hour(34)})
	if k := kindsOf(pendingOf(r, later)); !slices.Equal(k, []string{pendingExceptionCause, pendingReopenToAttribute}) {
		t.Fatalf("closed: %v", k)
	}
}

// A line whose accrual is not matched final or released, and one with an invoice disputed or awaiting a variance
// approval, are what the close refuses first: they are listed, for finance, with the way out.
func TestPendingItemsListACostNotFinalAndAnInvoiceHeld(t *testing.T) {
	p := boxAPool(t)
	it := onlyKind(t, "accrued", p.PendingItems(boxA()), pendingCostNotFinal)
	if it.LineNo != 1 || it.Actor != ActorFinance || it.Amount != 0 {
		t.Fatalf("the item: %+v", it)
	}
	mustSay(t, "accrued", it.Message, "cost line 1 is ACCRUED", "match the creditor invoice as final", "release the accrual", "FINANCE")
	p.Matched(map[int]Consumption{1: {Amount: 100_000, Local: 100_000}})
	it = onlyKind(t, "partly matched", p.PendingItems(boxA()), pendingCostNotFinal)
	mustSay(t, "partly matched", it.Message, "cost line 1 is PARTLY_MATCHED")
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	if got := p.PendingItems(boxA()); len(got) != 0 {
		t.Fatalf("matched final: %+v", got)
	}
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true, Held: true}})
	it = onlyKind(t, "held", p.PendingItems(boxA()), pendingInvoiceHeld)
	if it.LineNo != 1 || it.Actor != ActorFinance {
		t.Fatalf("the item: %+v", it)
	}
	mustSay(t, "held", it.Message, "a creditor invoice matched to cost line 1 is disputed or awaits a variance approval", "settle the dispute or approve the variance", "FINANCE")
	q := boxAPool(t)
	if err := q.ReleaseCost(1, 1, "nothing will be invoiced", "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	// released, the accrual is final at nothing: only the apportionment at the old cost is left
	onlyKind(t, "released", q.PendingItems(boxA()), pendingLineNotApportioned)
}

// The list is empty exactly when the pool can be closed, apart from an exception cost whose party is pending, which
// does not hold the close back: for each state below, the items other than EXCEPTION_CAUSE_PENDING are none if and only
// if the close is accepted.
func TestThePendingListIsEmptyExactlyWhenThePoolCloses(t *testing.T) {
	final := map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}}
	type state struct {
		name   string
		pool   func(t *testing.T) *ConsolCostPool
		basis  CostBasis
		closes bool
	}
	apportioned := func(match map[int]Consumption) func(t *testing.T) *ConsolCostPool {
		return func(t *testing.T) *ConsolCostPool {
			p := boxAPool(t)
			p.Matched(match)
			return p
		}
	}
	attributedWith := func(party string) func(t *testing.T) *ConsolCostPool {
		return func(t *testing.T) *ConsolCostPool {
			p := boxAPool(t)
			if err := attributed(p, "fin-1", 32, attribution(1, "H03", 10_000, 20_000, party)); err != nil {
				t.Fatal(err)
			}
			p.Matched(final)
			return p
		}
	}
	unloaded := boxA()
	unloaded.Houses = slices.Clone(unloaded.Houses)
	unloaded.Houses[2].LoadedPacks = 0
	working := boxA()
	working.LoadUnits = []LoadUnitFacts{{UnitNo: 1, ExecutionState: ExecutionRework}}
	cancelled := unloaded
	cancelled.State = "CANCELLED"
	states := []state{
		{"not matched", apportioned(nil), boxA(), false},
		{"partly matched", apportioned(map[int]Consumption{1: {Amount: 100_000, Local: 100_000}}), boxA(), false},
		{"matched final", apportioned(final), boxA(), true},
		{"held", apportioned(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true, Held: true}}), boxA(), false},
		{"a house left, not attributed", apportioned(final), boxAAfter(), false},
		{"attributed, party established", attributedWith(PartyCarrier), boxAAfter(), true},
		{"attributed, party pending", attributedWith(PartyPending), boxAAfter(), true},
		{"a house unloaded", apportioned(final), unloaded, false},
		{"in rework", apportioned(final), working, false},
		{"the consolidation cancelled", apportioned(final), cancelled, true},
		{"a cost with no apportionment", func(t *testing.T) *ConsolCostPool {
			p := pool(t, boxA())
			recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
			p.Matched(final)
			return p
		}, boxA(), false},
		{"every house gone, an amount to be allocated", func(t *testing.T) *ConsolCostPool {
			p := boxAPool(t)
			if err := apportion(p, ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""),
				attribution(1, "H03", 10_000, 20_000, PartyCarrier)}}, boxAGone(), "fin-1", hour(30)); err != nil {
				t.Fatal(err)
			}
			p.Matched(final)
			return p
		}, boxAGone(), false},
	}
	for _, s := range states {
		p := s.pool(t)
		var rest []string
		for _, it := range p.PendingItems(s.basis) {
			if it.Kind != pendingExceptionCause {
				rest = append(rest, it.Kind)
			}
		}
		err := p.Close(s.basis, "fin-1", hour(60))
		if (err == nil) != s.closes || (len(rest) == 0) != (err == nil) {
			t.Errorf("%s: the close says %v and the list says %v", s.name, err, rest)
		}
	}
}

// A line in a foreign currency: an item's local amount is the functional-currency amount, not the line's; a cost that
// moves only in the functional currency (the invoice matched final at another rate) is listed too, in both currencies.
func TestPendingItemsOfAForeignCurrencyLineCarryTheirLocalAmounts(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", usd(300_001), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	_, la := latest(t, p, 1)
	it := onlyKind(t, "former share", pendingOf(p, boxAAfter()), pendingFormerShare)
	if it.Currency != "USD" || it.Amount != la.Shares[2].Amount || it.Local != la.Shares[2].Local || it.Local == it.Amount {
		t.Fatalf("the item: %+v, the share %+v", it, la.Shares[2])
	}
	mustSay(t, "former share", it.Message, "a former share of USD "+strings.TrimPrefix(money("USD", la.Shares[2].Amount), "USD "))
	q := pool(t, boxA())
	recordCost(t, q, "OCEAN_FREIGHT", usd(300_001), MethodChargeable)
	if err := apportion(q, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err := attributed(q, "fin-1", 30, attribution(1, "H03", 12_345, 23_457, PartyPending)); err != nil {
		t.Fatal(err)
	}
	ex := onlyKind(t, "exception cost", pendingOf(q, boxAAfter()), pendingExceptionCause)
	if ex.Currency != "USD" || ex.Amount != 23_457 || ex.Local != 167_094 {
		t.Fatalf("the exception item: %+v", ex)
	}
	mustSay(t, "exception cost", ex.Message, "an exception cost of USD 234.57")
	// the invoice is matched final in USD at another rate: the same amount, another local cost
	p.Matched(map[int]Consumption{1: {Amount: 300_001, Local: 2_100_000, Final: true}})
	it = onlyKind(t, "cost moved in the functional currency", p.PendingItems(boxA()), pendingLineNotApportioned)
	mustSay(t, "cost moved", it.Message, "cost line 1 was apportioned at USD 3,000.01 (CNY 21,370.27 local), and its final cost is now USD 3,000.01 (CNY 21,000.00 local)")
	refused(t, "the close", p.Close(boxA(), "fin-1", hour(60)), ErrPoolNotClosable, "cost line 1 was apportioned at")
}

// While a container is in rework the way out of an item that asks for a preview says it is done once the container is
// sealed again or given up, and not at once.
func TestPendingItemsSayThatTheWayOutWaitsForTheContainer(t *testing.T) {
	working := LoadUnitFacts{UnitNo: 1, ExecutionState: ExecutionRework}
	after := boxAAfter()
	after.LoadUnits = []LoadUnitFacts{working}
	const wait = "once the container is sealed again or given up, "
	p := boxAPool(t)
	it := pendingOf(p, after)
	if len(it) != 2 || it[0].Kind != pendingInWork || it[1].Kind != pendingFormerShare {
		t.Fatalf("the items: %+v", it)
	}
	mustSay(t, "former share", it[1].Message, "to attribute it, "+wait+"preview the line again")
	if strings.Contains(it[0].Message, "preview the line again at once") {
		t.Fatalf("in rework: %q", it[0].Message)
	}
	if got := pendingOf(boxAPool(t), boxAAfter()); strings.Contains(got[0].Message, "sealed again or given up") {
		t.Fatalf("not in rework: %q", got[0].Message)
	}
	q := boxAPool(t)
	if err := attributed(q, "fin-1", 32, attribution(1, "H03", 10_000, 20_000, PartyPending)); err != nil {
		t.Fatal(err)
	}
	it = pendingOf(q, after)
	mustSay(t, "exception cost", it[1].Message, "once it is, "+wait+"preview the line again stating the party")
	gone := boxAGone()
	gone.LoadUnits = []LoadUnitFacts{working}
	r := boxAPool(t)
	if err := apportion(r, ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H01", 0, 0, ""), attribution(1, "H02", 0, 0, ""),
		attribution(1, "H03", 10_000, 20_000, PartyCarrier)}}, boxAGone(), "fin-1", hour(30)); err != nil {
		t.Fatal(err)
	}
	it = pendingOf(r, gone)
	mustSay(t, "to be allocated", it[1].Message, "no house of the consolidation is live: "+wait+"add cargo")
}

// The close's rules that the list now repeats: a retained line while PHY reports houses, a line that costs nothing and
// a cancelled consolidation need no apportionment on the current basis.
func TestPendingItemsReadTheRetainedAndTheZeroLinesAsTheCloseDoes(t *testing.T) {
	cancelled := boxAGone()
	cancelled.State = "CANCELLED"
	p := boxAPool(t)
	in := ApportionInput{Method: MethodRetained, Reason: "the consolidation was cancelled", Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H01", 0, 0, ""),
		attribution(1, "H02", 0, 0, ""), attribution(1, "H03", 10_000, 20_000, PartyOwnOperations)}}
	if err := apportion(p, in, cancelled, "fin-1", hour(30)); err != nil {
		t.Fatal(err)
	}
	if got := pendingOf(p, cancelled); len(got) != 0 {
		t.Fatalf("retained, the consolidation cancelled: %+v", got)
	}
	it := onlyKind(t, "retained while houses are reported", pendingOf(p, boxA()), pendingLineNotOnBasis)
	mustSay(t, "retained", it.Message, "cost line 1 is retained on the consolidation while PHY reports houses on it")
	refused(t, "the close", p.Close(boxA(), "fin-1", hour(60)), ErrPoolNotClosable, "retained on the consolidation while PHY reports houses on it")
	// a line that costs nothing is on no basis
	z := pool(t, boxA())
	recordCost(t, z, "OCEAN_FREIGHT", cny(0), MethodChargeable)
	if err := apportion(z, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	unloaded := boxA()
	unloaded.Houses = slices.Clone(unloaded.Houses)
	unloaded.Houses[2].LoadedPacks = 0
	if got := pendingOf(z, unloaded); len(got) != 0 {
		t.Fatalf("a line that costs nothing: %+v", got)
	}
	// a cancelled consolidation has no basis to compare
	c := boxAPool(t)
	gone := unloaded
	gone.State = "CANCELLED"
	if got := pendingOf(c, gone); len(got) != 0 {
		t.Fatalf("a cancelled consolidation: %+v", got)
	}
}

// The close's check of an amount to be allocated reads both currencies. The database's conservation rule forbids a line
// with an amount to be allocated in the functional currency only, so the state is restored with one written by hand.
func TestAnAmountToBeAllocatedInTheFunctionalCurrencyOnlyIsListedAndRefusedByTheClose(t *testing.T) {
	st := boxAPool(t).State()
	st.Apportionments[0].Lines[0].ToAllocateLocal = 5
	p, err := RestorePool(st)
	if err != nil {
		t.Fatal(err)
	}
	it := onlyKind(t, "to allocate in the functional currency", pendingOf(p, boxA()), pendingToBeAllocated)
	if it.LineNo != 1 || it.Amount != 0 || it.Local != 5 {
		t.Fatalf("the item: %+v", it)
	}
	refused(t, "the close", p.Close(boxA(), "fin-1", hour(60)), ErrPoolNotClosable, "cost line 1 has 0 to be allocated")
	st = boxAPool(t).State()
	st.Apportionments[0].Lines[0].ToAllocate = 5
	if p, err = RestorePool(st); err != nil {
		t.Fatal(err)
	}
	it = onlyKind(t, "to allocate in the line's currency only", pendingOf(p, boxA()), pendingToBeAllocated)
	if it.Amount != 5 || it.Local != 0 {
		t.Fatalf("the item: %+v", it)
	}
	refused(t, "the close", p.Close(boxA(), "fin-1", hour(60)), ErrPoolNotClosable, "cost line 1 has 5 to be allocated")
}

// The accrual kinds come after the line kinds, the cost not final before the invoice held, each by line.
func TestPendingItemsListTheAccrualKindsAfterTheLineKinds(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	recordCost(t, p, "THC", cny(90_000), MethodChargeable)
	recordCost(t, p, "DOC", cny(30_000), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs(), LineNos: []int{1, 2}}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true, Held: true}, 2: {Amount: 10_000, Local: 10_000}})
	unloaded := boxA()
	unloaded.Houses = slices.Clone(unloaded.Houses)
	unloaded.Houses[2].LoadedPacks = 0
	type row struct {
		kind string
		line int
	}
	var rows []row
	for _, it := range p.PendingItems(unloaded) {
		rows = append(rows, row{it.Kind, it.LineNo})
	}
	want := []row{{pendingLineNotApportioned, 3}, {pendingLineNotOnBasis, 1}, {pendingLineNotOnBasis, 2}, {pendingCostNotFinal, 2}, {pendingCostNotFinal, 3},
		{pendingInvoiceHeld, 1}}
	if !slices.Equal(rows, want) {
		t.Fatalf("the order: %v, want %v", rows, want)
	}
}

// A cost that moves in only one currency is refused by the close and listed: the amount alone, the functional amount alone.
func TestPendingItemsListACostThatMovedInOnlyOneCurrency(t *testing.T) {
	p := boxAPool(t)
	p.Matched(map[int]Consumption{1: {Amount: 310_000, Local: 300_000, Final: true}})
	onlyKind(t, "the amount alone", p.PendingItems(boxA()), pendingLineNotApportioned)
	refused(t, "the close", p.Close(boxA(), "fin-1", hour(60)), ErrPoolNotClosable, "cost line 1 was apportioned at 300000 (300000 local); its final cost is 310000 (300000 local)")
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 310_000, Final: true}})
	onlyKind(t, "the functional amount alone", p.PendingItems(boxA()), pendingLineNotApportioned)
}
