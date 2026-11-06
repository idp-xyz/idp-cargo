// Package messaging holds Cargo's event transport adapters.
package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"go.idp.xyz/idp-bento-go/eventing"
)

// Webhook publishes each envelope as a JSON POST with the event ID as Idempotency-Key. A 2xx answer
// means delivered; a 4xx other than 408 and 429 is a permanent rejection; anything else is retried.
type Webhook struct {
	URL    string
	Client *http.Client
}

var _ eventing.Publisher = Webhook{}

func (w Webhook) Publish(ctx context.Context, envelope eventing.Envelope) error {
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("%w: %w", eventing.ErrPublishRejected, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", string(envelope.ID))
	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		return nil
	case code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests:
		return fmt.Errorf("%w: sink answered %s", eventing.ErrPublishRejected, resp.Status)
	default:
		return fmt.Errorf("messaging: sink answered %s", resp.Status)
	}
}
