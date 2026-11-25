//go:build integration

package integration

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.97: FIN's rules read the settings F1, F2, F3, F6 and F7 and every decision records the settings version,
// through FIN's commands on PostgreSQL, with the settings changed through FIN's own settings commands: a change set
// that loosens is proposed by ann and approved by bob, one that only tightens is in force when proposed. The world is
// formerWorld: the air consolidation CONSOL-U with its freight line of CNY 1,000.00 apportioned 461.54, 307.69 and
// 230.77 to SH-A, SH-B and SH-C, then SH-C leaves (transferred by ops-9 after a rework ops-1 asked for). SH-C's former
// share is CNY 230.77.

var settingsCommands atomic.Int64

// time0 is the time zero: a change set that takes effect at once.
var time0 time.Time

func le(key, value string) api.FinSettingEntry {
	return api.FinSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: key, Value: value}
}

func thresholdOf(minor int64) api.FinSettingEntry {
	return api.FinSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "fin.F1", Value: "NOT_ABOVE_THRESHOLD", Threshold: &kernel.Money{Currency: "CNY", Minor: minor}}
}

// configure changes the legal entity's settings as FIN's commands do and answers the version in force: proposed by ann,
// and approved by bob when it loosens. Until bob approves it is not in force.
func (u usedWorld) configure(effective time.Time, entries ...api.FinSettingEntry) int {
	u.t.Helper()
	was := u.settingsVersion()
	n := settingsCommands.Add(1)
	id := fmt.Sprintf("set-%d", n)
	r := u.must(u.module.ProposeFinSettingsChange(u.ctx, api.ProposeFinSettingsChange{CommandID: id + "-propose", TenantID: u.tenant, ActorID: "ann", ChangeID: id,
		Entries: entries, Reason: "because " + id, EffectiveAt: effective}))
	if r.Version == 0 {
		if v := u.settingsVersion(); v != was {
			u.t.Fatalf("a change set that waits for its approval is in force: version %d, was %d", v, was)
		}
		r = u.must(u.module.ApproveFinSettingsChange(u.ctx, api.ApproveFinSettingsChange{CommandID: id + "-approve", TenantID: u.tenant, ActorID: "bob", ChangeID: id}))
	}
	return int(r.Version)
}

func (u usedWorld) settingsVersion() int {
	u.t.Helper()
	s, err := u.module.GetFinSettings(u.ctx, api.GetFinSettings{TenantID: u.tenant, ScopeKind: "LEGAL_ENTITY", ScopeID: "LE"})
	if err != nil {
		u.t.Fatal(err)
	}
	return s.Version
}

// attributeBy previews the statements as previewer and submits them as submitter, and answers what the submission said.
func (u usedWorld) attributeBy(id, previewer, submitter string, hour int, statements ...api.AttributionStatement) (api.Receipt, error) {
	u.t.Helper()
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd(id, previewer, hour, statements...)))
	return u.submit(id+"-submit", submitter, int(r.Version), hour)
}

func (u usedWorld) lastApportionment() api.PoolApportionment {
	u.t.Helper()
	p := u.ap.pool(u.pool)
	return p.Apportionments[len(p.Apportionments)-1]
}

