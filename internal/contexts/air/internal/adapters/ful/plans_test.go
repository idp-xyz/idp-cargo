package ful

import (
	"context"
	"errors"
	"fmt"
	"testing"

	app "go.idp.xyz/idp-cargo/internal/contexts/air/internal/application"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
)

type stub struct{ err error }

func (s stub) GetAirOperationPlan(context.Context, fulapi.GetShipmentPlanSnapshot) (fulapi.AirOperationPlan, error) {
	return fulapi.AirOperationPlan{}, s.err
}

// FUL's errors reach AIR's caller in AIR's own kinds, never FUL's: a shipment FUL does not know or a question it
// rejects is an unknown shipment, any other kind is text, and what is not FUL's passes unchanged.
func TestPlanErrorsAreAIRs(t *testing.T) {
	ctx := context.Background()
	kinds := []error{fulapi.ErrUnauthenticated, fulapi.ErrUnauthorized, fulapi.ErrNotFound, fulapi.ErrConflict, fulapi.ErrIdempotencyConflict, fulapi.ErrRejected}
	for _, kind := range kinds {
		_, err := Plans{FUL: stub{err: fmt.Errorf("%w: stub", kind)}}.Plan(ctx, "T", "SH-1")
		for _, k := range kinds {
			if errors.Is(err, k) {
				t.Errorf("%v reaches AIR's caller as FUL's kind: %v", kind, err)
			}
		}
		unknown := kind == fulapi.ErrNotFound || kind == fulapi.ErrRejected
		if err == nil || errors.Is(err, app.ErrUnknownShipment) != unknown {
			t.Errorf("%v: unknown shipment %v, want %v: %v", kind, !unknown, unknown, err)
		}
	}
	if _, err := (Plans{FUL: stub{err: context.DeadlineExceeded}}).Plan(ctx, "T", "SH-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a deadline passes unchanged: %v", err)
	}
}
