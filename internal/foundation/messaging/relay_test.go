package messaging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-bento-go/testkit"
)

type fakeOutbox struct {
	deliveries  []eventing.Delivery
	claims      []eventing.OutboxClaim
	published   []eventing.EventID
	failures    []eventing.DeliveryFailure
	abandoned   []eventing.FailureCode
	finalizeErr error
}

func (f *fakeOutbox) Claim(_ context.Context, c eventing.OutboxClaim) ([]eventing.Delivery, error) {
	f.claims = append(f.claims, c)
	d := f.deliveries
	f.deliveries = nil
	return d, nil
}

func (f *fakeOutbox) MarkPublished(_ context.Context, ref eventing.DeliveryRef, _ time.Time) error {
	f.published = append(f.published, ref.EventID)
	return f.finalizeErr
}

func (f *fakeOutbox) RecordFailure(_ context.Context, _ eventing.DeliveryRef, failure eventing.DeliveryFailure) error {
	f.failures = append(f.failures, failure)
	return f.finalizeErr
}

func (f *fakeOutbox) Abandon(_ context.Context, _ eventing.DeliveryRef, _ time.Time, code eventing.FailureCode) error {
	f.abandoned = append(f.abandoned, code)
	return f.finalizeErr
}

var relayNow = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

func delivery(id string, failures uint32) eventing.Delivery {
	envelope := testkit.ValidEnvelope()
	envelope.ID = eventing.EventID(id)
	return eventing.Delivery{Ref: eventing.DeliveryRef{Source: envelope.Source, EventID: envelope.ID, LeaseToken: "lease"}, Envelope: envelope, Attempt: failures + 1, Failures: failures}
}

func testRelay(outbox *fakeOutbox, publisher eventing.PublisherFunc) Relay {
	return Relay{
		Outbox: outbox, Publisher: publisher, Clock: testkit.NewFixedClock(relayNow),
		Batch: 10, LeaseFor: 30 * time.Second, PublishTimeout: 2 * time.Second, MaxAttempts: 5, MaxFailures: 3,
		RetryBase: time.Second, RetryMax: time.Minute, IdleWait: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRelayPublishesInClaimOrder(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0), delivery("b", 0)}}
	var sent []eventing.EventID
	relay := testRelay(outbox, func(_ context.Context, e eventing.Envelope) error { sent = append(sent, e.ID); return nil })
	claimed, err := relay.RunOnce(context.Background())
	if err != nil || claimed != 2 {
		t.Fatalf("RunOnce = %d, %v", claimed, err)
	}
	if fmt.Sprint(sent) != "[a b]" || fmt.Sprint(outbox.published) != "[a b]" {
		t.Fatalf("sent %v, published %v", sent, outbox.published)
	}
	if c := outbox.claims[0]; !c.Now.Equal(relayNow) || c.Limit != 10 || c.LeaseFor != 30*time.Second || c.MaxAttempts != 5 {
		t.Fatalf("claim = %+v", c)
	}
}

func TestRelayRecordsFailureWithBackoff(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 2)}}
	relay := testRelay(outbox, func(context.Context, eventing.Envelope) error { return errors.New("sink down") })
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.failures) != 1 || len(outbox.published) != 0 {
		t.Fatalf("failures %v, published %v", outbox.failures, outbox.published)
	}
	f := outbox.failures[0]
	if !f.RetryAt.Equal(relayNow.Add(4*time.Second)) || f.Code != FailurePublishFailed || f.MaxFailures != 3 {
		t.Fatalf("failure = %+v, want retry after 4s with budget 3", f)
	}
}

func TestRelayRecordsFailureWhenPublishTimesOut(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0)}}
	relay := testRelay(outbox, func(ctx context.Context, _ eventing.Envelope) error { <-ctx.Done(); return ctx.Err() })
	relay.PublishTimeout = 20 * time.Millisecond
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("a timed-out publish must not stop the relay: %v", err)
	}
	if len(outbox.failures) != 1 || outbox.failures[0].Code != FailurePublishFailed {
		t.Fatalf("failures %v, want one %s", outbox.failures, FailurePublishFailed)
	}
}

func TestRelayAbandonsRejectedAndInvalidDeliveries(t *testing.T) {
	invalid := delivery("invalid", 0)
	invalid.Envelope.Payload = nil
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("rejected", 0), invalid}}
	var sent []eventing.EventID
	relay := testRelay(outbox, func(_ context.Context, e eventing.Envelope) error {
		sent = append(sent, e.ID)
		return fmt.Errorf("%w: 422", eventing.ErrPublishRejected)
	})
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(outbox.abandoned) != fmt.Sprint([]eventing.FailureCode{FailurePublishRejected, eventing.FailureEnvelopeInvalidOnPublish}) {
		t.Fatalf("abandoned %v", outbox.abandoned)
	}
	if fmt.Sprint(sent) != "[rejected]" {
		t.Fatalf("an invalid envelope must not reach the publisher; sent %v", sent)
	}
}

func TestRelayTreatsLostLeaseAsSettled(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0)}, finalizeErr: eventing.ErrLeaseLost}
	relay := testRelay(outbox, func(context.Context, eventing.Envelope) error { return nil })
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("lost lease must not fail the round: %v", err)
	}
}

func TestRelayLeavesDeliveryLeasedWhenStopping(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0)}}
	ctx, cancel := context.WithCancel(context.Background())
	relay := testRelay(outbox, func(ctx context.Context, _ eventing.Envelope) error { cancel(); return ctx.Err() })
	if _, err := relay.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(outbox.published)+len(outbox.failures)+len(outbox.abandoned) != 0 {
		t.Fatal("a delivery interrupted by shutdown must stay leased")
	}
}

func TestRelayBackoffDoublesUpToMax(t *testing.T) {
	relay := Relay{RetryBase: time.Second, RetryMax: 10 * time.Second}
	for failures, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		if got := relay.backoff(uint32(failures)); got != want {
			t.Errorf("backoff(%d) = %v, want %v", failures, got, want)
		}
	}
}

func TestRelayRejectsMisconfiguration(t *testing.T) {
	if _, err := (Relay{}).RunOnce(context.Background()); err == nil {
		t.Fatal("zero Relay must not run")
	}
	relay := testRelay(&fakeOutbox{}, func(context.Context, eventing.Envelope) error { return nil })
	relay.PublishTimeout = relay.LeaseFor / time.Duration(relay.Batch)
	if _, err := relay.RunOnce(context.Background()); err == nil {
		t.Fatal("a batch whose publishes can outlast the lease must not run")
	}
}
