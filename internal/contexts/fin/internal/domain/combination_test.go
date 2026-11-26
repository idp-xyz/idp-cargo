package domain

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"
)

// V2.102 batch C: the configuration as a whole. Every pair of values of FIN's items (F1 to F9) and S1 appears in a
// configuration of a pairwise covering array, and on each one box A is run in the domain: H03 leaves after the rework, the
// preview and the submission are made with and without the rework's asker, the freight changes, the pool closes. Then
// seeded random legal configurations and random sequences of operations check four invariants after every step: amounts are
// conserved, the trail is complete, previews go stale, and every refusal names its way out.

// boxAScenario is box A in the domain with a clock (hours from hour 30), the settings in force over time and the log of
// what it did, which a failure prints.
type boxAScenario struct {
	t        *testing.T
	label    string
	p        *ConsolCostPool
	h        int
	rules    RuleSettings
	timeline []versionFrom
	log      []string
}

// tally counts what the random runs did, so that the test can say it exercised each kind of step and outcome.
var tally = map[string]int{}

func note(what string, err error) {
	switch {
	case err == nil:
		tally[what+" ok"]++
	case errors.Is(err, ErrPreviewStale):
		tally[what+" stale"]++
	default:
		tally[what+" refused"]++
	}
}

type versionFrom struct {
	from    time.Time
	version int
}

// rulesOf turns a configuration into the settings the domain reads.
func rulesOf(c config, version int) RuleSettings {
	return RuleSettings{Version: version, F1: c.f1Value(), F1Threshold: c.thresholdOf(), F2: c["fin.F2"], F3: c["fin.F3"], F4: c["fin.F4"], F6: c["fin.F6"],
		F7: c["fin.F7"], F8: c["fin.F8"], F9: c["fin.F9"]}
}

func newBoxAScenario(t *testing.T, label string, rules RuleSettings) *boxAScenario {
	t.Helper()
	s := &boxAScenario{t: t, label: label, p: boxAPool(t), h: 30, rules: rules}
	s.timeline = []versionFrom{{hour(0), 0}, {hour(30), rules.Version}}
	return s
}

func (s *boxAScenario) fail(format string, args ...any) {
	s.t.Helper()
	s.t.Fatalf("%s: %s\nsteps: %v", s.label, fmt.Sprintf(format, args...), s.log)
}

func (s *boxAScenario) tick() time.Time {
	s.h++
	return hour(s.h)
}

func (s *boxAScenario) versionAt(at time.Time) int {
	v := 0
	for _, f := range s.timeline {
		if !f.from.After(at) {
			v = f.version
		}
	}
	return v
}

// settingsChange puts new settings in force from the next hour, as a change set approved then.
func (s *boxAScenario) settingsChange(r RuleSettings) {
	s.rules = r
	s.timeline = append(s.timeline, versionFrom{hour(s.h + 1), r.Version})
	s.log = append(s.log, fmt.Sprintf("settings v%d", r.Version))
}

func (s *boxAScenario) preview(by string, attrs ...AttributionInput) error {
	at := s.tick()
	err := s.p.Preview(ApportionInput{Jobs: boxAJobs(), Attributions: attrs, Settings: s.rules}, boxAAfter(), by, at)
	note("preview", err)
	s.log = append(s.log, fmt.Sprintf("preview by %s (%d attributions): %v", by, len(attrs), err))
	s.check()
	return err
}

func (s *boxAScenario) submit(no int, by string) error {
	at := s.tick()
	err := s.p.Submit(no, SubmitInput{Basis: boxAAfter(), Jobs: boxAJobs(), Settings: s.rules}, by, at)
	note("submit", err)
	s.log = append(s.log, fmt.Sprintf("submit %d by %s: %v", no, by, err))
	s.check()
	return err
}

func (s *boxAScenario) oneStep(by string) error {
	at := s.tick()
	err := s.p.ApportionInOneStep(ApportionInput{Jobs: boxAJobs(), Settings: s.rules}, boxAAfter(), by, at)
	note("one step", err)
	s.log = append(s.log, fmt.Sprintf("one step by %s: %v", by, err))
	s.check()
	return err
}

