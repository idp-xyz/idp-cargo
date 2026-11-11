package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// CommandReceipts stores the result of every committed PHY command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

var _ app.Receipts = CommandReceipts{}

func (s CommandReceipts) Find(ctx context.Context, tenant phy.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var kind, result, consolidation string
	var entry *int
	err = q.QueryRow(ctx, `SELECT request_sha256,result_type,result_id,result_revision,result_consolidation_id,result_entry_no
      FROM cargo_phy.command_receipt WHERE tenant_id=$1 AND command_id=$2`, string(tenant), id).Scan(
		&r.RequestHash, &kind, &result, &r.NewRevision, &consolidation, &entry)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	r.ConsolidationID = phy.ConsolidationID(consolidation)
	item := 0
	if entry != nil {
		item = *entry
	}
	switch kind {
	case "ConsolMembership":
		r.ShipmentID, r.EntryNo = phy.ShipmentID(result), item
	case "CargoLot":
		r.LotID, r.ItemNo = phy.CargoLotID(result), item
	case "LoadExecution":
		r.ExecutionID, r.ItemNo = phy.LoadExecutionID(result), item
	}
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant phy.TenantID, r app.Receipt) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	kind, result, entry := "Consolidation", string(r.ConsolidationID), any(nil)
	switch {
	case r.LotID != "":
		kind, result, entry = "CargoLot", string(r.LotID), orNull(r.ItemNo)
	case r.ExecutionID != "":
		kind, result, entry = "LoadExecution", string(r.ExecutionID), orNull(r.ItemNo)
	case r.ShipmentID != "":
		kind, result, entry = "ConsolMembership", string(r.ShipmentID), r.EntryNo
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_phy.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,
      result_consolidation_id,result_entry_no) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		string(tenant), r.CommandID, r.RequestHash, kind, result, r.NewRevision, string(r.ConsolidationID), entry)
	return err
}

// Outbox writes PHY events to the Bento Outbox in the command's transaction.
type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueuePHYEvent(ctx context.Context, e phy.Event) error {
	envelope, err := PHYEnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// PHYEnvelope maps a PHY domain event to its Bento envelope. The event ID is derived from the command, the
// subject, the consolidation, the entry or version and, for cargo lot and load execution events, the lot, the
// execution and the item, so a retried command yields the same event IDs. All events of a subject share its
// partition.
func PHYEnvelope(commandID string, e phy.Event, recordedAt time.Time) (eventing.Envelope, error) {
	key := commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.SubjectID + "\x00" + string(e.ConsolidationID) + "\x00" +
		strconv.Itoa(e.Version) + "\x00" + strconv.Itoa(e.EntryNo)
	if e.LotID != "" || e.ExecutionID != "" || e.ItemNo != 0 {
		key += "\x00" + string(e.LotID) + "\x00" + string(e.ExecutionID) + "\x00" + strconv.Itoa(e.ItemNo)
	}
	digest := sha256.Sum256([]byte(key))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), SubjectID: e.SubjectID, ConsolidationID: string(e.ConsolidationID),
		Version: e.Version, ShipmentID: string(e.ShipmentID), EntryNo: e.EntryNo, FromConsolidationID: string(e.FromConsolidationID), Reason: e.Reason,
		LotID: string(e.LotID), ExecutionID: string(e.ExecutionID), ItemNo: e.ItemNo})
	if err != nil {
		return eventing.Envelope{}, err
	}
	envelope := eventing.Envelope{
		SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(hex.EncodeToString(digest[:])),
		Source: api.Source, Type: eventing.EventType(e.Type), Version: 1,
		Scope: string(e.TenantID), Subject: e.SubjectID,
		PartitionKey: string(e.TenantID) + ":" + e.SubjectID,
		OccurredAt:   e.At.UTC(), RecordedAt: recordedAt.UTC(),
		ContentType: eventing.JSONContentType, Payload: payload,
	}
	return envelope, envelope.Validate()
}
