//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	docmodule "go.idp.xyz/idp-cargo/internal/contexts/doc"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// carrierFacts publishes OceanDocFacts version v of a movement whose master bill is of the given kind, the carrier (or
// the co-loader) in the given role, with the carrier's number.
func (f *fixture) carrierFacts(movement string, v int, status, billKind, role, carrier, number string) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	f.w.ocean[movement] = seaapi.OceanDocFacts{MovementID: movement, Version: v, Status: status, SHA256: fmt.Sprintf("%064d", v), ConsolidationID: "C-" + movement,
		OperatingLegalEntityID: "LE", CarrierPartyID: carrier, CarrierPartyRevision: 5, CarrierRole: role, MasterBillNumber: number, MasterBillKind: billKind,
		Vessel: "EVER GIVEN", Voyage: "0456E", PlaceOfReceipt: "CNSHA", PortOfLoading: "CNSHG", PortOfDischarge: "DEHAM", LocationEdition: "E1", OnBoardAt: f.hours(1)}
}

// mbl publishes FINAL facts of a movement whose carrier issued a master bill of lading.
func (f *fixture) mbl(movement, carrier, number string) {
	f.carrierFacts(movement, 1, seaapi.DocFactsFinal, seaapi.MasterBillCarrierMBL, seaapi.RoleCarrier, carrier, number)
}

// recordCmd records the carrier's master bill of three originals, entered by clerk-1.
func (f *fixture) recordCmd(command, id, movement string) api.RecordCarrierDocument {
	return api.RecordCarrierDocument{CommandID: command, TenantID: f.tenant, ActorID: "clerk-1", DocumentID: id, MovementID: movement, Release: api.ReleaseOriginals,
		Originals: 3, IssuePlace: "CNSHA", IssuedOn: f.hours(0).AddDate(0, 0, -1).Format(time.DateOnly),
		Evidence: api.CarrierEvidence{SHA256: repeat('e', 64), StorageID: "evidence/" + id + ".pdf", MediaType: "application/pdf", CapturedAt: f.hours(1)}, OccurredAt: f.hours(2)}
}

func (f *fixture) verifyCmd(command, id, actor string, revision int64) api.VerifyCarrierDocument {
	return api.VerifyCarrierDocument{CommandID: command, TenantID: f.tenant, ActorID: actor, DocumentID: id, ExpectedRevision: revision, OccurredAt: f.hours(3)}
}

func (f *fixture) cancelCmd(command, id string, revision int64) api.CancelCarrierDocument {
	return api.CancelCarrierDocument{CommandID: command, TenantID: f.tenant, ActorID: "clerk-3", DocumentID: id, ExpectedRevision: revision,
		Reason: "recorded from the wrong bill", OccurredAt: f.hours(4)}
}

func (f *fixture) carrierDoc(t *testing.T, id string) api.CarrierDocument {
	t.Helper()
	d, err := f.module.GetCarrierDocument(context.Background(), api.GetCarrierDocument{TenantID: f.tenant, DocumentID: id})
	if err != nil {
		t.Fatalf("carrier document %s: %v", id, err)
	}
	return d
}

