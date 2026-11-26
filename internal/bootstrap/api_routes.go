package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/org"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate"
	"go.idp.xyz/idp-cargo/internal/contexts/sea"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
	transport "go.idp.xyz/idp-cargo/internal/transport/http"
)

// The API's route table (since V2.106): every context's routes and the platform's own. APIRoutes answers it without a
// database, for the OpenAPI document (cmd/cargo-openapi, contracts/openapi/cargo.json) and its test.

// apiRegistrars are the registrars the API serves, in the order they register.
func apiRegistrars(r *rate.Module, s *sea.Module, f *fin.Module, o *org.Module, onboarding *Onboarding) []transport.Registrar {
	return []transport.Registrar{r, s, f, o, platformRoutes{onboarding}}
}

// APIRoutes answers the API's route table.
func APIRoutes() []httpapi.Op {
	return transport.Table(apiRegistrars(nil, nil, nil, nil, nil)...)
}

// platformRoutes are the routes of what only the platform composes: a customer's onboarding, which ORG records and FIN
// and SEA apply (V2.102). Each checks the person's own permissions by name before it calls a module.
type platformRoutes struct{ o *Onboarding }

// onboardingCodes are the business codes of the onboarding's own refusals, and the status each is answered with.
var onboardingCodes = []struct {
	err  error
	kind error
	code string
}{
	{ErrOnboardingIncomplete, orgapi.ErrConflict, "ONBOARDING_INCOMPLETE"}, // recorded; the same command sent again completes it
	{ErrOnboardingBlocked, orgapi.ErrRejected, "ONBOARDING_BLOCKED"},
	{ErrOnboardingBuild, orgapi.ErrRejected, "ONBOARDING_BUILD_DIFFERS"},
	{ErrOnboardingHeld, orgapi.ErrRejected, "ONBOARDING_HELD_CHANGE_SET"},
	{ErrInvalidActor, orgapi.ErrRejected, "ONBOARDING_INVALID_ACTOR"},
}

// onboardingError gives an onboarding refusal its status and business code; ORG's, FIN's and SEA's own errors keep theirs.
func onboardingError(err error) error {
	for _, c := range onboardingCodes {
		if errors.Is(err, c.err) {
			return fmt.Errorf("%w: %w", c.kind, errors.New("bootstrap: "+c.code+": "+err.Error()))
		}
	}
	return err
}

func platform[C, R any](call func(context.Context, C) (R, error)) func(context.Context, C) (R, error) {
	return func(ctx context.Context, c C) (R, error) {
		out, err := call(ctx, c)
		return out, onboardingError(err)
	}
}

func (p platformRoutes) RegisterHTTP(r *httpapi.Routes) {
	op := func(path, calls, action, summary string) httpapi.Op {
		return httpapi.Op{Path: path, Context: "org", Calls: calls, Action: action, Named: true, Summary: summary}
	}
	propose := op("/v1/onboardings", "ProposeOnboarding", orgapi.ActionProposeOnboarding,
		"The implementation consultant proposes a customer's onboarding: org:ProposeOnboarding, fin:ConfigureSettings and sea:ConfigureSettings by name")
	propose.Status = http.StatusCreated
	httpapi.Command(r, orgKinds, propose, platform(p.o.ProposeOnboarding))
	httpapi.Command(r, orgKinds, op("/v1/onboardings/{onboarding_id}/confirm", "ConfirmOnboarding", orgapi.ActionConfirmOnboarding,
		"The customer's owner confirms the plan by its SHA-256: org:ConfirmOnboarding, fin:ApproveSettings and sea:ApproveSettings by name"), platform(p.o.ConfirmOnboarding))
	httpapi.Command(r, orgKinds, op("/v1/onboardings/{onboarding_id}/withdraw", "WithdrawOnboarding", orgapi.ActionProposeOnboarding,
		"The consultant who proposed it, or the owner, withdraws an onboarding that waits"), platform(p.o.WithdrawOnboarding))
}

var orgKinds = httpapi.Errors{Unauthenticated: orgapi.ErrUnauthenticated, Unauthorized: orgapi.ErrUnauthorized, NotFound: orgapi.ErrNotFound,
	Conflict: orgapi.ErrConflict, IdempotencyConflict: orgapi.ErrIdempotencyConflict, Rejected: orgapi.ErrRejected}

// APIVersion is the version the OpenAPI document states: the archive version that last changed the routes.
const APIVersion = "2.106"

// OpenAPIDocument answers the API's OpenAPI document.
func OpenAPIDocument() ([]byte, error) {
	return httpapi.OpenAPI("IDP Cargo API", APIVersion, APIRoutes())
}
