//go:build bento

// Package bento is the explicit Cargo-to-Bento seam; only compiled using Go 1.26.5
// with exact dependency resolution. It has NOT been validated by this sandbox.
package bento

import (
	"context"
	"encoding/json"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentodomain "go.idp.xyz/idp-bento-go/domain"
	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-bento-go/repository"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/domain"
)

// Prevent type-system drift by compiling against the actual public contracts in target CI.
var _ application.Clock = SystemClock{}
var _ application.Transactor = (*TransactionPort)(nil)
var _ eventing.OutboxWriter = (*OutboxPort)(nil)
var _ bentodomain.Event = ORGDomainEvent{}
var _ repository.VersionedUpdater[ScopedPartyKey, *org.BusinessParty] = (*PartyRepositoryPort)(nil)

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type ScopedPartyKey struct {
	TenantID org.TenantID
	ID       org.PartyID
}

// Ports below deliberately have no concrete storage wiring; adapters must be backed by
// pgx repository and bento/postgres in a genuine integration run.
type TransactionPort struct {
	Within func(context.Context, application.TxFunc) error
}

func (p *TransactionPort) WithinTransaction(ctx context.Context, fn application.TxFunc) error {
	return p.Within(ctx, fn)
}

type OutboxPort struct {
	Write func(context.Context, eventing.Envelope) error
}

func (p *OutboxPort) Enqueue(ctx context.Context, e eventing.Envelope) error { return p.Write(ctx, e) }

type PartyRepositoryPort struct {
	Write func(context.Context, ScopedPartyKey, *org.BusinessParty, repository.Revision) (repository.Revision, error)
}

func (p *PartyRepositoryPort) Update(ctx context.Context, key ScopedPartyKey, a *org.BusinessParty, expected repository.Revision) (repository.Revision, error) {
	return p.Write(ctx, key, a, expected)
}

type ORGDomainEvent struct{ org.Event }

func (e ORGDomainEvent) Type() bentodomain.EventType       { return bentodomain.EventType(e.Event.Type) }
func (e ORGDomainEvent) Version() bentodomain.EventVersion { return 1 }
func (e ORGDomainEvent) OccurredAt() time.Time             { return e.Event.At }

func ToEnvelope(id string, e org.Event, recordedAt time.Time) (eventing.Envelope, error) {
	payload, err := json.Marshal(struct {
		Tenant string `json:"tenant"`
		Party  string `json:"party"`
	}{string(e.TenantID), e.SubjectID})
	if err != nil {
		return eventing.Envelope{}, err
	}
	out := eventing.Envelope{SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(id), Source: "idp-cargo.org", Type: eventing.EventType(e.Type), Version: 1, Scope: string(e.TenantID), Subject: e.SubjectID, PartitionKey: string(e.TenantID) + ":" + e.SubjectID, OccurredAt: e.At, RecordedAt: recordedAt, ContentType: eventing.JSONContentType, Payload: payload}
	return out, out.Validate()
}