func (s *boxAScenario) closePool(by string) error {
	settle(s.p)
	at := s.tick()
	err := s.p.CloseUnder(s.rules, boxAAfter(), by, at)
	note("close", err)
	s.log = append(s.log, fmt.Sprintf("close by %s: %v", by, err))
	if err != nil {
		s.p.Matched(map[int]Consumption{}) // the invoices matched the lines only for this attempt
	}
	s.check()
	return err
}

func (s *boxAScenario) revise(minor int64) {
	versions := len(s.p.State().Lines[0].Versions)
	at := s.tick()
	if err := s.p.ReviseCost(1, versions, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(minor)}, "the carrier re-invoiced", policy(s.t), "fin-1", at); err != nil {
		s.fail("revise: %v", err)
	}
	s.log = append(s.log, fmt.Sprintf("freight revised to %d", minor))
	s.check()
}

func (s *boxAScenario) addLine(code string, minor int64) {
	at := s.tick()
	if err := s.p.RecordCost(code, "", "INV-"+code, MethodChargeable, SideInput{PartyID: "CARRIER-1", PartyRevision: 3, Amount: cny(minor)}, "carrier buy rate", policy(s.t), "ops-1", at); err != nil {
		s.fail("record %s: %v", code, err)
	}
	s.log = append(s.log, "line "+code)
	s.check()
}

// latestPreview is the number of the latest preview, which a submission takes.
func (s *boxAScenario) latestPreview() int { return len(s.p.State().Previews) }

// refusalFollowsTheConvention checks a refusal the domain gave: it has a kind and a way out.
func (s *boxAScenario) refusalFollowsTheConvention(err error) {
	s.t.Helper()
	if cerr := refusalConvention(err.Error()); cerr != nil {
		s.fail("a refusal without its way out: %v", cerr)
	}
}

// conserved answers whether the parts of a line add up to the line, in its currency and in the functional currency.
func conserved(la LineApportionment) error {
	amount, local := la.ToAllocate, la.ToAllocateLocal
	for _, sh := range la.Shares {
		if sh.Used {
			amount, local = amount+sh.Amount, local+sh.Local
		}
	}
	for _, x := range la.Attributions {
		amount, local = amount+x.Loss+x.Exception, local+x.LossLocal+x.ExceptionLocal
	}
	if amount != la.Amount || local != la.Local {
		return fmt.Errorf("line %d: the parts add up to %d (%d local), the line is %d (%d local)", la.LineNo, amount, local, la.Amount, la.Local)
	}
	return nil
}

// check is the invariants of the amounts and of the trail, after a step: every line of every preview and apportionment is
// conserved, and every preview, apportionment and closure records the settings version that was in force at its recorded
// time.
func (s *boxAScenario) check() {
	s.t.Helper()
	st := s.p.State()
	for _, v := range st.Previews {
		for _, la := range v.Lines {
			if err := conserved(la); err != nil {
				s.fail("preview %d: %v", v.No, err)
			}
		}
		if want := s.versionAt(v.At); v.SettingsVersion != want {
			s.fail("preview %d at %s records settings version %d, %d was in force", v.No, v.At.Format(time.RFC3339), v.SettingsVersion, want)
		}
	}
	for _, a := range st.Apportionments {
		for _, la := range a.Lines {
			if err := conserved(la); err != nil {
				s.fail("apportionment %d: %v", a.No, err)
			}
		}
		// the first apportionment was made before any setting was in force, by boxAPool
		if want := s.versionAt(a.At); a.SettingsVersion != want {
			s.fail("apportionment %d at %s records settings version %d, %d was in force", a.No, a.At.Format(time.RFC3339), a.SettingsVersion, want)
		}
	}
	for _, c := range st.Closures {
		if want := s.versionAt(c.At); c.SettingsVersion != want {
			s.fail("closure %d records settings version %d, %d was in force", c.No, c.SettingsVersion, want)
		}
	}
}

// pendingCause says whether an exception cost of party PENDING is in force on a line.
func (s *boxAScenario) pendingCause() bool {
	for _, l := range s.p.State().Lines {
		if _, la, ok := s.p.SharesOfLine(l.No); ok {
			for _, x := range la.Attributions {
				if x.Exception > 0 && x.Party == PartyPending {
					return true
				}
			}
		}
	}
	return false
}

func h03(loss, exception int64, party string) AttributionInput {
	return attribution(1, "H03", loss, exception, party)
}

