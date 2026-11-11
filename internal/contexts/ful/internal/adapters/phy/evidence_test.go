package phy

import (
	"context"
	"errors"
	"testing"

	app "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type evidenceStub struct {
	got phyapi.PhysicalLoadEvidence
	err error
}

func (s evidenceStub) GetPhysicalLoadEvidence(context.Context, phyapi.GetPhysicalLoadEvidence) (phyapi.PhysicalLoadEvidence, error) {
	return s.got, s.err
}

func TestEvidenceIsSummedAsFULKeepsIt(t *testing.T) {
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	m3 := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "MTQ", Amount: n} }
	got := phyapi.PhysicalLoadEvidence{TenantID: "T", ShipmentID: "SH-1",
		Lots: []phyapi.LotEvidence{
			{LotID: "L-1", Completed: true, ReceivedPacks: 4, Measurement: phyapi.Measurement{Version: 2, GrossWeight: kg(200), Volume: m3(1)},
				Screening: phyapi.Screening{ResultNo: 1, Outcome: phyapi.ScreeningPassed}},
			{LotID: "L-2", Completed: true, ReceivedPacks: 2, Measurement: phyapi.Measurement{Version: 1, GrossWeight: kg(110), Volume: m3(2)},
				Screening: phyapi.Screening{ResultNo: 1, Outcome: phyapi.ScreeningNotRequired}, OpenDiscrepancies: []phyapi.Discrepancy{{DiscrepancyNo: 1}}},
		},
		Tenders: []phyapi.TenderEvidence{
			{ExecutionID: "X-1", ConsolidationID: "C-1", State: phyapi.ExecutionClosed, LoadedPacks: 6, GrossWeight: kg(312), Volume: m3(3), Departed: 6},
			{ExecutionID: "X-2", ConsolidationID: "C-1", State: phyapi.ExecutionLoading, LoadedPacks: 2, GrossWeight: kg(100)}, // not handed over yet
			{ExecutionID: "X-0", ConsolidationID: "C-0", LoadedPacks: 0, OpenDiscrepancies: []phyapi.LoadingDiscrepancy{{DiscrepancyNo: 1}}},
		}}
	e, err := Evidence{PHY: evidenceStub{got: got}}.Evidence(context.Background(), "T", "SH-1", "C-1", true)
	want := ful.LoadEvidence{Lots: 2, ReceiptComplete: true, Received: ful.CargoScope{Packs: 6, GrossWeight: kg(310), Volume: m3(3)},
		ConsolidationID: "C-1", Tendered: ful.CargoScope{Packs: 6, GrossWeight: kg(312), Volume: m3(3)}, Departed: 6, Security: ful.SecurityCleared,
		OpenDiscrepancies: 2}
	if err != nil || e != want {
		t.Fatalf("summed:\n%+v\nwant\n%+v (%v)", e, want, err)
	}
	got.Lots[1].Measurement = phyapi.Measurement{}
	got.Lots[1].Completed = false
	got.Lots[0].Measurement.Volume = kernel.Quantity{Unit: "DMQ", Amount: 900}
	got.Lots[1].Screening = phyapi.Screening{}
	if e, _ = (Evidence{PHY: evidenceStub{got: got}}).Evidence(context.Background(), "T", "SH-1", "", true); e.Received.GrossWeight != (kernel.Quantity{}) ||
		e.Received.Volume != (kernel.Quantity{}) ||
		e.ReceiptComplete || e.Security != ful.SecurityPending || e.Tendered.Packs != 0 || e.Departed != 0 || e.ConsolidationID != "" {
		t.Fatalf("a lot not measured, receiving and not screened; not attached: %+v", e)
	}
	if e, _ = (Evidence{PHY: evidenceStub{got: got}}).Evidence(context.Background(), "T", "SH-1", "", false); e.Security != "" {
		t.Fatalf("no security state for SEA cargo: %+v", e)
	}
	if _, err = (Evidence{PHY: evidenceStub{err: phyapi.ErrRejected}}).Evidence(context.Background(), "T", "SH-1", "", true); !errors.Is(err, app.ErrEvidenceUnavailable) {
		t.Fatalf("PHY refuses: %v", err)
	}
	got.Lots[1] = phyapi.LotEvidence{LotID: "L-2", Completed: true, ReceivedPacks: 2, Measurement: phyapi.Measurement{Version: 1, GrossWeight: kg(110)}}
	got.Tenders[0].Volume = kernel.Quantity{}
	if e, _ = (Evidence{PHY: evidenceStub{got: got}}).Evidence(context.Background(), "T", "SH-1", "C-1", true); e.Received.GrossWeight != kg(310) ||
		e.Received.Volume != (kernel.Quantity{}) || e.Tendered.Volume != (kernel.Quantity{}) {
		t.Fatalf("a lot or tender without a volume makes the volume unknown: %+v", e)
	}
}