// The rule of V2.93 under F1 and F2, each value end to end: the asker of the rework (ops-1) and the one who ended the
// house's place (ops-9) confirm SH-C's attribution or are refused, the previewer is checked under F2, and the threshold
// is read on the former share of CNY 230.77, strictly above. A submission records the version in force, as its preview.
func TestF1AndF2OnTheAttributionOnPostgres(t *testing.T) {
	statement := statementOf("C", 5_000, 10_000, api.PartyWarehouse)
	for _, c := range []struct {
		name                 string
		entries              []api.FinSettingEntry
		previewer, submitter string
		refusal              string // empty: the submission is accepted
		mentions             []string
	}{
		{"no settings: the asker is refused", nil, "ops-2", "ops-1", "FIN_ATTRIBUTION_CONFIRMER", []string{"ops-1 asked for rework 1", "another finance user submits"}},
		{"no settings: the ender is refused", nil, "ops-2", "ops-9", "FIN_ATTRIBUTION_CONFIRMER", []string{"ops-9 ended the place of house SH-C"}},
		{"no settings: the previewer is not checked", nil, "ops-1", "fin-2", "", nil},
		{"F1 UNRESTRICTED: the asker", []api.FinSettingEntry{le("fin.F1", "UNRESTRICTED")}, "ops-2", "ops-1", "", nil},
		{"F1 UNRESTRICTED: the ender", []api.FinSettingEntry{le("fin.F1", "UNRESTRICTED")}, "ops-2", "ops-9", "", nil},
		{"F1 threshold below the share: the asker is refused", []api.FinSettingEntry{thresholdOf(23_076)}, "ops-2", "ops-1", "FIN_ATTRIBUTION_CONFIRMER",
			[]string{"house SH-C on line 1", "CNY 230.77 is above the threshold of CNY 230.76"}},
		{"F1 threshold at the share: the asker confirms", []api.FinSettingEntry{thresholdOf(23_077)}, "ops-2", "ops-1", "", nil},
		{"F1 threshold above the share: the ender confirms", []api.FinSettingEntry{thresholdOf(100_000)}, "ops-2", "ops-9", "", nil},
		{"F2 both: the previewer who asked is refused", []api.FinSettingEntry{le("fin.F2", "SUBMITTER_AND_PREVIEWER")}, "ops-1", "fin-2", "FIN_ATTRIBUTION_CONFIRMER",
			[]string{"the previewer ops-1 asked for rework 1", "preview again as another finance user"}},
		{"F2 both: the previewer who ended is refused", []api.FinSettingEntry{le("fin.F2", "SUBMITTER_AND_PREVIEWER")}, "ops-9", "fin-2", "FIN_ATTRIBUTION_CONFIRMER",
			[]string{"the previewer ops-9 ended the place of house SH-C"}},
		{"F2 both: another previewer", []api.FinSettingEntry{le("fin.F2", "SUBMITTER_AND_PREVIEWER")}, "ops-2", "fin-2", "", nil},
		{"F2 both and F1 threshold at the share: the previewer confirms", []api.FinSettingEntry{le("fin.F2", "SUBMITTER_AND_PREVIEWER"), thresholdOf(23_077)}, "ops-1", "fin-2", "", nil},
		{"F2 both and F1 UNRESTRICTED: the previewer", []api.FinSettingEntry{le("fin.F2", "SUBMITTER_AND_PREVIEWER"), le("fin.F1", "UNRESTRICTED")}, "ops-1", "fin-2", "", nil},
	} {
		u := formerWorld(t)
		u.depart()
		version := 0
		if c.entries != nil {
			version = u.configure(time.Time{}, c.entries...)
			if version == 0 || u.settingsVersion() != version {
				t.Fatalf("%s: the settings are not in force: %d", c.name, version)
			}
		}
		_, err := u.attributeBy("a", c.previewer, c.submitter, 6, statement)
		p := u.ap.pool(u.pool)
		if c.refusal != "" {
			u.refusedWith(c.name, err, c.refusal)
			for _, part := range c.mentions {
				if err == nil || !strings.Contains(err.Error(), part) {
					t.Errorf("%s: the refusal does not say %q: %v", c.name, part, err)
				}
			}
			if len(p.Apportionments) != 1 {
				t.Errorf("%s: a refused submission was written", c.name)
			}
			if len(p.Previews) != 2 || p.Previews[1].SettingsVersion != version {
				t.Errorf("%s: the preview records version %d: %+v", c.name, version, p.Previews)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		last, preview := p.Apportionments[len(p.Apportionments)-1], p.Previews[len(p.Previews)-1]
		if last.SettingsVersion != version || preview.SettingsVersion != version || last.PreviewNo != preview.PreviewNo || last.By != c.submitter || preview.By != c.previewer {
			t.Errorf("%s: the versions recorded (%d wanted): apportionment %+v, preview %+v", c.name, version, last, preview)
		}
	}
}

// A submission with two parts, one above the threshold and one below: it is refused when the submitter is barred,
// naming the part above; and a part above is read on its own former share, a correction too.
func TestF1ThresholdNamesThePartAboveOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.must(u.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: "cost-2", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		ChargeCode: "HANDLING", Method: api.MethodChargeable, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: cny(10_000)}, Reason: "entered for the consolidation",
		OccurredAt: u.hours(4)}))
	a := u.apportionAll("apportion-2", 5, []int{2})
	u.depart()
	// SH-C's former shares: 230.77 on line 1 and 23.08 on line 2 (the same proportion of CNY 100.00)
	var share1, share2 int64
	for _, s := range a.Lines[0].Shares {
		if s.ShipmentID == "SH-C" {
			share2 = s.Amount.Minor
		}
	}
	for _, s := range u.ap.pool(u.pool).Apportionments[0].Lines[0].Shares {
		if s.ShipmentID == "SH-C" {
			share1 = s.Amount.Minor
		}
	}
	if share1 != 23_077 || share2 != 2_308 {
		t.Fatalf("the former shares: %d and %d", share1, share2)
	}
	u.configure(time.Time{}, thresholdOf(2_307))
	both := func(id, actor string) (api.Receipt, error) {
		cmd := u.apportionCmd(id, 6, []int{1, 2})
		cmd.ActorID = "ops-2"
		cmd.Attributions = []api.AttributionStatement{statementOf("C", 5_000, 0, ""), {LineNo: 2, ShipmentID: "SH-C", Loss: 100, Voucher: "CLAIM-2"}}
		r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, cmd))
		return u.submit(id+"-submit", actor, int(r.Version), 6)
	}
	_, err := both("both-1", "ops-1")
	u.refusedWith("both parts above 23.07", err, "FIN_ATTRIBUTION_CONFIRMER")
	if !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), "CNY 230.77 is above the threshold of CNY 23.07") {
		t.Fatalf("the refusal names the part above: %v", err)
	}
	// raise the threshold between the two: the part below passes for the asker, the part above does not
	u.configure(time.Time{}, thresholdOf(2_308))
	_, err = both("both-2", "ops-1")
	u.refusedWith("one part above", err, "FIN_ATTRIBUTION_CONFIRMER")
	if !strings.Contains(err.Error(), "line 1") || strings.Contains(err.Error(), "line 2") {
		t.Fatalf("the refusal names line 1 and not line 2: %v", err)
	}
	// above both: nobody is barred; each part was read on its own, though together they are above the threshold
	u.configure(time.Time{}, thresholdOf(23_077))
	if _, err = both("both-3", "ops-1"); err != nil {
		t.Fatalf("both parts not above: %v", err)
	}
}

