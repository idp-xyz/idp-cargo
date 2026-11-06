package messaging

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
)

// Failure codes recorded by Relay.
const (
	FailurePublishFailed   eventing.FailureCode = "publish.failed"
	FailurePublishRejected eventing.FailureCode = "publish.rejected"
)

// Relay moves committed Outbox deliveries to a Publisher in claim order. Delivery is at least once:
// a crash between Publish and MarkPublished re-delivers after the lease expires, so consumers must
// deduplicate by event ID.
//
// A batch shares one lease and is published one delivery at a time, so Batch × PublishTimeout must
// be shorter than LeaseFor. Leases compare Clock readings, so workers need synchronized clocks.
type Relay struct {
	Outbox interface {
		eventing.OutboxClaimer
		eventing.OutboxFinalizer
	}
	Publisher      eventing.Publisher
	Clock          application.Clock
	Batch          int
	LeaseFor       time.Duration
	PublishTimeout time.Duration // bound on one Publish; a publish cut off by it counts as a failure
	MaxAttempts    uint32
	MaxFailures    uint32
	RetryBase      time.Duration // delay after the first failure, doubled per recorded failure up to RetryMax
	RetryMax       time.Duration
	IdleWait       time.Duration
	Logger         *slog.Logger
}

// Run relays until ctx is canceled and then returns ctx.Err().
func (r Relay) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	for {
		claimed, err := r.RunOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			r.logger().Error("outbox relay round failed", "error", err)
		}
		if claimed > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.IdleWait):
		}
	}
}

// RunOnce claims one batch and settles each delivery. It returns how many deliveries it claimed.
func (r Relay) RunOnce(ctx context.Context) (int, error) {
	if err := r.validate(); err != nil {
		return 0, err
	}
	deliveries, err := r.Outbox.Claim(ctx, eventing.OutboxClaim{
		Now: r.Clock.Now(), Limit: r.Batch, LeaseFor: r.LeaseFor, MaxAttempts: r.MaxAttempts,
	})
	if err != nil {
		return 0, err
	}
	for _, d := range deliveries {
		if err := r.settle(ctx, d); err != nil {
			return len(deliveries), err
		}
	}
	return len(deliveries), nil
}

func (r Relay) settle(ctx context.Context, d eventing.Delivery) error {
	if err := d.Envelope.Validate(); err != nil {
		return r.leaseLostIsSettled(d, r.Outbox.Abandon(ctx, d.Ref, r.Clock.Now(), eventing.FailureEnvelopeInvalidOnPublish))
	}
	publishCtx, cancel := context.WithTimeout(ctx, r.PublishTimeout)
	err := r.Publisher.Publish(publishCtx, d.Envelope)
	cancel()
	switch {
	case err == nil:
		return r.leaseLostIsSettled(d, r.Outbox.MarkPublished(ctx, d.Ref, r.Clock.Now()))
	case ctx.Err() != nil:
		// Stopping: the lease expires and another worker re-delivers.
		return ctx.Err()
	case errors.Is(err, eventing.ErrPublishRejected):
		r.logger().Warn("outbox delivery rejected by sink", "event_id", d.Envelope.ID, "error", err)
		return r.leaseLostIsSettled(d, r.Outbox.Abandon(ctx, d.Ref, r.Clock.Now(), FailurePublishRejected))
	default:
		r.logger().Warn("outbox publish failed", "event_id", d.Envelope.ID, "failures", d.Failures+1, "error", err)
		failedAt := r.Clock.Now()
		return r.leaseLostIsSettled(d, r.Outbox.RecordFailure(ctx, d.Ref, eventing.DeliveryFailure{
			FailedAt: failedAt, RetryAt: failedAt.Add(r.backoff(d.Failures)), Code: FailurePublishFailed, MaxFailures: r.MaxFailures,
		}))
	}
}

// leaseLostIsSettled treats a lost lease as settled: another worker owns the delivery now.
func (r Relay) leaseLostIsSettled(d eventing.Delivery, err error) error {
	if errors.Is(err, eventing.ErrLeaseLost) {
		r.logger().Warn("outbox lease lost", "event_id", d.Envelope.ID)
		return nil
	}
	return err
}

func (r Relay) backoff(failures uint32) time.Duration {
	delay := r.RetryBase
	for i := uint32(0); i < failures && delay < r.RetryMax; i++ {
		delay *= 2
	}
	return min(delay, r.RetryMax)
}

func (r Relay) validate() error {
	if r.Outbox == nil || r.Publisher == nil || r.Clock == nil || r.Batch < 1 || r.Batch > eventing.MaxOutboxClaimLimit ||
		r.LeaseFor <= 0 || r.PublishTimeout <= 0 || r.MaxAttempts == 0 || r.MaxFailures == 0 || r.IdleWait <= 0 {
		return errors.New("messaging: relay misconfigured")
	}
	if time.Duration(r.Batch)*r.PublishTimeout >= r.LeaseFor {
		return errors.New("messaging: relay lease must outlast Batch × PublishTimeout")
	}
	return nil
}

func (r Relay) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}