func (f *fixture) count(t *testing.T, table string) (n int) {
	t.Helper()
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM cargo_doc.`+table).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// The carrier's master bill of three originals is recorded from FINAL facts, read back by id and by movement, and
// verified by a second person, never by the one who recorded it.
func TestACarrierMasterBillIsRecordedOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newSeaFixture(t, 1)
	f.mbl("OM-M", "line-1", "MAEU261100123")
	r, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-1", "CD-1", "OM-M"))
	if err != nil || r.DocumentID != "CD-1" || r.NewRevision != 1 || r.CommandID != "rec-1" {
		t.Fatalf("record: %+v %v", r, err)
	}
	byID := f.carrierDoc(t, "CD-1")
	byMovement, err := f.module.FindCarrierDocumentByMovement(ctx, api.FindCarrierDocumentByMovement{TenantID: f.tenant, MovementID: "OM-M"})
	if err != nil || byMovement.DocumentID != "CD-1" || string(byMovement.Content) != string(byID.Content) {
		t.Fatalf("by movement: %v", err)
	}
	sum := sha256.Sum256(byID.Content)
	got := byID
	got.Content, got.RecordedAt, got.Evidence.CapturedAt = nil, time.Time{}, time.Time{}
	want := api.CarrierDocument{DocumentID: "CD-1", Kind: api.KindMBL, Number: "MAEU261100123", MovementID: "OM-M", ConsolidationID: "C-OM-M", LegalEntityID: "LE",
		CarrierPartyID: "line-1", CarrierPartyRevision: 5, CarrierRole: "CARRIER", IssuerCapacity: api.CapacityCarrier, Release: api.ReleaseOriginals, Originals: 3,
		IssuePlace: "CNSHA", IssuedOn: f.hours(0).AddDate(0, 0, -1).Format(time.DateOnly), Evidence: api.CarrierEvidence{SHA256: repeat('e', 64),
			StorageID: "evidence/CD-1.pdf", MediaType: "application/pdf"}, SourceVersion: 1, SourceSHA256: fmt.Sprintf("%064d", 1), ContentSHA256: hex.EncodeToString(sum[:]),
		State: api.CarrierIssued, RecordedBy: "clerk-1", Revision: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read back\n%+v\nwant\n%+v", got, want)
	}
	if !byID.RecordedAt.Equal(f.hours(2)) || !byID.Evidence.CapturedAt.Equal(f.hours(1)) || byID.Verified || !byID.VerifiedAt.IsZero() {
		t.Fatalf("times: %+v", byID)
	}
	var content map[string]any
	if err = json.Unmarshal(byID.Content, &content); err != nil || content["kind"] != "MBL" || content["originals"] != float64(3) {
		t.Fatalf("content %s: %v", byID.Content, err)
	}
	// the event names identifiers only
	var payload []byte
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT payload FROM bento.outbox WHERE scope=$1 AND source=$2 AND event_type='CarrierDocumentRecorded.v1'`, f.tenant,
			api.Source).Scan(&payload)
	}); err != nil {
		t.Fatal(err)
	}
	var event api.EventPayload
	if err = json.Unmarshal(payload, &event); err != nil || event != (api.EventPayload{TenantID: f.tenant, SubjectID: "CD-1", Kind: "MBL", Version: 1,
		ConsolidationID: "C-OM-M", MovementID: "OM-M"}) || strings.Contains(string(payload), "MAEU") {
		t.Fatalf("event %s: %v", payload, err)
	}

	// the person who recorded it does not verify it; a second person does, once
	if _, err = f.module.VerifyCarrierDocument(ctx, f.verifyCmd("ver-self", "CD-1", "clerk-1", 1)); !refused(err, "DOC_SELF_VERIFICATION") {
		t.Fatalf("self-verification: %v", err)
	}
	if d := f.carrierDoc(t, "CD-1"); d.Verified || d.Revision != 1 {
		t.Fatalf("a refused verification changed it: %+v", d)
	}
	v, err := f.module.VerifyCarrierDocument(ctx, f.verifyCmd("ver-1", "CD-1", "clerk-2", 1))
	if err != nil || v.NewRevision != 2 || v.DocumentID != "CD-1" {
		t.Fatalf("verify: %+v %v", v, err)
	}
	if d := f.carrierDoc(t, "CD-1"); !d.Verified || d.VerifiedBy != "clerk-2" || !d.VerifiedAt.Equal(f.hours(3)) || d.Revision != 2 || d.State != api.CarrierIssued ||
		d.RecordedBy != "clerk-1" {
		t.Fatalf("verified: %+v", d)
	}
	for _, by := range []string{"clerk-3", "clerk-1", "clerk-2"} {
		if _, err = f.module.VerifyCarrierDocument(ctx, f.verifyCmd("ver-again-"+by, "CD-1", by, 2)); !errors.Is(err, api.ErrRejected) {
			t.Errorf("verified again by %s: %v", by, err)
		}
	}
	// the same commands replayed answer the same receipts, and write nothing more
	if again, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-1", "CD-1", "OM-M")); err != nil || again != r {
		t.Fatalf("replayed record: %+v %v", again, err)
	}
	if again, err := f.module.VerifyCarrierDocument(ctx, f.verifyCmd("ver-1", "CD-1", "clerk-2", 1)); err != nil || again != v {
		t.Fatalf("replayed verification: %+v %v", again, err)
	}
	if got, want := f.outboxTypes(t), []string{"IssuanceAuthorityRecorded", "CarrierDocumentRecorded", "CarrierDocumentVerified"}; !slices.Equal(got, want) {
		t.Fatalf("outbox %v", got)
	}
	// the same command id with another request is the idempotency conflict
	other := f.recordCmd("rec-1", "CD-1", "OM-M")
	other.Originals = 4
	if _, err = f.module.RecordCarrierDocument(ctx, other); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a record under a command id used for another: %v", err)
	}
	if _, err = f.module.CancelCarrierDocument(ctx, f.cancelCmd("ver-1", "CD-1", 1)); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a cancellation under the command id of a verification: %v", err)
	}
	// a command that changes a document at its revision, under the command id of a verification with the same fields,
	// is not that verification: the request's hash includes the command's name
	if _, err = f.module.DraftTransportDocument(ctx, seaDraft(f, "DOC-1", api.KindHBL, 1, "", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "same-id", TenantID: f.tenant, ActorID: "clerk-2", DocumentID: "DOC-1",
		ExpectedRevision: 1, OccurredAt: f.hours(3)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.VerifyCarrierDocument(ctx, f.verifyCmd("same-id", "DOC-1", "clerk-2", 1)); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a verification under the command id of an approval with the same fields: %v", err)
	}
	// a document id a transport document uses is no obstacle today: the carrier's documents are checked against their own ids only
	f.mbl("OM-N", "line-1", "MAEU261100124")
	if _, err = f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-dup", "DOC-1", "OM-N")); err != nil {
		t.Fatalf("a record under the id of a transport document: %v", err)
	}
	if d, err := f.module.GetTransportDocument(ctx, api.GetTransportDocument{TenantID: f.tenant, DocumentID: "DOC-1"}); err != nil || d.Kind != api.KindHBL {
		t.Fatalf("the transport document: %+v %v", d, err)
	}
	// a stale expected revision is a conflict, for a verification as for a cancellation, and changes nothing
	if _, err = f.module.CancelCarrierDocument(ctx, f.cancelCmd("cancel-stale", "CD-1", 1)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale cancellation: %v", err)
	}
	if _, err = f.module.VerifyCarrierDocument(ctx, f.verifyCmd("ver-stale", "CD-1", "clerk-3", 1)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale verification: %v", err)
	}
	if d := f.carrierDoc(t, "CD-1"); d.Revision != 2 || d.State != api.CarrierIssued {
		t.Fatalf("after the refused commands: %+v", d)
	}
	// the document is not a transport document, and the carrier's document is not found under another tenant's scope
	if _, err = f.module.GetTransportDocument(ctx, api.GetTransportDocument{TenantID: f.tenant, DocumentID: "CD-1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a carrier document read as a transport document: %v", err)
	}
	if _, err = f.module.GetCarrierDocument(ctx, api.GetCarrierDocument{TenantID: "another-tenant", DocumentID: "CD-1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("another tenant's read: %v", err)
	}
}

