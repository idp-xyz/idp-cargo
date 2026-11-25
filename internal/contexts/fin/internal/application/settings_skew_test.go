package application

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// A decision on a clock that is behind a change set already committed (the database tolerates a minute of skew) reads
// that change set and is stamped at its time, not before it.
func TestADecisionOnALaggingClockReadsTheLatestCommittedChange(t *testing.T) {
	at := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	ledger := settings.NewLedger([]settings.ChangeSet{{ID: "c1", Entries: []settings.Entry{{Scope: settings.TenantScope, Key: SettingF7, Value: "BLOCKS"}},
		By: "ann", At: at, Effective: at, Approval: &settings.Approval{By: "ann", At: at, Version: 1, InForce: at, Auto: true}}})
	var trace []string
	d := Deps{Clock: fixedClock{now: at.Add(-20 * time.Second)}, Settings: ledgerStore{ledger: ledger, trace: &trace}}
	rules, when, err := d.DecisionSettings(context.Background(), "t1", "LE")
	if err != nil || rules.F7 != fin.F7Blocks || rules.Version != 1 || !when.Equal(at) {
		t.Fatalf("a decision 20 s behind the committed change: %+v at %s, %v", rules, when, err)
	}
	// a query is not a decision: it reads at its own clock
	d.Clock = fixedClock{now: at.Add(-20 * time.Second)}
	if rules, err = d.RuleSettingsOf(context.Background(), "t1", "LE"); err != nil || rules.Version != 0 {
		t.Fatalf("a query on the same clock: %+v, %v", rules, err)
	}
	d.Clock = fixedClock{now: at.Add(time.Minute)}
	if _, when, err = d.DecisionSettings(context.Background(), "t1", "LE"); err != nil || !when.Equal(at.Add(time.Minute)) {
		t.Fatalf("a clock ahead: %s, %v", when, err)
	}
}

// A final match is read at FIN's clock, and never before the latest approval of a threshold in force.
func TestAMatchIsNotReadBeforeTheLatestApproval(t *testing.T) {
	at := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	p, err := fin.OpenPolicy("t1", "LE", "CNY", "MAIN", "acct-1", at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ProposeVarianceThreshold(10_000, 100, "acct-1", at, at); err != nil {
		t.Fatal(err)
	}
	if got := matchTime(at.Add(-time.Minute), p); !got.Equal(at.Add(-time.Minute)) {
		t.Fatalf("a version that waits is not an approval: %s", got)
	}
	if err = p.ApproveVarianceThreshold(1, "ctrl-1", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := matchTime(at, p); !got.Equal(at.Add(time.Second)) {
		t.Fatalf("a clock behind the approval: %s", got)
	}
	if got := matchTime(at.Add(time.Hour), p); !got.Equal(at.Add(time.Hour)) {
		t.Fatalf("a clock after the approval: %s", got)
	}
}

// The text of the legal entities that stay looser lists as many as the receipt holds, then says how many more there are
// and where to read them.
func TestTheNoticeOfAnExceptionListFitsTheReceipt(t *testing.T) {
	var parts []string
	for i := 0; i < 40; i++ {
		parts = append(parts, fmt.Sprintf("LEGAL-ENTITY-SHANGHAI-%03d keeps its own fin.F7 DOES_NOT_BLOCK (change set c2), looser than the company's BLOCKS", i))
	}
	long := exceptionsText(parts)
	if len(long) > 4000 || !strings.Contains(long, "more legal entities; see GetFinSettings") || !strings.HasSuffix(long, "for that legal entity") {
		t.Fatalf("%d characters: %s", len(long), long)
	}
	var shown int
	for _, p := range parts {
		if strings.Contains(long, p) {
			shown++
		}
	}
	if want := fmt.Sprintf("and %d more legal entities", len(parts)-shown); shown == 0 || shown == len(parts) || !strings.Contains(long, want) {
		t.Fatalf("%d shown, the text says: %s", shown, long[len(long)-140:])
	}
	if short := exceptionsText(parts[:3]); strings.Contains(short, "more legal entities") || !strings.Contains(short, parts[2]) {
		t.Fatalf("three fit: %s", short)
	}
}
