package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-bento-go/testkit"
)

func TestWebhookPostsEnvelopeWithIdempotencyKey(t *testing.T) {
	envelope := testkit.ValidEnvelope()
	var got eventing.Envelope
	var key, contentType string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, contentType = r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()
	if err := (Webhook{URL: sink.URL}).Publish(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if got.ID != envelope.ID || key != string(envelope.ID) || contentType != "application/json" {
		t.Fatalf("sink saw id=%s key=%s content-type=%s", got.ID, key, contentType)
	}
}

func TestWebhookClassifiesSinkAnswers(t *testing.T) {
	for status, rejected := range map[int]bool{
		http.StatusBadRequest:          true,
		http.StatusUnprocessableEntity: true,
		http.StatusRequestTimeout:      false,
		http.StatusTooManyRequests:     false,
		http.StatusServiceUnavailable:  false,
	} {
		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		err := (Webhook{URL: sink.URL}).Publish(context.Background(), testkit.ValidEnvelope())
		sink.Close()
		if err == nil || errors.Is(err, eventing.ErrPublishRejected) != rejected {
			t.Errorf("status %d: err = %v, want rejected=%v", status, err, rejected)
		}
	}
}
