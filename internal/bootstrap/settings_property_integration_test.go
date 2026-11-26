//go:build integration

package bootstrap

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/settings/settingstest"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.102 batch C: seeded random legal configurations on PostgreSQL, with the real contexts. Each seed builds box A after the
// transfer of H03, puts a random legal configuration in force through the modules' own settings commands (proposed by the
// consultant, approved by the owner when it loosens), and runs a random sequence of operations (previews, submissions, a
// settings change in the middle, revisions of the freight, closes). After every step it checks the four invariants:
// amounts are conserved, the trail is complete (every preview and apportionment records the version the ledger says was in
// force at its recorded time), a preview of an earlier settings version is refused at submission as stale, and every
// refusal follows the convention (settingstest.Check). A failure prints its seed; replay it with FIN_PG_PROPERTY_SEED.

type propertyRun struct {
	t         *testing.T
	seed      int64
	d         *oceanDay
	sd        *settingsDay
	rng       *rand.Rand
	n         int
	versions  int // the freight's versions
	log       []string
	revisedAt int
}

func (r *propertyRun) fail(format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf("seed %d (replay with FIN_PG_PROPERTY_SEED=%d): %s\nsteps: %v", r.seed, r.seed, fmt.Sprintf(format, args...), r.log)
}

func (r *propertyRun) id(prefix string) string {
	r.n++
	return fmt.Sprintf("%s-%d", prefix, r.n)
}

// apply puts a configuration in force through the modules' settings commands: the consultant proposes, and the owner
// approves what loosens.
func (r *propertyRun) apply(c settingstest.Config) {
	r.t.Helper()
	f, s := entriesOfConfiguration(c)
	id := r.id("prop")
	rf, err := r.sd.fin.ProposeFinSettingsChange(r.sd.as("ann", consultant...), finapi.ProposeFinSettingsChange{CommandID: id + "-f", TenantID: r.d.tenant, ActorID: "ann", ChangeID: id + "-f",
		Entries: f, Reason: "random configuration " + id})
	if err != nil {
		r.fail("propose FIN's configuration %v: %v", map[string]string(c), err)
	}
	if rf.Version == 0 {
		if _, err = r.sd.fin.ApproveFinSettingsChange(r.sd.as("olga", owner...), finapi.ApproveFinSettingsChange{CommandID: id + "-fa", TenantID: r.d.tenant, ActorID: "olga",
			ChangeID: id + "-f"}); err != nil {
			r.fail("approve FIN's configuration: %v", err)
		}
	}
	rs, err := r.sd.sea.ProposeSeaSettingsChange(r.sd.as("ann", consultant...), seaapi.ProposeSeaSettingsChange{CommandID: id + "-s", TenantID: r.d.tenant, ActorID: "ann", ChangeID: id + "-s",
		Entries: s, Reason: "random configuration " + id})
	if err != nil {
		r.fail("propose SEA's configuration: %v", err)
	}
	if rs.SettingsVersion == 0 {
		if _, err = r.sd.sea.ApproveSeaSettingsChange(r.sd.as("olga", owner...), seaapi.ApproveSeaSettingsChange{CommandID: id + "-sa", TenantID: r.d.tenant, ActorID: "olga",
			ChangeID: id + "-s"}); err != nil {
			r.fail("approve SEA's configuration: %v", err)
		}
	}
	r.log = append(r.log, fmt.Sprintf("configuration %v", map[string]string(c)))
}

