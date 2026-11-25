//go:build integration

package bootstrap

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN's rules read the settings (V2.97), on the owner's box A with the real ORG, RATE, COM, FUL, PHY and FIN: C-1 with
// H01, H02 and H03, freight CNY 3,000.00 apportioned 1,000.00, 1,500.00 and 500.00; H03 is unloaded in a rework asked
// by ops-1, the container sealed again and H03 transferred to C-2 by ops-9. H03's former share is CNY 500.00. The
// settings of the legal entity LE are changed through FIN's own commands: the change that loosens is proposed by cfg-1
// and approved by cfg-2.

var boxASettings atomic.Int64

// configure changes the settings of LE and answers the version in force.
func (d *oceanDay) configure(entries ...finapi.FinSettingEntry) int {
	d.t.Helper()
	id := fmt.Sprintf("boxa-set-%d", boxASettings.Add(1))
	r, err := d.fin.ProposeFinSettingsChange(d.ctx, finapi.ProposeFinSettingsChange{CommandID: id + "-p", TenantID: d.tenant, ActorID: "cfg-1", ChangeID: id, Entries: entries,
		Reason: "because " + id})
	d.must("propose "+id, err)
	if r.Version == 0 {
		r, err = d.fin.ApproveFinSettingsChange(d.ctx, finapi.ApproveFinSettingsChange{CommandID: id + "-a", TenantID: d.tenant, ActorID: "cfg-2", ChangeID: id})
		d.must("approve "+id, err)
	}
	return int(r.Version)
}

func leSetting(key, value string) finapi.FinSettingEntry {
	return finapi.FinSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: key, Value: value}
}

func leThreshold(minor int64) finapi.FinSettingEntry {
	return finapi.FinSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "fin.F1", Value: "NOT_ABOVE_THRESHOLD", Threshold: &kernel.Money{Currency: "CNY", Minor: minor}}
}

// boxAAfterTheTransfer is box A on the day after: the first apportionment submitted, H03 transferred, the freight
// invoice matched final, and the settings (when any) in force.
func boxAAfterTheTransfer(t *testing.T, prefix string, entries ...finapi.FinSettingEntry) (*oceanDay, int) {
	t.Helper()
	d := boxA(t, prefix)
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	d.transferH03()
	d.settleFreight()
	version := 0
	if len(entries) > 0 {
		version = d.configure(entries...)
	}
	return d, version
}

// Under UNRESTRICTED the person who asked for the rework (ops-1) and the person who transferred H03 (ops-9) may each
// submit H03's attribution, which by default neither may; the apportionment and its preview record the version in
// force.
func TestUnrestrictedLetsTheAskerAndTheTransfererSubmitWithEveryContext(t *testing.T) {
	for _, who := range []string{"ops-1", "ops-9"} {
		d, v := boxAAfterTheTransfer(t, "cargo-box-a-f1-free-", leSetting("fin.F1", "UNRESTRICTED"))
		pre, err := d.previewWith("ops-2", "fin-pre-attr", statement("H03", 10_000, 20_000, finapi.PartyPending))
		d.must("the preview", err)
		if _, err = d.submitBy(who, "fin-sub", pre.Version); err != nil {
			t.Fatalf("%s under UNRESTRICTED: %v", who, err)
		}
		p := d.pool()
		if a := p.Apportionments[1]; a.By != who || a.SettingsVersion != v || v == 0 || p.Previews[len(p.Previews)-1].SettingsVersion != v {
			t.Fatalf("%s: version %d, apportionment %+v", who, v, p.Apportionments[1])
		}
	}
	// without the setting both are refused, as before
	d, _ := boxAAfterTheTransfer(t, "cargo-box-a-f1-default-")
	pre, err := d.previewWith("ops-2", "fin-pre-attr", statement("H03", 10_000, 20_000, finapi.PartyPending))
	d.must("the preview", err)
	for _, who := range []string{"ops-1", "ops-9"} {
		_, err = d.submitBy(who, "fin-sub-"+who, pre.Version)
		said(t, who+" by default", err, "FIN_ATTRIBUTION_CONFIRMER")
	}
}

