package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// V2.102: the two actions of onboarding and the declaration of a single administrator are granted by name only.
func TestOnboardingAndDeclarationPermissionsAreGrantedByNameOnly(t *testing.T) {
	as := func(p ...string) context.Context {
		return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t1", ActorID: "ann", Permissions: p})
	}
	// these actions run under the platform's mark (batch E); what is asked here is the name, so every context carries the mark
	marked := func(ctx context.Context) context.Context { return asPlatform(ctx) }
	check := func(name string, f func(ctx context.Context) error, perm, bc string) {
		for _, wild := range []string{"*", bc + ":*"} {
			if err := f(marked(as(wild))); !errors.Is(err, identity.ErrDenied) {
				t.Errorf("%s with %s: %v", name, wild, err)
			}
		}
		if err := f(marked(as(bc + ":Other"))); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s with another name: %v", name, err)
		}
		if err := f(marked(as(perm))); err != nil {
			t.Errorf("%s with its name: %v", name, err)
		}
		if err := f(marked(identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t2", ActorID: "ann", Permissions: []string{perm}}))); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s for another tenant: %v", name, err)
		}
	}
	for _, a := range []string{orgapi.ActionProposeOnboarding, orgapi.ActionConfirmOnboarding} {
		check(a, func(ctx context.Context) error { return permissions{}.AuthorizePartyAction(ctx, "t1", "ann", a, "ob") }, "org:"+a, "org")
	}
	check("fin declare", func(ctx context.Context) error {
		return permissions{}.AuthorizeFinAction(ctx, "t1", "ann", finapi.ActionDeclareSingleAdministrator, "ob")
	}, "fin:DeclareSingleAdministrator", "fin")
	check("sea declare", func(ctx context.Context) error {
		return permissions{}.AuthorizeSeaAction(ctx, "t1", "ann", seaapi.ActionDeclareSingleAdministrator, "ob")
	}, "sea:DeclareSingleAdministrator", "sea")
	// another party action is still granted by the wildcard
	if err := (permissions{}).AuthorizePartyAction(as("org:*"), "t1", "ann", "RecordPartyContact", "p"); err != nil {
		t.Errorf("org:* for another action: %v", err)
	}
}

// V2.102 batch D: org:RecordOnboardingModule is the platform's own. It is granted by name only ("org:*" and "*" do not grant
// it), and only to the principal the onboarding builds: a token that carries the name has not the platform's mark, and the
// mark without the name is not enough.
func TestRecordOnboardingModuleIsHeldOnlyByThePrincipalTheOnboardingBuilds(t *testing.T) {
	as := func(p ...string) context.Context {
		return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t1", ActorID: "olga", Permissions: p})
	}
	ask := func(ctx context.Context) error {
		return permissions{}.AuthorizePartyAction(ctx, "t1", "olga", orgapi.ActionRecordOnboardingModule, "ob")
	}
	for name, ctx := range map[string]context.Context{
		"the wildcards": as("*", "org:*"), "the owner's names": as("org:ConfirmOnboarding", "fin:ApproveSettings", "sea:ApproveSettings"),
		"the name in a person's token": as("org:RecordOnboardingModule"), "every name": as("org:*", "org:ConfirmOnboarding", "org:ProposeOnboarding", "org:RecordOnboardingModule"),
		"the mark without the name": context.WithValue(as("org:ConfirmOnboarding"), platformRecorder{}, true),
		"another tenant":            recordingAsPlatform(context.Background(), "t2", "olga"), "another actor": recordingAsPlatform(context.Background(), "t1", "ann"),
	} {
		if err := ask(ctx); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := ask(recordingAsPlatform(context.Background(), "t1", "olga")); err != nil {
		t.Errorf("the principal the onboarding builds: %v", err)
	}
}

// V2.102 batch E (R2-F1, R2-F3): the declaration of a single administrator in FIN and in SEA, and ORG's own ProposeOnboarding
// and ConfirmOnboarding, are the platform's own: a token that carries the permission's name, alone or with every other name, is
// refused, the mark without the name is refused, and the name with the mark is granted, only to the principal and the tenant of
// the name.
func TestThePlatformsOwnActionsNeedItsMarkAndNotOnlyThePermissionsName(t *testing.T) {
	as := func(actor string, p ...string) context.Context {
		return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t1", ActorID: actor, Permissions: p})
	}
	every := []string{"*", "org:*", "fin:*", "sea:*", "org:ProposeOnboarding", "org:ConfirmOnboarding", "org:RecordOnboardingModule", "fin:ConfigureSettings", "fin:ApproveSettings",
		"fin:DeclareSingleAdministrator", "sea:ConfigureSettings", "sea:ApproveSettings", "sea:DeclareSingleAdministrator"}
	for _, a := range []struct {
		name, bc, action string
		ask              func(ctx context.Context) error
	}{
		{"ORG's ProposeOnboarding", "org", orgapi.ActionProposeOnboarding, func(ctx context.Context) error {
			return permissions{}.AuthorizePartyAction(ctx, "t1", "ann", orgapi.ActionProposeOnboarding, "ob")
		}},
		{"ORG's ConfirmOnboarding", "org", orgapi.ActionConfirmOnboarding, func(ctx context.Context) error {
			return permissions{}.AuthorizePartyAction(ctx, "t1", "ann", orgapi.ActionConfirmOnboarding, "ob")
		}},
		{"FIN's declaration", "fin", finapi.ActionDeclareSingleAdministrator, func(ctx context.Context) error {
			return permissions{}.AuthorizeFinAction(ctx, "t1", "ann", finapi.ActionDeclareSingleAdministrator, "ob")
		}},
		{"SEA's declaration", "sea", seaapi.ActionDeclareSingleAdministrator, func(ctx context.Context) error {
			return permissions{}.AuthorizeSeaAction(ctx, "t1", "ann", seaapi.ActionDeclareSingleAdministrator, "ob")
		}},
	} {
		name := a.bc + ":" + a.action
		for what, ctx := range map[string]context.Context{"the name in a token": as("ann", name), "every name and wildcard in a token": as("ann", every...)} {
			if err := a.ask(ctx); !errors.Is(err, identity.ErrDenied) || !strings.Contains(err.Error(), "platform's own") {
				t.Errorf("%s, %s: %v", a.name, what, err)
			}
		}
		if err := a.ask(asPlatform(as("ann", "*", a.bc+":*"))); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s, the mark without the name: %v", a.name, err)
		}
		if err := a.ask(asPlatform(as("ann", name))); err != nil {
			t.Errorf("%s, the name with the mark: %v", a.name, err)
		}
		if err := a.ask(asPlatform(identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t2", ActorID: "ann", Permissions: []string{name}}))); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s, another tenant's principal: %v", a.name, err)
		}
		if err := a.ask(asPlatform(as("eve", name))); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s, another actor's principal: %v", a.name, err)
		}
	}
}
