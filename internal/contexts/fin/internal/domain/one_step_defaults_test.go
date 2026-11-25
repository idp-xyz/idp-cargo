package domain

import (
	"errors"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.98: the rules read F4, F8 and F9. The owner's box A: the freight line is CNY 3,000.00 over H01, H02 and H03
// (10, 15 and 5 W/M), H03 leaves, and its former share is CNY 500.00. The rules are RuleSettings given to the domain, as
// the application reads them.

// F8 DEFAULT_TO_REMAINING goes only with F3 ONLY_WHEN_AMOUNT_MOVES (the catalogue's rule across items).
var f8Default = RuleSettings{Version: 2, F3: F3OnlyWhenAmountMoves, F8: F8DefaultToRemaining}

// conservedLine checks that the parts of a line add up to its cost, in its currency and in the functional currency.
func conservedLine(t *testing.T, what string, la LineApportionment) {
	t.Helper()
	amount, local := la.ToAllocate, la.ToAllocateLocal
	for _, s := range la.Shares {
		if s.Used {
			amount, local = amount+s.Amount, local+s.Local
		}
	}
	for _, x := range la.Attributions {
		amount, local = amount+x.Loss+x.Exception, local+x.LossLocal+x.ExceptionLocal
	}
	if amount != la.Amount || local != la.Local {
		t.Fatalf("%s: the parts add up to %d (%d local), the line is %d (%d local): %+v", what, amount, local, la.Amount, la.Local, la)
	}
}

func reviseTo(t *testing.T, p *ConsolCostPool, amount kernel.Money, h int) {
	t.Helper()
	versions := len(p.State().Lines[0].Versions)
	if err := p.ReviseCost(1, versions, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: amount}, "the carrier re-invoiced", policy(t), "fin-1", hour(h)); err != nil {
		t.Fatal(err)
	}
}

// stated is box A after H03 left, with its attribution stated and submitted at the line's cost of CNY 3,000.00, in the
// currency the cost is given in (the freight is a line of its own; the cost is recorded in cost).
func stated(t *testing.T, cost kernel.Money, loss, exception int64) *ConsolCostPool {
	t.Helper()
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cost, MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	party := ""
	if exception > 0 {
		party = PartyWarehouse
	}
	if err := attributed(p, "fin-1", 30, attribution(1, "H03", loss, exception, party)); err != nil {
		t.Fatal(err)
	}
	return p
}

// F8 DEFAULT_TO_REMAINING: a former house that holds a pending former share and is not stated gets an attribution of
// nothing, made by default, with no voucher; its former share goes to the cargo that remains (CNY 1,200.00 and 1,800.00),
// and the line is conserved. Without the setting the preview is refused as before.
func TestF8DefaultsAnUnstatedFormerShareToTheCargoThatRemains(t *testing.T) {
	p := boxAPool(t)
	refused(t, "REJECT_WITH_DEFAULT", p.Preview(ApportionInput{Jobs: boxAJobs()}, boxAAfter(), "ops-2", hour(30)), ErrAttributionMissing, "line 1", "house H03")
	refused(t, "F8 with F3 asking a voucher on every attribution", p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: RuleSettings{F8: F8DefaultToRemaining}}, boxAAfter(),
		"ops-2", hour(30)), ErrAttributionMissing)
	if err := attributedUnder(f8Default, p, "ops-2", "fin-2", 31); err != nil {
		t.Fatal(err)
	}
	a, la := latest(t, p, 1)
	if a.No != 2 || a.PreviewNo != 2 || a.SettingsVersion != 2 || len(la.Attributions) != 1 {
		t.Fatalf("the apportionment: %+v", a)
	}
	x := la.Attributions[0]
	if x.ShipmentID != "H03" || !x.ByDefault || x.ByRatio || x.Carried || x.Loss != 0 || x.Exception != 0 || x.Party != "" || x.Voucher != "" || x.Reason != "" ||
		x.JobID != "JOB-H03" || !x.DepartedAt.Equal(hour(26)) {
		t.Fatalf("the default attribution: %+v", x)
	}
	for _, s := range la.Shares {
		switch s.ShipmentID {
		case "H01":
			if s.Amount != 120_000 {
				t.Fatalf("H01 bears %d", s.Amount)
			}
		case "H02":
			if s.Amount != 180_000 {
				t.Fatalf("H02 bears %d", s.Amount)
			}
		}
	}
	conservedLine(t, "the default", la)
	// the next apportionment carries it: still marked as made by default, and not a new confirmation
	if err := attributedUnder(f8Default, p, "ops-2", "ops-1", 32); err != nil {
		t.Fatalf("a carried default is no new confirmation: %v", err)
	}
	if _, la = latest(t, p, 1); !la.Attributions[0].Carried || !la.Attributions[0].ByDefault {
		t.Fatalf("carried: %+v", la.Attributions)
	}
}

// The default never replaces an attribution in force: with one stated, F8 changes nothing.
func TestF8NeverReplacesAnAttributionInForce(t *testing.T) {
	p := boxAPool(t)
	if err := attributed(p, "fin-1", 30, attribution(1, "H03", 10_000, 20_000, PartyCarrier)); err != nil {
		t.Fatal(err)
	}
	if err := attributedUnder(f8Default, p, "ops-2", "ops-1", 31); err != nil {
		t.Fatal(err)
	}
	_, la := latest(t, p, 1)
	x := la.Attributions[0]
	if x.ByDefault || !x.Carried || x.Loss != 10_000 || x.Exception != 20_000 || x.Voucher != "CLAIM-1" {
		t.Fatalf("an attribution in force: %+v", x)
	}
}

// A statement beats the default, and a statement of nothing with no voucher is not a default: it is stated.
func TestF8AStatementIsNotADefault(t *testing.T) {
	p := boxAPool(t)
	none := AttributionInput{LineNo: 1, ShipmentID: "H03"}
	if err := attributedUnder(f8Default, p, "ops-2", "fin-2", 31, none); err != nil {
		t.Fatal(err)
	}
	if _, la := latest(t, p, 1); la.Attributions[0].ByDefault || la.Attributions[0].Carried {
		t.Fatalf("stated: %+v", la.Attributions[0])
	}
}