// The rule of V2.91 under F1 and F2: the asker of a rework submits the first apportionment after it, or is refused, by
// the local cost of the line (CNY 1,000.00), strictly above the threshold; under F2 the previewer is checked as well.
func TestF1AndF2OnTheReworkRuleOnPostgres(t *testing.T) {
	for _, c := range []struct {
		name                 string
		entries              []api.FinSettingEntry
		previewer, submitter string
		refused              bool
	}{
		{"no settings: the asker", nil, "fin-1", "ops-asker", true},
		{"no settings: the previewer who asked", nil, "ops-asker", "fin-1", false},
		{"F1 UNRESTRICTED: the asker", []api.FinSettingEntry{le("fin.F1", "UNRESTRICTED")}, "fin-1", "ops-asker", false},
		{"F1 threshold below the line: the asker", []api.FinSettingEntry{thresholdOf(99_999)}, "fin-1", "ops-asker", true},
		{"F1 threshold at the line: the asker", []api.FinSettingEntry{thresholdOf(100_000)}, "fin-1", "ops-asker", false},
		{"F2 both: the previewer who asked", []api.FinSettingEntry{le("fin.F2", "SUBMITTER_AND_PREVIEWER")}, "ops-asker", "fin-1", true},
		{"F2 both with the threshold at the line: the previewer who asked", []api.FinSettingEntry{le("fin.F2", "SUBMITTER_AND_PREVIEWER"), thresholdOf(100_000)}, "ops-asker", "fin-1", false},
		{"F2 both with the threshold below the line: the asker as both", []api.FinSettingEntry{le("fin.F2", "SUBMITTER_AND_PREVIEWER"), thresholdOf(99_999)}, "ops-asker", "ops-asker", true},
	} {
		u := previewWorld(t)
		u.openRework("ops-asker")
		u.reseal(5)
		version := 0
		if c.entries != nil {
			version = u.configure(time.Time{}, c.entries...)
		}
		cmd := u.apportionCmd("preview", 6, []int{1}, "SH-C")
		cmd.ActorID = c.previewer
		r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, cmd))
		_, err := u.submit("submit", c.submitter, int(r.Version), 6)
		if c.refused {
			u.refusedWith(c.name, err, "FIN_CONFIRMER_ASKED_REWORK")
			if err != nil && !strings.Contains(err.Error(), "asked for rework 1 of load unit 1 (stuffing X-1)") {
				t.Errorf("%s: %v", c.name, err)
			}
			if err != nil && strings.Contains(c.name, "below the line") && !strings.Contains(err.Error(), "CNY 1,000.00 is above the threshold of CNY 999.99") {
				t.Errorf("%s: the refusal does not name the amount and the threshold: %v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if a := u.lastApportionment(); a.SettingsVersion != version {
			t.Errorf("%s: version %d recorded as %d", c.name, version, a.SettingsVersion)
		}
	}
}

