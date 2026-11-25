package domain

import (
	"errors"
	"strings"
	"testing"
)

// FIN V2.97: the rules read F1, F2, F3, F6 and F7 and every decision records the settings version. The owner's box A:
// H03's former share on the freight line is CNY 500.00, the asker of the rework is ops-1 and ops-9 ended H03's place.

// attributedUnder previews box A's attributions after H03 left, by previewer, and submits them by submitter, both under
// the rules given.
func attributedUnder(rules RuleSettings, p *ConsolCostPool, previewer, submitter string, h int, attributions ...AttributionInput) error {
	b := boxAAfter()
	in := ApportionInput{Jobs: boxAJobs(), Attributions: attributions, Settings: rules}
	if err := p.Preview(in, b, previewer, hour(h)); err != nil {
		return err
	}
	return p.Submit(lastPreview(p), SubmitInput{Basis: b, Jobs: in.Jobs, Settings: rules}, submitter, hour(h))
}

func threshold(minor int64) RuleSettings {
	return RuleSettings{F1: F1AboveThreshold, F1Threshold: minor}
}

// F1 NOT_ALLOWED, which the zero value is, bars the asker and the person who ended the house's place as V2.93 did.
func TestF1NotAllowedBarsTheAskerAndTheEnder(t *testing.T) {
	for _, who := range []string{"ops-1", "ops-9"} {
		p := boxAPool(t)
		err := attributedUnder(RuleSettings{}, p, "ops-2", who, 32, attribution(1, "H03", 10_000, 0, ""))
		refused(t, who, err, ErrAttributionConfirmer, who, "H03", "line 1", "another finance user submits")
		if strings.Contains(err.Error(), "threshold") {
			t.Fatalf("no threshold is in force: %v", err)
		}
		if len(p.State().Apportionments) != 1 {
			t.Fatal("a refused submission was written")
		}
	}
}

// F1 UNRESTRICTED lifts both rules: the asker and the ender submit H03's attribution, and the asker of the rework
// submits the first apportionment after it.
func TestF1UnrestrictedLiftsBothRules(t *testing.T) {
	free := RuleSettings{Version: 3, F1: F1Unrestricted}
	for _, who := range []string{"ops-1", "ops-9"} {
		p := boxAPool(t)
		if err := attributedUnder(free, p, "ops-2", who, 32, attribution(1, "H03", 10_000, 0, "")); err != nil {
			t.Fatalf("%s under UNRESTRICTED: %v", who, err)
		}
	}
	sealed := reworked("ops-asker", ReworkSealed)
	p := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, p, sealed, "ops-1", 8)
	refused(t, "standard", submit(p, no, sealed, "ops-asker", 8), ErrConfirmer)
	if err := p.Submit(no, SubmitInput{Basis: sealed, Jobs: jobs(), Settings: free}, "ops-asker", hour(8)); err == nil || !errors.Is(err, ErrPreviewStale) {
		t.Fatalf("a preview made under no settings is stale under version 3: %v", err)
	}
	no = previewUnder(t, p, free, sealed, "ops-1", 9)
	if err := p.Submit(no, SubmitInput{Basis: sealed, Jobs: jobs(), Settings: free}, "ops-asker", hour(9)); err != nil {
		t.Fatalf("the asker under UNRESTRICTED: %v", err)
	}
}

func previewUnder(t *testing.T, p *ConsolCostPool, rules RuleSettings, basis CostBasis, by string, h int) int {
	t.Helper()
	if err := p.Preview(ApportionInput{Jobs: jobs(), Settings: rules}, basis, by, hour(h)); err != nil {
		t.Fatal(err)
	}
	return lastPreview(p)
}

