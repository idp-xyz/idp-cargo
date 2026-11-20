package domain

import (
	"errors"
	"fmt"
	"testing"
)

// TestARetryableConflictIsAConflict: it is an ErrConflict for every caller that asks, says the words of one, and a
// plain ErrConflict, the stale revision, is not retryable.
func TestARetryableConflictIsAConflict(t *testing.T) {
	err := fmt.Errorf("%w: load execution X-1 is busy with another command; try again", ErrRetryable)
	if !errors.Is(err, ErrConflict) || !errors.Is(err, ErrRetryable) || ErrRetryable.Error() != ErrConflict.Error() {
		t.Errorf("%v: conflict %v, retryable %v", err, errors.Is(err, ErrConflict), errors.Is(err, ErrRetryable))
	}
	if stale := fmt.Errorf("%w: stale", ErrConflict); errors.Is(stale, ErrRetryable) || errors.Is(ErrConflict, ErrRetryable) {
		t.Error("a stale revision is not retryable")
	}
}