// A CARRIER_SEA_WAYBILL records as a master sea waybill, released as SEA_WAYBILL; a COLOAD_BILL of a co-loader records
// as an MBL whose issuer is the co-loader; a bill whose kind and carrier's role do not fit is refused.
func TestACarrierSeaWaybillAndACoLoadBillOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.carrierFacts("OM-W", 1, seaapi.DocFactsFinal, seaapi.MasterBillCarrierSeaWaybill, seaapi.RoleCarrier, "line-1", "MSWB261100001")
	f.carrierFacts("OM-L", 1, seaapi.DocFactsFinal, seaapi.MasterBillCoLoad, seaapi.RoleCoLoader, "coloader-1", "COLOAD-77")
	f.carrierFacts("OM-X", 1, seaapi.DocFactsFinal, seaapi.MasterBillCoLoad, seaapi.RoleCarrier, "line-1", "COLOAD-78")

	cmd := f.recordCmd("rec-w", "CD-W", "OM-W")
	if _, err := f.module.RecordCarrierDocument(ctx, cmd); !refused(err, "DOC_INCOMPLETE") {
		t.Fatalf("originals on a master sea waybill: %v", err)
	}
	cmd.Release, cmd.Originals = api.ReleaseExpress, 0
	if _, err := f.module.RecordCarrierDocument(ctx, cmd); !refused(err, "DOC_INCOMPLETE") {
		t.Fatalf("EXPRESS on a master sea waybill: %v", err)
	}
	if f.count(t, "carrier_document") != 0 {
		t.Fatal("a refused record wrote something")
	}
	cmd.Release = ""
	if r, err := f.module.RecordCarrierDocument(ctx, cmd); err != nil || r.NewRevision != 1 {
		t.Fatalf("a master sea waybill entered without a release: %+v %v", r, err)
	}
	if d := f.carrierDoc(t, "CD-W"); d.Kind != api.KindMasterSeaWaybill || d.Release != api.ReleaseSeaWaybill || d.Originals != 0 || d.IssuerCapacity != api.CapacityCarrier ||
		d.CarrierRole != "CARRIER" || d.Number != "MSWB261100001" || strings.Contains(string(d.Content), `"originals"`) {
		t.Fatalf("master sea waybill: %+v", d)
	}
	// SEA_WAYBILL entered as such is the same
	again := f.recordCmd("rec-w2", "CD-W2", "OM-W")
	again.Release, again.Originals = api.ReleaseSeaWaybill, 0
	if _, err := f.module.RecordCarrierDocument(ctx, again); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a second document of the movement: %v", err)
	}

	if _, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-l", "CD-L", "OM-L")); err != nil {
		t.Fatalf("a co-loader's bill: %v", err)
	}
	if d := f.carrierDoc(t, "CD-L"); d.Kind != api.KindMBL || d.IssuerCapacity != api.CapacityCoLoader || d.CarrierRole != "CO_LOADER" || d.CarrierPartyID != "coloader-1" ||
		d.Release != api.ReleaseOriginals || d.Originals != 3 || d.Number != "COLOAD-77" {
		t.Fatalf("co-loader's bill: %+v", d)
	}
	if _, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-x", "CD-X", "OM-X")); !refused(err, "invalid transport document") {
		t.Fatalf("a co-load bill with the carrier in role CARRIER: %v", err)
	}
	// the carrier's own bill with the co-loader in role: the other mismatch
	f.carrierFacts("OM-Y", 1, seaapi.DocFactsFinal, seaapi.MasterBillCarrierMBL, seaapi.RoleCoLoader, "coloader-1", "COLOAD-79")
	if _, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-y", "CD-Y", "OM-Y")); !refused(err, "invalid transport document") {
		t.Fatalf("a carrier's bill with the co-loader in role: %v", err)
	}
	if _, err := f.module.FindCarrierDocumentByMovement(ctx, api.FindCarrierDocumentByMovement{TenantID: f.tenant, MovementID: "OM-X"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a refused record is found: %v", err)
	}
	if f.count(t, "carrier_document") != 2 {
		t.Fatalf("rows: %d documents", f.count(t, "carrier_document"))
	}
}

// A movement has one carrier document that is not cancelled, and for a carrier a number is on one document that is not
// cancelled. A cancelled one is recorded again, under another document id, with the same number; once it is cancelled
// its number is free for another movement of the carrier.
func TestACarrierDocumentIsCancelledAndRecordedAgainOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.mbl("OM-1", "line-1", "MAEU261100777")
	f.mbl("OM-2", "line-1", "MAEU261100777") // the same number, on another movement
	f.mbl("OM-3", "line-2", "MAEU261100777") // the same number under another carrier
	f.mbl("OM-9", "line-1", "MAEU261100009") // a movement with no document and a number in no use
	r1, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-1", "CD-1", "OM-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-1b", "CD-1B", "OM-1")); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a second document of the movement: %v", err)
	}
	// only the document id can refuse this one: the movement has no document and the number is not in use
	if _, err = f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-1c", "CD-1", "OM-9")); !errors.Is(err, api.ErrConflict) ||
		!strings.Contains(err.Error(), "carrier document CD-1") || strings.Contains(err.Error(), "not cancelled") {
		t.Fatalf("a document id used twice: %v", err)
	}
	if _, err = f.module.FindCarrierDocumentByMovement(ctx, api.FindCarrierDocumentByMovement{TenantID: f.tenant, MovementID: "OM-9"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a refused record is found: %v", err)
	}
	if _, err = f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-2", "CD-2", "OM-2")); !refused(err, "DOC_NUMBER_ISSUED") || !strings.Contains(err.Error(), "OM-1") {
		t.Fatalf("a number on another movement's document: %v", err)
	}
	if f.count(t, "carrier_document") != 1 {
		t.Fatal("a refused record wrote something")
	}
	c, err := f.module.CancelCarrierDocument(ctx, f.cancelCmd("cancel-1", "CD-1", 1))
	if err != nil || c.NewRevision != 2 {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	if again, err := f.module.CancelCarrierDocument(ctx, f.cancelCmd("cancel-1", "CD-1", 1)); err != nil || again != c {
		t.Fatalf("replayed cancellation: %+v %v", again, err)
	}
	// the record's own command id, replayed after the cancellation, answers its first receipt and records nothing
	if again, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-1", "CD-1", "OM-1")); err != nil || again != r1 {
		t.Fatalf("replayed record after the cancellation: %+v %v", again, err)
	}
	if _, err = f.module.CancelCarrierDocument(ctx, f.cancelCmd("cancel-1b", "CD-1", 2)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("cancelled twice: %v", err)
	}
	if _, err = f.module.VerifyCarrierDocument(ctx, f.verifyCmd("ver-1", "CD-1", "clerk-2", 2)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a cancelled document is verified: %v", err)
	}
	if d := f.carrierDoc(t, "CD-1"); d.State != api.CarrierCancelled || d.CancelReason != "recorded from the wrong bill" || d.CancelledBy != "clerk-3" ||
		!d.CancelledAt.Equal(f.hours(4)) || d.Revision != 2 || d.Verified {
		t.Fatalf("cancelled: %+v", d)
	}
	if _, err = f.module.FindCarrierDocumentByMovement(ctx, api.FindCarrierDocumentByMovement{TenantID: f.tenant, MovementID: "OM-1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a cancelled document is found by its movement: %v", err)
	}
	// the same movement records it again under a new document id
	if r, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-1d", "CD-1D", "OM-1")); err != nil || r.NewRevision != 1 {
		t.Fatalf("recorded again: %+v %v", r, err)
	}
	if d, err := f.module.FindCarrierDocumentByMovement(ctx, api.FindCarrierDocumentByMovement{TenantID: f.tenant, MovementID: "OM-1"}); err != nil || d.DocumentID != "CD-1D" ||
		d.Number != "MAEU261100777" || d.State != api.CarrierIssued {
		t.Fatalf("by movement: %+v %v", d, err)
	}
	// while it is live the number is refused again; once it is cancelled another movement of the carrier records it
	if _, err = f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-2b", "CD-2", "OM-2")); !refused(err, "DOC_NUMBER_ISSUED") {
		t.Fatalf("a number on another movement's document: %v", err)
	}
	if _, err = f.module.CancelCarrierDocument(ctx, f.cancelCmd("cancel-1d", "CD-1D", 1)); err != nil {
		t.Fatal(err)
	}
	if r, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-2c", "CD-2", "OM-2")); err != nil || r.NewRevision != 1 {
		t.Fatalf("the number of a cancelled record, on another movement: %+v %v", r, err)
	}
	if d, err := f.module.FindCarrierDocumentByMovement(ctx, api.FindCarrierDocumentByMovement{TenantID: f.tenant, MovementID: "OM-2"}); err != nil || d.Number != "MAEU261100777" {
		t.Fatalf("by movement: %+v %v", d, err)
	}
	// the number is on OM-2's document now, so the first movement is refused in its turn; another carrier's number is another number
	if _, err = f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-1e", "CD-1E", "OM-1")); !refused(err, "DOC_NUMBER_ISSUED") {
		t.Fatalf("the first movement, the number now on another's: %v", err)
	}
	if r, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-3", "CD-3", "OM-3")); err != nil || r.NewRevision != 1 {
		t.Fatalf("the same number under another carrier: %+v %v", r, err)
	}
	if f.count(t, "carrier_document") != 4 {
		t.Fatalf("documents: %d", f.count(t, "carrier_document"))
	}
}