// With no house live the whole former share of the default is to be allocated on the line, as for any attribution of
// nothing.
func TestF8WithNoHouseLiveLeavesTheFormerShareToBeAllocated(t *testing.T) {
	p := boxAPool(t)
	b := boxAAfter()
	b.Houses = nil
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: f8Default}, b, "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	la := p.State().Previews[lastPreview(p)-1].Lines[0]
	if la.ToAllocate != 300_000 || la.ToAllocateLocal != 300_000 || !la.Attributions[0].ByDefault || len(la.Shares) != 0 {
		t.Fatalf("to be allocated: %+v", la)
	}
	conservedLine(t, "to be allocated", la)
}

// The bars of F1 and F2 apply to the default as to a stated attribution, measured by the former share (CNY 500.00): the
// asker of the rework (ops-1) and whoever ended H03's place (ops-9) do not confirm it unless F1 lets them.
func TestF8TheBarsOfF1AndF2ApplyToTheDefault(t *testing.T) {
	under := func(rules RuleSettings) RuleSettings {
		rules.Version, rules.F3, rules.F8 = 2, F3OnlyWhenAmountMoves, F8DefaultToRemaining
		return rules
	}
	for _, c := range []struct {
		name                 string
		rules                RuleSettings
		previewer, submitter string
		refused              bool
		says                 string
	}{
		{"NOT_ALLOWED: the asker", under(RuleSettings{}), "ops-2", "ops-1", true, "ops-1 asked for rework 1"},
		{"NOT_ALLOWED: the ender", under(RuleSettings{}), "ops-2", "ops-9", true, "ops-9 ended the place of house H03"},
		{"NOT_ALLOWED: another person", under(RuleSettings{}), "ops-2", "fin-3", false, ""},
		{"UNRESTRICTED: the asker", under(RuleSettings{F1: F1Unrestricted}), "ops-2", "ops-1", false, ""},
		{"a threshold below the former share: the asker", under(threshold(49_999)), "ops-2", "ops-1", true, "CNY 500.00 is above the threshold of CNY 499.99"},
		{"a threshold at the former share: the asker", under(threshold(50_000)), "ops-2", "ops-1", false, ""},
		{"F2 both: the previewer who asked", under(RuleSettings{F2: F2SubmitterAndPreviewer}), "ops-1", "fin-3", true, "the previewer ops-1 asked"},
		{"F2 submitter only: the previewer who asked", under(RuleSettings{}), "ops-1", "fin-3", false, ""},
	} {
		p := boxAPool(t)
		err := attributedUnder(c.rules, p, c.previewer, c.submitter, 31)
		if !c.refused {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if !errors.Is(err, ErrAttributionConfirmer) || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), "house H03 on line 1") {
			t.Errorf("%s: %v", c.name, err)
		}
		if len(p.State().Apportionments) != 1 {
			t.Errorf("%s: a refused submission was written", c.name)
		}
	}
}

// A preview made under F8 is stale under another version of the settings, and a preview made under no F8 is stale when
// F8 is set: the fingerprint holds the version.
func TestAPreviewOfTheDefaultIsStaleWhenTheSettingsChange(t *testing.T) {
	p := boxAPool(t)
	b := boxAAfter()
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: f8Default}, b, "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	later := f8Default
	later.Version = 3
	refused(t, "another version", p.Submit(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs(), Settings: later}, "fin-2", hour(31)), ErrPreviewStale, "the settings changed from version 2 to 3")
	if err := p.Submit(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs(), Settings: f8Default}, "fin-2", hour(31)); err != nil {
		t.Fatal(err)
	}
}

// The pending item says what the default will do, only under F8 DEFAULT_TO_REMAINING; the preview's effect says that the
// attribution is a default and what it does, and the house's profit does not move.
func TestF8ThePendingItemAndThePreviewEffectSayWhatTheDefaultDoes(t *testing.T) {
	p := boxAPool(t)
	formerShare := func(items []PendingItem) (out []PendingItem) {
		for _, it := range items {
			if it.Kind == "FORMER_SHARE_PENDING" {
				out = append(out, it)
			}
		}
		return out
	}
	plain := formerShare(p.PendingItemsUnder(RuleSettings{}, boxAAfter()))
	under := formerShare(p.PendingItemsUnder(f8Default, boxAAfter()))
	if len(plain) != 1 || len(under) != 1 || strings.Contains(plain[0].Message, "F8") {
		t.Fatalf("the items: %+v %+v", plain, under)
	}
	for _, part := range []string{"Under F8 DEFAULT_TO_REMAINING", "house H03", "loss 0 and exception cost 0", "no voucher", "cargo that remains", "bars of F1 and F2"} {
		if !strings.Contains(under[0].Message, part) {
			t.Fatalf("the item does not say %q: %s", part, under[0].Message)
		}
	}
	if !strings.HasPrefix(under[0].Message, plain[0].Message) {
		t.Fatalf("the item under F8 keeps the item as it was: %s", under[0].Message)
	}
	b := boxAAfter()
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: f8Default}, b, "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	e, err := p.PreviewEffect(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs(), Settings: f8Default})
	if err != nil || !e.Current {
		t.Fatalf("%v %+v", err, e)
	}
	var reason ReasonEffect
	for _, r := range e.Reasons {
		if r.ShipmentID == "H03" {
			reason = r
		}
	}
	if !reason.Attributed || !reason.ByDefault || reason.Carried || !strings.Contains(reason.Says, "F8 DEFAULT_TO_REMAINING") || !strings.Contains(reason.Says, "CNY 500.00") ||
		!strings.Contains(reason.Says, "cargo that remains") {
		t.Fatalf("the reason: %+v", reason)
	}
	for _, h := range e.Houses {
		if h.ShipmentID == "H03" && (h.NowKind != BearsFormerShare || h.PreviewKind != BearsNothing || h.ProfitChange != 0) {
			t.Fatalf("H03: %+v", h)
		}
	}
	if len(e.Lines) != 1 || !e.Lines[0].Conserved {
		t.Fatalf("the line: %+v", e.Lines)
	}
}

