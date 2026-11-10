package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueORGEvent(ctx context.Context, e org.Event) error {
	envelope, err := ORGEnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// ORGEnvelope maps an ORG domain event to its versioned Bento integration envelope.
func ORGEnvelope(commandID string, e org.Event, recordedAt time.Time) (eventing.Envelope, error) {
	// Event ID is deterministic per immutable command ID and event type.
	// This makes retried uncertain outcomes traceable; receipts still enforce command idempotency.
	digest := sha256.Sum256([]byte(commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.SubjectID))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), SubjectID: e.SubjectID,
		EntryID: e.EntryID, EntryVersion: e.EntryVersion, Revision: e.Revision, Purpose: e.Purpose})
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
