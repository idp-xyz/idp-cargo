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
	"go.idp.xyz/idp-cargo/internal/contexts/book/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/book/internal/application"
	book "go.idp.xyz/idp-cargo/internal/contexts/book/internal/domain"
)

// CommandReceipts stores the result of every committed BOOK command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

var _ app.Receipts = CommandReceipts{}

func (s CommandReceipts) Find(ctx context.Context, tenant book.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var booking string
	err = q.QueryRow(ctx, `SELECT request_sha256,result_id,result_revision FROM cargo_book.command_receipt WHERE tenant_id=$1 AND command_id=$2`,
		string(tenant), id).Scan(&r.RequestHash, &booking, &r.NewRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	r.BookingID = book.BookingID(booking)
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant book.TenantID, r app.Receipt) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_book.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision)
      VALUES ($1,$2,$3,'CarrierBooking',$4,$5)`, string(tenant), r.CommandID, r.RequestHash, string(r.BookingID), r.NewRevision)
	return err
}

// Outbox writes BOOK events to the Bento Outbox in the command's transaction.
type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueBOOKEvent(ctx context.Context, e book.Event) error {
	envelope, err := BOOKEnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// BOOKEnvelope maps a BOOK domain event to its Bento envelope. The event ID is derived from the command, the
// type and the versions and allocation named, so a retried command yields the same event ID. All events of a
// booking share its partition.
func BOOKEnvelope(commandID string, e book.Event, recordedAt time.Time) (eventing.Envelope, error) {
	digest := sha256.Sum256([]byte(commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + string(e.BookingID) + "\x00" +
		strconv.Itoa(e.RequestVersion) + "\x00" + strconv.Itoa(e.CapacityVersion) + "\x00" + strconv.Itoa(e.AllocationNo)))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), BookingID: string(e.BookingID), RequestVersion: e.RequestVersion,
		CapacityVersion: e.CapacityVersion, AllocationNo: e.AllocationNo, ConsumerKind: e.ConsumerKind, ConsumerID: e.ConsumerID})
	if err != nil {
		return eventing.Envelope{}, err
	}
	envelope := eventing.Envelope{
		SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(hex.EncodeToString(digest[:])),
		Source: api.Source, Type: eventing.EventType(e.Type), Version: 1,
		Scope: string(e.TenantID), Subject: string(e.BookingID),
		PartitionKey: string(e.TenantID) + ":" + string(e.BookingID),
		OccurredAt:   e.At.UTC(), RecordedAt: recordedAt.UTC(),
		ContentType: eventing.JSONContentType, Payload: payload,
	}
	return envelope, envelope.Validate()
}