// F1 NOT_ABOVE_THRESHOLD bars on an attribution only when the former share's local amount is strictly above the
// threshold: an amount equal to it is not above it.
func TestF1ThresholdBoundaryOfTheAttribution(t *testing.T) {
	for _, c := range []struct {
		threshold int64
		barred    bool
	}{{49_999, true}, {50_000, false}, {50_001, false}, {0, true}} {
		for _, who := range []string{"ops-1", "ops-9"} {
			p := boxAPool(t)
			err := attributedUnder(threshold(c.threshold), p, "ops-2", who, 32, attribution(1, "H03", 10_000, 0, ""))
			if c.barred {
				refused(t, who, err, ErrAttributionConfirmer, who, "H03", "line 1", "CNY 500.00 is above the threshold of "+money("CNY", c.threshold))
			} else if err != nil {
				t.Fatalf("threshold %d, %s: %v", c.threshold, who, err)
			}
		}
	}
	// another person is never barred
	if err := attributedUnder(threshold(0), boxAPool(t), "ops-2", "fin-3", 32, attribution(1, "H03", 10_000, 0, "")); err != nil {
		t.Fatal(err)
	}
}

// With two attributions, each is read on its own: the refusal names the one above, and a submission whose parts are
// each not above the threshold passes although they are above it together.
func TestF1ThresholdReadsEachAttributionOnItsOwn(t *testing.T) {
	two := func(t *testing.T) *ConsolCostPool {
		p := pool(t, boxA())
		recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
		recordCost(t, p, "HANDLING", cny(30_000), MethodChargeable) // H03's share of it is CNY 50.00
		if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	both := []AttributionInput{attribution(1, "H03", 10_000, 0, ""), attribution(2, "H03", 1_000, 0, "")}
	err := attributedUnder(threshold(10_000), two(t), "ops-2", "ops-1", 32, both...)
	refused(t, "one part above", err, ErrAttributionConfirmer, "ops-1", "line 1", "CNY 500.00 is above the threshold of CNY 100.00")
	if strings.Contains(err.Error(), "line 2") {
		t.Fatalf("the part below is not named: %v", err)
	}
	// 500.00 and 50.00 are each not above 500.00, and 550.00 together is
	if err := attributedUnder(threshold(50_000), two(t), "ops-2", "ops-1", 32, both...); err != nil {
		t.Fatalf("each part on its own: %v", err)
	}
	// a threshold below both parts bars on both, and the refusal names each part on its own
	refused(t, "both parts above", attributedUnder(threshold(4_999), two(t), "ops-2", "ops-9", 32, both...), ErrAttributionConfirmer, "line 1", "line 2",
		"CNY 50.00 is above the threshold of CNY 49.99")
}

// A correction of an attribution in force is read on the former share the house held when it left.
func TestF1ThresholdReadsTheFormerShareOfACorrection(t *testing.T) {
	for _, c := range []struct {
		threshold int64
		barred    bool
	}{{49_999, true}, {50_000, false}} {
		p := boxAPool(t)
		if err := attributedUnder(RuleSettings{}, p, "ops-2", "fin-3", 32, attribution(1, "H03", 10_000, 0, "")); err != nil {
			t.Fatal(err)
		}
		fix := attribution(1, "H03", 20_000, 0, "")
		fix.Reason = "the carrier's claim was larger"
		err := attributedUnder(threshold(c.threshold), p, "ops-2", "ops-1", 40, fix)
		if c.barred {
			refused(t, "correction", err, ErrAttributionConfirmer, "ops-1", "CNY 500.00 is above the threshold")
		} else if err != nil {
			t.Fatalf("correction at %d: %v", c.threshold, err)
		}
	}
}

// F1 for the rule of V2.91: under NOT_ABOVE_THRESHOLD the asker of the rework is barred only from a line whose local
// cost is above the threshold, and the refusal names that line.
func TestF1ThresholdOfTheReworkRule(t *testing.T) {
	sealed := reworked("ops-asker", ReworkSealed)
	for _, c := range []struct {
		threshold int64
		barred    bool
	}{{99_999, true}, {100_000, false}, {100_001, false}} {
		p := previewed(t, sealedBasis(), MethodChargeable)
		rules := threshold(c.threshold)
		no := previewUnder(t, p, rules, sealed, "ops-1", 8)
		err := p.Submit(no, SubmitInput{Basis: sealed, Jobs: jobs(), Settings: rules}, "ops-asker", hour(8))
		if c.barred {
			refused(t, "above", err, ErrConfirmer, "ops-asker asked for rework 1", "for line 1 (CNY 1,000.00 is above the threshold of "+money("CNY", c.threshold))
		} else if err != nil {
			t.Fatalf("threshold %d: %v", c.threshold, err)
		}
	}
	// two lines, one above and one below: the refusal names the line above only
	p := previewed(t, sealedBasis(), MethodChargeable, MethodChargeable)
	recordCost(t, p, "SMALL", cny(5_000), MethodChargeable)
	rules := threshold(50_000)
	no := previewUnder(t, p, rules, sealed, "ops-1", 8)
	err := p.Submit(no, SubmitInput{Basis: sealed, Jobs: jobs(), Settings: rules}, "ops-asker", hour(8))
	refused(t, "lines 1 and 2 above", err, ErrConfirmer, "for line 1 (", "2 (")
	if strings.Contains(err.Error(), "3 (") || strings.Contains(err.Error(), ", 3") {
		t.Fatalf("line 3 is below the threshold: %v", err)
	}
}

// F2 SUBMITTER_AND_PREVIEWER checks the person who made the preview against the same bars, for both rules.
func TestF2ChecksThePreviewerAsTheSubmitter(t *testing.T) {
	both := RuleSettings{F2: F2SubmitterAndPreviewer}
	// V2.93's rule: the asker previews and another person submits
	p := boxAPool(t)
	if err := attributedUnder(RuleSettings{}, p, "ops-1", "fin-3", 32, attribution(1, "H03", 10_000, 0, "")); err != nil {
		t.Fatalf("under SUBMITTER_ONLY the previewer is not checked: %v", err)
	}
	for _, previewer := range []string{"ops-1", "ops-9"} {
		p = boxAPool(t)
		err := attributedUnder(both, p, previewer, "fin-3", 32, attribution(1, "H03", 10_000, 0, ""))
		refused(t, previewer, err, ErrAttributionConfirmer, "the previewer "+previewer, "H03", "preview again as another finance user")
		if len(p.State().Apportionments) != 1 {
			t.Fatal("a refused submission was written")
		}
	}
	// the previewer is the submitter: refused once, as the submitter
	err := attributedUnder(both, boxAPool(t), "ops-1", "ops-1", 32, attribution(1, "H03", 10_000, 0, ""))
	refused(t, "the same person", err, ErrAttributionConfirmer, "ops-1 asked for rework")
	if strings.Contains(err.Error(), "the previewer") {
		t.Fatalf("one person is named once: %v", err)
	}
	// neither is barred
	if err = attributedUnder(both, boxAPool(t), "ops-2", "fin-3", 32, attribution(1, "H03", 10_000, 0, "")); err != nil {
		t.Fatal(err)
	}
	// with the threshold, the previewer is barred only above it; with UNRESTRICTED not at all
	both.F1, both.F1Threshold = F1AboveThreshold, 50_000
	if err = attributedUnder(both, boxAPool(t), "ops-1", "fin-3", 32, attribution(1, "H03", 10_000, 0, "")); err != nil {
		t.Fatalf("the former share is not above the threshold: %v", err)
	}
	both.F1Threshold = 49_999
	refused(t, "above", attributedUnder(both, boxAPool(t), "ops-1", "fin-3", 32, attribution(1, "H03", 10_000, 0, "")), ErrAttributionConfirmer, "the previewer ops-1")
	both.F1 = F1Unrestricted
	if err = attributedUnder(both, boxAPool(t), "ops-1", "fin-3", 32, attribution(1, "H03", 10_000, 0, "")); err != nil {
		t.Fatalf("UNRESTRICTED: %v", err)
	}

	// V2.91's rule: the asker of the rework previews and another person submits
	sealed := reworked("ops-asker", ReworkSealed)
	q := previewed(t, sealedBasis(), MethodChargeable)
	no := preview(t, q, sealed, "ops-asker", 8)
	if err = submit(q, no, sealed, "fin-2", 8); err != nil {
		t.Fatalf("under SUBMITTER_ONLY: %v", err)
	}
	r := previewed(t, sealedBasis(), MethodChargeable)
	rules := RuleSettings{Version: 2, F2: F2SubmitterAndPreviewer}
	no = previewUnder(t, r, rules, sealed, "ops-asker", 8)
	err = r.Submit(no, SubmitInput{Basis: sealed, Jobs: jobs(), Settings: rules}, "fin-2", hour(8))
	refused(t, "the previewer asked", err, ErrConfirmer, "the previewer ops-asker asked for rework 1 of load unit 1 (stuffing X-1)", "preview again as another finance user")
	no = previewUnder(t, r, rules, sealed, "ops-1", 9)
	if err = r.Submit(no, SubmitInput{Basis: sealed, Jobs: jobs(), Settings: rules}, "fin-2", hour(9)); err != nil {
		t.Fatalf("another previewer: %v", err)
	}
}

// F3: under EVERY_ATTRIBUTION every attribution has a voucher; under ONLY_WHEN_AMOUNT_MOVES one with loss 0 and
// exception cost 0 has none, and a correction still states its reason.
func TestF3VoucherOnlyWhenTheAmountMoves(t *testing.T) {
	none := AttributionInput{LineNo: 1, ShipmentID: "H03"}
	for _, rules := range []RuleSettings{{}, {F3: "EVERY_ATTRIBUTION"}} {
		err := attributedUnder(rules, boxAPool(t), "ops-2", "fin-3", 32, none)
		refused(t, "no voucher", err, ErrInvalid, "house H03 on line 1 names its voucher", "state a voucher")
	}
	only := RuleSettings{Version: 2, F3: F3OnlyWhenAmountMoves}
	p := boxAPool(t)
	if err := attributedUnder(only, p, "ops-2", "fin-3", 32, none); err != nil {
		t.Fatalf("an attribution that moves nothing: %v", err)
	}
	_, la := latest(t, p, 1)
	if len(la.Attributions) != 1 || la.Attributions[0].Voucher != "" || la.Attributions[0].Loss != 0 || la.Attributions[0].Exception != 0 {
		t.Fatalf("the attribution: %+v", la.Attributions)
	}
	if sameAmounts(shareAmounts(la), 120_000, 180_000) == false {
		t.Fatalf("the whole former share goes to the cargo that remains: %v", shareAmounts(la))
	}
	// an amount without a voucher is refused whatever F3 says
	loss := AttributionInput{LineNo: 1, ShipmentID: "H03", Loss: 100}
	refused(t, "a loss", attributedUnder(only, boxAPool(t), "ops-2", "fin-3", 32, loss), ErrInvalid, "names its voucher")
	exception := AttributionInput{LineNo: 1, ShipmentID: "H03", Exception: 100, Party: PartyCarrier}
	refused(t, "an exception cost", attributedUnder(only, boxAPool(t), "ops-2", "fin-3", 32, exception), ErrInvalid, "names its voucher")
	// a voucher given is checked even when none is needed
	blankish := AttributionInput{LineNo: 1, ShipmentID: "H03", Voucher: strings.Repeat("x", 501)}
	refused(t, "too long", attributedUnder(only, boxAPool(t), "ops-2", "fin-3", 32, blankish), ErrInvalid, "names its voucher", "at most 500 characters")
	if err := attributedUnder(only, boxAPool(t), "ops-2", "fin-3", 32, AttributionInput{LineNo: 1, ShipmentID: "H03", Voucher: "V-9"}); err != nil {
		t.Fatalf("a voucher with no amount: %v", err)
	}
	// a correction to nothing needs its reason and its voucher, under both values of F3
	q := boxAPool(t)
	if err := attributedUnder(only, q, "ops-2", "fin-3", 32, attribution(1, "H03", 10_000, 0, "")); err != nil {
		t.Fatal(err)
	}
	none.Voucher = "V-0"
	err := attributedUnder(only, q, "ops-2", "fin-3", 40, none)
	refused(t, "no reason", err, ErrInvalid, "a correction states its reason")
	if strings.Contains(err.Error(), "its voucher") {
		t.Fatalf("the reason is what is missing: %v", err)
	}
	none.Voucher = ""
	none.Reason = "the claim was withdrawn"
	refused(t, "a correction to nothing with no voucher", attributedUnder(only, q, "ops-2", "fin-3", 41, none), ErrInvalid, "a correction states its reason and its voucher")
	none.Voucher = "V-2"
	if err := attributedUnder(only, q, "ops-2", "fin-3", 42, none); err != nil {
		t.Fatalf("a correction to nothing: %v", err)
	}
}

// F7: a pending cause holds back the close under BLOCKS, naming the line and the way out, and not under DOES_NOT_BLOCK;
// the item says which. The list is empty exactly when the pool closes, under both values.
func TestF7ABlockingPendingCauseHoldsBackTheClose(t *testing.T) {
	blocks := RuleSettings{Version: 4, F7: F7Blocks}
	closable := func(t *testing.T, party string) *ConsolCostPool {
		p := boxAPool(t)
		if err := attributed(p, "fin-1", 32, attribution(1, "H03", 10_000, 20_000, party)); err != nil {
			t.Fatal(err)
		}
		p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
		return p
	}
	p := closable(t, PartyPending)
	items := p.PendingItemsUnder(blocks, boxAAfter())
	it := onlyKind(t, "blocks", items, pendingExceptionCause)
	mustSay(t, "blocks", it.Message, "holds back the close (F7 BLOCKS)", "preview the line again stating the party for house H03 as a correction, with its reason and a voucher")
	err := p.CloseUnder(blocks, boxAAfter(), "fin-1", hour(33))
	refused(t, "the close", err, ErrPoolNotClosable, "cost line 1", "house H03", "party is PENDING", "F7 BLOCKS", "establish the party and correct the attribution",
		"with its reason and a voucher")
	if p.State().State != PoolOpen || len(p.State().Closures) != 0 {
		t.Fatal("a refused close changed the pool")
	}
	// DOES_NOT_BLOCK, as the zero value: the item says so and the close passes
	it = onlyKind(t, "does not block", p.PendingItemsUnder(RuleSettings{F7: "DOES_NOT_BLOCK"}, boxAAfter()), pendingExceptionCause)
	mustSay(t, "does not block", it.Message, "does not hold back the close (F7 DOES_NOT_BLOCK; under F7 BLOCKS it would)")
	q := closable(t, PartyPending)
	if err = q.CloseUnder(RuleSettings{Version: 5}, boxAAfter(), "fin-1", hour(33)); err != nil {
		t.Fatalf("DOES_NOT_BLOCK: %v", err)
	}
	if c := q.State().Closures[0]; c.SettingsVersion != 5 {
		t.Fatalf("the closure records version 5: %+v", c)
	}
	// once the party is named the pool closes under BLOCKS, with its list empty, and the closure records the version
	r := closable(t, PartyWarehouse)
	if got := r.PendingItemsUnder(blocks, boxAAfter()); len(got) != 0 {
		t.Fatalf("an established party: %+v", got)
	}
	if err = r.CloseUnder(blocks, boxAAfter(), "fin-1", hour(33)); err != nil {
		t.Fatalf("the party is established: %v", err)
	}
	if c := r.State().Closures[0]; c.SettingsVersion != 4 {
		t.Fatalf("the closure records version 4: %+v", c)
	}
	// the correction that names the party lets the first pool close
	fix := attribution(1, "H03", 10_000, 20_000, PartyCarrier)
	fix.Reason, fix.Voucher = "the carrier is liable", "CLAIM-2"
	if err = attributedUnder(blocks, p, "ops-2", "fin-1", 34, fix); err != nil {
		t.Fatal(err)
	}
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	if err = p.CloseUnder(blocks, boxAAfter(), "fin-1", hour(35)); err != nil {
		t.Fatalf("after the correction: %v", err)
	}
	// Close without settings is the standard rule
	s := closable(t, PartyPending)
	if err = s.Close(boxAAfter(), "fin-1", hour(33)); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// The preview records the version it was made under, the submission records the version in force, and the fingerprint
// holds the version: a preview made under one version is stale under another, saying what changed.
func TestThePreviewsFingerprintHoldsTheSettingsVersion(t *testing.T) {
	one, two := RuleSettings{Version: 1}, RuleSettings{Version: 2}
	p := boxAPool(t)
	in := ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H03", 10_000, 0, "")}, Settings: one}
	if err := p.Preview(in, boxAAfter(), "ops-2", hour(32)); err != nil {
		t.Fatal(err)
	}
	if v := p.State().Previews[1]; v.SettingsVersion != 1 || !strings.Contains(v.Fingerprint, "settings\tversion 1\n") {
		t.Fatalf("the preview: %+v", v)
	}
	err := p.Submit(2, SubmitInput{Basis: boxAAfter(), Jobs: boxAJobs(), Settings: two}, "fin-3", hour(32))
	refused(t, "under another version", err, ErrPreviewStale, "the settings changed from version 1 to 2")
	err = p.Submit(2, SubmitInput{Basis: boxAAfter(), Jobs: boxAJobs()}, "fin-3", hour(32))
	refused(t, "under no settings", err, ErrPreviewStale, "the settings changed from version 1 to 0")
	if len(p.State().Apportionments) != 1 {
		t.Fatal("a stale submission was written")
	}
	if err = p.Submit(2, SubmitInput{Basis: boxAAfter(), Jobs: boxAJobs(), Settings: one}, "fin-3", hour(33)); err != nil {
		t.Fatalf("the same version: %v", err)
	}
	if a := p.State().Apportionments[1]; a.SettingsVersion != 1 || a.PreviewNo != 2 {
		t.Fatalf("the apportionment: %+v", a)
	}
	// with no settings the fingerprint is what V2.96 wrote, and a preview made then is current while none is set
	q := boxAPool(t)
	if err = q.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: in.Attributions}, boxAAfter(), "ops-2", hour(32)); err != nil {
		t.Fatal(err)
	}
	if v := q.State().Previews[1]; v.SettingsVersion != 0 || strings.Contains(v.Fingerprint, "settings") ||
		v.Fingerprint != q.fingerprint(boxAAfter(), boxAJobs(), nil, v.Lines) {
		t.Fatalf("the preview under no settings: %+v", v)
	}
	err = q.Submit(2, SubmitInput{Basis: boxAAfter(), Jobs: boxAJobs(), Settings: one}, "fin-3", hour(33))
	refused(t, "settings set after the preview", err, ErrPreviewStale, "the settings changed from version 0 to 1")
	// the effect of a preview says that it is not current
	e, err := q.PreviewEffect(2, SubmitInput{Basis: boxAAfter(), Jobs: boxAJobs(), Settings: two})
	if err != nil || e.Current || len(e.Changes) != 1 || e.Changes[0] != "the settings changed from version 0 to 2" {
		t.Fatalf("the effect: %+v %v", e, err)
	}
}