// barredByF1 says whether F1 bars the asker and the ender for box A's former share of CNY 500.00.
func barredByF1(r RuleSettings) bool { return r.bars(50_000) }

type who struct{ name, previewer, submitter string }

// The three ways box A is previewed and submitted: the rework's asker (ops-1) submits another's preview; another submits the
// asker's preview; and neither is the asker.
var whos = []who{{"the asker submits", "ops-2", "ops-1"}, {"the asker previews", "ops-1", "fin-3"}, {"neither is the asker", "ops-2", "fin-3"}}

// boxAEndToEnd runs box A on one configuration and one way of previewing and submitting, asserting what the settings say.
func boxAEndToEnd(t *testing.T, c config, w who, stated bool) {
	label := fmt.Sprintf("configuration %v, %s, stated %v", map[string]string(c), w.name, stated)
	rules := rulesOf(c, 7)
	s := newBoxAScenario(t, label, rules)
	// H03 left after the rework (box A after): the first preview and its submission
	var attrs []AttributionInput
	if stated {
		attrs = append(attrs, h03(10_000, 20_000, PartyPending))
	}
	err := s.preview(w.previewer, attrs...)
	if !stated && rules.F8 != F8DefaultToRemaining {
		if !errors.Is(err, ErrAttributionMissing) {
			s.fail("an unstated former share under REJECT_WITH_DEFAULT: %v", err)
		}
		s.refusalFollowsTheConvention(err)
		attrs = []AttributionInput{h03(10_000, 20_000, PartyPending)}
		if err = s.preview(w.previewer, attrs...); err != nil {
			s.fail("the statement: %v", err)
		}
	} else if err != nil {
		s.fail("the preview: %v", err)
	}
	err = s.submit(s.latestPreview(), w.submitter)
	barred := barredByF1(rules)
	wantRefused := barred && (w.submitter == "ops-1" || (w.previewer == "ops-1" && rules.F2 == F2SubmitterAndPreviewer))
	switch {
	case wantRefused:
		if !errors.Is(err, ErrAttributionConfirmer) && !errors.Is(err, ErrConfirmer) {
			s.fail("F1 %s (threshold %d), F2 %s bar the asker: %v", rules.F1, rules.F1Threshold, rules.F2, err)
		}
		s.refusalFollowsTheConvention(err)
		if len(s.p.State().Apportionments) != 1 {
			s.fail("a refused submission wrote an apportionment")
		}
		// the way out: another person previews and submits it
		if err = s.preview("ops-2", attrs...); err != nil {
			s.fail("preview again: %v", err)
		}
		if err = s.submit(s.latestPreview(), "fin-3"); err != nil {
			s.fail("submit by another person: %v", err)
		}
	case err != nil:
		s.fail("F1 %s (threshold %d), F2 %s do not bar this: %v", rules.F1, rules.F1Threshold, rules.F2, err)
	}
	if n := len(s.p.State().Apportionments); n != 2 {
		s.fail("%d apportionments", n)
	}
	if a := s.p.State().Apportionments[1]; a.SettingsVersion != 7 || len(a.Lines[0].Attributions) != 1 {
		s.fail("the apportionment: %+v", a)
	}
	// previews go stale: a settings change after a preview refuses its submission, with the way out
	if err = s.preview("ops-2"); err != nil {
		s.fail("a preview that carries the attribution: %v", err)
	}
	stale := s.latestPreview()
	next := rules
	next.Version = 8
	s.settingsChange(next)
	if err = s.submit(stale, "fin-3"); !errors.Is(err, ErrPreviewStale) {
		s.fail("a preview made before a settings change: %v", err)
	} else {
		s.refusalFollowsTheConvention(err)
	}
	if err = s.preview("ops-2"); err != nil {
		s.fail("preview again: %v", err)
	}
	if err = s.submit(s.latestPreview(), "fin-3"); err != nil {
		s.fail("submit the new preview: %v", err)
	}
	// F4: a second line is apportioned in one step only when F4 allows it
	s.addLine("DOC_FEE", 60_000)
	err = s.oneStep("fin-3")
	if rules.F4 != F4OneStep {
		if !errors.Is(err, ErrOneStepRefused) {
			s.fail("a one-step apportionment under ALL_PREVIEWED: %v", err)
		}
		s.refusalFollowsTheConvention(err)
		if err = s.preview("ops-2"); err != nil {
			s.fail("preview the second line: %v", err)
		}
		if err = s.submit(s.latestPreview(), "fin-3"); err != nil {
			s.fail("submit the second line: %v", err)
		}
	} else if err != nil {
		s.fail("a one-step apportionment under F4: %v", err)
	}
	// the freight changes: F9 says whether the attribution must be stated again
	s.revise(360_000)
	err = s.preview("ops-2")
	if rules.F9 == F9Reconfirm {
		if !errors.Is(err, ErrAttributionReconfirm) {
			s.fail("F9 RECONFIRM after the freight changed: %v", err)
		}
		s.refusalFollowsTheConvention(err)
		restated := h03(12_000, 24_000, PartyPending)
		restated.Reason = "stated again after the freight changed"
		if err = s.preview("ops-2", restated); err != nil {
			s.fail("the attribution stated again: %v", err)
		}
	} else if err != nil {
		s.fail("F9 %s carries the attribution: %v", rules.F9, err)
	}
	if err = s.submit(s.latestPreview(), "fin-3"); err != nil {
		s.fail("submit after the freight changed: %v", err)
	}
	// the close: a pending cause holds it back under F7 BLOCKS
	pending := s.pendingCause()
	err = s.closePool("fin-1")
	if rules.F7 == F7Blocks && pending {
		if !errors.Is(err, ErrPoolNotClosable) {
			s.fail("a pending cause under F7 BLOCKS: %v", err)
		}
		s.refusalFollowsTheConvention(err)
		fix := h03(12_000, 24_000, PartyCarrier)
		fix.Reason, fix.Voucher = "the carrier is liable", "CLAIM-2"
		if err = s.preview("ops-2", fix); err != nil {
			s.fail("the correction: %v", err)
		}
		if err = s.submit(s.latestPreview(), "fin-3"); err != nil {
			s.fail("submit the correction: %v", err)
		}
		err = s.closePool("fin-1")
	}
	if err != nil {
		s.fail("the close: %v", err)
	}
	if c := s.p.State().Closures; len(c) != 1 || c[0].SettingsVersion != 8 {
		s.fail("the closure: %+v", c)
	}
}

