package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueRATEEvent(ctx context.Context, e rate.Event) error {
	envelope, err := RATEEnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// RATEEnvelope maps a RATE domain event to its Bento envelope. The event ID is derived from the
// command, so a retried command yields the same event ID.
func RATEEnvelope(commandID string, e rate.Event, recordedAt time.Time) (eventing.Envelope, error) {
	key := commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.SubjectID
	if e.Version != 0 {
		key += "\x00" + strconv.Itoa(e.Version)
	}
	digest := sha256.Sum256([]byte(key))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), SubjectID: e.SubjectID, Version: e.Version})
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