// What a legal entity's settings say reaches the rules only through RuleSettings: its zero value is the standard
// profile.
func TestTheZeroRuleSettingsAreTheStandardProfile(t *testing.T) {
	var r RuleSettings
	if !r.bars(0) || !r.bars(1) || r.F1 != "" || len(r.confirmers("a", "b")) != 1 {
		t.Fatalf("%+v", r)
	}
	if (RuleSettings{F1: F1Unrestricted}).bars(1 << 40) {
		t.Fatal("UNRESTRICTED bars nobody")
	}
	if got := (RuleSettings{F1: F1AboveThreshold, F1Threshold: 10}); got.bars(10) || !got.bars(11) {
		t.Fatalf("the threshold is strict: %+v", got)
	}
}

// F7 names every house whose exception cost is pending on the line, not only the first.
func TestF7NamesEveryPendingHouseOnTheLine(t *testing.T) {
	b := boxAAfter()
	b.Houses = b.Houses[:1]
	b.FormerHouses = append([]FormerHouseFacts{{ShipmentID: "H02", End: "TRANSFERRED", TransferredTo: "C-2", Reason: "WRONG_CONSOL", EndedBy: "ops-8", EndedAt: hour(26),
		EndedEnteredAt: hour(27)}}, b.FormerHouses...)
	p := boxAPool(t)
	in := ApportionInput{Jobs: boxAJobs(), Attributions: []AttributionInput{attribution(1, "H02", 0, 15_000, PartyPending), attribution(1, "H03", 0, 5_000, PartyPending)}}
	if err := p.Preview(in, b, "ops-2", hour(32)); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(lastPreview(p), SubmitInput{Basis: b, Jobs: in.Jobs}, "fin-1", hour(32)); err != nil {
		t.Fatal(err)
	}
	p.Matched(map[int]Consumption{1: {Amount: 300_000, Local: 300_000, Final: true}})
	err := p.CloseUnder(RuleSettings{F7: F7Blocks}, b, "fin-1", hour(33))
	refused(t, "two houses", err, ErrPoolNotClosable, "cost line 1", "CNY 150.00 of house H02", "CNY 50.00 of house H03", "for house H02, and for house H03")
}