// F3: a voucher on every attribution, or only when an amount moves. The settings that loosen wait for a second person.
// An attribution that moves nothing gives its whole former share to the cargo that remains (SH-A 600.00, SH-B 400.00 of
// the line), has no voucher, and a correction of it needs its reason.
func TestF3VoucherOnlyWhenAmountMovesOnPostgres(t *testing.T) {
	nothing := api.AttributionStatement{LineNo: 1, ShipmentID: "SH-C"}
	u := formerWorld(t)
	u.depart()
	_, err := u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("none", "ops-2", 6, nothing))
	u.refusedWith("no voucher by default", err, "names its voucher")
	if !strings.Contains(err.Error(), "state a voucher") {
		t.Fatalf("the way out: %v", err)
	}
	v := u.configure(time.Time{}, le("fin.F3", "ONLY_WHEN_AMOUNT_MOVES"))
	_, err = u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("loss", "ops-2", 6, api.AttributionStatement{LineNo: 1, ShipmentID: "SH-C", Loss: 100}))
	u.refusedWith("a loss with no voucher", err, "names its voucher")
	if _, err = u.attributeBy("nothing", "ops-2", "fin-2", 6, nothing); err != nil {
		t.Fatalf("an attribution that moves nothing: %v", err)
	}
	a := u.lastApportionment()
	if a.SettingsVersion != v || len(a.Lines) != 1 || len(a.Lines[0].Attributions) != 1 || a.Lines[0].Attributions[0].Voucher != "" || a.Lines[0].Attributions[0].Loss.Minor != 0 {
		t.Fatalf("the apportionment: %+v", a)
	}
	var shares []int64
	for _, s := range a.Lines[0].Shares {
		shares = append(shares, s.Amount.Minor)
	}
	if len(shares) != 2 || shares[0]+shares[1] != 100_000 || shares[0] != 60_000 || shares[1] != 40_000 {
		t.Fatalf("the former share goes to the cargo that remains: %v", shares)
	}
	// a correction to a loss needs its reason and a voucher; one back to nothing needs its reason and no voucher
	loss := statementOf("C", 5_000, 0, "")
	_, err = u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("fix-1", "ops-2", 7, loss))
	u.refusedWith("a correction with no reason", err, "a correction states its reason")
	loss.Reason = "the carrier's claim"
	if _, err = u.attributeBy("fix-2", "ops-2", "fin-2", 8, loss); err != nil {
		t.Fatal(err)
	}
	back := nothing
	_, err = u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("back-1", "ops-2", 9, back))
	u.refusedWith("a correction back to nothing with no reason", err, "a correction states its reason")
	back.Reason = "the claim was withdrawn"
	_, err = u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("back-2", "ops-2", 9, back))
	u.refusedWith("a correction back to nothing with its reason and no voucher (the review's case)", err, "a correction states its reason and its voucher")
	back.Voucher = "WITHDRAWN-1"
	if _, err = u.attributeBy("back-3", "ops-2", "fin-2", 10, back); err != nil {
		t.Fatalf("a correction back to nothing: %v", err)
	}
	if got := u.lastApportionment(); got.Lines[0].Attributions[0].Voucher != "WITHDRAWN-1" || got.Lines[0].Attributions[0].Reason != "the claim was withdrawn" {
		t.Fatalf("the correction: %+v", got.Lines[0].Attributions)
	}
}

