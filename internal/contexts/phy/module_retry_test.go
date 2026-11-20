package phy

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// TestARetryableConflictIsAnsweredAsAConflict: what reaches the caller after the retries ran out is the api's conflict
// kind, as it was before they existed, and it is still the retryable cause underneath.
func TestARetryableConflictIsAnsweredAsAConflict(t *testing.T) {
	cause := fmt.Errorf("%w: load execution X-1 is busy with another command; try again", domain.ErrRetryable)
	got := classify(cause)
	if !errors.Is(got, api.ErrConflict) || !errors.Is(got, domain.ErrRetryable) || errors.Is(got, api.ErrRejected) {
		t.Errorf("classify(%v) = %v", cause, got)
	}
}

type noIntentQueries struct{ fulapi.LoadIntentQueries }
type noAuthorizer struct{ api.Authorizer }

// TestWithConflictRetryReachesTheCommandPath: what WithConflictRetry gives is what every command's Deps carry (the
// retry stopped working with the second argument dropped from deps and nothing failed), and without the option the Deps
// carry zeroes, which the command path reads as its defaults of 6 attempts and 5 ms.
func TestWithConflictRetryReachesTheCommandPath(t *testing.T) {
	m := &Module{}
	if d := m.deps("T", "c"); d.Attempts != 0 || d.Backoff != 0 {
		t.Errorf("without the option the Deps carry %d attempts and %v", d.Attempts, d.Backoff)
	}
	WithConflictRetry(3, 7*time.Millisecond)(m)
	if d := m.deps("T", "c"); d.Attempts != 3 || d.Backoff != 7*time.Millisecond {
		t.Errorf("the Deps carry %d attempts and %v, want 3 and 7 ms", d.Attempts, d.Backoff)
	}
}

// TestWithConflictRetryRefusesWhatItCannotMean: 0 attempts would silently mean six and fewer are meaningless, and a
// negative wait is meaningless; New refuses them, naming the option, before it touches the database. 1 attempt and a
// backoff of 0 (the default) are accepted by the option.
func TestWithConflictRetryRefusesWhatItCannotMean(t *testing.T) {
	for name, opt := range map[string]Option{"no attempts": WithConflictRetry(0, time.Millisecond), "negative attempts": WithConflictRetry(-1, time.Millisecond),
		"negative backoff": WithConflictRetry(3, -time.Millisecond)} {
		_, err := New(&bentopg.DB{}, noIntentQueries{}, noAuthorizer{}, opt)
		if err == nil || !strings.Contains(err.Error(), "WithConflictRetry") {
			t.Errorf("%s: New answered %v, want an error naming WithConflictRetry", name, err)
		}
	}
	m := &Module{}
	WithConflictRetry(1, 0)(m)
	if !m.retrySet || m.attempts != 1 || m.backoff != 0 || m.attempts < 1 || m.backoff < 0 {
		t.Errorf("one attempt and the default backoff: %+v", m)
	}
}
