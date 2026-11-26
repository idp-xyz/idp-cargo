//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

var (
	adminFin = []string{"fin:ConfigureSettings", "fin:ApproveSettings"}
	adminSea = []string{"sea:ConfigureSettings", "sea:ApproveSettings"}
)

// The declaration of a single administrator reaches FIN and SEA only through a confirmed onboarding: before the owner's
// confirmation, and for a tenant whose onboarding declared none, the proposer is refused as before; after it, the
// administrator approves their own loosening with an acknowledgement in each module. No permission but the name of the
// declaration makes one, and ConfigureSettings does not grant it.
func TestASingleAdministratorIsDeclaredOnlyByAConfirmedOnboarding(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-single-")
	dan := append(append([]string{}, adminFin...), adminSea...)
	// before the confirmation: dan makes a loosening change set and may not approve it, with or without an acknowledgement
	prep := func(tag string) string {
		t.Helper()
		_, err := d.fin.ProposeFinSettingsChange(d.as("dan", dan...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "t-" + tag,
			Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "BLOCKS"}}, Reason: "tighten " + tag})
		d.must("tighten", err)
		_, err = d.fin.ProposeFinSettingsChange(d.as("dan", dan...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "l-" + tag,
			Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}, Reason: "loosen " + tag})
		d.must("loosen", err)
		return "l-" + tag
	}
	l0 := prep("0")
	_, err := d.fin.ApproveFinSettingsChange(d.as("dan", dan...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: l0,
		RiskAcknowledgement: "I accept"})
	saidKind(t, "before any confirmation", err, finapi.ErrRejected, "SETTINGS_SAME_ACTOR", "has not declared a single administrator")
	// the onboarding is proposed after those (a tenant takes no other change set while one waits for its owner)
	if _, err = d.propose("ob-1", "STRICT", 3, true, nil); err != nil {
		t.Fatal(err)
	}
	_, err = d.fin.ApproveFinSettingsChange(d.as("dan", dan...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: l0, RiskAcknowledgement: "I accept"})
	saidKind(t, "while the onboarding waits for its owner", err, finapi.ErrRejected, "SETTINGS_ONBOARDING_WAITING", "onboarding ob-1 waits", "confirm the onboarding")
	// no permission makes the declaration but its own name, and the onboarding is the one that holds it
	for name, perms := range map[string][]string{"fin:*": {"fin:*"}, "ConfigureSettings and ApproveSettings": adminFin} {
		_, err = d.fin.DeclareFinSingleAdministrator(d.as("olga", perms...), finapi.DeclareFinSingleAdministrator{CommandID: d.command(), TenantID: d.tenant, ActorID: "olga",
			OnboardingID: "ob-1", ProposedBy: "ann"})
		saidKind(t, name, err, finapi.ErrUnauthorized)
	}
	if _, err = d.sea.DeclareSeaSingleAdministrator(d.as("olga", "sea:ConfigureSettings", "sea:*"), seaapi.DeclareSeaSingleAdministrator{CommandID: d.command(), TenantID: d.tenant,
		ActorID: "olga", OnboardingID: "ob-1", ProposedBy: "ann"}); err == nil {
		t.Fatal("SEA's declaration by a configurer")
	}
	// the confirmation declares it in both modules
	if got, err := d.confirm("ob-1"); err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the confirmation: %+v, %v", got, err)
	}
	// the confirmation tightened F7 to BLOCKS under STRICT: l0 is stale in effect but still waits; approve it as the single administrator
	_, err = d.fin.ApproveFinSettingsChange(d.as("dan", dan...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: l0})
	saidKind(t, "no acknowledgement", err, finapi.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT")
	r, err := d.fin.ApproveFinSettingsChange(d.as("dan", dan...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: l0,
		RiskAcknowledgement: "I am the customer's only administrator and I accept the risk."})
	if err != nil || r.Version == 0 {
		t.Fatalf("FIN's self approval: %+v, %v", r, err)
	}
	if f := d.finValues("TENANT", ""); f["fin.F7"] != "DOES_NOT_BLOCK" {
		t.Fatalf("F7: %s", f["fin.F7"])
	}
	// SEA the same: S1 is SUPERVISOR_ONLY after STRICT; loosen it, and approve it as the same person
	_, err = d.sea.ProposeSeaSettingsChange(d.as("dan", dan...), seaapi.ProposeSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "s-l",
		Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: "WITH_REASON"}}, Reason: "loosen S1"})
	d.must("SEA's loosening", err)
	_, err = d.sea.ApproveSeaSettingsChange(d.as("dan", dan...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "s-l"})
	saidKind(t, "SEA without an acknowledgement", err, seaapi.ErrRejected, "SETTINGS_ACKNOWLEDGEMENT")
	if _, err = d.sea.ApproveSeaSettingsChange(d.as("dan", dan...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "s-l",
		RiskAcknowledgement: "same administrator, same risk"}); err != nil {
		t.Fatalf("SEA's self approval: %v", err)
	}
	// a tenant whose onboarding declared none is refused, whatever the acknowledgement
	other := newSettingsDay(t, "cargo-onb-nosingle-")
	if _, err = other.propose("ob-1", "STRICT", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = other.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	id := prepOn(other, dan)
	_, err = other.fin.ApproveFinSettingsChange(other.as("dan", dan...), finapi.ApproveFinSettingsChange{CommandID: other.command(), TenantID: other.tenant, ActorID: "dan", ChangeID: id,
		RiskAcknowledgement: "I accept"})
	saidKind(t, "a tenant that declared none", err, finapi.ErrRejected, "SETTINGS_SAME_ACTOR", "has not declared")
}