// Under NOT_ABOVE_THRESHOLD the rule applies to H03's attribution only when its former share, CNY 500.00, is above the
// threshold: with CNY 300.00 the asker is refused, naming the attribution and the amounts; with CNY 600.00 the asker
// may confirm the attribution, as may the person who transferred it; the rule of V2.91 reads the local cost of the
// line, CNY 3,000.00, so under 600.00 the asker is still stopped by that rule (the rework is not carried by anyone's
// submission) and under 3,000.00 is not.
func TestTheThresholdIsReadOnTheFormerShareWithEveryContext(t *testing.T) {
	d, v := boxAAfterTheTransfer(t, "cargo-box-a-f1-300-", leThreshold(30_000))
	pre, err := d.previewWith("ops-2", "fin-pre-attr", statement("H03", 10_000, 20_000, finapi.PartyPending))
	d.must("the preview", err)
	for _, who := range []string{"ops-1", "ops-9"} {
		_, err = d.submitBy(who, "fin-sub-"+who, pre.Version)
		said(t, who+" under 300.00", err, "FIN_ATTRIBUTION_CONFIRMER", "the attribution of house SH-H03 on line 1 is new or changed",
			"CNY 500.00 is above the threshold of CNY 300.00 set by F1 NOT_ABOVE_THRESHOLD", "another finance user submits")
	}
	if len(d.pool().Apportionments) != 1 {
		t.Fatalf("a refused submission was written: %+v", d.pool().Apportionments)
	}
	// a person who asked for nothing is accepted whatever the threshold; the version is recorded
	if _, err = d.submitBy("fin-1", "fin-sub-fin-1", pre.Version); err != nil {
		t.Fatalf("fin-1: %v", err)
	}
	if a := d.pool().Apportionments[1]; a.SettingsVersion != v {
		t.Fatalf("the version recorded: %+v", a)
	}

	// CNY 600.00: H03's former share is not above it, so neither person is barred from the attribution. ops-9, who only
	// ended H03's place, confirms it. ops-1 is stopped by the other rule, that of V2.91, which reads the local cost of
	// the line, CNY 3,000.00, and the rework nobody has carried yet; the refusal names the line and the amount, and is
	// not the attribution's.
	e, v2 := boxAAfterTheTransfer(t, "cargo-box-a-f1-600-", leThreshold(60_000))
	pre, err = e.previewWith("ops-2", "fin-pre-attr", statement("H03", 10_000, 20_000, finapi.PartyPending))
	e.must("the preview", err)
	_, err = e.submitBy("ops-1", "fin-sub-ops-1", pre.Version)
	said(t, "ops-1 under 600.00", err, "FIN_CONFIRMER_ASKED_REWORK", "ops-1 asked for rework 1 of load unit 1 (stuffing X-1)",
		"for line 1 (CNY 3,000.00 is above the threshold of CNY 600.00 set by F1 NOT_ABOVE_THRESHOLD)")
	if strings.Contains(err.Error(), "FIN_ATTRIBUTION_CONFIRMER") {
		t.Fatalf("under 600.00 the asker is not barred from the attribution: %v", err)
	}
	if _, err = e.submitBy("ops-9", "fin-sub-ops-9", pre.Version); err != nil {
		t.Fatalf("under 600.00 the person who transferred H03 confirms the attribution: %v", err)
	}
	if a := e.pool().Apportionments[1]; a.By != "ops-9" || a.SettingsVersion != v2 {
		t.Fatalf("the apportionment: %+v", a)
	}

	// CNY 3,000.00, the cost of the line, which is not above it: the asker confirms the attribution and passes the
	// rework rule
	g, v3 := boxAAfterTheTransfer(t, "cargo-box-a-f1-3000-", leThreshold(300_000))
	pre, err = g.previewWith("ops-2", "fin-pre-attr", statement("H03", 10_000, 20_000, finapi.PartyPending))
	g.must("the preview", err)
	if _, err = g.submitBy("ops-1", "fin-sub-ops-1", pre.Version); err != nil {
		t.Fatalf("under 3,000.00 the asker confirms: %v", err)
	}
	if a := g.pool().Apportionments[1]; a.By != "ops-1" || a.SettingsVersion != v3 {
		t.Fatalf("the apportionment: %+v", a)
	}
}

// Under BLOCKS C-1's close is refused while the exception cost of H03 is pending, naming the line and the way out, and
// the pending item says so; it is accepted once the party is named, with a correction that has its reason and a
// voucher; the closure records the version.
func TestBlocksRefusesTheCloseWhileTheCauseIsPendingWithEveryContext(t *testing.T) {
	d, v := boxAAfterTheTransfer(t, "cargo-box-a-f7-blocks-", leSetting("fin.F7", "BLOCKS"))
	if err := d.attribute("fin-attr-1", "fin-1", statement("H03", 10_000, 20_000, finapi.PartyPending)); err != nil {
		t.Fatal(err)
	}
	items, err := d.fin.GetConsolCostPoolPendingItems(d.ctx, finapi.GetConsolCostPoolPendingItems{TenantID: d.tenant, PoolID: "P-1"})
	d.must("the pending items", err)
	if len(items) != 1 || items[0].Kind != finapi.PendingExceptionCause || !strings.Contains(items[0].Message, "holds back the close (F7 BLOCKS)") {
		t.Fatalf("the pending items under BLOCKS: %+v", items)
	}
	said(t, "the close with the cause pending", d.closeP1("fin-close-1"), "FIN_POOL_NOT_CLOSABLE", "cost line 1", "house SH-H03", "party is PENDING", "F7 BLOCKS",
		"establish the party and correct the attribution", "with its reason and a voucher")
	fix := statement("H03", 10_000, 20_000, finapi.PartyCarrier)
	fix.Voucher, fix.Reason = "CLAIM-2", "the carrier is liable"
	if err = d.attribute("fin-attr-2", "fin-1", fix); err != nil {
		t.Fatal(err)
	}
	if err = d.closeP1("fin-close-2"); err != nil {
		t.Fatalf("the close once the party is named: %v", err)
	}
	if c := d.pool().Closures; len(c) != 1 || c[0].SettingsVersion != v || v == 0 {
		t.Fatalf("the closure: %+v (version %d)", c, v)
	}
	// the same pool under the default closes with the cause pending
	e, _ := boxAAfterTheTransfer(t, "cargo-box-a-f7-default-")
	if err = e.attribute("fin-attr-1", "fin-1", statement("H03", 10_000, 20_000, finapi.PartyPending)); err != nil {
		t.Fatal(err)
	}
	if err = e.closeP1("fin-close"); err != nil {
		t.Fatalf("the close under DOES_NOT_BLOCK: %v", err)
	}
	if c := e.pool().Closures; len(c) != 1 || c[0].SettingsVersion != 0 {
		t.Fatalf("the closure under no settings: %+v", c)
	}
}
