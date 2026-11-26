package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
	transport "go.idp.xyz/idp-cargo/internal/transport/http"
)

// APIConfig configures the HTTP API: how bearer tokens are verified, the logger, and RATE's margin policy until the
// legal entities' commercial policy is configurable (the floor of a quote's margin and how far an overridden line may
// sell below its rated sell, in basis points).
//
// Until the IDP identity platform is connected, tokens are HS256-signed with TokenKey (32 bytes or more), name
// TokenIssuer and TokenAudience, and live 24 hours at most: for development and tests only.
type APIConfig struct {
	TokenKey           []byte
	TokenIssuer        string
	TokenAudience      string
	Logger             *slog.Logger
	MarginFloorBP      int
	RateCardDiscountBP int
}

func (c APIConfig) verifier() identity.HS256Verifier {
	return identity.HS256Verifier{Key: c.TokenKey, Issuer: c.TokenIssuer, Audience: c.TokenAudience, Leeway: time.Minute}
}

// API assembles the contexts the HTTP API serves, each authorized by the permissions of the request's verified
// principal (identity.Authorize), and answers the handler: RATE's catalogue, with ORG for RATE's party checks,
// SEA's ocean movements and container visits, with BOOK's bookings and FUL's plans, which SEA reads, and FIN's profit
// and cost review (V2.99), with FUL's and PHY's facts, which FIN reads. COM, FUL, PHY and BOOK have no routes yet.
func (p *Platform) API(cfg APIConfig) (http.Handler, error) {
	switch {
	case len(cfg.TokenKey) < 32 || cfg.TokenIssuer == "" || cfg.TokenAudience == "":
		return nil, errors.New("bootstrap: the API needs a token key of 32 bytes or more, an issuer and an audience")
	case cfg.MarginFloorBP < 0 || cfg.MarginFloorBP > 10_000 || cfg.RateCardDiscountBP < 0 || cfg.RateCardDiscountBP > 10_000:
		return nil, errors.New("bootstrap: the margin floor and rate card discount are 0 to 10,000 basis points")
	}
	org, err := p.ORG(permissions{})
	if err != nil {
		return nil, err
	}
	rate, err := p.RATE(org, permissions{}, margins{floor: cfg.MarginFloorBP, discount: cfg.RateCardDiscountBP})
	if err != nil {
		return nil, err
	}
	com, err := p.COM(org, rate, permissions{})
	if err != nil {
		return nil, err
	}
	ful, phy, err := p.FULAndPHY(com, permissions{}, permissions{})
	if err != nil {
		return nil, err
	}
	book, err := p.BOOK(org, permissions{})
	if err != nil {
		return nil, err
	}
	sea, err := p.SEA(book, ful, permissions{})
	if err != nil {
		return nil, err
	}
	// FIN's routes read the profit and run the cost review; no route issues a debit note, which is the only use FIN has
	// for DOC's snapshots, so they stay unbound (a debit note asked for in process would be refused
	// ErrDocumentsUnbound).
	fin, err := p.FIN(org, rate, ful, &DocumentSnapshots{}, phy, permissions{})
	if err != nil {
		return nil, err
	}
	return transport.NewHandler(transport.Config{Verifier: cfg.verifier(), Logger: cfg.Logger, Ready: p.pool.Ping}, rate, sea, fin)
}

// APIServer is an http.Server for the API's handler with Cargo's timeouts; its requests run under base.
func APIServer(addr string, h http.Handler, logger *slog.Logger, base context.Context) *http.Server {
	return transport.Server(addr, h, logger, base)
}