// F9 under each value, with the line raised to CNY 3,600.00 and lowered to CNY 2,400.00 after H03's attribution of loss
// CNY 200.00 and exception cost CNY 300.00 (WAREHOUSE) was stated at CNY 3,000.00.
func TestF9WhenTheCostOfALineChanges(t *testing.T) {
	for _, c := range []struct {
		name            string
		rules           RuleSettings
		cost            int64
		loss, exception int64
		carried, ratio  bool
		houses          [2]int64
		refusal         error
		mentions        []string
	}{
		{"default, raised", RuleSettings{}, 360_000, 20_000, 30_000, true, false, [2]int64{124_000, 186_000}, nil, nil},
		{"KEEP_AMOUNT, raised", RuleSettings{F9: "KEEP_AMOUNT"}, 360_000, 20_000, 30_000, true, false, [2]int64{124_000, 186_000}, nil, nil},
		{"KEEP_RATIO, raised", RuleSettings{F9: F9KeepRatio}, 360_000, 24_000, 36_000, true, true, [2]int64{120_000, 180_000}, nil, nil},
		{"KEEP_RATIO, lowered", RuleSettings{F9: F9KeepRatio}, 240_000, 16_000, 24_000, true, true, [2]int64{80_000, 120_000}, nil, nil},
		{"KEEP_AMOUNT, lowered", RuleSettings{}, 240_000, 20_000, 30_000, true, false, [2]int64{76_000, 114_000}, nil, nil},
		{"KEEP_RATIO, unchanged", RuleSettings{F9: F9KeepRatio}, 300_000, 20_000, 30_000, true, false, [2]int64{100_000, 150_000}, nil, nil},
		{"RECONFIRM, unchanged", RuleSettings{F9: F9Reconfirm}, 300_000, 20_000, 30_000, true, false, [2]int64{100_000, 150_000}, nil, nil},
		{"RECONFIRM, raised", RuleSettings{F9: F9Reconfirm}, 360_000, 0, 0, false, false, [2]int64{}, ErrAttributionReconfirm,
			[]string{"house H03", "line 1", "CNY 3,000.00", "CNY 3,600.00", "state the attribution of house H03 on line 1 again", "F9 RECONFIRM"}},
		{"RECONFIRM, lowered", RuleSettings{F9: F9Reconfirm}, 240_000, 0, 0, false, false, [2]int64{}, ErrAttributionReconfirm,
			[]string{"CNY 3,000.00", "CNY 2,400.00"}},
	} {
		p := stated(t, cny(300_000), 20_000, 30_000)
		if c.cost != 300_000 {
			reviseTo(t, p, cny(c.cost), 32)
		}
		err := attributedUnder(c.rules, p, "ops-2", "fin-1", 33)
		if c.refusal != nil {
			refused(t, c.name, err, c.refusal, c.mentions...)
			if len(p.State().Apportionments) != 2 || len(p.State().Previews) != 2 {
				t.Errorf("%s: a refused preview was stored", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		_, la := latest(t, p, 1)
		x := la.Attributions[0]
		if x.Loss != c.loss || x.Exception != c.exception || x.Carried != c.carried || x.ByRatio != c.ratio || x.ByDefault || x.Voucher != "CLAIM-1" || x.Party != PartyWarehouse ||
			la.Amount != c.cost || !sameAmounts(shareAmounts(la), c.houses[0], c.houses[1]) {
			t.Errorf("%s: %+v (shares %v)", c.name, x, shareAmounts(la))
		}
		conservedLine(t, c.name, la)
	}
}

// A line in a foreign currency: the loss and the exception cost are scaled in the line's currency and the local amounts
// are divided by the largest remainder, so that both currencies conserve under each value.
func TestF9ConservesALineInAForeignCurrency(t *testing.T) {
	for _, rules := range []RuleSettings{{}, {F9: F9KeepRatio}} {
		p := stated(t, usd(300_001), 20_001, 30_003)
		reviseTo(t, p, usd(360_007), 32)
		if err := attributedUnder(rules, p, "ops-2", "fin-1", 33); err != nil {
			t.Fatal(err)
		}
		_, la := latest(t, p, 1)
		if la.Amount == la.Local {
			t.Fatalf("the line is not foreign: %+v", la)
		}
		conservedLine(t, rules.F9, la)
		x := la.Attributions[0]
		if rules.F9 == F9KeepRatio && (x.Loss != scaled(20_001, 300_001, 360_007) || x.Exception != scaled(30_003, 300_001, 360_007) || !x.ByRatio) {
			t.Fatalf("the ratio: %+v", x)
		}
		if rules.F9 == "" && (x.Loss != 20_001 || x.Exception != 30_003 || x.ByRatio) {
			t.Fatalf("the amounts: %+v", x)
		}
	}
}

// The ratio rounds to the nearest minor unit and a half goes away from zero.
func TestF9RoundsAtAHalfAwayFromZero(t *testing.T) {
	for _, c := range []struct{ amount, then, now, want int64 }{
		{1, 300_000, 150_000, 1}, // 0.5
		{3, 300_000, 150_000, 2}, // 1.5
		{1, 300_000, 149_999, 0}, // just below a half
		{1, 300_000, 150_001, 1}, // just above
		{5, 300_000, 330_000, 6}, // 5.5
		{5, 300_000, 360_000, 6}, // 6
		{2, 3, 1, 1},             // 0.666
		{0, 300_000, 360_000, 0}, // nothing
		{20_000, 300_000, 450_000, 30_000},
		{1_000_000_000_000_000, 1_000_000_000_000_000, 1_000_000_000_000_000, 1_000_000_000_000_000},
	} {
		if got := scaled(c.amount, c.then, c.now); got != c.want {
			t.Errorf("%d x %d / %d = %d, want %d", c.amount, c.now, c.then, got, c.want)
		}
	}
	// through the pool: the loss and the exception cost are rounded each on its own
	p := stated(t, cny(300_000), 1, 3)
	reviseTo(t, p, cny(150_000), 32)
	if err := attributedUnder(RuleSettings{F9: F9KeepRatio}, p, "ops-2", "fin-1", 33); err != nil {
		t.Fatal(err)
	}
	_, la := latest(t, p, 1)
	if x := la.Attributions[0]; x.Loss != 1 || x.Exception != 2 || !x.ByRatio {
		t.Fatalf("a half: %+v", x)
	}
	conservedLine(t, "a half", la)
}

// Each later rescale starts again from the apportionment that stated the attribution, never from an earlier rescale: a
// loss of 5 on CNY 3,000.00 is 6 on CNY 3,300.00 (5.5), and 6 on CNY 3,600.00 (6.0, where 6 rescaled from 3,300.00 would
// give 7), and 5 again once the line is back at CNY 3,000.00.
func TestF9EachRescaleStartsFromTheStatingApportionment(t *testing.T) {
	rules := RuleSettings{F9: F9KeepRatio}
	p := stated(t, cny(300_000), 5, 0)
	h := 40
	for _, c := range []struct {
		cost, loss int64
		ratio      bool
	}{{330_000, 6, true}, {360_000, 6, true}, {300_000, 5, false}, {330_000, 6, true}} {
		h += 2
		reviseTo(t, p, cny(c.cost), h)
		if err := attributedUnder(rules, p, "ops-2", "fin-1", h+1); err != nil {
			t.Fatal(err)
		}
		_, la := latest(t, p, 1)
		if x := la.Attributions[0]; x.Loss != c.loss || x.ByRatio != c.ratio || !x.Carried {
			t.Fatalf("at %d: %+v", c.cost, x)
		}
		conservedLine(t, "a rescale", la)
	}
	// a statement again makes a new stating apportionment, and the next rescale starts from it
	again := attribution(1, "H03", 7, 0, "")
	again.Reason = "the carrier changed its claim"
	reviseTo(t, p, cny(400_000), 60)
	if err := attributedUnder(rules, p, "ops-2", "fin-1", 61, again); err != nil {
		t.Fatal(err)
	}
	reviseTo(t, p, cny(200_000), 62)
	if err := attributedUnder(rules, p, "ops-2", "fin-1", 63); err != nil {
		t.Fatal(err)
	}
	if _, la := latest(t, p, 1); la.Attributions[0].Loss != 4 { // 7 x 200,000 / 400,000 = 3.5
		t.Fatalf("from the new statement: %+v", la.Attributions[0])
	}
}

// KEEP_RATIO needs the line's cost above 0 then and now: with no cost now (or none then) the preview is refused, with the
// way out of stating the attribution again.
func TestF9KeepRatioNeedsACostAboveZero(t *testing.T) {
	p := stated(t, cny(300_000), 20_000, 30_000)
	reviseTo(t, p, cny(0), 32)
	err := p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: RuleSettings{F9: F9KeepRatio}}, boxAAfter(), "ops-2", hour(33))
	refused(t, "no cost now", err, ErrAttributionReconfirm, "house H03", "line 1", "CNY 3,000.00", "CNY 0.00", "state the attribution of house H03 on line 1 again")
	if len(p.State().Previews) != 2 {
		t.Fatal("a refused preview was stored")
	}
	// under KEEP_AMOUNT the carried amounts no longer fit, which is its own refusal
	refused(t, "KEEP_AMOUNT", p.Preview(ApportionInput{Jobs: boxAJobs()}, boxAAfter(), "ops-2", hour(33)), ErrInvalid, "state them again")
	// no cost then cannot be reached through a command (an attribution answers a former share, which is above 0); the
	// rule is its own guard
	held := Attribution{ShipmentID: "H03", Loss: 1}
	_, err = p.carry(RuleSettings{F9: F9KeepRatio}, 1, "CNY", held, stating{amount: 0, local: 0, row: held}, true, 100, 100)
	refused(t, "no cost then", err, ErrAttributionReconfirm, "house H03", "CNY 0.00", "CNY 1.00")
	// the cost back up, the attribution is carried by ratio from where it was stated
	reviseTo(t, p, cny(600_000), 34)
	if err = attributedUnder(RuleSettings{F9: F9KeepRatio}, p, "ops-2", "fin-1", 35); err != nil {
		t.Fatal(err)
	}
	if _, la := latest(t, p, 1); la.Attributions[0].Loss != 40_000 || la.Attributions[0].Exception != 60_000 {
		t.Fatalf("%+v", la.Attributions[0])
	}
}

// F9 RECONFIRM: an equal restatement of a changed line is recorded as stated, not carried, with no reason (it is not a
// correction), and the next change is measured from it; a different statement is a correction and needs its reason; and
// a restatement is a confirmation, so the bars of F1 and F2 apply.
func TestF9ReconfirmTheEqualRestatementIsRecordedAsStated(t *testing.T) {
	rules := RuleSettings{F9: F9Reconfirm}
	p := stated(t, cny(300_000), 20_000, 30_000)
	reviseTo(t, p, cny(360_000), 32)
	same := attribution(1, "H03", 20_000, 30_000, PartyWarehouse)
	// a restatement with another voucher is a correction
	other := same
	other.Voucher = "CLAIM-2"
	refused(t, "another voucher", p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{other}, Settings: rules}, boxAAfter(), "ops-2", hour(33)), ErrInvalid,
		"replaces a different one in force", "reason")
	// the equal restatement: the asker of the rework is barred, another person is not
	b := boxAAfter()
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{same}, Settings: rules}, b, "ops-2", hour(33)); err != nil {
		t.Fatal(err)
	}
	refused(t, "the asker", p.Submit(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs(), Settings: rules}, "ops-1", hour(34)), ErrAttributionConfirmer, "ops-1", "house H03", "line 1")
	if err := p.Submit(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs(), Settings: rules}, "fin-2", hour(34)); err != nil {
		t.Fatal(err)
	}
	a, la := latest(t, p, 1)
	x := la.Attributions[0]
	if a.No != 3 || x.Carried || x.ByRatio || x.ByDefault || x.Reason != "" || x.Loss != 20_000 || x.Exception != 30_000 || la.Amount != 360_000 ||
		!sameAmounts(shareAmounts(la), 124_000, 186_000) {
		t.Fatalf("restated: %+v", la)
	}
	conservedLine(t, "restated", la)
	// the next change is measured from the restatement: the same line carries as it is, a new cost asks again
	if err := attributedUnder(rules, p, "ops-2", "fin-1", 36); err != nil {
		t.Fatalf("the line is as it was stated: %v", err)
	}
	if _, la = latest(t, p, 1); !la.Attributions[0].Carried {
		t.Fatalf("carried: %+v", la.Attributions[0])
	}
	reviseTo(t, p, cny(400_000), 37)
	refused(t, "another change", p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: rules}, boxAAfter(), "ops-2", hour(38)), ErrAttributionReconfirm, "CNY 3,600.00", "CNY 4,000.00")
	// under F1 UNRESTRICTED the asker confirms it too
	free := RuleSettings{F9: F9Reconfirm, F1: F1Unrestricted}
	if err := attributedUnder(free, p, "ops-2", "ops-1", 39, same); err != nil {
		t.Fatalf("F1 UNRESTRICTED: %v", err)
	}
}