func TestBoxAOnEveryPairOfSettings(t *testing.T) {
	_, rows := coveringArray(t)
	t.Logf("%d configurations cover every pair of values of F1 (four levels), F2, F3, F4, F6, F7, F8, F9 and S1", len(rows))
	runs := 0
	for i, c := range rows {
		for _, w := range whos {
			for _, stated := range []bool{true, false} {
				runs++
				t.Run(fmt.Sprintf("configuration %d/%s/stated %v", i, w.name, stated), func(t *testing.T) { boxAEndToEnd(t, c, w, stated) })
			}
		}
	}
	t.Logf("%d runs of box A", runs)
}

// randomConfiguration is a legal configuration: each item at a random value, and F3 set where F8 asks for it.
func randomConfiguration(t testing.TB, rng *rand.Rand) config {
	factors, _ := coveringArray(t)
	c := config{}
	for _, f := range factors {
		c[f.name] = f.levels[rng.Intn(len(f.levels))]
	}
	if c["fin.F8"] == "DEFAULT_TO_REMAINING" {
		c["fin.F3"] = "ONLY_WHEN_AMOUNT_MOVES"
	}
	return c
}

// operation is one step of the random sequences: it runs one operation of box A and checks what must hold of its outcome.
type operation struct {
	name string
	run  func(s *boxAScenario, rng *rand.Rand)
}

var people = []string{"ops-1", "ops-2", "ops-9", "fin-1", "fin-3"}

func pick(rng *rand.Rand, from []string) string { return from[rng.Intn(len(from))] }

// refusalOrNil checks that an error is a refusal that follows the convention: every refusal of a rule names its cause and
// its way out. An error that is not a refusal of a rule (an invalid call) fails.
func (s *boxAScenario) refusalOrNil(err error) {
	if err == nil {
		return
	}
	s.refusalFollowsTheConvention(err)
}