// When the former share a person's bar is read on is not found, the bar applies whatever the threshold: the amount at
// risk is not known. No command reaches it, so it is shown on the function.
func TestAFormerShareNotFoundBarsThePerson(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", cny(300_000), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs(), NotUsed: []string{"H03"}}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if share, found := p.formerShareLocal(1, Attribution{ShipmentID: "H03", LossLocal: 1}); found || share != 0 {
		t.Fatalf("H03 held no used share: %d %v", share, found)
	}
	lines := []LineApportionment{{LineNo: 1, Attributions: []Attribution{{ShipmentID: "H03", Loss: 1, LossLocal: 1}}}}
	got := p.attributionsConfirmedBy(threshold(1_000_000_000), boxAAfter(), confirmer{name: "ops-1", label: "ops-1"}, lines)
	if len(got) != 1 || strings.Contains(got[0], "threshold") {
		t.Fatalf("the asker is barred and the refusal names no amount: %v", got)
	}
	if got = p.attributionsConfirmedBy(RuleSettings{F1: F1Unrestricted}, boxAAfter(), confirmer{name: "ops-1", label: "ops-1"}, lines); len(got) != 0 {
		t.Fatalf("UNRESTRICTED bars nobody: %v", got)
	}
}

// The former share is read in the functional currency: a line in USD has a former share of USD 500.00 or so, and CNY
// 3,5xx as the local amount the threshold (in CNY) is compared with.
func TestF1ThresholdReadsTheFormerShareInTheFunctionalCurrency(t *testing.T) {
	p := pool(t, boxA())
	recordCost(t, p, "OCEAN_FREIGHT", usd(300_001), MethodChargeable)
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	_, la := latest(t, p, 1)
	local := la.Shares[2].Local
	if local <= la.Shares[2].Amount {
		t.Fatalf("the local amount is more than the amount: %+v", la.Shares[2])
	}
	err := attributedUnder(threshold(local-1), p, "ops-2", "ops-1", 32, attribution(1, "H03", 10_000, 0, ""))
	refused(t, "above in CNY", err, ErrAttributionConfirmer, "ops-1", money("CNY", local)+" is above the threshold of "+money("CNY", local-1))
	q := pool(t, boxA())
	recordCost(t, q, "OCEAN_FREIGHT", usd(300_001), MethodChargeable)
	if err = apportion(q, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err = attributedUnder(threshold(local), q, "ops-2", "ops-1", 32, attribution(1, "H03", 10_000, 0, "")); err != nil {
		t.Fatalf("at the local amount: %v", err)
	}
}

// A correction is read on the share the house held when it left, the latest apportionment that gave it a used share:
// here the cost was revised and the line apportioned again before H03 left, so the share is CNY 550.00 and not CNY
// 500.00.
func TestF1ThresholdReadsTheLatestFormerShare(t *testing.T) {
	p := boxAPool(t) // H03's share: CNY 500.00
	if err := p.ReviseCost(1, 1, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(330_000)}, "re-quoted", policy(t), "ops-1", hour(5)); err != nil {
		t.Fatal(err)
	}
	if err := apportion(p, ApportionInput{Jobs: boxAJobs()}, boxA(), "fin-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	if _, la := latest(t, p, 1); la.Shares[2].Local != 55_000 {
		t.Fatalf("H03's latest share: %+v", la.Shares)
	}
	err := attributedUnder(threshold(52_000), p, "ops-2", "ops-1", 32, attribution(1, "H03", 10_000, 0, ""))
	refused(t, "the latest share is above", err, ErrAttributionConfirmer, "CNY 550.00 is above the threshold of CNY 520.00")
}
