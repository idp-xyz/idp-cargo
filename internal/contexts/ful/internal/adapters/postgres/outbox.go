package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueFULEvent(ctx context.Context, e ful.Event) error {
	envelope, err := FULEnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// FULEnvelope maps a FUL domain event to its Bento envelope. The event ID is derived from the command and
// the obligation or consolidation, so a retried command yields the same event ID. All events of a shipment
// share its partition.
func FULEnvelope(commandID string, e ful.Event, recordedAt time.Time) (eventing.Envelope, error) {
	key := commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.SubjectID +
		"\x00" + string(e.ObligationID) + "\x00" + strconv.FormatInt(e.ObligationVersion, 10)
	if e.ConsolidationID != "" { // V0.4; the ids of earlier events stay as they were
		key += "\x00" + e.Stage + "\x00" + e.ConsolidationID
	}
	digest := sha256.Sum256([]byte(key))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), SubjectID: e.SubjectID,
		ObligationID: string(e.ObligationID), ObligationVersion: e.ObligationVersion, Stage: e.Stage, ConsolidationID: e.ConsolidationID})
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
