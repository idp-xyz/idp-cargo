package application

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// A decision on an application server whose clock is behind a change set that is already committed (the database
// tolerates a minute of skew between the two) reads that change set and is stamped at its time, not before it.
func TestADecisionOnALaggingClockReadsTheLatestCommittedChange(t *testing.T) {
	at := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	company := settings.Scope{Kind: settings.KindTenant}
	ledger := settings.NewLedger([]settings.ChangeSet{{ID: "c1", Entries: []settings.Entry{{Scope: company, Key: SettingS1, Value: sea.S1SupervisorOnly}},
		By: "ann", At: at, Effective: at, Approval: &settings.Approval{By: "ann", At: at, Version: 1, InForce: at, Auto: true}}})
	trace := &decisionTrace{}
	d := Deps{Clock: tracedClock{trace, at.Add(-20 * time.Second)}, Settings: tracedSettings{trace: trace, ledger: ledger}}
	got, err := d.DecisionSettings(context.Background(), "t1", "LE")
	if err != nil || got.S1 != sea.S1SupervisorOnly || got.Version != 1 || !got.At.Equal(at) {
		t.Fatalf("a decision 20 s behind the committed change: %+v, %v", got, err)
	}
	// a clock ahead of the ledger is the time of the decision, as before
	d.Clock = tracedClock{trace, at.Add(time.Minute)}
	if got, err = d.DecisionSettings(context.Background(), "t1", "LE"); err != nil || !got.At.Equal(at.Add(time.Minute)) {
		t.Fatalf("a clock ahead: %+v, %v", got, err)
	}
}

// The text of the legal entities that stay looser lists as many as the receipt holds, then says how many more there are
// and where to read them.
func TestTheNoticeOfAnExceptionListFitsTheReceipt(t *testing.T) {
	var parts []string
	for i := 0; i < 40; i++ {
		parts = append(parts, fmt.Sprintf("LEGAL-ENTITY-SHANGHAI-%03d keeps its own sea.S1 WITH_REASON (change set c2), looser than the company's SUPERVISOR_ONLY", i))
	}
	long := exceptionsText(parts)
	if len(long) > 4000 || !strings.Contains(long, "more legal entities; see GetSeaSettings") || !strings.HasSuffix(long, "for that legal entity") {
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