// MintDevToken signs a development token the API configured with the same key, issuer and audience accepts, and
// checks that it does: never for production identities.
func MintDevToken(cfg APIConfig, tenant, actor string, permissions []string, ttl time.Duration) (string, error) {
	now := time.Now()
	token, err := identity.SignHS256(cfg.TokenKey, identity.Claims{Issuer: cfg.TokenIssuer, Audience: cfg.TokenAudience, Subject: actor,
		TenantID: tenant, Permissions: permissions, IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	if _, err = cfg.verifier().Verify(context.Background(), token); err != nil {
		return "", err
	}
	return token, nil
}

// permissions authorizes each context's actions by the principal's permissions: "<context>:<Action>".
type permissions struct{}

func (permissions) AuthorizeRoleApproval(ctx context.Context, tenant, actor, _, _ string) error {
	return identity.Authorize(ctx, "org", tenant, actor, "ApproveRole")
}

// AuthorizePartyAction covers ORG's party actions. The actions of customer onboarding (V2.102), "org:ProposeOnboarding"
// (the implementation consultant's proposal), "org:ConfirmOnboarding" (the customer's owner's confirmation) and
// "org:RecordOnboardingModule" (the platform's record of what each module made of it), are granted by name only: "org:*"
// and "*" grant none, so that a general grant never lets a person choose how a customer's controls are set, confirm that
// choice or write its progress. The last is held only by the principal the onboarding builds, which carries a mark no
// token can carry, so no person's role holds it.
func (permissions) AuthorizePartyAction(ctx context.Context, tenant, actor, action, _ string) error {
	if action == orgapi.ActionRecordOnboardingModule {
		if ctx.Value(platformRecorder{}) == nil { // the principal the onboarding builds, not a person's token that carries the permission
			return fmt.Errorf("%w: org:%s is the platform's own, which no person holds", identity.ErrDenied, action)
		}
		return identity.AuthorizeNamed(ctx, "org", tenant, actor, action)
	}
	if action == orgapi.ActionProposeOnboarding || action == orgapi.ActionConfirmOnboarding {
		return identity.AuthorizeNamed(ctx, "org", tenant, actor, action)
	}
	return identity.Authorize(ctx, "org", tenant, actor, action)
}

func (permissions) AuthorizeQuoteAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "rate", tenant, actor, action)
}

func (permissions) AuthorizeOrderAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "com", tenant, actor, action)
}

func (permissions) AuthorizeShipmentAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "ful", tenant, actor, action)
}

func (permissions) AuthorizeConsolidationAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "phy", tenant, actor, action)
}

func (permissions) AuthorizeBookingAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "book", tenant, actor, action)
}

// AuthorizeSeaAction covers SEA's commands and the two reads SEA itself authorizes: a VGM signatory's name
// ("sea:ReadVgmSignatory") and the log of those reads ("sea:ReadVgmSignatoryAccess"). Those two are granted by name
// only: "sea:Read", "sea:*" and "*" grant neither, so that a general grant never includes a person's name kept
// apart (the owner's decision of 2026-11-18, with P4, P6 and P7). So are the three of SEA's settings (V2.100):
// "sea:ConfigureSettings" (propose and cancel a change), "sea:ApproveSettings" (approve one) and
// "sea:SuperviseCarrierConflicts" (release a carrier's contradiction under S1 SUPERVISOR_ONLY), so that a general grant
// never lets a person change how SEA's controls work, approve the change or release as a supervisor. That the approver
// is not the proposer is SEA's own rule, not a permission.
func (permissions) AuthorizeSeaAction(ctx context.Context, tenant, actor, action, _ string) error {
	switch action {
	case seaapi.ActionReadVgmSignatory, seaapi.ActionReadVgmSignatoryLog, seaapi.ActionConfigureSettings, seaapi.ActionApproveSettings,
		seaapi.ActionSuperviseCarrierConflicts, seaapi.ActionDeclareSingleAdministrator:
		return identity.AuthorizeNamed(ctx, "sea", tenant, actor, action)
	}
	return identity.Authorize(ctx, "sea", tenant, actor, action)
}

// AuthorizeFinAction covers FIN's commands. The two actions of the settings, "fin:ConfigureSettings" (propose and
// cancel a change) and "fin:ApproveSettings" (approve one), are granted by name only: "fin:*" and "*" grant neither, so
// that a general grant never lets a person change how FIN's controls work or approve the change, as with a VGM
// signatory's name above; so is "fin:DeclareSingleAdministrator" (V2.102), which only the platform's onboarding holds,
// and "fin:ReviewAfterTheFact" (V2.102, batch B), which marks a week of the after-the-fact review list reviewed, so
// that a general grant never lets a person sign off the weekly compensation of the LEAN profile. That the approver is
// not the proposer, and that the reviewer is neither the submitter nor the previewer of a line of the week, are FIN's
// own rules, not permissions.
func (permissions) AuthorizeFinAction(ctx context.Context, tenant, actor, action, _ string) error {
	if action == finapi.ActionConfigureSettings || action == finapi.ActionApproveSettings || action == finapi.ActionDeclareSingleAdministrator ||
		action == finapi.ActionReviewAfterTheFact {
		return identity.AuthorizeNamed(ctx, "fin", tenant, actor, action)
	}
	return identity.Authorize(ctx, "fin", tenant, actor, action)
}

// margins is RATE's margin and discount policy, one for every legal entity.
type margins struct{ floor, discount int }

func (m margins) MarginFloorBP(context.Context, string, string) (int, error) { return m.floor, nil }
func (m margins) RateCardDiscountBP(context.Context, string, string) (int, error) {
	return m.discount, nil
}
