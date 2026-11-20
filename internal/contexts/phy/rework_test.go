package phy

import (
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// The events of a rework (V2.85): their types, their payloads and their ids. The payload holds identifiers only: the
// reason of a rework, free text, is not in it, and the unit is the item of the opening, as the cancellation's is.
func TestReworkEventsMatchThePublishedContract(t *testing.T) {
	opened := domain.Event{Type: api.LoadExecutionReworkOpened, At: at, TenantID: "T", SubjectID: "X-1", ConsolidationID: "C-S", ExecutionID: "X-1", ItemNo: 1}
	reopened := domain.Event{Type: api.CargoLotReopened, At: at, TenantID: "T", SubjectID: "L-3", ShipmentID: "SH-3", LotID: "L-3", ExecutionID: "X-1", ItemNo: 1}
	other := domain.Event{Type: api.CargoLotReopened, At: at, TenantID: "T", SubjectID: "L-4", ShipmentID: "SH-3", LotID: "L-4", ExecutionID: "X-1", ItemNo: 1}
	for _, tc := range []struct {
		event     domain.Event
		want      string
		partition string
	}{
		{opened, `{"tenant_id":"T","subject_id":"X-1","consolidation_id":"C-S","execution_id":"X-1","item_no":1}`, "T:X-1"},
		{reopened, `{"tenant_id":"T","subject_id":"L-3","consolidation_id":"","shipment_id":"SH-3","lot_id":"L-3","execution_id":"X-1","item_no":1}`, "T:L-3"},
	} {
		envelope, err := postgres.PHYEnvelope("cmd-1", tc.event, at)
		if err != nil || string(envelope.Payload) != tc.want || envelope.PartitionKey != tc.partition || string(envelope.Type) != tc.event.Type {
			t.Errorf("%s: %s %s %v, want %s %s", tc.event.Type, envelope.Payload, envelope.PartitionKey, err, tc.want, tc.partition)
		}
	}
	if api.LoadExecutionReworkOpened != "LoadExecutionReworkOpened.v1" || api.CargoLotReopened != "CargoLotReopened.v1" {
		t.Fatal("the names of the new events: the domain emits these strings (TestReworkEventsAreTheTypesThePublishedContractNames)")
	}
	a, _ := postgres.PHYEnvelope("cmd-1", reopened, at)
	b, _ := postgres.PHYEnvelope("cmd-1", other, at)
	c, _ := postgres.PHYEnvelope("cmd-1", reopened, at)
	if a.ID == b.ID || a.ID != c.ID {
		t.Fatal("two lots opened again by one command have two event ids, and a retried command has the same ones")
	}
}