// F6: a pending former share is shown apart from the profit, or counted in it with a mark; every profit view says which
// basis it used. Once it is attributed it is no longer pending.
func TestF6PendingFormerShareInTheProfitOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.sell("JOB-C", 50_000)
	sep := u.jobProfit("JOB-C")
	if sep.ProfitBasis != "SEPARATE" || sep.PoolCost.Minor != 0 || sep.PendingInProfit.Minor != 0 || sep.FormerSharesPending.Minor != 23_077 || sep.Profit.Minor != 50_000 ||
		len(sep.FormerShares) != 1 || !sep.FormerShares[0].Pending || sep.SettingsVersion != 0 {
		t.Fatalf("under SEPARATE: %+v", sep)
	}
	consolSep := u.consolProfit()
	if consolSep.ProfitBasis != "SEPARATE" || consolSep.FormerSharesPending.Minor != 23_077 {
		t.Fatalf("the consolidation under SEPARATE: %+v", consolSep)
	}
	var housesSep = map[string]api.HouseProfit{}
	for _, h := range consolSep.Houses {
		housesSep[h.ShipmentID] = h
		if h.PendingInProfit.Minor != 0 {
			t.Fatalf("under SEPARATE nothing is pending in a profit: %+v", h)
		}
	}
	v := u.configure(time.Time{}, le("fin.F6", "INCLUDED_MARKED"))
	inc := u.jobProfit("JOB-C")
	if inc.ProfitBasis != "INCLUDED_MARKED" || inc.SettingsVersion != v || inc.PoolCost.Minor != 23_077 || inc.PendingInProfit.Minor != 23_077 ||
		inc.Profit.Minor != 50_000-23_077 || inc.FormerSharesPending.Minor != 23_077 || len(inc.FormerShares) != 1 || !inc.FormerShares[0].Pending {
		t.Fatalf("under INCLUDED_MARKED: %+v", inc)
	}
	if other := u.jobProfit("JOB-A"); other.ProfitBasis != "INCLUDED_MARKED" || other.PendingInProfit.Minor != 0 || len(other.PoolShares) == 0 {
		t.Fatalf("a job with no former share: %+v", other)
	}
	for _, job := range []string{"JOB-A", "JOB-B"} { // a share borne is never marked pending
		for _, s := range u.jobProfit(job).PoolShares {
			if s.Pending {
				t.Fatalf("%s: a borne share is marked pending: %+v", job, s)
			}
		}
	}
	consol := u.consolProfit()
	if consol.ProfitBasis != "INCLUDED_MARKED" || consol.SettingsVersion != v || consol.FormerSharesPending.Minor != 23_077 || consol.Unapportioned.Minor != consolSep.Unapportioned.Minor ||
		consol.Pooled != consolSep.Pooled {
		t.Fatalf("the consolidation under INCLUDED_MARKED: %+v", consol)
	}
	for _, h := range consol.Houses {
		was := housesSep[h.ShipmentID]
		if h.ShipmentID == "SH-C" {
			if h.PendingInProfit.Minor != 23_077 || h.PoolCost.Minor != was.PoolCost.Minor+23_077 || h.Profit.Minor != was.Profit.Minor-23_077 {
				t.Fatalf("SH-C under INCLUDED_MARKED: %+v, was %+v", h, was)
			}
		} else if h != was {
			t.Fatalf("%s changed: %+v, was %+v", h.ShipmentID, h, was)
		}
	}
	// attributed, the former share is a loss on its job and nothing is pending any more
	u.attribute("attribute", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
	done := u.jobProfit("JOB-C")
	if done.PendingInProfit.Minor != 0 || done.PoolCost.Minor != 5_000 || done.FormerSharesPending.Minor != 0 {
		t.Fatalf("after the attribution: %+v", done)
	}
}