func prepOn(d *settingsDay, perms []string) string {
	_, err := d.fin.ProposeFinSettingsChange(d.as("dan", perms...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "l-x",
		Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}, Reason: "loosen"})
	d.must("loosen", err)
	return "l-x"
}

// The exception report lists, for a holder of a module's ApproveSettings and for that module's part, each kind of line: the
// approved legal-entity exceptions looser than the company, the loosenings a single administrator approved with their
// acknowledgements, every item now in force looser than STANDARD with who proposed and who approved it, and the actors who
// in the period both changed settings and did FIN's work (here fixed by a fake, since the apportionments need box A; the
// FIN query that reads them is tested on box A below).
func TestTheExceptionReportListsEachKindOfLine(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-report-")
	d.legalEntity()
	dan := append(append([]string{}, adminFin...), adminSea...)
	if _, err := d.propose("ob-1", "STRICT", 3, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	// an exception: LE keeps F1 UNRESTRICTED looser than the company's NOT_ALLOWED, proposed by ann and approved by olga
	_, err := d.fin.ProposeFinSettingsChange(d.as("ann", consultant...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "ann", ChangeID: "x-1",
		Entries: []finapi.FinSettingEntry{{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "fin.F1", Value: "UNRESTRICTED"}}, Reason: "the legal entity has two clerks"})
	d.must("the exception", err)
	_, err = d.fin.ApproveFinSettingsChange(d.as("olga", owner...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "olga", ChangeID: "x-1"})
	d.must("approving it", err)
	// a self approval by the single administrator: F3 to the looser value
	_, err = d.fin.ProposeFinSettingsChange(d.as("dan", dan...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "s-1",
		Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F3", Value: "ONLY_WHEN_AMOUNT_MOVES"}}, Reason: "fewer vouchers"})
	d.must("the loosening", err)
	_, err = d.fin.ApproveFinSettingsChange(d.as("dan", dan...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "s-1",
		RiskAcknowledgement: "I accept fewer vouchers."})
	d.must("approving it as the administrator", err)

	now := time.Now().UTC()
	rep, err := d.tools.GetExceptionReport(d.as("olga", owner...), d.tenant, "olga", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || rep.Fin == nil || rep.Sea == nil {
		t.Fatalf("the report: %+v, %v", rep, err)
	}
	if len(rep.Fin.Exceptions) != 1 || rep.Fin.Exceptions[0].ScopeID != "LE" || rep.Fin.Exceptions[0].Key != "fin.F1" || rep.Fin.Exceptions[0].ThanValue != "NOT_ALLOWED" ||
		rep.Fin.Exceptions[0].ProposedBy != "ann" || rep.Fin.Exceptions[0].ApprovedBy != "olga" {
		t.Fatalf("exceptions: %+v", rep.Fin.Exceptions)
	}
	if len(rep.Fin.SelfApproved) != 1 || rep.Fin.SelfApproved[0].ApprovedBy != "dan" || rep.Fin.SelfApproved[0].Acknowledgement != "I accept fewer vouchers." {
		t.Fatalf("self approved: %+v", rep.Fin.SelfApproved)
	}
	var looser []string
	for _, l := range rep.Fin.LooserThanStandard {
		looser = append(looser, l.ScopeKind+"/"+l.ScopeID+" "+l.Key+"="+l.Value+" by "+l.ProposedBy+"/"+l.ApprovedBy)
	}
	want := []string{"LEGAL_ENTITY/LE fin.F1=UNRESTRICTED by ann/olga", "TENANT/ fin.F3=ONLY_WHEN_AMOUNT_MOVES by dan/dan"}
	if strings.Join(looser, ";") != strings.Join(want, ";") {
		t.Fatalf("looser than standard: %v, want %v", looser, want)
	}
	if len(rep.Fin.Declarations) != 1 || len(rep.Sea.Declarations) != 1 || len(rep.Sea.LooserThanStandard) != 0 || len(rep.Sea.Exceptions) != 0 {
		t.Fatalf("declarations and SEA's part: %+v %+v", rep.Fin.Declarations, rep.Sea)
	}
	if len(rep.Both) != 0 {
		t.Fatalf("nobody did FIN's work: %+v", rep.Both)
	}
	// each module's part goes to a holder of that module's ApproveSettings only; with neither the report is refused
	finOnly, err := d.tools.GetExceptionReport(d.as("bob", "fin:ApproveSettings"), d.tenant, "bob", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || finOnly.Fin == nil || finOnly.Sea != nil {
		t.Fatalf("a holder of fin:ApproveSettings: %+v, %v", finOnly, err)
	}
	seaOnly, err := d.tools.GetExceptionReport(d.as("bob", "sea:ApproveSettings"), d.tenant, "bob", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || seaOnly.Sea == nil || seaOnly.Fin != nil {
		t.Fatalf("a holder of sea:ApproveSettings: %+v, %v", seaOnly, err)
	}
	for name, perms := range map[string][]string{"the wildcards": {"*", "fin:*", "sea:*"}, "ConfigureSettings": {"fin:ConfigureSettings", "sea:ConfigureSettings"}, "none": nil} {
		_, err = d.tools.GetExceptionReport(d.as("bob", perms...), d.tenant, "bob", now.Add(-time.Hour), now.Add(time.Hour))
		saidKind(t, name, err, orgapi.ErrUnauthorized, "fin:ApproveSettings or sea:ApproveSettings")
	}
	// a period before everything lists no self approval and no changer
	past, err := d.tools.GetExceptionReport(d.as("olga", owner...), d.tenant, "olga", now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	if err != nil || len(past.Fin.SelfApproved) != 0 || len(past.Fin.Changers) != 0 || len(past.Fin.Exceptions) != 1 {
		t.Fatalf("a past period: %+v, %v", past, err)
	}
}

// Who both changed settings and submitted an apportionment or confirmed an attribution, from the records of box A: ops-9
// proposed a change and confirmed H03's attribution; fin-1 submitted the first apportionment and changed no setting;
// cfg-1 changed settings and submitted nothing.
func TestTheExceptionReportNamesWhoChangedSettingsAndDidFinsWork(t *testing.T) {
	d, _ := boxAAfterTheTransfer(t, "cargo-box-a-report-", leSetting("fin.F1", "UNRESTRICTED"))
	pre, err := d.previewWith("ops-2", "fin-pre-attr", statement("H03", 10_000, 20_000, finapi.PartyPending))
	d.must("the preview", err)
	_, err = d.submitBy("ops-9", "fin-sub-attr", pre.Version)
	d.must("the confirmation by ops-9", err)
	// a later apportionment carries H03's attribution forward: its submitter confirmed nothing
	pre2, err := d.previewBy("ops-2", "fin-pre-carry")
	d.must("the second preview", err)
	_, err = d.submitBy("ops-3", "fin-sub-carry", pre2.Version)
	d.must("the carrying submission by ops-3", err)
	_, err = d.fin.ProposeFinSettingsChange(d.ctx, finapi.ProposeFinSettingsChange{CommandID: "ops9-set", TenantID: d.tenant, ActorID: "ops-9", ChangeID: "ops9-chg",
		Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "BLOCKS"}}, Reason: "tighten"})
	d.must("ops-9 changes a setting", err)
	tools, err := NewSettingsTools(d.fin, noSea{})
	d.must("the tools", err)
	now := time.Now().UTC()
	rep, err := tools.GetExceptionReport(d.ctx, d.tenant, "olga", now.Add(-time.Hour), now.Add(time.Hour))
	d.must("the report", err)
	if len(rep.Both) != 1 || rep.Both[0].ActorID != "ops-9" || rep.Both[0].Proposed != 1 || rep.Both[0].Approved != 1 || rep.Both[0].Submitted != 1 || rep.Both[0].Confirmed != 1 {
		t.Fatalf("both: %+v", rep.Both)
	}
	var work []string
	for _, w := range rep.Fin.Work {
		work = append(work, w.ActorID)
	}
	for _, w := range rep.Fin.Work {
		if w.ActorID == "ops-3" && (w.Submitted != 1 || w.Confirmed != 0) {
			t.Fatalf("ops-3 only carried an attribution forward: %+v", w)
		}
	}
	if strings.Join(work, ",") != "fin-1,ops-3,ops-9" {
		t.Fatalf("FIN's work: %v", rep.Fin.Work)
	}
	// outside the period nothing is both
	past, err := tools.GetExceptionReport(d.ctx, d.tenant, "olga", now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	if err != nil || len(past.Both) != 0 {
		t.Fatalf("a past period: %+v, %v", past.Both, err)
	}
}

// noSea is a SEA that answers an empty part, for the day that has FIN only.
type noSea struct{ SEASettings }

func (noSea) GetSeaExceptionReport(context.Context, seaapi.GetSeaExceptionReport) (seaapi.SeaExceptionReport, error) {
	return seaapi.SeaExceptionReport{}, nil
}

// An exported configuration, read on another tenant, answers the entries that differ, as entries a proposal takes: they
// propose cleanly in each module, and what is then in force equals the export. An item or value this environment does
// not know is listed as unknown, with the way out, and refused; the import proposes nothing by itself.
func TestAnExportedConfigurationDiffsOnAnotherTenantAndProposesCleanly(t *testing.T) {
	src := newSettingsDay(t, "cargo-export-src-")
	src.legalEntity()
	if _, err := src.propose("ob-1", "STRICT", 3, false, map[string][]orgapi.OnboardingEntry{
		"FIN": {{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "fin.F1", Value: "NOT_ABOVE_THRESHOLD", ThresholdMinor: ptr(50_000), ThresholdCurrency: "CNY"}}}); err != nil {
		t.Fatal(err)
	}
	if got, err := src.confirm("ob-1"); err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the source: %+v, %v", got, err)
	}
	file, err := src.tools.ExportSettingsJSON(src.as("ann", consultant...), src.tenant, "ann")
	if err != nil {
		t.Fatal(err)
	}
	var exp SettingsExport
	if err = json.Unmarshal(file, &exp); err != nil || exp.Format != ExportFormat || exp.Modules[ModuleFIN].CatalogueVersion == "" || exp.Modules[ModuleSEA].CatalogueVersion == "" ||
		exp.Modules[ModuleFIN].SettingsVersion != 1 {
		t.Fatalf("the export: %s, %v", file, err)
	}
	if !strings.Contains(string(file), `"thresholdMinor": 50000`) || !strings.Contains(string(file), `"scopeId": "LE"`) {
		t.Fatalf("the legal entity's threshold is exported: %s", file)
	}
	// a caller who sees FIN only gets FIN's part, and the answer says SEA was left out and why (the new tests below read it)
	if part, err := src.tools.ExportSettings(src.as("ann", "fin:ConfigureSettings"), src.tenant, "ann"); err != nil || len(part.Modules) != 1 || len(part.LeftOut) != 1 || part.LeftOut[0].Module != ModuleSEA {
		t.Fatalf("an export by someone who holds no name in SEA: %+v, %v", part, err)
	}

	dst := newSettingsDay(t, "cargo-export-dst-")
	dst.legalEntity()
	diff, err := dst.tools.DiffSettingsImport(dst.as("ann", consultant...), dst.tenant, "ann", file)
	if err != nil || len(diff.Unknown) != 0 || len(diff.Notes) != 0 {
		t.Fatalf("the diff: %+v, %v", diff, err)
	}
	// STRICT differs from the defaults in F2, F7, F9 and S1; the legal entity's threshold is one more
	if len(diff.Fin) != 4 || len(diff.Sea) != 1 || diff.Sea[0].Value != "SUPERVISOR_ONLY" {
		t.Fatalf("entries: %+v %+v", diff.Fin, diff.Sea)
	}
	if f, s := dst.count(); f != 0 || s != 0 {
		t.Fatal("the diff proposed something")
	}
	// they propose cleanly through the modules' commands, and the result equals the export
	r, err := dst.fin.ProposeFinSettingsChange(dst.as("ann", consultant...), finapi.ProposeFinSettingsChange{CommandID: "imp-f", TenantID: dst.tenant, ActorID: "ann", ChangeID: "imp-fin",
		Entries: diff.Fin, Reason: "import"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 0 {
		t.Fatalf("a legal entity's threshold is looser than the company's, so it waits for a second person: %+v", r)
	}
	if _, err = dst.fin.ApproveFinSettingsChange(dst.as("bob", "fin:ApproveSettings"), finapi.ApproveFinSettingsChange{CommandID: "imp-fa", TenantID: dst.tenant, ActorID: "bob",
		ChangeID: "imp-fin"}); err != nil {
		t.Fatal(err)
	}
	if _, err = dst.sea.ProposeSeaSettingsChange(dst.as("ann", consultant...), seaapi.ProposeSeaSettingsChange{CommandID: "imp-s", TenantID: dst.tenant, ActorID: "ann", ChangeID: "imp-sea",
		Entries: diff.Sea, Reason: "import"}); err != nil {
		t.Fatal(err)
	}
	again, err := dst.tools.DiffSettingsImport(dst.as("ann", consultant...), dst.tenant, "ann", file)
	if err != nil || len(again.Fin) != 0 || len(again.Sea) != 0 || len(again.Unknown) != 0 {
		t.Fatalf("after the import nothing differs: %+v, %v", again, err)
	}
	// an unknown item, an unknown value and an unknown module are refused with the way out; the rest still diffs
	exp.Modules[ModuleFIN] = ExportedModule{CatalogueVersion: "old", Scopes: []ExportedScope{{ScopeKind: "TENANT", Items: []ExportedItem{
		{Key: "fin.F42", Value: "X"}, {Key: "fin.F7", Value: "NO_SUCH"}, {Key: "fin.F6", Value: "INCLUDED_MARKED"}}}}}
	exp.Modules["AIR"] = ExportedModule{}
	bad, _ := json.Marshal(exp)
	diff, err = dst.tools.DiffSettingsImport(dst.as("ann", consultant...), dst.tenant, "ann", bad)
	if err != nil || len(diff.Unknown) != 3 || len(diff.Fin) != 1 || diff.Fin[0].Key != "fin.F6" || len(diff.Notes) != 1 {
		t.Fatalf("the diff of a file with unknowns: %+v, %v", diff, err)
	}
	for _, u := range diff.Unknown {
		if !strings.Contains(u.Reason, "upgrade this environment") && !strings.Contains(u.Reason, "remove it from the file") {
			t.Errorf("an unknown item without a way out: %+v", u)
		}
	}
	// a file that is not an export, and one of another format
	for name, in := range map[string]string{"not JSON": "nope", "another format": `{"format":"x/2"}`, "an unknown field": `{"format":"cargo-settings/1","extra":1}`} {
		if _, err = dst.tools.DiffSettingsImport(dst.as("ann", consultant...), dst.tenant, "ann", []byte(in)); err == nil || !strings.Contains(err.Error(), "export") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err = dst.tools.DiffSettingsImport(context.Background(), dst.tenant, "ann", file); err == nil {
		t.Fatal("a diff without a principal")
	}
}

func ptr(n int64) *int64 { return &n }