// carrierAuth is an authorizer of this file: every actor may do every action of DOC's other documents, and the actions of
// the carrier's documents only as the actor's entry says; it keeps what it was asked. (The shared authorizer of the
// package refuses by actor only, and changing it would change other tests.)
type carrierAuth struct {
	mu      sync.Mutex
	allowed map[string][]string // the carrier actions an actor may do; an actor without an entry may do all of them
	asked   [][3]string         // actor, action, subject
}

func (a *carrierAuth) AuthorizeDocAction(_ context.Context, _, actor, action, subject string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, [3]string{actor, action, subject})
	switch action {
	case api.ActionRecordCarrierDocument, api.ActionVerifyCarrierDocument, api.ActionCancelCarrierDocument:
		if list, ok := a.allowed[actor]; ok && !slices.Contains(list, action) {
			return errors.New("not allowed " + action)
		}
	}
	return nil
}

// Facts that are not FINAL, a movement SEA does not know, and an actor without the action record nothing; the commands
// of the documents DOC issues still refuse the carrier's kinds.
func TestACarrierDocumentNeedsFinalFactsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.carrierFacts("OM-S", 2, seaapi.DocFactsSuperseded, seaapi.MasterBillCarrierMBL, seaapi.RoleCarrier, "line-1", "MAEU261100888")
	f.mbl("OM-F", "line-1", "MAEU261100889")
	if _, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-s", "CD-S", "OM-S")); !refused(err, "DOC_SOURCE_NOT_FINAL") {
		t.Fatalf("facts not FINAL: %v", err)
	}
	if _, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-n", "CD-N", "OM-NONE")); !refused(err, "OceanDocFacts of OM-NONE") {
		t.Fatalf("an unknown movement: %v", err)
	}
	if _, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-nm", "CD-NM", "")); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("no movement: %v", err)
	}
	intruder := f.recordCmd("rec-i", "CD-I", "OM-F")
	intruder.ActorID = "intruder"
	if _, err := f.module.RecordCarrierDocument(ctx, intruder); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("an actor without the action records: %v", err)
	}
	if f.count(t, "carrier_document") != 0 {
		t.Fatal("a refused record wrote something")
	}
	if _, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-f", "CD-F", "OM-F")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.VerifyCarrierDocument(ctx, f.verifyCmd("ver-i", "CD-F", "intruder", 1)); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("an actor without the action verifies: %v", err)
	}
	cancel := f.cancelCmd("cancel-i", "CD-F", 1)
	cancel.ActorID = "intruder"
	if _, err := f.module.CancelCarrierDocument(ctx, cancel); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("an actor without the action cancels: %v", err)
	}
	if d := f.carrierDoc(t, "CD-F"); d.Revision != 1 || d.Verified || d.State != api.CarrierIssued {
		t.Fatalf("after the refused commands: %+v", d)
	}
	// the facts' version moves on: a newer version that is not FINAL refuses a new record, and the old one stays as it was
	f.carrierFacts("OM-F", 2, seaapi.DocFactsSuperseded, seaapi.MasterBillCarrierMBL, seaapi.RoleCarrier, "line-1", "MAEU261100889")
	if d := f.carrierDoc(t, "CD-F"); d.SourceVersion != 1 {
		t.Fatalf("source version %d", d.SourceVersion)
	}
	// DOC's own commands still refuse the kinds, though AIR has FINAL facts for the movement
	f.w.finalize(1, true, 318_000)
	for _, kind := range []string{api.KindMBL, api.KindMasterSeaWaybill} {
		_, err := f.module.DraftTransportDocument(ctx, api.DraftTransportDocument{CommandID: "draft-" + kind, TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-" + kind,
			Kind: kind, MovementID: "AM-1", ShipmentID: "SH-A", PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.hours(2)})
		if !errors.Is(err, api.ErrRejected) {
			t.Errorf("a %s drafted as a transport document: %v", kind, err)
		}
		if _, err = f.module.GetTransportDocument(ctx, api.GetTransportDocument{TenantID: f.tenant, DocumentID: "DOC-" + kind}); !errors.Is(err, api.ErrNotFound) {
			t.Errorf("a %s was drafted: %v", kind, err)
		}
		_, err = f.module.RecordIssuanceAuthority(ctx, api.RecordIssuanceAuthority{CommandID: "auth-" + kind, TenantID: f.tenant, ActorID: "legal-1", AuthorityID: "AUTH-" + kind,
			LegalEntityID: "LE", Kind: kind, IssuePlace: "CNSHG", Qualification: "none", EvidenceRef: "evidence", OccurredAt: f.hours(2)})
		if !errors.Is(err, api.ErrRejected) {
			t.Errorf("an issuance authority for %s: %v", kind, err)
		}
	}
	// the actions: an actor allowed every action of the other documents but none of the carrier's is refused each of
	// them; one allowed to record only is refused to verify and to cancel; each command asks for its own action, on the document
	auth := &carrierAuth{allowed: map[string][]string{"no-carrier": nil, "record-only": {api.ActionRecordCarrierDocument}}}
	m, err := docmodule.New(f.db, f.w, f.w, f.w, f.w, f.w, auth)
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	f.mbl("OM-A", "line-1", "MAEU261100890")
	f.mbl("OM-B", "line-1", "MAEU261100891")
	f.mbl("OM-C", "line-1", "MAEU261100892")
	for _, actor := range []string{"no-carrier", "record-only"} {
		cmd := f.recordCmd("rec-a-"+actor, "CD-A-"+actor, "OM-A")
		cmd.ActorID = actor
		_, err := f.module.RecordCarrierDocument(ctx, cmd)
		if (actor == "no-carrier") != errors.Is(err, api.ErrUnauthorized) || (actor == "record-only") != (err == nil) {
			t.Fatalf("%s records: %v", actor, err)
		}
	}
	if _, err = f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-b", "CD-B", "OM-B")); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"no-carrier", "record-only"} {
		v := f.verifyCmd("ver-"+actor, "CD-B", actor, 1)
		if _, err = f.module.VerifyCarrierDocument(ctx, v); !errors.Is(err, api.ErrUnauthorized) {
			t.Errorf("%s verifies: %v", actor, err)
		}
		c := f.cancelCmd("cancel-"+actor, "CD-B", 1)
		c.ActorID = actor
		if _, err = f.module.CancelCarrierDocument(ctx, c); !errors.Is(err, api.ErrUnauthorized) {
			t.Errorf("%s cancels: %v", actor, err)
		}
	}
	if d := f.carrierDoc(t, "CD-B"); d.Revision != 1 || d.Verified {
		t.Fatalf("after the refused commands: %+v", d)
	}
	if _, err = f.module.VerifyCarrierDocument(ctx, f.verifyCmd("ver-ok", "CD-B", "clerk-2", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.CancelCarrierDocument(ctx, f.cancelCmd("cancel-ok", "CD-B", 2)); err != nil {
		t.Fatal(err)
	}
	// every command asked for its own action, on its document, before it did anything
	asked := func(actor, action string) string {
		auth.mu.Lock()
		defer auth.mu.Unlock()
		for _, a := range auth.asked {
			if a[0] == actor && a[1] == action {
				return a[2]
			}
		}
		return "(not asked)"
	}
	for _, want := range [][3]string{{"record-only", api.ActionRecordCarrierDocument, "CD-A-record-only"}, {"no-carrier", api.ActionVerifyCarrierDocument, "CD-B"},
		{"no-carrier", api.ActionCancelCarrierDocument, "CD-B"}, {"clerk-2", api.ActionVerifyCarrierDocument, "CD-B"}, {"clerk-3", api.ActionCancelCarrierDocument, "CD-B"}} {
		if got := asked(want[0], want[1]); got != want[2] {
			t.Errorf("%s was asked %s on %s, want %s", want[0], want[1], got, want[2])
		}
	}
}

// Eight records at once for one movement under eight document ids: one commits, the others are conflicts, and one
// document and one number are written.
func TestCarrierDocumentsRaceForOneMovement(t *testing.T) {
	f := newFixture(t)
	f.mbl("OM-R", "line-1", "MAEU261100999")
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.module.RecordCarrierDocument(context.Background(), f.recordCmd(fmt.Sprintf("rec-r%d", i), fmt.Sprintf("CD-R%d", i), "OM-R"))
		}()
	}
	close(start)
	wg.Wait()
	committed, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrConflict):
			conflicts++
		}
	}
	if committed != 1 || conflicts != workers-1 {
		t.Fatalf("%d committed, %d conflicts: %v", committed, conflicts, errs)
	}
	if f.count(t, "carrier_document") != 1 {
		t.Fatalf("rows: %d documents", f.count(t, "carrier_document"))
	}
}

