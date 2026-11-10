package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueCOMEvent(ctx context.Context, e com.Event) error {
	envelope, err := COMEnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// COMEnvelope maps a COM domain event to its Bento envelope. The event ID is derived from the command, the
// obligation and the shipment, so a retried command yields the same event ID. All events of an order share
// its partition.
func COMEnvelope(commandID string, e com.Event, recordedAt time.Time) (eventing.Envelope, error) {
	key := commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.SubjectID +
		"\x00" + string(e.ObligationID) + "\x00" + strconv.FormatInt(e.ObligationVersion, 10)
	if e.ShipmentID != "" {
		key += "\x00" + e.ShipmentID
	}
	if e.LineNo != 0 {
		key += "\x00line:" + strconv.Itoa(e.LineNo) + "/" + strconv.FormatInt(e.LineVersion, 10)
	}
	digest := sha256.Sum256([]byte(key))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), SubjectID: e.SubjectID,
		ObligationID: string(e.ObligationID), ObligationVersion: e.ObligationVersion, ShipmentID: e.ShipmentID,
		LineNo: e.LineNo, LineVersion: e.LineVersion})
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
