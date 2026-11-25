package bootstrap

import (
	"context"
	"errors"
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// The one-step apportionment (V2.98) is authorized like FIN's other pool actions: "fin:*" and "*" grant it, as does its
// own name, and nothing grants it to another context's name or to a person with no permission. It is not one of the two
// actions granted by name only.
func TestTheOneStepActionIsAuthorizedLikeThePoolActions(t *testing.T) {
	as := func(permissions ...string) context.Context {
		return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t1", ActorID: "fin-1", Permissions: permissions})
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"fin:*", as("fin:*"), true},
		{"*", as("*"), true},
		{"its own name", as("fin:ApportionConsolCostInOneStep"), true},
		{"the preview's name", as("fin:PreviewConsolCostApportionment"), false},
		{"the settings' name", as("fin:ConfigureSettings", "fin:ApproveSettings"), false},
		{"another context's name", as("sea:*", "sea:ApportionConsolCostInOneStep"), false},
		{"no permission", as(), false},
		{"no principal", context.Background(), false},
	} {
		for _, action := range []string{finapi.ActionApportionConsolCostInOneStep, finapi.ActionPreviewConsolCostApportionment, finapi.ActionSubmitConsolCostApportionment} {
			err := permissions{}.AuthorizeFinAction(c.ctx, "t1", "fin-1", action, "P-1")
			want := c.want
			if c.name == "its own name" && action != finapi.ActionApportionConsolCostInOneStep {
				want = false
			}
			if c.name == "the preview's name" && action == finapi.ActionPreviewConsolCostApportionment {
				want = true
			}
			if (err == nil) != want || (err != nil && !errors.Is(err, identity.ErrDenied)) {
				t.Errorf("%s, %s: %v, want allowed %v", c.name, action, err, want)
			}
		}
	}
}