func (r *propertyRun) versionAt(at time.Time) int {
	got, err := r.d.fin.GetFinSettings(context.Background(), finapi.GetFinSettings{TenantID: r.d.tenant, ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", AsOf: at})
	if err != nil {
		r.fail("settings at %s: %v", at, err)
	}
	return got.Version
}

func conservedErr(la finapi.ApportionedLine) error {
	amount, local := la.ToAllocate.Minor, la.ToAllocateLocal.Minor
	for _, s := range la.Shares {
		if !s.NotUsed {
			amount, local = amount+s.Amount.Minor, local+s.Local.Minor
		}
	}
	for _, x := range la.Attributions {
		amount, local = amount+x.Loss.Minor+x.Exception.Minor, local+x.LossLocal.Minor+x.ExceptionLocal.Minor
	}
	if amount != la.Amount.Minor || local != la.Local.Minor {
		return fmt.Errorf("line %d: the parts add up to %d (%d local), the line is %d (%d local)", la.LineNo, amount, local, la.Amount.Minor, la.Local.Minor)
	}
	return nil
}

// check is the invariants of the amounts and of the trail.
func (r *propertyRun) check() {
	r.t.Helper()
	p := r.d.pool()
	for _, v := range p.Previews {
		for _, la := range v.Lines {
			if err := conservedErr(la); err != nil {
				r.fail("preview %d: %v", v.PreviewNo, err)
			}
		}
		if want := r.versionAt(v.EnteredAt); v.SettingsVersion != want {
			r.fail("preview %d entered at %s records settings version %d, the ledger says %d was in force", v.PreviewNo, v.EnteredAt.Format(time.RFC3339Nano), v.SettingsVersion, want)
		}
	}
	for _, a := range p.Apportionments {
		for _, la := range a.Lines {
			if err := conservedErr(la); err != nil {
				r.fail("apportionment %d: %v", a.ApportionmentNo, err)
			}
		}
		if want := r.versionAt(a.EnteredAt); a.SettingsVersion != want {
			r.fail("apportionment %d entered at %s records settings version %d, the ledger says %d was in force", a.ApportionmentNo, a.EnteredAt.Format(time.RFC3339Nano), a.SettingsVersion, want)
		}
	}
}

// pgTally counts what the runs did, by the first word of the step and its outcome.
var pgTally = map[string]int{}

func (r *propertyRun) refusal(what string, err error) {
	r.t.Helper()
	kind := "ok"
	if err != nil {
		kind = "refused"
		if strings.Contains(err.Error(), "FIN_PREVIEW_STALE") {
			kind = "stale"
		}
	}
	pgTally[strings.Fields(what)[0]+" "+kind]++
	r.log = append(r.log, what+": "+outcome(err))
	if err != nil {
		if cerr := settingstest.Check(err.Error()); cerr != nil {
			r.fail("%s: a refusal without its way out: %v", what, cerr)
		}
	}
}

var propertyPeople = []string{"ops-1", "ops-2", "ops-9", "fin-1", "fin-2"}

func (r *propertyRun) person() string { return propertyPeople[r.rng.Intn(len(propertyPeople))] }

func (r *propertyRun) step() {
	r.t.Helper()
	switch r.rng.Intn(6) {
	case 0, 1: // a preview, stating H03 or not
		who, id := r.person(), r.id("pre")
		var err error
		switch r.rng.Intn(3) {
		case 0:
			_, err = r.d.previewBy(who, id)
		case 1:
			_, err = r.d.previewWith(who, id, statement("H03", 10_000, 20_000, []string{finapi.PartyPending, finapi.PartyCarrier, finapi.PartyWarehouse}[r.rng.Intn(3)]))
		default:
			a := statement("H03", int64(r.rng.Intn(3))*5_000, int64(r.rng.Intn(3))*5_000, finapi.PartyCarrier)
			if a.Exception == 0 {
				a.Party = ""
			}
			a.Reason = "a correction"
			_, err = r.d.previewWith(who, id, a)
		}
		r.refusal("preview by "+who, err)
	case 2, 3: // submit the latest preview that is not submitted yet
		p := r.d.pool()
		if len(p.Previews) == 0 || p.Previews[len(p.Previews)-1].SubmittedAs != 0 {
			return
		}
		v := p.Previews[len(p.Previews)-1]
		who := r.person()
		now := r.versionAt(time.Now())
		_, err := r.d.submitBy(who, r.id("sub"), v.PreviewNo)
		if v.SettingsVersion != now && (err == nil || !strings.Contains(err.Error(), "FIN_PREVIEW_STALE")) {
			r.fail("a preview of settings version %d submitted under %d: %v", v.SettingsVersion, now, err)
		}
		r.refusal(fmt.Sprintf("submit preview %d by %s", v.PreviewNo, who), err)
	case 4: // a settings change in the middle
		r.apply(randomConfigurationOf(r.rng))
	default:
		if r.rng.Intn(2) == 0 {
			amount := int64(250_000 + r.rng.Intn(200_000))
			r.d.reviseFreight(r.id("rev"), r.versions, amount)
			r.versions++
			r.log = append(r.log, fmt.Sprintf("freight revised to %d", amount))
		} else {
			r.refusal("close", r.d.closeP1(r.id("close")))
		}
	}
}

func randomConfigurationOf(rng *rand.Rand) settingstest.Config {
	c := settingstest.Config{}
	for _, f := range settingstest.Items {
		c[f.Name] = f.Levels[rng.Intn(len(f.Levels))]
	}
	if c["fin.F8"] == "DEFAULT_TO_REMAINING" {
		c["fin.F3"] = "ONLY_WHEN_AMOUNT_MOVES"
	}
	return c
}

func TestRandomLegalConfigurationsKeepTheInvariantsOnPostgres(t *testing.T) {
	seeds := make([]int64, 20)
	for i := range seeds {
		seeds[i] = int64(i + 1)
	}
	if v := os.Getenv("FIN_PG_PROPERTY_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		seeds = []int64{n}
	}
	clear(pgTally)
	defer func() {
		t.Logf("what the runs did: %v", pgTally)
		if t.Failed() || len(seeds) == 1 {
			return
		}
		for _, want := range []string{"preview ok", "submit ok", "submit stale", "close refused"} {
			if pgTally[want] < 5 {
				t.Errorf("the runs made only %d of %q: widen the operations", pgTally[want], want)
			}
		}
	}()
	for _, seed := range seeds {
		t.Run("seed "+strconv.FormatInt(seed, 10), func(t *testing.T) {
			d, _ := boxAOpenAfterTheTransfer(t, "cargo-box-a-property-")
			r := &propertyRun{t: t, seed: seed, d: d, sd: settingsDayOn(t, d.tenant), rng: rand.New(rand.NewSource(seed)), versions: 1}
			r.apply(randomConfigurationOf(r.rng))
			steps := 8 + r.rng.Intn(8)
			for i := 0; i < steps; i++ {
				if i == steps/2 {
					r.apply(randomConfigurationOf(r.rng))
				}
				r.step()
				r.check()
			}
			// a preview made before a settings change is stale: shown outright, whatever the sequence did
			pre, err := d.previewBy("ops-2", r.id("final-pre"))
			if err != nil {
				r.refusal("the final preview", err)
				return
			}
			r.apply(randomConfigurationOf(r.rng))
			_, err = d.submitBy("fin-2", r.id("final-sub"), pre.Version)
			if err == nil || !strings.Contains(err.Error(), "FIN_PREVIEW_STALE") {
				r.fail("a preview made before a settings change was not refused as stale: %v", err)
			}
			r.refusal("the stale submission", err)
			r.check()
		})
	}
}

var _ = kernel.CurrencyCode("")
