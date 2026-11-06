//go:build bento

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/domain"
)

type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueORGEvent(ctx context.Context, e org.Event) error {
	// Event ID is deterministic per immutable command ID and event type.
	// This makes retried uncertain outcomes traceable; receipts still enforce command idempotency.
	digest := sha256.Sum256([]byte(o.CommandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.SubjectID))
	payload, err := json.Marshal(struct {
		TenantID  string `json:"tenant_id"`
		SubjectID string `json:"subject_id"`
	}{string(e.TenantID), e.SubjectID})
	if err != nil {
		return err
	}
	envelope := eventing.Envelope{
		SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(hex.EncodeToString(digest[:])),
		Source: "idp-cargo.org", Type: eventing.EventType(e.Type), Version: 1,
		Scope: string(e.TenantID), Subject: e.SubjectID,
		PartitionKey: string(e.TenantID) + ":" + e.SubjectID,
		OccurredAt:   e.At.UTC(), RecordedAt: o.RecordedAt.UTC(),
		ContentType: eventing.JSONContentType, Payload: payload,
	}
	if err = envelope.Validate(); err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}