func (u usedWorld) consolProfit() api.ConsolProfit {
	u.t.Helper()
	p, err := u.module.GetConsolProfit(u.ctx, api.GetConsolProfit{TenantID: u.tenant, ConsolidationID: "CONSOL-U"})
	if err != nil {
		u.t.Fatal(err)
	}
	return p
}

// F7: a pending cause holds back the close under BLOCKS and not under DOES_NOT_BLOCK; the refusal names the line and
// the way out, the pending item says which, the list is empty exactly when the close is accepted, and the closure
// records the version in force.
func TestF7APendingCauseAndTheCloseOnPostgres(t *testing.T) {
	pendingCause := statementOf("C", 5_000, 10_000, api.PartyPending)
	// DOES_NOT_BLOCK, the default
	u := formerWorld(t)
	u.depart()
	u.attribute("attribute", 6, pendingCause)
	u.settleInvoice(7)
	items := u.pending()
	if len(items) != 1 || items[0].Kind != api.PendingExceptionCause || !strings.Contains(items[0].Message, "does not hold back the close (F7 DOES_NOT_BLOCK") {
		t.Fatalf("under DOES_NOT_BLOCK: %+v", items)
	}
	u.closeOnly(8)
	if c := u.ap.pool(u.pool).Closures; len(c) != 1 || c[0].SettingsVersion != 0 {
		t.Fatalf("the closure under no settings: %+v", c)
	}

	// BLOCKS
	b := formerWorld(t)
	b.depart()
	v := b.configure(time.Time{}, le("fin.F7", "BLOCKS"))
	b.attribute("attribute", 6, pendingCause)
	b.settleInvoice(7)
	items = b.pending()
	if len(items) != 1 || items[0].Kind != api.PendingExceptionCause || !strings.Contains(items[0].Message, "holds back the close (F7 BLOCKS)") ||
		!strings.Contains(items[0].Message, "with its reason and a voucher") {
		t.Fatalf("under BLOCKS: %+v", items)
	}
	_, err := b.module.CloseConsolCostPool(b.ctx, api.CloseConsolCostPool{CommandID: "close-1", TenantID: b.tenant, ActorID: "fin-1", PoolID: b.pool, ExpectedRevision: b.revision(),
		OccurredAt: b.hours(8)})
	b.refusedWith("the close under BLOCKS", err, "FIN_POOL_NOT_CLOSABLE")
	for _, part := range []string{"cost line 1", "house SH-C", "party is PENDING", "F7 BLOCKS", "establish the party and correct the attribution", "with its reason and a voucher"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("the refusal does not say %q: %v", part, err)
		}
	}
	if p := b.ap.pool(b.pool); p.State != "OPEN" || len(p.Closures) != 0 {
		t.Fatalf("a refused close changed the pool: %+v", p)
	}
	fix := statementOf("C", 5_000, 10_000, api.PartyCarrier)
	fix.Voucher, fix.Reason = "CLAIM-2", "the carrier is liable"
	if _, err = b.attributeBy("fix", "ops-2", "fin-2", 9, fix); err != nil {
		t.Fatal(err)
	}
	if items = b.pending(); len(items) != 0 {
		t.Fatalf("the party is established: %+v", items)
	}
	b.closeOnly(10)
	if c := b.ap.pool(b.pool).Closures; len(c) != 1 || c[0].SettingsVersion != v || v == 0 {
		t.Fatalf("the closure under BLOCKS (version %d): %+v", v, c)
	}
}

