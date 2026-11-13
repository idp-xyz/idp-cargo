package application

import (
	"testing"
	"time"

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// Commands recorded before PHY V0.3 hash as they did: the fields V0.3 added are left out when empty.
func TestRequestHashesKeepTheirFormerShape(t *testing.T) {
	at := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	scope := Scope{TenantID: "T", ActorID: "ops"}
	type closeBefore struct {
		CommandID        string
		Scope            Scope
		ConsolidationID  phy.ConsolidationID
		ExpectedRevision int64
		OccurredAt       time.Time
	}
	type openBefore struct {
		CommandID       string
		Scope           Scope
		ExecutionID     phy.LoadExecutionID
		ConsolidationID phy.ConsolidationID
		Kind            phy.ExecutionKind
		Facility        phy.Facility
		OccurredAt      time.Time
	}
	for name, pair := range map[string][2]any{
		"close": {CloseConsolidationCommand{CommandID: "c", Scope: scope, ConsolidationID: "C-1", ExpectedRevision: 2, OccurredAt: at},
			closeBefore{CommandID: "c", Scope: scope, ConsolidationID: "C-1", ExpectedRevision: 2, OccurredAt: at}},
		"open a tender": {OpenLoadExecutionCommand{CommandID: "o", Scope: scope, ExecutionID: "X-1", ConsolidationID: "C-1", Kind: phy.ExecutionTender,
			OccurredAt: at}, openBefore{CommandID: "o", Scope: scope, ExecutionID: "X-1", ConsolidationID: "C-1", Kind: phy.ExecutionTender, OccurredAt: at}},
	} {
		now, err := requestHash(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		before, err := requestHash(pair[1])
		if err != nil || now != before {
			t.Errorf("%s: %s, before %s (%v)", name, now, before, err)
		}
	}
	with, _ := requestHash(CloseConsolidationCommand{CommandID: "c", Scope: scope, ConsolidationID: "C-1", ExpectedRevision: 2, OverrunReason: "x", OccurredAt: at})
	without, _ := requestHash(closeBefore{CommandID: "c", Scope: scope, ConsolidationID: "C-1", ExpectedRevision: 2, OccurredAt: at})
	if with == without {
		t.Error("an overrun reason is part of the hash")
	}
}
