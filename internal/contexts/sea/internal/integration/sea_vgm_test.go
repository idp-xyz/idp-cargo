//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// TestVGMOnPostgres submits a verified gross mass, keeps the signatory's name off the visit and the events, and
// logs a read of the name only for a restricted purpose.
func TestVGMOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 1)
	if _, err := f.associate("assoc-1", "OM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AssignContainer(ctx, api.AssignContainer{CommandID: "assign-CV-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		MovementID: "OM-1", LoadUnitNo: 1, ContainerNumber: "CSQU3054383", SizeType: "22G1", TareKg: 2200, MaxGrossKg: 30480, PlateSource: api.PlateCSC,
		OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.seal("seal-1", "CV-1", 1, "CN-SEAL-1", api.SealShipper, 2); err != nil {
		t.Fatal(err)
	}
	f.stuffing("SH-1", "X-1", 1, "22G1", "CN-SEAL-1")
	if _, err := f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-1", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 2, ShipmentID: "SH-1", OccurredAt: f.hours(3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.stuffingDone("done-1", "CV-1", 2, "", 3); err != nil {
		t.Fatal(err)
	}

	onTime := api.AssertVgm{CommandID: "vgm-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: 3,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 2200}, Method: api.VgmMethod1, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
		DeterminedAt: f.hours(4), Signatory: "Ada Lovelace", OccurredAt: f.hours(4)}
	below := onTime
	below.CommandID, below.Mass.Amount = "vgm-low", 2199
	if _, err := f.module.AssertVgm(ctx, below); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "tare") {
		t.Fatalf("below the tare: %v", err)
	}
	early := onTime
	early.CommandID, early.LateReason = "vgm-early-reason", "to be safe"
	if _, err := f.module.AssertVgm(ctx, early); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "only after") {
		t.Fatalf("a reason before the cut-off: %v", err)
	}
	got, err := f.module.AssertVgm(ctx, onTime)
	if err != nil || got != (api.Receipt{CommandID: "vgm-1", VisitID: "CV-1", NewRevision: 4, AssertionNo: 1}) {
		t.Fatalf("asserted: %+v %v", got, err)
	}
	if again, err := f.module.AssertVgm(ctx, onTime); err != nil || again != got {
		t.Fatalf("replayed: %+v %v", again, err)
	}

	v := f.visit(t, "CV-1")
	body, err := json.Marshal(v)
	if err != nil || strings.Contains(string(body), "Ada") || strings.Contains(string(body), "Lovelace") || v.Status != api.VisitVGM || len(v.Assertions) != 1 ||
		v.Assertions[0] != (api.VgmAssertion{Version: 1, MassGrams: 2_200_000, Method: api.VgmMethod1, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
			DeterminedAt: f.hours(4), SubmittedAt: f.hours(4), RecordedBy: "ops-1", RecordedAt: f.hours(4)}) {
		t.Fatalf("the visit carries the mass and not the name: %s", body)
	}
	for _, e := range f.outbox(t) {
		if strings.Contains(e.Raw, "Ada") || strings.Contains(e.Raw, "Lovelace") || strings.Contains(e.Raw, "2200000") {
			t.Fatalf("an event carries the name or the mass: %s", e.Raw)
		}
	}
	m := f.get(t, "OM-1")
	if len(m.Houses) != 1 || len(m.Houses[0].Containers) != 1 || m.Houses[0].Containers[0].ContainerNumber != "CSQU3054383" {
		t.Fatalf("the house still names its container: %+v", m.Houses)
	}

	// The application role cannot read the name or forge a log entry. A purpose outside the three restricted ones
	// is refused, and a refused read writes nothing.
	if err = f.past(t, `SELECT name FROM cargo_sea.vgm_signatory WHERE tenant_id=$1`); sqlState(err) != "42501" {
		t.Fatalf("selecting the name: %v (state %s), want 42501", err, sqlState(err))
	}
	if err = f.past(t, `INSERT INTO cargo_sea.vgm_signatory_access (tenant_id,visit_id,version,actor_id,purpose)
        VALUES ($1,'CV-1',1,'intruder','CUSTOMS_AUDIT')`); sqlState(err) != "42501" {
		t.Fatalf("forging a log entry: %v (state %s), want 42501", err, sqlState(err))
	}
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `SELECT cargo_sea.read_vgm_signatory('CV-1',1,'ops-1','DOCUMENTS')`)
		return err
	}); sqlState(err) != "23514" {
		t.Fatalf("the function with a documents purpose: %v (state %s), want 23514", err, sqlState(err))
	}
	logOf := func() []api.VgmSignatoryAccess {
		t.Helper()
		rows, err := f.module.ReadVgmSignatoryAccess(ctx, api.ReadVgmSignatoryAccess{TenantID: f.tenant, ActorID: "audit-1", VisitID: "CV-1", Version: 1})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := logOf(); len(rows) != 0 {
		t.Fatalf("a refused read was logged: %+v", rows)
	}
	if _, err = f.module.ReadVgmSignatory(ctx, api.ReadVgmSignatory{TenantID: f.tenant, ActorID: "intruder", VisitID: "CV-1", Version: 1,
		Purpose: string(kernel.PurposeCustomsAudit)}); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("an actor without the right: %v", err)
	}
	if _, err = f.module.ReadVgmSignatory(ctx, api.ReadVgmSignatory{TenantID: f.tenant, ActorID: "audit-1", VisitID: "CV-1", Version: 1,
		Purpose: string(kernel.PurposeDocuments)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a documents purpose: %v", err)
	}
	if _, err = f.module.ReadVgmSignatory(ctx, api.ReadVgmSignatory{TenantID: f.tenant, ActorID: "audit-1", VisitID: "CV-1", Version: 9,
		Purpose: string(kernel.PurposeCustomsAudit)}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an assertion SEA does not know: %v", err)
	}
	if rows := logOf(); len(rows) != 0 {
		t.Fatalf("a refused read was logged: %+v", rows)
	}
	name, err := f.module.ReadVgmSignatory(ctx, api.ReadVgmSignatory{TenantID: f.tenant, ActorID: "audit-1", VisitID: "CV-1", Version: 1,
		Purpose: string(kernel.PurposeCustomsAudit)})
	if err != nil || name.Name != "Ada Lovelace" {
		t.Fatalf("the name: %+v %v", name, err)
	}
	if _, err = f.module.ReadVgmSignatory(ctx, api.ReadVgmSignatory{TenantID: f.tenant, ActorID: "counsel-1", VisitID: "CV-1", Version: 1,
		Purpose: string(kernel.PurposeLegalClaim)}); err != nil {
		t.Fatal(err)
	}
	rows := logOf()
	if len(rows) != 2 || rows[0].ActorID != "counsel-1" || rows[0].Purpose != string(kernel.PurposeLegalClaim) || rows[1].ActorID != "audit-1" ||
		rows[1].Purpose != string(kernel.PurposeCustomsAudit) || rows[0].AccessID == "" || strings.Contains(rows[0].ActorID+rows[0].Purpose, "Ada") {
		t.Fatalf("the log: %+v", rows)
	}

	// A mass under the tare is refused at commit as well, and the refused write leaves the stored assertion.
	low := `WITH v AS (UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1' RETURNING visit_id),
        a AS (INSERT INTO cargo_sea.vgm_assertion (tenant_id,visit_id,version,mass_grams,method,responsible_party_id,responsible_party_revision,
          determined_at,submitted_at,recorded_by,recorded_at,entered_at)
          SELECT $1,visit_id,2,1,'METHOD_1','LE',1,'` + f.hours(4).Format(time.RFC3339) + `','` + f.hours(4).Format(time.RFC3339) + `','x','` + f.hours(4).Format(time.RFC3339) + `',now() FROM v
          RETURNING visit_id, version)
        INSERT INTO cargo_sea.vgm_signatory (tenant_id,visit_id,version,name) SELECT $1,visit_id,version,'Pat' FROM a`
	if err = f.past(t, low); sqlState(err) != "23514" || !strings.Contains(err.Error(), "not below the tare") {
		t.Fatalf("a gram below the tare, past SEA: %v", err)
	}
	if got := f.visit(t, "CV-1"); got.Revision != 4 || len(got.Assertions) != 1 {
		t.Fatalf("the refused write stayed out: %+v", got)
	}

	lateAt := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	late := api.AssertVgm{CommandID: "vgm-late", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: 4,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 4000}, Method: api.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
		DeterminedAt: lateAt, Signatory: "Lin Wei", OccurredAt: lateAt}
	if _, err = f.module.AssertVgm(ctx, late); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "late") {
		t.Fatalf("late with no reason: %v", err)
	}
	late.CommandID, late.LateReason = "vgm-2", "the weighbridge queued the container"
	second, err := f.module.AssertVgm(ctx, late)
	if err != nil || second.AssertionNo != 2 || second.NewRevision != 5 {
		t.Fatalf("late: %+v %v", second, err)
	}
	answered, err := f.module.RecordVgmResponse(ctx, api.RecordVgmResponse{CommandID: "vgm-answer", TenantID: f.tenant, ActorID: "line-1", VisitID: "CV-1",
		ExpectedRevision: 5, Response: api.VgmAccepted, Source: "carrier portal", OccurredAt: lateAt})
	if err != nil || answered.AssertionNo != 2 || answered.NewRevision != 6 {
		t.Fatalf("the carrier's answer: %+v %v", answered, err)
	}
	if _, err = f.module.RecordVgmResponse(ctx, api.RecordVgmResponse{CommandID: "vgm-answer-2", TenantID: f.tenant, ActorID: "line-1", VisitID: "CV-1",
		ExpectedRevision: 6, Response: api.VgmRejected, Source: "carrier portal", OccurredAt: lateAt}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an answer twice: %v", err)
	}
	v = f.visit(t, "CV-1")
	if v.Status != api.VisitVGM || len(v.Assertions) != 2 || v.Assertions[0].Version != 2 || v.Assertions[0].Response != api.VgmAccepted ||
		v.Assertions[0].LateReason != "the weighbridge queued the container" || v.Assertions[1].Response != "" || v.Assertions[0].MassGrams != 4_000_000 {
		t.Fatalf("two versions: %+v", v.Assertions)
	}
	body, _ = json.Marshal(v)
	if strings.Contains(string(body), "Lin") || strings.Contains(string(body), "Wei") {
		t.Fatalf("the second name is on the visit: %s", body)
	}
}
