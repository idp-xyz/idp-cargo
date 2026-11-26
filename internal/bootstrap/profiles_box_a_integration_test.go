//go:build integration

package bootstrap

import (
	"fmt"
	"strings"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.102 batch C: box A end to end under each profile, with the real ORG, RATE, COM, FUL, PHY, FIN and SEA on PostgreSQL,
// through onboarding: the consultant (ann) proposes the profile and the owner (olga) confirms it, and the modules' own
// settings commands put it in force. The day is box A of the owner's V2.91 to V2.99 tests: H03 leaves C-1 after a rework
// ops-1 asked for, and ops-9 transfers it; its former share is CNY 500.00.

// onboardTenantOf onboards the tenant of a day with the profile through the real onboarding, and answers the day's settings.
func onboardTenantOf(t *testing.T, tenant, id, profile string) *settingsDay {
	t.Helper()
	sd := settingsDayOn(t, tenant)
	if _, err := sd.propose(id, profile, 3, false, nil); err != nil {
		t.Fatalf("the proposal of %s: %v", profile, err)
	}
	got, err := sd.confirm(id)
	if err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the confirmation of %s: %+v, %v", profile, got, err)
	}
	return sd
}

func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// standardFlow is what a finance team does with box A under the standard rules (V2.99), as a transcript of the outcomes.
func standardFlow(t *testing.T, d *oceanDay) (transcript []string, apportionmentVersion, closureVersion int) {
	t.Helper()
	add := func(what string, err error) { transcript = append(transcript, what+": "+outcome(err)) }
	_, err := d.previewBy("ops-2", "std-missing")
	said(t, "no statement", err, "FIN_ATTRIBUTION_MISSING", "house SH-H03", "state who bears it")
	add("no statement", err)
	pre, err := d.previewWith("ops-1", "std-pre", statement("H03", 10_000, 20_000, finapi.PartyPending))
	d.must("the asker previews H03's attribution", err)
	for _, who := range []string{"ops-1", "ops-9"} {
		_, err = d.submitBy(who, "std-sub-"+who, pre.Version)
		said(t, who, err, "FIN_ATTRIBUTION_CONFIRMER", "another finance user submits")
		add(who+" submits", err)
	}
	// F2 SUBMITTER_ONLY: the previewer is not checked, so another person submits the asker's preview
	_, err = d.submitBy("fin-2", "std-sub-ok", pre.Version)
	d.must("another person submits the asker's preview", err)
	add("fin-2 submits", err)
	a := d.pool().Apportionments[1]
	conservedBox(t, "the attribution", a.Lines[0])
	apportionmentVersion = a.SettingsVersion
	_, err = d.oneStep("fin-2", "std-one", 1)
	said(t, "F4 default", err, "FIN_ONE_STEP_REFUSED", "preview the lines and submit the preview", "ask a configurer")
	add("one step", err)
	// F7 DOES_NOT_BLOCK: the pending cause does not hold back the close
	err = d.closeP1("std-close")
	d.must("the close with a pending cause", err)
	add("close", err)
	p := d.pool()
	if len(p.Closures) != 1 {
		t.Fatalf("the closures: %+v", p.Closures)
	}
	return transcript, apportionmentVersion, p.Closures[0].SettingsVersion
}

