package bootstrap

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// V2.102 batch E (R2-F6): each error a module can answer to a step of a confirmation is classed as one that sending the
// same confirmation again can never cure (permanent: the way out is a new onboarding) or not. A cause that is not permanent is
// never called so, and one that depends on the build names both ways out. SETTINGS_CANCELLED, which asPermanent also classes as
// permanent, is reached only by a cancellation between the listing of the module's change sets and the approval, a race: it has no
// test (a refused change set, SETTINGS_REFUSED, is the same branch of the condition and is tested).
func TestEachModuleErrorIsClassedAsPermanentOrNotForItsStep(t *testing.T) {
	refused := func(kind error) error { return settings.Refuse(kind, "the module's own words") }
	for _, c := range []struct {
		name      string
		step      string
		err       error
		permanent bool
		build     bool // not permanent, and the build is a way out too
	}{
		{"a derived command id taken, FIN", "propose", fmt.Errorf("%w: x", finapi.ErrIdempotencyConflict), true, false},
		{"a derived command id taken, SEA", "approve", fmt.Errorf("%w: x", seaapi.ErrIdempotencyConflict), true, false},
		{"a change set refused", "approve", refused(settings.ErrRefusedChange), true, false},
		{"an approver who is the proposer", "approve", refused(settings.ErrSameActor), true, false},
		{"an acknowledgement where none is due", "approve", refused(settings.ErrAcknowledgement), true, false},
		{"an unknown item when proposing", "propose", refused(settings.ErrUnknownItem), false, true},
		{"an unknown value when proposing", "propose", refused(settings.ErrUnknownValue), false, true},
		{"a scope when proposing", "propose", refused(settings.ErrScope), false, true},
		{"a parameter when proposing", "propose", refused(settings.ErrParameter), false, true},
		{"a duplicate when proposing", "propose", refused(settings.ErrDuplicate), false, true},
		{"a shape when proposing", "propose", refused(settings.ErrInvalid), false, true},
		{"a shape when approving (the clock)", "approve", refused(settings.ErrInvalid), false, false},
		{"a cross-item rule against the tenant's other change sets", "propose", refused(settings.ErrRule), false, false},
		{"a clock behind", "propose", refused(settings.ErrClock), false, false},
		{"a clock skew", "propose", refused(settings.ErrClockSkew), false, false},
		{"an order", "propose", refused(settings.ErrOrder), false, false},
		{"an effective time", "propose", refused(settings.ErrEffective), false, false},
		{"already approved", "approve", refused(settings.ErrAlreadyApproved), false, false},
		{"a declaration not later than the latest", "declare", refused(settings.ErrInvalid), false, false},
		{"a database that is down", "propose", errors.New("connection refused"), false, false},
	} {
		got := asPermanent("FIN", c.step, c.err)
		var p permanent
		isPermanent := errors.As(got, &p)
		if isPermanent != c.permanent {
			t.Errorf("%s: permanent %v, want %v (%v)", c.name, isPermanent, c.permanent, got)
		}
		text := got.Error()
		if c.permanent && !strings.Contains(text, "propose a new onboarding under another id") {
			t.Errorf("%s: a permanent failure without its way out: %v", c.name, got)
		}
		if c.build {
			if !errors.Is(got, ErrOnboardingBuild) || !strings.Contains(text, "run the build the plan was made with") || !strings.Contains(text, "propose a new onboarding under another id") {
				t.Errorf("%s: the build is not named as a way out beside the new onboarding: %v", c.name, got)
			}
		} else if !c.permanent && got != c.err {
			t.Errorf("%s: a curable error was changed: %v", c.name, got)
		}
	}
}

func TestAnActorIdThatNoModuleAcceptsIsRefusedWithACodeAndTheWayOut(t *testing.T) {
	for name, id := range map[string]string{"blank": "", "only spaces": "   ", "padded": " olga", "padded after": "olga ", "129 bytes": strings.Repeat("o", 129),
		"120 and 40 spaces": strings.Repeat("o", 120) + strings.Repeat(" ", 40), "a tab": "olga\t"} {
		err := checkActor("the owner's actor id", id, false)
		if !errors.Is(err, ErrInvalidActor) || !strings.Contains(err.Error(), "ONBOARDING_INVALID_ACTOR") || !strings.Contains(err.Error(), "give the actor id exactly as the identity platform names the person") ||
			!strings.Contains(err.Error(), "nothing is recorded") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, id := range map[string]string{"one byte": "o", "128 bytes": strings.Repeat("o", 128), "an inner space": "o l g a"} {
		if err := checkActor("the owner's actor id", id, false); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := checkActor("the administrator's actor id", "", true); err != nil {
		t.Errorf("an optional id left empty: %v", err)
	}
	if err := checkActor("the administrator's actor id", " ", true); !errors.Is(err, ErrInvalidActor) {
		t.Errorf("an optional id of one space: %v", err)
	}
}