// Eight movements of one carrier, each with FINAL facts naming the same master bill number, record at once: one
// commits and the others are refused with DOC_NUMBER_ISSUED; one live document holds the number.
func TestCarrierDocumentsRaceForOneNumber(t *testing.T) {
	f := newFixture(t)
	const workers = 8
	for i := range workers {
		f.mbl(fmt.Sprintf("OM-N%d", i), "line-1", "MAEU261100555")
	}
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.module.RecordCarrierDocument(context.Background(), f.recordCmd(fmt.Sprintf("rec-n%d", i), fmt.Sprintf("CD-N%d", i), fmt.Sprintf("OM-N%d", i)))
		}()
	}
	close(start)
	wg.Wait()
	committed, issued := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case refused(err, "DOC_NUMBER_ISSUED"):
			issued++
		}
	}
	if committed != 1 || issued != workers-1 {
		t.Fatalf("%d committed, %d refused as DOC_NUMBER_ISSUED: %v", committed, issued, errs)
	}
	var live int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM cargo_doc.carrier_document WHERE number='MAEU261100555' AND state<>'CANCELLED'`).Scan(&live)
	}); err != nil || live != 1 || f.count(t, "carrier_document") != 1 {
		t.Fatalf("%d live documents hold the number, %d rows: %v", live, f.count(t, "carrier_document"), err)
	}
}

// carrierRow is a carrier document written past DOC; each field can be given another value than
// the one that makes it a consistent row.
type carrierRow struct {
	id, kind, movement, number, carrier, role, capacity, release string
	originals                                                    int
	contentKind, contentNumber, contentCarrier, contentMovement  string // another than the column's, when set
	contentDocument, contentRole, contentLegal                   string
	contentConsolidation, state                                  string
	sourceVersion, revision                                      int
	recordedBy                                                   string
	badDigest, verified, cancelled, reasonOnly                   bool // inserted already verified, or already cancelled
}

func (r carrierRow) sql() []string {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&r.kind, "MBL")
	def(&r.number, "N-"+r.id)
	def(&r.carrier, "line-1")
	def(&r.role, "CARRIER")
	def(&r.capacity, r.role)
	def(&r.release, "ORIGINALS")
	def(&r.contentKind, r.kind)
	def(&r.contentNumber, r.number)
	def(&r.contentCarrier, r.carrier)
	def(&r.contentMovement, r.movement)
	def(&r.contentDocument, r.id)
	def(&r.contentRole, r.role)
	def(&r.contentLegal, "LE")
	def(&r.contentConsolidation, "C-1")
	def(&r.state, "ISSUED")
	if r.sourceVersion == 0 {
		r.sourceVersion = 1
	}
	originals := ""
	if r.originals > 0 {
		originals = fmt.Sprintf(`"originals":%d,`, r.originals)
	}
	content := fmt.Sprintf(`{"kind":"%s","document_id":"%s","number":"%s","issuer_capacity":"%s","carrier":{"party_id":"%s","party_revision":5,"role":"%s"},`+
		`"legal_entity_id":"%s","release_type":"%s",%s"issue_place":"CNSHA","issued_on":"2026-11-10","source":{"contract":"OceanDocFacts v1","movement_id":"%s",`+
		`"version":%d,"sha256":"%s"},"consolidation_id":"%s","vessel":"V","voyage":"1","port_of_loading":"CNSHG","port_of_discharge":"DEHAM","evidence":{}}`,
		r.contentKind, r.contentDocument, r.contentNumber, r.capacity, r.contentCarrier, r.contentRole, r.contentLegal, r.release, originals, r.contentMovement,
		r.sourceVersion, strings.Repeat("a", 64), r.contentConsolidation)
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	if r.badDigest {
		digest = strings.Repeat("0", 64)
	}
	if r.revision == 0 {
		r.revision = 1
	}
	def(&r.recordedBy, "clerk-1")
	columns, values := "", ""
	if r.verified {
		columns, values = columns+",verified_by,verified_at,verify_entered_at", values+",'clerk-2',now(),now()"
	}
	if r.reasonOnly {
		columns, values = columns+",cancel_reason,cancelled_by,cancelled_at", values+",'x','clerk-3',now()"
	}
	if r.cancelled {
		columns, values = columns+",cancel_reason,cancelled_by,cancelled_at", values+",'x','clerk-3',now()"
	}
	return []string{fmt.Sprintf(`INSERT INTO cargo_doc.carrier_document (tenant_id,document_id,kind,movement_id,consolidation_id,legal_entity_id,carrier_party_id,carrier_role,
        number,content,content_sha256,source_version,recorded_by,recorded_at,entered_at,state,revision%s) VALUES ($1,'%s','%s','%s','C-1','LE','%s','%s','%s','%s','%s',1,'%s',
        now(),now(),'%s',%d%s)`, columns, r.id, r.kind, r.movement, r.carrier, r.role, r.number, content, digest, r.recordedBy, r.state, r.revision, values)}
}

// sqlWithVersion is the row's statements with the document's source_version column another than its content says.
func (r carrierRow) sqlWithVersion(v int) []string {
	out := r.sql()
	out[0] = strings.Replace(out[0], ",1,'clerk-1'", fmt.Sprintf(",%d,'clerk-1'", v), 1)
	return out
}

// Past DOC on the application role: a carrier document and its number are another tenant's to neither read nor write,
// are written only together and consistent with each other, and change only as a verification and a cancellation do.
func TestCarrierDocumentsPastDOCOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for _, m := range []string{"OM-V", "OM-U", "OM-C"} {
		f.mbl(m, "line-1", "NUM-"+m)
	}
	record := func(id, movement string) {
		t.Helper()
		if _, err := f.module.RecordCarrierDocument(ctx, f.recordCmd("rec-"+id, id, movement)); err != nil {
			t.Fatal(err)
		}
	}
	record("CD-V", "OM-V") // verified by clerk-2
	if _, err := f.module.VerifyCarrierDocument(ctx, f.verifyCmd("ver-v", "CD-V", "clerk-2", 1)); err != nil {
		t.Fatal(err)
	}
	record("CD-U", "OM-U") // recorded only
	record("CD-C", "OM-C") // cancelled
	if _, err := f.module.CancelCarrierDocument(ctx, f.cancelCmd("cancel-c", "CD-C", 1)); err != nil {
		t.Fatal(err)
	}
	other := newFixture(t)
	if mine, theirs := f.count(t, "carrier_document"), other.count(t, "carrier_document"); mine != 3 || theirs != 0 {
		t.Fatalf("rows in sight: %d of the tenant's own, %d from another tenant's scope", mine, theirs)
	}

	// the table has forced row security and a policy with USING and WITH CHECK that name the tenant the transaction is
	// bound to (the check of TestOriginalsPastDOCOnPostgres walks every table of cargo_doc as well)
	var tables int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_policy p ON p.polrelid = c.oid
            WHERE n.nspname = 'cargo_doc' AND c.relname = 'carrier_document' AND c.relrowsecurity AND c.relforcerowsecurity
              AND p.polname = 'tenant_isolation' AND pg_get_expr(p.polqual, p.polrelid) LIKE '%app.tenant_id%'
              AND pg_get_expr(p.polwithcheck, p.polrelid) LIKE '%app.tenant_id%'`).Scan(&tables)
	}); err != nil || tables != 1 {
		t.Fatalf("%d of the one table with forced row security and a policy naming the tenant: %v", tables, err)
	}
	// the application role holds SELECT and INSERT on the table and UPDATE of the columns that change, no more
	var changeable []string
	var extra int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		rows, err := e.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid = 'cargo_doc.carrier_document'::regclass AND a.attnum > 0 AND NOT a.attisdropped
            AND has_column_privilege(current_user, a.attrelid, a.attname, 'UPDATE') ORDER BY a.attname`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				return err
			}
			changeable = append(changeable, name)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		return e.QueryRow(ctx, `SELECT count(*) FROM (SELECT 'cargo_doc.carrier_document' AS name) t
            WHERE has_table_privilege(current_user, t.name, 'DELETE') OR has_table_privilege(current_user, t.name, 'TRUNCATE') OR NOT has_table_privilege(current_user, t.name, 'SELECT')
              OR NOT has_table_privilege(current_user, t.name, 'INSERT')`).Scan(&extra)
	}); err != nil || extra != 0 || !slices.Equal(changeable, []string{"cancel_reason", "cancelled_at", "cancelled_by", "revision", "state", "updated_at", "verified_at",
		"verified_by", "verify_entered_at"}) {
		t.Fatalf("the application role's privileges: updates %v, %d more than SELECT and INSERT: %v", changeable, extra, err)
	}
	const (
		touch = `UPDATE cargo_doc.carrier_document SET revision=revision+1 WHERE tenant_id=$1 AND document_id='%s'`
		set   = `UPDATE cargo_doc.carrier_document SET %s WHERE tenant_id=$1 AND document_id='%s'`
	)
	for name, tc := range map[string]struct {
		as    *fixture
		sql   []string
		state string
		check string
	}{
		// the unique indexes: one live document per movement, and per carrier and number
		"a second live document of another movement with the number": {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, number: "NUM-OM-V"}.sql(), "23505", "carrier_document_one_number"},
		// a row is inserted as a fresh record
		"inserted verified":                   {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, verified: true}.sql(), "23514", "carrier_document_inserted_fresh"},
		"inserted cancelled":                  {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, cancelled: true, state: "CANCELLED"}.sql(), "23514", "carrier_document_inserted_fresh"},
		"inserted at revision 2":              {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, revision: 2}.sql(), "23514", "carrier_document_inserted_fresh"},
		"inserted ISSUED with a cancellation": {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, reasonOnly: true}.sql(), "23514", "carrier_document_inserted_fresh"},
		"recorded by an id with a space":      {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, recordedBy: "clerk-1 "}.sql(), "23514", "carrier_document_recorded_by_check"},
		// a person's id and a number have no surrounding space
		"a number with a leading space":                   {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, number: " N-1"}.sql(), "23514", "carrier_document_number_check"},
		"a verification by the recorder's id and a space": {f, []string{fmt.Sprintf(set, `verified_by='clerk-1 ',verified_at=now(),verify_entered_at=now(),revision=revision+1`, "CD-U")}, "23514", "carrier_document_verified_by_check"},
		"a cancellation by an id with a space":            {f, []string{fmt.Sprintf(set, `state='CANCELLED',cancel_reason='x',cancelled_by=' clerk-3',cancelled_at=now(),revision=revision+1`, "CD-U")}, "23514", "carrier_document_cancelled_by_check"},
		// the times are in order
		"a cancellation before the record":                                   {f, []string{fmt.Sprintf(set, `state='CANCELLED',cancel_reason='x',cancelled_by='clerk-3',cancelled_at=recorded_at-interval '1 hour',revision=revision+1`, "CD-U")}, "23514", "carrier_document_cancelled_after_record"},
		"a cancellation before the verification":                             {f, []string{fmt.Sprintf(set, `state='CANCELLED',cancel_reason='x',cancelled_by='clerk-3',cancelled_at=verified_at-interval '1 second',revision=revision+1`, "CD-V")}, "23514", "carrier_document_cancelled_after_verification"},
		"a content of another document":                                      {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, contentDocument: "CD-OTHER"}.sql(), "23514", "carrier_document_content"},
		"a content of another role":                                          {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, contentRole: "CO_LOADER"}.sql(), "23514", "carrier_document_content"},
		"a content of another legal entity":                                  {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, contentLegal: "LE-9"}.sql(), "23514", "carrier_document_content"},
		"a content of another consolidation":                                 {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, contentConsolidation: "C-9"}.sql(), "23514", "carrier_document_content"},
		"a content of another source version":                                {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1}.sqlWithVersion(2), "23514", "carrier_document_content"},
		"a state that is neither ISSUED nor CANCELLED":                       {f, []string{fmt.Sprintf(set, `state='DONE',revision=revision+1`, "CD-U")}, "23514", "carrier_document_state_check"},
		"a sea waybill issued by a co-loader's capacity":                     {f, carrierRow{id: "CD-1", movement: "OM-1", kind: "MASTER_SEA_WAYBILL", release: "SEA_WAYBILL", capacity: "CO_LOADER"}.sql(), "23514", "carrier_document_capacity"},
		"a content whose digest is wrong":                                    {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, badDigest: true}.sql(), "23514", "carrier_document_content"},
		"a content of another kind":                                          {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, contentKind: "MASTER_SEA_WAYBILL"}.sql(), "23514", "carrier_document_content"},
		"a content of another number":                                        {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, contentNumber: "N-OTHER"}.sql(), "23514", "carrier_document_content"},
		"a content of another carrier":                                       {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, contentCarrier: "line-9"}.sql(), "23514", "carrier_document_content"},
		"a content of another movement":                                      {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, contentMovement: "OM-9"}.sql(), "23514", "carrier_document_content"},
		"a kind that is no carrier document's":                               {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, kind: "HBL"}.sql(), "23514", "carrier_document_capacity|carrier_document_kind_check"},
		"a number of 36 characters":                                          {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 1, number: strings.Repeat("N", 36)}.sql(), "23514", "carrier_document_number_check"},
		"an MBL released as SEA_WAYBILL":                                     {f, carrierRow{id: "CD-1", movement: "OM-1", release: "SEA_WAYBILL"}.sql(), "23514", "carrier_document_release"},
		"an MBL of originals without their count":                            {f, carrierRow{id: "CD-1", movement: "OM-1"}.sql(), "23514", "carrier_document_release"},
		"an MBL of ten originals":                                            {f, carrierRow{id: "CD-1", movement: "OM-1", originals: 10}.sql(), "23514", "carrier_document_release"},
		"an MBL of EXPRESS with originals":                                   {f, carrierRow{id: "CD-1", movement: "OM-1", release: "EXPRESS", originals: 1}.sql(), "23514", "carrier_document_release"},
		"a sea waybill with originals":                                       {f, carrierRow{id: "CD-1", movement: "OM-1", kind: "MASTER_SEA_WAYBILL", release: "SEA_WAYBILL", originals: 2}.sql(), "23514", "carrier_document_release"},
		"a sea waybill released as originals":                                {f, carrierRow{id: "CD-1", movement: "OM-1", kind: "MASTER_SEA_WAYBILL", originals: 2}.sql(), "23514", "carrier_document_release"},
		"a co-loader with capacity CARRIER":                                  {f, carrierRow{id: "CD-1", movement: "OM-1", role: "CO_LOADER", capacity: "CARRIER", originals: 1}.sql(), "23514", "carrier_document_capacity"},
		"a carrier with capacity CO_LOADER":                                  {f, carrierRow{id: "CD-1", movement: "OM-1", capacity: "CO_LOADER", originals: 1}.sql(), "23514", "carrier_document_capacity"},
		"a co-loader's master sea waybill":                                   {f, carrierRow{id: "CD-1", movement: "OM-1", kind: "MASTER_SEA_WAYBILL", role: "CO_LOADER", release: "SEA_WAYBILL"}.sql(), "23514", "carrier_document_capacity"},
		"a second live document of a movement":                               {f, carrierRow{id: "CD-1", movement: "OM-U", originals: 1}.sql(), "23505", "carrier_document_one_live"},
		"another tenant's, writing into this one":                            {other, carrierRow{id: "CD-1", movement: "OM-1", originals: 1}.sql(), "42501", ""},
		"a verification by the recorder":                                     {f, []string{fmt.Sprintf(set, `verified_by=recorded_by,verified_at=now(),verify_entered_at=now(),revision=revision+1`, "CD-U")}, "23514", "carrier_document_second_person"},
		"a verification before the record":                                   {f, []string{fmt.Sprintf(set, `verified_by='clerk-2',verified_at=recorded_at-interval '1 hour',verify_entered_at=now(),revision=revision+1`, "CD-U")}, "23514", "carrier_document_verified_after_record"},
		"a verification without its time":                                    {f, []string{fmt.Sprintf(set, `verified_by='clerk-2',revision=revision+1`, "CD-U")}, "23514", "carrier_document_verified_together"},
		"a verification changed":                                             {f, []string{fmt.Sprintf(set, `verified_by='clerk-9',revision=revision+1`, "CD-V")}, "23001", "carrier_document_verification_is_final"},
		"a verification moved in time":                                       {f, []string{fmt.Sprintf(set, `verified_at=verified_at+interval '1 hour',revision=revision+1`, "CD-V")}, "23001", "carrier_document_verification_is_final"},
		"a verification removed":                                             {f, []string{fmt.Sprintf(set, `verified_by=NULL,verified_at=NULL,verify_entered_at=NULL,revision=revision+1`, "CD-V")}, "23001", "carrier_document_verification_is_final"},
		"a revision stepping by 2":                                           {f, []string{fmt.Sprintf(set, `revision=revision+2`, "CD-U")}, "23001", "carrier_document_revision_step"},
		"a revision stepping back":                                           {f, []string{fmt.Sprintf(set, `revision=revision-1`, "CD-V")}, "23001", "carrier_document_revision_step"},
		"a revision left as it was":                                          {f, []string{fmt.Sprintf(set, `updated_at=now()`, "CD-U")}, "23001", "carrier_document_revision_step"},
		"a cancellation without its reason":                                  {f, []string{fmt.Sprintf(set, `state='CANCELLED',revision=revision+1`, "CD-U")}, "23514", "carrier_document_cancellation"},
		"a reason without a cancellation":                                    {f, []string{fmt.Sprintf(set, `cancel_reason='x',cancelled_by='x',cancelled_at=now(),revision=revision+1`, "CD-U")}, "23514", "carrier_document_cancellation"},
		"a column that does not change, the application role lacking UPDATE": {f, []string{fmt.Sprintf(set, `number='N-OTHER'`, "CD-U")}, "42501", ""},
		"a cancelled row updated":                                            {f, []string{fmt.Sprintf(touch, "CD-C")}, "23001", "carrier_document_cancelled_is_final"},
		"a cancelled row reopened":                                           {f, []string{fmt.Sprintf(set, `state='ISSUED',cancel_reason=NULL,cancelled_by=NULL,cancelled_at=NULL,revision=revision+1`, "CD-C")}, "23001", "carrier_document_cancelled_is_final"},
		"a row deleted, the application role lacking DELETE":                 {f, []string{`DELETE FROM cargo_doc.carrier_document WHERE tenant_id=$1`}, "42501", ""},
	} {
		err := tc.as.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, sql := range tc.sql {
				args := []any{}
				if strings.Contains(sql, "$1") {
					args = append(args, f.tenant)
				}
				if _, err := e.Exec(ctx, sql, args...); err != nil {
					return err
				}
			}
			return nil
		})
		var pgErr *pgconn.PgError
		if state := sqlState(err); state != tc.state || tc.check != "" && (!errors.As(err, &pgErr) || !slices.Contains(strings.Split(tc.check, "|"), pgErr.ConstraintName)) {
			t.Errorf("%s: %v (state %s), want %s %s", name, err, state, tc.state, tc.check)
		}
		t.Logf("%s refused: %v", name, err)
	}

	// another tenant's scope writes none of this tenant's rows: an update or a delete finds none (and the role may not delete)
	if err := other.within(t, func(ctx context.Context, e bentopg.Executor) error {
		tag, err := e.Exec(ctx, `UPDATE cargo_doc.carrier_document SET revision=revision+1 WHERE document_id='CD-U'`)
		if err != nil || tag.RowsAffected() != 0 {
			return fmt.Errorf("another tenant's update of CD-U: %v rows, %v", tag.RowsAffected(), err)
		}
		return nil
	}); err != nil {
		t.Error(err)
	}
	// and the same writes as another tenant's own are not refused for their form: the tenant's rows are what they were
	for id, want := range map[string]int64{"CD-V": 2, "CD-U": 1, "CD-C": 2} {
		if d := f.carrierDoc(t, id); d.Revision != want {
			t.Errorf("%s at revision %d after the refused writes, want %d", id, d.Revision, want)
		}
	}
	if f.count(t, "carrier_document") != 3 {
		t.Fatalf("rows after the refused writes: %d documents", f.count(t, "carrier_document"))
	}
	// the number rule lets through what it does not forbid: the number under another carrier, and a cancelled record's number
	// on another movement
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, sql := range (carrierRow{id: "CD-L2", movement: "OM-L2", originals: 1, carrier: "line-2", number: "NUM-OM-V"}).sql() {
			if _, err := e.Exec(ctx, sql, f.tenant); err != nil {
				return err
			}
		}
		for _, sql := range (carrierRow{id: "CD-C2", movement: "OM-C2", originals: 1, number: "NUM-OM-C"}).sql() { // CD-C is cancelled
			if _, err := e.Exec(ctx, sql, f.tenant); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("the same number under another carrier, and a cancelled record's number: %v", err)
	}
	if f.count(t, "carrier_document") != 5 {
		t.Fatalf("rows: %d documents", f.count(t, "carrier_document"))
	}

	// past the grants, as the migrator: the triggers keep the rows even from a role that may write them
	dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_MIGRATOR_DSN required in CI; skipping forbidden")
		}
		t.Log("IDP_CARGO_MIGRATOR_DSN not set: the triggers are not tried as the migrator")
		return
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for name, tc := range map[string]struct{ sql, check string }{
		"a column that does not change": {fmt.Sprintf(set, `number='N-OTHER'`, "CD-U"), "carrier_document_changes"},
		"the kind":                      {fmt.Sprintf(set, `kind='MASTER_SEA_WAYBILL',revision=revision+1`, "CD-U"), "carrier_document_changes"},
		"a row deleted":                 {`DELETE FROM cargo_doc.carrier_document WHERE tenant_id=$1`, "carrier_document_never_deleted"},
		"a cancelled row updated":       {fmt.Sprintf(touch, "CD-C"), "carrier_document_cancelled_is_final"},
		"a verification changed":        {fmt.Sprintf(set, `verified_by='clerk-9',revision=revision+1`, "CD-V"), "carrier_document_verification_is_final"},
	} {
		_, err := conn.Exec(ctx, tc.sql, f.tenant)
		var pgErr *pgconn.PgError
		if sqlState(err) != "23001" || tc.check != "" && (!errors.As(err, &pgErr) || pgErr.ConstraintName != tc.check) {
			t.Errorf("%s as the migrator: %v", name, err)
		}
	}
}