// An equal statement outside RECONFIRM, or on a line whose cost has not changed, is the attribution in force carried, as
// before; under KEEP_RATIO on a changed line it is carried by ratio.
func TestAnEqualStatementOutsideReconfirmIsCarried(t *testing.T) {
	same := attribution(1, "H03", 20_000, 30_000, PartyWarehouse)
	for _, c := range []struct {
		name  string
		rules RuleSettings
		cost  int64
		loss  int64
		ratio bool
	}{
		{"default, unchanged", RuleSettings{}, 300_000, 20_000, false},
		{"default, raised", RuleSettings{}, 360_000, 20_000, false},
		{"KEEP_RATIO, raised", RuleSettings{F9: F9KeepRatio}, 360_000, 24_000, true},
		{"RECONFIRM, unchanged", RuleSettings{F9: F9Reconfirm}, 300_000, 20_000, false},
	} {
		p := stated(t, cny(300_000), 20_000, 30_000)
		if c.cost != 300_000 {
			reviseTo(t, p, cny(c.cost), 32)
		}
		if err := attributedUnder(c.rules, p, "ops-2", "ops-1", 33, same); err != nil { // the asker: nothing is confirmed
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if _, la := latest(t, p, 1); !la.Attributions[0].Carried || la.Attributions[0].Loss != c.loss || la.Attributions[0].ByRatio != c.ratio {
			t.Errorf("%s: %+v", c.name, la.Attributions[0])
		}
	}
}

// F9 reads the line's local amount as well as its amount: a line whose cost in its currency is as it was but whose local
// cost is not has changed (the local cost is set on the pool directly: no command changes it with the amount).
func TestF9ALocalChangeIsAChange(t *testing.T) {
	for _, c := range []struct {
		rules   RuleSettings
		refused bool
		ratio   bool
	}{{RuleSettings{F9: F9Reconfirm}, true, false}, {RuleSettings{F9: F9KeepRatio}, false, true}, {RuleSettings{}, false, false}} {
		p := stated(t, usd(300_000), 20_000, 30_000)
		versions := p.state.Lines[0].Versions
		versions[len(versions)-1].Local += 1_000
		err := attributedUnder(c.rules, p, "ops-2", "fin-1", 33)
		if c.refused {
			refused(t, "a local change", err, ErrAttributionReconfirm, "(CNY 21,370.20 local)")
			// and the other way: the amount in its currency changes and the local amount does not
			q := stated(t, usd(300_000), 20_000, 30_000)
			versions = q.state.Lines[0].Versions
			versions[len(versions)-1].Amount.Minor += 1_000
			refused(t, "an amount change", attributedUnder(c.rules, q, "ops-2", "fin-1", 33), ErrAttributionReconfirm, "USD 3,010.00")
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		_, la := latest(t, p, 1)
		if x := la.Attributions[0]; x.ByRatio != c.ratio || x.Loss != 20_000 || x.Exception != 30_000 || !x.Carried {
			t.Fatalf("%+v: %+v", c.rules, x)
		}
		conservedLine(t, "a local change", la)
	}
}

// The apportionment that stated an attribution is found through the ones that carried it, and a broken chain reads the
// apportionment in force as the one that stated it.
func TestF9TheStatingApportionmentIsFoundThroughTheCarriedOnes(t *testing.T) {
	p := stated(t, usd(300_000), 20_000, 30_000)
	_, then := latest(t, p, 1)
	st := p.statingOf(1, "H03", then, then.Attributions[0])
	if st.amount != 300_000 || st.local != then.Local || st.row.Carried {
		t.Fatalf("stating: %+v", st)
	}
	// the stating apportionment is read through the carried ones
	if err := attributedUnder(RuleSettings{}, p, "ops-2", "fin-1", 31); err != nil {
		t.Fatal(err)
	}
	if err := attributedUnder(RuleSettings{}, p, "ops-2", "fin-1", 32); err != nil {
		t.Fatal(err)
	}
	_, now := latest(t, p, 1)
	if st = p.statingOf(1, "H03", now, now.Attributions[0]); st.amount != 300_000 || st.row.Carried || st.row.Loss != 20_000 {
		t.Fatalf("stating through carried rows: %+v", st)
	}
	// a broken chain reads the apportionment in force as the one that stated it
	p.state.Apportionments[1].Lines[0].Attributions = nil
	if st = p.statingOf(1, "H03", now, now.Attributions[0]); st.amount != now.Amount || !st.row.Carried {
		t.Fatalf("a broken chain: %+v", st)
	}
}

// The one-step apportionment (F4): a preview and a submission in one command, with no preview stored.
var f4OneStep = RuleSettings{Version: 4, F4: F4OneStep}

func oneStep(p *ConsolCostPool, rules RuleSettings, basis CostBasis, by string, h int, attributions ...AttributionInput) error {
	return p.ApportionInOneStep(ApportionInput{Jobs: boxAJobs(), Attributions: attributions, Settings: rules}, basis, by, hour(h))
}

func TestF4OneStepWritesTheApportionmentOfAPreviewAndNoPreview(t *testing.T) {
	twin := pool(t, boxA())
	recordCost(t, twin, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err := apportion(twin, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	p.Acknowledge()
	if err := oneStep(p, f4OneStep, boxA(), "fin-1", 4); err != nil {
		t.Fatal(err)
	}
	st := p.State()
	if len(st.Previews) != 0 || len(st.Apportionments) != 1 || len(p.AddedPreviews()) != 0 {
		t.Fatalf("a preview was stored: %+v", st)
	}
	a := st.Apportionments[0]
	if a.No != 1 || a.PreviewNo != 0 || a.SettingsVersion != 4 || a.By != "fin-1" || !a.At.Equal(hour(4)) || a.Stage != BasisActual || a.BasisRevision != 7 {
		t.Fatalf("the apportionment: %+v", a)
	}
	// the figures are those of a preview and its submission
	want := twin.State().Apportionments[0].Lines
	if len(a.Lines) != 1 || a.Lines[0].Amount != want[0].Amount || !sameAmounts(shareAmounts(a.Lines[0]), shareAmounts(want[0])...) || a.Lines[0].Shares[0].JobID != "JOB-H01" {
		t.Fatalf("the figures: %+v, a preview gives %+v", a.Lines, want)
	}
	conservedLine(t, "one step", a.Lines[0])
	events := p.Pending()
	if last := events[len(events)-1]; last.Type != "ConsolCostsApportioned.v1" || last.Version != 1 || last.SubjectID != "POOL-1" || last.LegalEntityID != "LE" {
		t.Fatalf("the event: %+v", events)
	}
	// the next apportionment is numbered after it, from a preview
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if got := p.State().Apportionments[1]; got.No != 2 || got.PreviewNo != 1 {
		t.Fatalf("after: %+v", got)
	}
}

func TestF4OneStepIsRefusedUnlessF4AllowsIt(t *testing.T) {
	for _, rules := range []RuleSettings{{}, {F4: "ALL_PREVIEWED"}, {Version: 3, F9: F9KeepRatio, F8: F8DefaultToRemaining, F3: F3OnlyWhenAmountMoves}} {
		p := boxAPool(t)
		err := oneStep(p, rules, boxA(), "fin-1", 30)
		refused(t, "F4 "+rules.F4, err, ErrOneStepRefused, "F4", "preview the lines and submit the preview", "ask a configurer")
		if len(p.State().Apportionments) != 1 {
			t.Fatal("a refused command was written")
		}
	}
	// a one-step command states no attribution
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	refused(t, "an attribution", oneStep(p, f4OneStep, boxA(), "fin-1", 4, attribution(1, "H03", 1, 0, "")), ErrInvalid, "states no attribution")
}

// Each condition that refuses a one-step apportionment, on its own, with its way out; and the same pool accepted once the
// condition is lifted.
func TestF4OneStepRefusalsEachOnItsOwn(t *testing.T) {
	// a former share pending on a line
	p := boxAPool(t)
	err := oneStep(p, f4OneStep, boxAAfter(), "fin-1", 30)
	refused(t, "a former share", err, ErrOneStepRefused, "former share pending", "house H03 on line 1", "preview the lines and submit the preview", "stating who bears it")
	if len(p.State().Apportionments) != 1 {
		t.Fatal("a refused command was written")
	}
	if err = attributed(p, "fin-1", 31, attribution(1, "H03", 10_000, 0, "")); err != nil {
		t.Fatal(err)
	}
	if err = oneStep(p, f4OneStep, boxAAfter(), "fin-1", 32); err != nil {
		t.Fatalf("once H03 is attributed: %v", err)
	}
	if a := p.State().Apportionments; len(a) != 3 || a[2].PreviewNo != 0 || !a[2].Lines[0].Attributions[0].Carried || a[2].Lines[0].Attributions[0].Loss != 10_000 {
		t.Fatalf("it carries the attribution in force: %+v", a)
	}
	conservedLine(t, "carried", p.State().Apportionments[2].Lines[0])

	// a container in rework
	w := previewed(t, inRework(sealedBasis()), MethodChargeable)
	refused(t, "in rework", w.ApportionInOneStep(ApportionInput{Jobs: jobs(), Settings: f4OneStep}, inRework(sealedBasis()), "fin-1", hour(5)), ErrPoolInWork, "load unit 1")

	// a line not apportioned since a rework ended: line 1 was apportioned before the rework, line 2 is new
	b := sealedBasis()
	q := previewed(t, b, MethodChargeable, MethodChargeable)
	if err = q.Preview(ApportionInput{Jobs: jobs(), LineNos: []int{1}}, b, "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err = q.Submit(1, SubmitInput{Basis: b, Jobs: jobs()}, "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	ended := reworked("ops-asker", ReworkSealed)
	one := func(by string, h int, lines ...int) error {
		return q.ApportionInOneStep(ApportionInput{Jobs: jobs(), LineNos: lines, Settings: f4OneStep}, ended, by, hour(h))
	}
	refused(t, "a rework not carried", one("fin-2", 8, 2), ErrOneStepRefused, "rework 1 of load unit 1 (stuffing X-1)", "for line 1", "preview the lines and submit the preview")
	if strings.Contains(one("fin-2", 8, 2).Error(), "line 2") {
		t.Fatal("a line not apportioned yet is not named")
	}
	// line 1 previewed and submitted by another person carries the rework for it
	if err = q.Preview(ApportionInput{Jobs: jobs(), LineNos: []int{1}}, ended, "fin-2", hour(9)); err != nil {
		t.Fatal(err)
	}
	if err = q.Submit(2, SubmitInput{Basis: ended, Jobs: jobs()}, "fin-2", hour(9)); err != nil {
		t.Fatal(err)
	}
	// the asker does not make the first apportionment of line 2 after the rework, whatever the way
	refused(t, "the asker", one("ops-asker", 10, 2), ErrConfirmer, "ops-asker asked for rework 1", "for line 2")
	if err = one("fin-2", 10, 2); err != nil {
		t.Fatalf("line 2, once the rework is carried for line 1: %v", err)
	}
	if a := q.State().Apportionments; len(a) != 3 || a[2].PreviewNo != 0 || a[2].Lines[0].LineNo != 2 {
		t.Fatalf("line 2: %+v", a)
	}
	// under F1 UNRESTRICTED the asker is not barred
	free := f4OneStep
	free.F1 = F1Unrestricted
	r := previewed(t, b, MethodChargeable)
	if err = r.ApportionInOneStep(ApportionInput{Jobs: jobs(), Settings: free}, ended, "ops-asker", hour(10)); err != nil {
		t.Fatalf("F1 UNRESTRICTED: %v", err)
	}
}

// A carried attribution that F9 asks to be stated again, or cannot scale, or that no longer fits, refuses the one-step
// apportionment of its line, with the way out of previewing it; with no change it carries (by ratio under KEEP_RATIO).
func TestF4OneStepCarriesTheAttributionsUnderF9(t *testing.T) {
	under := func(f9 string) RuleSettings { return RuleSettings{Version: 5, F4: F4OneStep, F9: f9} }
	for _, c := range []struct {
		name    string
		f9      string
		cost    int64
		refusal error
		says    string
		loss    int64
		ratio   bool
	}{
		{"RECONFIRM, raised", F9Reconfirm, 360_000, ErrAttributionReconfirm, "F9 RECONFIRM", 0, false},
		{"RECONFIRM, unchanged", F9Reconfirm, 300_000, nil, "", 20_000, false},
		{"KEEP_AMOUNT, raised", "", 360_000, nil, "", 20_000, false},
		{"KEEP_AMOUNT, no longer fits", "", 25_000, ErrInvalid, "state them again", 0, false},
		{"KEEP_RATIO, raised", F9KeepRatio, 360_000, nil, "", 24_000, true},
		{"KEEP_RATIO, no cost now", F9KeepRatio, 0, ErrAttributionReconfirm, "F9 KEEP_RATIO", 0, false},
	} {
		p := stated(t, cny(300_000), 20_000, 30_000)
		if c.cost != 300_000 {
			reviseTo(t, p, cny(c.cost), 32)
		}
		err := oneStep(p, under(c.f9), boxAAfter(), "fin-2", 33)
		if c.refusal != nil {
			refused(t, c.name, err, ErrOneStepRefused, "states none", "preview the lines and submit the preview", "stating the attribution again")
			refused(t, c.name, err, c.refusal, c.says)
			if len(p.State().Apportionments) != 2 || len(p.State().Previews) != 2 {
				t.Errorf("%s: a refused command was written", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		a := p.State().Apportionments[2]
		x := a.Lines[0].Attributions[0]
		if a.PreviewNo != 0 || a.SettingsVersion != 5 || !x.Carried || x.ByRatio != c.ratio || x.Loss != c.loss || a.Lines[0].Amount != c.cost {
			t.Errorf("%s: %+v", c.name, a)
		}
		conservedLine(t, c.name, a.Lines[0])
	}
}

// The checks of a submission that apply to a carried loss: its job is open (the application locks it).
func TestF4OneStepNeedsTheJobOfACarriedLoss(t *testing.T) {
	p := stated(t, cny(300_000), 20_000, 30_000)
	jobsNow := map[string]string{"H01": "JOB-H01", "H02": "JOB-H02"}
	err := p.ApportionInOneStep(ApportionInput{Jobs: jobsNow, Settings: f4OneStep}, boxAAfter(), "fin-2", hour(33))
	refused(t, "the job of the loss", err, ErrPoolJobMissing, "house H03", "line 1", "JOB-H03")
	if len(p.State().Apportionments) != 2 {
		t.Fatal("a refused command was written")
	}
}

// A one-step command is a change of an open pool at a time not before its last change, as any apportionment.
func TestF4OneStepKeepsTheRulesOfAnyChangeOfThePool(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	refused(t, "before the last change", oneStep(p, f4OneStep, boxA(), "fin-1", 1), ErrInvalid, "before the pool's last change")
	refused(t, "no author", oneStep(p, f4OneStep, boxA(), "", 4), ErrInvalid)
	other := boxA()
	other.ConsolidationID = "C-9"
	refused(t, "another consolidation", oneStep(p, f4OneStep, other, "fin-1", 4), ErrPoolNoBasis)
	if err := oneStep(p, f4OneStep, boxA(), "fin-1", 4); err != nil {
		t.Fatal(err)
	}
}

// F8 alone does nothing without its value: with F3 asking for no voucher on an attribution of nothing but F8 left at
// its default, an unstated former share is refused as before; and a default takes the job of the former share when the
// application locked no other.
func TestF8NeedsItsValueAndTakesTheJobOfTheFormerShare(t *testing.T) {
	p := boxAPool(t)
	refused(t, "F8 at its default", p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: RuleSettings{F3: F3OnlyWhenAmountMoves}}, boxAAfter(), "ops-2", hour(30)),
		ErrAttributionMissing)
	live := map[string]string{"H01": "JOB-H01", "H02": "JOB-H02"}
	if err := p.Preview(ApportionInput{Jobs: live, Settings: f8Default}, boxAAfter(), "ops-2", hour(30)); err != nil {
		t.Fatal(err)
	}
	if x := p.State().Previews[lastPreview(p)-1].Lines[0].Attributions[0]; x.JobID != "JOB-H03" || !x.ByDefault {
		t.Fatalf("the default: %+v", x)
	}
}

// The effect of a preview that carries by ratio says so and what it does.
func TestF9ThePreviewEffectSaysAnAttributionIsCarriedByRatio(t *testing.T) {
	p := stated(t, cny(300_000), 20_000, 30_000)
	reviseTo(t, p, cny(360_000), 32)
	rules := RuleSettings{Version: 3, F9: F9KeepRatio}
	b := boxAAfter()
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: rules}, b, "ops-2", hour(33)); err != nil {
		t.Fatal(err)
	}
	e, err := p.PreviewEffect(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs(), Settings: rules})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range e.Reasons {
		if r.ShipmentID == "H03" {
			if !r.ByRatio || r.ByDefault || !r.Carried || !strings.Contains(r.Says, "F9 KEEP_RATIO") || !strings.Contains(r.Says, "CNY 240.00") || !strings.Contains(r.Says, "CNY 360.00") {
				t.Fatalf("the reason: %+v", r)
			}
			return
		}
	}
	t.Fatalf("no reason of H03: %+v", e.Reasons)
}

// A missing link in the chain of carried attributions is not stepped over: the apportionment in force is read as the one
// that stated the attribution, not an older one that did.
func TestF9ABrokenChainIsNotSteppedOver(t *testing.T) {
	p := stated(t, cny(300_000), 20_000, 30_000)
	reviseTo(t, p, cny(360_000), 32)
	if err := attributedUnder(RuleSettings{}, p, "ops-2", "fin-1", 33); err != nil {
		t.Fatal(err)
	}
	if err := attributedUnder(RuleSettings{}, p, "ops-2", "fin-1", 34); err != nil {
		t.Fatal(err)
	}
	apportionments := p.state.Apportionments
	apportionments[2].Lines[0].Attributions = nil // the apportionment before the one in force carried it, and lost it
	_, now := latest(t, p, 1)
	if st := p.statingOf(1, "H03", now, now.Attributions[0]); st.amount != 360_000 || !st.row.Carried {
		t.Fatalf("stating: %+v", st)
	}
}

// A rescale by F9 KEEP_RATIO is the configured arithmetic applied to a confirmed attribution, and nobody chooses its
// amount, as with an amount carried under KEEP_AMOUNT: the bars of F1 and F2 do not apply to it. The person who asked for
// the rework and the one who ended the house's place submit it, in a preview and in one step, and are accepted.
func TestF9ARescaleIsConfirmedByNobodyAndTheBarredMaySubmitIt(t *testing.T) {
	rules := RuleSettings{Version: 3, F4: F4OneStep, F9: F9KeepRatio}
	for _, who := range []string{"ops-1", "ops-9"} {
		p := stated(t, cny(300_000), 20_000, 30_000)
		reviseTo(t, p, cny(360_000), 32)
		if err := attributedUnder(rules, p, "ops-2", who, 33); err != nil {
			t.Fatalf("%s submits a rescale: %v", who, err)
		}
		if _, la := latest(t, p, 1); la.Attributions[0].Loss != 24_000 || la.Attributions[0].Exception != 36_000 || !la.Attributions[0].Carried || !la.Attributions[0].ByRatio {
			t.Fatalf("%s: %+v", who, la.Attributions[0])
		}
		q := stated(t, cny(300_000), 20_000, 30_000)
		reviseTo(t, q, cny(360_000), 32)
		if err := q.ApportionInOneStep(ApportionInput{Jobs: boxAJobs(), Settings: rules}, boxAAfter(), who, hour(33)); err != nil {
			t.Fatalf("%s makes a rescale in one step: %v", who, err)
		}
		if _, la := latest(t, q, 1); la.Attributions[0].Loss != 24_000 || !la.Attributions[0].ByRatio {
			t.Fatalf("%s one step: %+v", who, la.Attributions[0])
		}
	}
	// what a person states anew is still a confirmation: the asker is barred from restating different amounts
	p := stated(t, cny(300_000), 20_000, 30_000)
	reviseTo(t, p, cny(360_000), 32)
	other := attribution(1, "H03", 25_000, 36_000, PartyWarehouse)
	other.Reason = "the warehouse claimed more"
	refused(t, "the asker states other amounts", attributedUnder(rules, p, "ops-2", "ops-1", 33, other), ErrAttributionConfirmer, "ops-1")
}

// The mark of a rescale belongs to the apportionment that rescaled: after F9 changes from KEEP_RATIO to KEEP_AMOUNT the
// row carries the amounts in force, as they are, and is not marked; the effect says which amounts are stated, rescaled
// or carried.
func TestF9TheRatioMarkBelongsToTheApportionmentThatRescaled(t *testing.T) {
	p := stated(t, cny(300_000), 20_000, 30_000)
	reviseTo(t, p, cny(360_000), 32)
	if err := attributedUnder(RuleSettings{F9: F9KeepRatio}, p, "ops-2", "fin-1", 33); err != nil {
		t.Fatal(err)
	}
	reviseTo(t, p, cny(480_000), 34)
	b := boxAAfter()
	says := func(rules RuleSettings, h int) (Attribution, string) {
		if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: rules}, b, "ops-2", hour(h)); err != nil {
			t.Fatal(err)
		}
		e, err := p.PreviewEffect(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs(), Settings: rules})
		if err != nil {
			t.Fatal(err)
		}
		x := p.State().Previews[lastPreview(p)-1].Lines[0].Attributions[0]
		for _, r := range e.Reasons {
			if r.ShipmentID == "H03" {
				if r.ByRatio != x.ByRatio {
					t.Fatalf("the effect's mark: %+v", r)
				}
				return x, r.Says
			}
		}
		t.Fatal("no reason of H03")
		return x, ""
	}
	x, said := says(RuleSettings{}, 35)
	if x.ByRatio || !x.Carried || x.Loss != 24_000 || x.Exception != 36_000 || !strings.Contains(said, "carried as it is") || !strings.Contains(said, "no ratio applies") ||
		strings.Contains(said, "rescaled") {
		t.Fatalf("carried by amount: %+v %q", x, said)
	}
	x, said = says(RuleSettings{Version: 2, F9: F9KeepRatio}, 36)
	if !x.ByRatio || x.Loss != 32_000 || !strings.Contains(said, "carried and rescaled (F9 KEEP_RATIO)") || !strings.Contains(said, "amounts stated are scaled") {
		t.Fatalf("rescaled: %+v %q", x, said)
	}
	// a statement is said to be stated
	stated := attribution(1, "H03", 5_000, 0, "")
	stated.Reason = "again"
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{stated}}, b, "ops-2", hour(37)); err != nil {
		t.Fatal(err)
	}
	e, err := p.PreviewEffect(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs()})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range e.Reasons {
		if r.ShipmentID == "H03" && !strings.HasPrefix(r.Says, "stated in this preview") {
			t.Fatalf("a statement: %q", r.Says)
		}
	}
}

// A default that is carried, with no former share pending, says it was made by default, in the second sentence.
func TestF8ACarriedDefaultSaysItWasMadeByDefault(t *testing.T) {
	p := boxAPool(t)
	if err := attributedUnder(f8Default, p, "ops-2", "fin-2", 31); err != nil {
		t.Fatal(err)
	}
	b := boxAAfter()
	if err := p.Preview(ApportionInput{Jobs: boxAJobs(), Settings: f8Default}, b, "ops-2", hour(32)); err != nil {
		t.Fatal(err)
	}
	e, err := p.PreviewEffect(lastPreview(p), SubmitInput{Basis: b, Jobs: boxAJobs(), Settings: f8Default})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range e.Reasons {
		if r.ShipmentID == "H03" {
			if !r.ByDefault || !r.Carried || r.Says != "attributed by default (F8 DEFAULT_TO_REMAINING): loss 0 and exception cost 0, no party and no voucher" {
				t.Fatalf("%+v", r)
			}
			return
		}
	}
	t.Fatal("no reason of H03")
}