// A preview made before a settings change is stale at submission, saying so and the way out (preview again); the
// preview's effect says it is not current; a preview made again, under the new version, is submitted and records it.
func TestAPreviewMadeBeforeASettingsChangeIsStaleOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-1", "ops-2", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))))
	if e := u.effect(int(r.Version)); !e.Current {
		t.Fatalf("the preview is current before: %+v", e)
	}
	v := u.configure(time.Time{}, le("fin.F2", "SUBMITTER_AND_PREVIEWER"))
	if e := u.effect(int(r.Version)); e.Current || len(e.Changes) != 1 || e.Changes[0] != fmt.Sprintf("the settings changed from version 0 to %d", v) {
		t.Fatalf("the effect after the change: %+v", e)
	}
	_, err := u.submit("submit-1", "fin-2", int(r.Version), 6)
	u.refusedWith("the old preview", err, "FIN_PREVIEW_STALE")
	if !strings.Contains(err.Error(), fmt.Sprintf("the settings changed from version 0 to %d", v)) || !strings.Contains(err.Error(), "preview again and submit the new preview") {
		t.Fatalf("the refusal says what changed and the way out: %v", err)
	}
	if n := len(u.ap.pool(u.pool).Apportionments); n != 1 {
		t.Fatalf("a stale submission was written: %d", n)
	}
	r = u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-2", "ops-2", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))))
	if _, err = u.submit("submit-2", "fin-2", int(r.Version), 6); err != nil {
		t.Fatalf("the new preview: %v", err)
	}
	p := u.ap.pool(u.pool)
	if p.Previews[0].SettingsVersion != 0 || p.Previews[1].SettingsVersion != 0 || p.Previews[2].SettingsVersion != v || p.Apportionments[1].SettingsVersion != v ||
		p.Apportionments[0].SettingsVersion != 0 {
		t.Fatalf("the versions: previews %+v, apportionments %+v", p.Previews, p.Apportionments)
	}
}

// A change set that takes effect in the future does not apply before: a module whose clock is after its time reads the
// new value, one before it reads the old, in the same queries; the change set is approved but not in force.
func TestAChangeTakingEffectInTheFutureDoesNotApplyBeforeOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.attribute("attribute", 6, statementOf("C", 5_000, 10_000, api.PartyPending))
	effective := time.Now().UTC().Add(time.Hour)
	v := u.configure(effective, le("fin.F7", "BLOCKS"), le("fin.F6", "INCLUDED_MARKED"))
	if v == 0 {
		t.Fatal("the change set that only tightens is approved on the spot")
	}
	if now := u.settingsVersion(); now != 0 {
		t.Fatalf("before its time the version in force is %d", now)
	}
	later, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, allow{}, finmodule.WithClock(offsetClock{2 * time.Hour}))
	if err != nil {
		t.Fatal(err)
	}
	says := func(m *finmodule.Module) string {
		items, err := m.GetConsolCostPoolPendingItems(u.ctx, api.GetConsolCostPoolPendingItems{TenantID: u.tenant, PoolID: u.pool})
		if err != nil || len(items) == 0 {
			t.Fatalf("%v %v", items, err)
		}
		for _, it := range items {
			if it.Kind == api.PendingExceptionCause {
				return it.Message
			}
		}
		t.Fatalf("no item for the cause: %+v", items)
		return ""
	}
	if m := says(u.module); !strings.Contains(m, "does not hold back the close") {
		t.Fatalf("before the time: %s", m)
	}
	if m := says(later); !strings.Contains(m, "holds back the close (F7 BLOCKS)") {
		t.Fatalf("after the time: %s", m)
	}
	if p, err := u.module.GetJobProfit(u.ctx, api.GetJobProfit{TenantID: u.tenant, JobID: "JOB-C"}); err != nil || p.ProfitBasis != "SEPARATE" || p.SettingsVersion != 0 {
		t.Fatalf("the profit before the time: %+v %v", p, err)
	}
	if p, err := later.GetJobProfit(u.ctx, api.GetJobProfit{TenantID: u.tenant, JobID: "JOB-C"}); err != nil || p.ProfitBasis != "INCLUDED_MARKED" || p.SettingsVersion != v {
		t.Fatalf("the profit after the time: %+v %v", p, err)
	}
	// a cancelled change set never applies
	if _, err = u.module.CancelFinSettingsChange(u.ctx, api.CancelFinSettingsChange{CommandID: "cancel-set", TenantID: u.tenant, ActorID: "ann",
		ChangeID: fmt.Sprintf("set-%d", settingsCommands.Load()), Reason: "not wanted"}); err != nil {
		t.Fatal(err)
	}
	if m := says(later); !strings.Contains(m, "does not hold back the close") {
		t.Fatalf("after the cancellation: %s", m)
	}
}