var operations = []operation{
	{"preview", func(s *boxAScenario, rng *rand.Rand) {
		var attrs []AttributionInput
		switch rng.Intn(3) {
		case 0:
			attrs = nil
		case 1:
			attrs = []AttributionInput{h03(10_000, 20_000, []string{PartyPending, PartyCarrier, PartyWarehouse}[rng.Intn(3)])}
		default:
			a := h03(int64(rng.Intn(3))*5_000, int64(rng.Intn(3))*5_000, PartyCarrier)
			if a.Exception == 0 {
				a.Party = ""
			}
			a.Reason = "a correction"
			attrs = []AttributionInput{a}
		}
		s.refusalOrNil(s.preview(pick(rng, people), attrs...))
	}},
	{"submit the latest preview", func(s *boxAScenario, rng *rand.Rand) {
		n := s.latestPreview()
		if n == 0 {
			return
		}
		v := s.p.State().Previews[n-1]
		submitted := false
		for _, a := range s.p.State().Apportionments {
			submitted = submitted || a.PreviewNo == n
		}
		if submitted {
			return
		}
		err := s.submit(n, pick(rng, people))
		if v.SettingsVersion != s.rules.Version && !errors.Is(err, ErrPreviewStale) {
			s.fail("a preview of settings version %d submitted under %d: %v", v.SettingsVersion, s.rules.Version, err)
		}
		s.refusalOrNil(err)
	}},
	{"settings change", func(s *boxAScenario, rng *rand.Rand) {
		s.settingsChange(rulesOf(randomConfiguration(s.t, rng), s.rules.Version+1))
	}},
	{"revise the freight", func(s *boxAScenario, rng *rand.Rand) { s.revise(int64(200_000 + rng.Intn(300_000))) }},
	{"one step", func(s *boxAScenario, rng *rand.Rand) { s.refusalOrNil(s.oneStep(pick(rng, people))) }},
	{"close", func(s *boxAScenario, rng *rand.Rand) {
		if len(s.p.State().Closures) > 0 {
			return
		}
		s.refusalOrNil(s.closePool(pick(rng, people)))
	}},
}

func seedsToRun() []int64 {
	if v := os.Getenv("FIN_PROPERTY_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			panic(err)
		}
		return []int64{n}
	}
	seeds := make([]int64, 200)
	for i := range seeds {
		seeds[i] = int64(i + 1)
	}
	return seeds
}

// TestRandomLegalConfigurationsKeepTheInvariantsOnBoxA runs 200 seeded random legal configurations, each through a random
// sequence of operations with a settings change in the middle, and checks after every step that amounts are conserved, the
// trail is complete (the version recorded is the one in force at the recorded time), a preview of an older version is
// refused as stale, and every refusal follows the convention. A failure prints its seed: replay it with FIN_PROPERTY_SEED.
func TestRandomLegalConfigurationsKeepTheInvariantsOnBoxA(t *testing.T) {
	clear(tally)
	defer func() {
		t.Logf("what the random runs did: %v", tally)
		if os.Getenv("FIN_PROPERTY_SEED") != "" || t.Failed() {
			return
		}
		for _, want := range []string{"preview ok", "preview refused", "submit ok", "submit refused", "submit stale", "one step ok", "one step refused", "close ok", "close refused"} {
			if tally[want] < 5 {
				t.Errorf("the random runs made only %d of %q: widen the operations", tally[want], want)
			}
		}
	}()
	for _, seed := range seedsToRun() {
		t.Run("seed "+strconv.FormatInt(seed, 10), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			c := randomConfiguration(t, rng)
			s := newBoxAScenario(t, fmt.Sprintf("seed %d (replay with FIN_PROPERTY_SEED=%d), configuration %v", seed, seed, map[string]string(c)), rulesOf(c, 1))
			// the opening step, so that previews and closes have something to read
			steps := 12 + rng.Intn(14)
			changeAt := steps / 2
			for i := 0; i < steps; i++ {
				if i == changeAt {
					s.settingsChange(rulesOf(randomConfiguration(s.t, rng), s.rules.Version+1))
				}
				if s.p.State().State == PoolClosed {
					break // a closed pool takes no change until reopened
				}
				op := operations[rng.Intn(len(operations))]
				s.log = append(s.log, "-> "+op.name)
				op.run(s, rng)
			}
		})
	}
}