// STANDARD behaves as V2.99 does: a tenant onboarded with the STANDARD profile has every refusal and every outcome of a tenant
// that never set anything, and its decisions record the settings version the onboarding made (1, against 0).
func TestBoxAUnderTheStandardProfileBehavesAsV299Does(t *testing.T) {
	plain, _ := boxAAfterTheTransfer(t, "cargo-box-a-std-plain-")
	want, aV, cV := standardFlow(t, plain)
	if aV != 0 || cV != 0 {
		t.Fatalf("with nothing set the versions are 0: %d, %d", aV, cV)
	}
	d, _ := boxAAfterTheTransfer(t, "cargo-box-a-std-onboarded-")
	sd := onboardTenantOf(t, d.tenant, "ob-std", "STANDARD")
	if got := sd.finValues("TENANT", ""); got["version"] != "1" || got["fin.F2"] != "SUBMITTER_ONLY" || got["fin.F7"] != "DOES_NOT_BLOCK" {
		t.Fatalf("FIN's settings: %+v", got)
	}
	got, aV, cV := standardFlow(t, d)
	if aV != 1 || cV != 1 {
		t.Fatalf("the decisions record the onboarding's version: %d, %d", aV, cV)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the onboarded tenant differs from V2.99:\n%s\nagainst\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// STRICT: the previewer is checked as well as the submitter; a pending cause holds back C-1's close until the party is
// established; after the freight is revised H03's attribution must be stated again; and a carrier's conflict is released
// only by a supervisor.
func TestBoxAUnderTheStrictProfile(t *testing.T) {
	t.Run("the previewer is checked and a pending cause holds back the close", func(t *testing.T) {
		d, _ := boxAAfterTheTransfer(t, "cargo-box-a-strict-")
		sd := onboardTenantOf(t, d.tenant, "ob-strict", "STRICT")
		if got := sd.finValues("TENANT", ""); got["version"] != "1" || got["fin.F2"] != "SUBMITTER_AND_PREVIEWER" || got["fin.F7"] != "BLOCKS" || got["fin.F9"] != "RECONFIRM" {
			t.Fatalf("FIN's settings: %+v", got)
		}
		pre, err := d.previewWith("ops-1", "strict-pre", statement("H03", 10_000, 20_000, finapi.PartyPending))
		d.must("the asker previews", err)
		_, err = d.submitBy("fin-2", "strict-sub", pre.Version)
		said(t, "the asker's preview", err, "FIN_ATTRIBUTION_CONFIRMER", "the previewer ops-1", "F2 SUBMITTER_AND_PREVIEWER", "preview again as another finance user")
		if len(d.pool().Apportionments) != 1 {
			t.Fatal("a refused submission wrote an apportionment")
		}
		pre, err = d.previewWith("ops-2", "strict-pre-2", statement("H03", 10_000, 20_000, finapi.PartyPending))
		d.must("another person previews", err)
		if _, err = d.submitBy("fin-2", "strict-sub-2", pre.Version); err != nil {
			t.Fatalf("a preview by another person: %v", err)
		}
		if v := d.pool().Apportionments[1].SettingsVersion; v != 1 {
			t.Fatalf("the apportionment records version %d", v)
		}
		err = d.closeP1("strict-close")
		said(t, "the close with a pending cause", err, "FIN_POOL_NOT_CLOSABLE", "cost line 1", "house SH-H03", "F7 BLOCKS", "establish the party and correct the attribution")
		if p := d.pool(); len(p.Closures) != 0 {
			t.Fatalf("a refused close: %+v", p.Closures)
		}
		fix := statement("H03", 10_000, 20_000, finapi.PartyCarrier)
		fix.Reason = "the carrier is liable"
		fix.Voucher = "CLAIM-2"
		pre, err = d.previewWith("ops-2", "strict-fix", fix)
		d.must("the correction", err)
		if _, err = d.submitBy("fin-2", "strict-fix-sub", pre.Version); err != nil {
			t.Fatal(err)
		}
		d.must("the close once the party is established", d.closeP1("strict-close-2"))
		if p := d.pool(); len(p.Closures) != 1 || p.Closures[0].SettingsVersion != 1 {
			t.Fatalf("the closure: %+v", p.Closures)
		}
	})
	t.Run("the attribution is stated again after the freight is revised", func(t *testing.T) {
		d, _ := boxAOpenAfterTheTransfer(t, "cargo-box-a-strict-f9-")
		sd := onboardTenantOf(t, d.tenant, "ob-strict", "STRICT")
		_ = sd
		pre, err := d.previewWith("ops-2", "strict-pre", statement("H03", 10_000, 20_000, finapi.PartyWarehouse))
		d.must("the attribution is previewed", err)
		if _, err = d.submitBy("fin-2", "strict-sub", pre.Version); err != nil {
			t.Fatal(err)
		}
		d.reviseFreight("strict-revise", 1, 360_000)
		_, err = d.previewBy("fin-2", "strict-again")
		said(t, "RECONFIRM", err, "FIN_ATTRIBUTION_RECONFIRM", "house SH-H03", "line 1", "CNY 3,000.00", "CNY 3,600.00", "state the attribution of house SH-H03 on line 1 again")
		pre, err = d.previewWith("ops-2", "strict-restate", statement("H03", 10_000, 20_000, finapi.PartyWarehouse))
		d.must("the attribution stated again", err)
		if _, err = d.submitBy("fin-2", "strict-restate-sub", pre.Version); err != nil {
			t.Fatal(err)
		}
		la := d.lastLine(1)
		conservedBox(t, "the restated line", la)
		if la.Amount.Minor != 360_000 || la.Attributions[0].Carried {
			t.Fatalf("the line: %+v", la)
		}
	})
}

// STRICT's S1: on the conflict day of SEA's V2.100 tests, a carrier's contradiction is released only by a supervisor.
func TestBoxAUnderTheStrictProfileASupervisorReleasesACarrierConflict(t *testing.T) {
	d := conflictDayUnderPermissions(t, "cargo-ocean-strict-s1-")
	onboardTenantOf(t, d.tenant, "ob-strict", "STRICT")
	if v, version := d.s1("LE"); v != "SUPERVISOR_ONLY" || version != 1 {
		t.Fatalf("S1: %s, version %d", v, version)
	}
	_, err := d.release("ops-9", ops, "acc-1", 33)
	said(t, "an operator", err, "SuperviseCarrierConflicts", "ask a holder")
	if _, err = d.release("sup-1", sup, "acc-2", 34); err != nil {
		t.Fatalf("a supervisor: %v", err)
	}
	if rel := d.cv().Releases[0]; rel.RecordedBy != "sup-1" || rel.S1 != "SUPERVISOR_ONLY" || rel.SettingsVersion != 1 {
		t.Fatalf("the release: %+v", rel)
	}
}

// LEAN: ops-1, who asked for the rework, may submit; H03's default goes to the cargo that remains; a second line is
// apportioned in one step; the week's after-the-fact list shows the default, and another person marks the week reviewed.
func TestBoxAUnderTheLeanProfile(t *testing.T) {
	d, _ := boxAAfterTheTransfer(t, "cargo-box-a-lean-onboarded-")
	sd := onboardTenantOf(t, d.tenant, "ob-lean", "LEAN")
	got := sd.finValues("TENANT", "")
	if got["version"] != "1" || got["fin.F1"] != "UNRESTRICTED" || got["fin.F3"] != "ONLY_WHEN_AMOUNT_MOVES" || got["fin.F4"] != "ONE_STEP_WITHOUT_OPENED_OR_LEFT" || got["fin.F8"] != "DEFAULT_TO_REMAINING" {
		t.Fatalf("FIN's settings: %+v", got)
	}
	// the loosening waited for the owner, who approved it
	if c, ok := sd.finChange("onb-ob-lean-fin"); !ok || c.ProposedBy != "ann" || c.ApprovedBy != "olga" || !c.Loosening {
		t.Fatalf("FIN's change set: %+v", c)
	}
	pre, err := d.previewBy("ops-1", "lean-pre")
	d.must("ops-1 previews without stating H03", err)
	if _, err = d.submitBy("ops-1", "lean-sub", pre.Version); err != nil {
		t.Fatalf("ops-1 submits under F1 UNRESTRICTED: %v", err)
	}
	a := d.pool().Apportionments[1]
	if a.By != "ops-1" || a.SettingsVersion != 1 || len(a.Lines[0].Attributions) != 1 || !a.Lines[0].Attributions[0].ByDefault {
		t.Fatalf("the default: %+v", a)
	}
	la := d.lastLine(1)
	conservedBox(t, "the default", la)
	for _, s := range la.Shares {
		if s.ShipmentID == "SH-H01" && s.Amount.Minor != 120_000 || s.ShipmentID == "SH-H02" && s.Amount.Minor != 180_000 {
			t.Fatalf("H03's CNY 500.00 goes to the cargo that remains: %+v", la.Shares)
		}
	}
	// a second line in one step
	_, err = d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "lean-handling", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		ChargeCode: "HANDLING", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 100_000}},
		Reason: "the handling at the origin", OccurredAt: d.later()})
	d.must("the second cost line", err)
	previews := len(d.pool().Previews)
	r, err := d.oneStep("fin-2", "lean-one-step", 2)
	if err != nil {
		t.Fatalf("the second line in one step: %v", err)
	}
	p := d.pool()
	one := p.Apportionments[len(p.Apportionments)-1]
	if int(r.Version) != one.ApportionmentNo || one.PreviewNo != 0 || len(p.Previews) != previews || one.SettingsVersion != 1 {
		t.Fatalf("the one step: %+v", one)
	}
	conservedBox(t, "the second line", d.lastLine(2))
	// the week's list shows the default with all its reasons; another person marks the week reviewed
	y, w := a.EnteredAt.UTC().ISOWeek()
	week := fmt.Sprintf("%04d-W%02d", y, w)
	list, err := d.fin.GetAfterTheFactReview(d.ctx, finapi.GetAfterTheFactReview{TenantID: d.tenant, ActorID: "head-1", LegalEntityID: "LE", Week: week, Limit: 10})
	d.must("the week's list", err)
	if len(list.Lines) != 1 || list.Lines[0].ShipmentID != "SH-H03" || list.Lines[0].SubmittedBy != "ops-1" || list.Review.Reviewed ||
		strings.Join(list.Lines[0].Reasons, ",") != finapi.ReviewReasonNoSecondPerson+","+finapi.ReviewReasonNoVoucher+","+finapi.ReviewReasonMadeByDefault {
		t.Fatalf("the week's list: %+v", list)
	}
	review := d.finAt(weekEnd(a.EnteredAt).Add(time.Hour)) // a week is reviewed once it has ended
	mark := func(id, who string) (finapi.Receipt, error) {
		return review.MarkAfterTheFactReviewed(d.ctx, finapi.MarkAfterTheFactReviewed{CommandID: id, TenantID: d.tenant, ActorID: who, LegalEntityID: "LE", Week: week, Conclusion: "ACCEPTED",
			Note: "the default of H03 is right: the line's whole cost stays with the cargo that remains"})
	}
	_, err = mark("lean-review-ops-1", "ops-1")
	said(t, "the person who made it", err, "FIN_AFTER_THE_FACT_REVIEWER", "another person reviews it")
	if _, err = mark("lean-review", "head-1"); err != nil {
		t.Fatalf("another person reviews the week: %v", err)
	}
	list, err = d.fin.GetAfterTheFactReview(d.ctx, finapi.GetAfterTheFactReview{TenantID: d.tenant, ActorID: "head-1", LegalEntityID: "LE", Week: week, Limit: 10})
	if d.must("the week's list again", err); !list.Review.Reviewed || list.Review.ReviewedBy != "head-1" || !list.Review.Matches {
		t.Fatalf("the week reviewed: %+v", list.Review)
	}
}
