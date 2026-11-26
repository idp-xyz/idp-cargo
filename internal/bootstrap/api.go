package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
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
// and cost review (V2.99), with FUL's and PHY's facts, which FIN reads; ORG's parties, legal entities, address books,
// contact transfer register and onboardings, with the platform's onboarding commands (V2.106); COM's orders and FUL's
// shipments (V2.107); PHY's consolidations, cargo lots and load executions (V2.107); BOOK's bookings and AIR's movements
// (V2.108); DOC's transport documents, carrier documents and dispatches (V2.109).
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
	air, err := p.AIR(book, ful, permissions{})
	if err != nil {
		return nil, err
	}
	// FIN asks DOC for the house bill a debit note is on; DOC asks FIN for a customer's credit status before issuing.
	// FIN is made with DOC's snapshots unbound and they are bound once DOC is made (since V2.109).
	snapshots := &DocumentSnapshots{}
	fin, err := p.FIN(org, rate, ful, snapshots, phy, permissions{})
	if err != nil {
		return nil, err
	}
	doc, err := p.DOC(air, sea, com, org, fin, permissions{})
	if err != nil {
		return nil, err
	}
	snapshots.Bind(doc)
	onboarding, err := NewOnboarding(org, fin, sea)
	if err != nil {
		return nil, err
	}
	return transport.NewHandler(transport.Config{Verifier: cfg.verifier(), Logger: cfg.Logger, Ready: p.pool.Ping}, apiRegistrars(rate, sea, fin, org, com, ful, phy, book, air, doc, onboarding)...)
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

// onlyThePlatform refuses an action that the onboarding's own principal alone may take: a context without the platform's
// mark, which no token and no code outside this package can set, is refused whatever permissions its principal carries.
func onlyThePlatform(ctx context.Context, bc, action string) error {
	if ctx.Value(platformRecorder{}) == nil {
		return fmt.Errorf("%w: %s:%s is the platform's own, which no person holds: it is taken through the platform's onboarding, which no token can stand in for", identity.ErrDenied, bc, action)
	}
	return nil
}

// authorizeHeld authorizes the proposal of a change set held for an onboarding, in the consultant's own name, and its approval, in the
// owner's own name (V2.102, batch F): each needs the platform's mark, which only the onboarding's own calls carry, besides the
// person's own permission by name (the proposal's ConfigureSettings, the approval's ApproveSettings). So the onboarding alone holds a
// change set for an onboarding or approves one, and a person who holds ConfigureSettings cannot freeze a tenant's settings with a
// made-up onboarding.
func authorizeHeld(ctx context.Context, bc, tenant, actor, action, proposeAction, configure, approve string) error {
	if err := onlyThePlatform(ctx, bc, action); err != nil {
		return err
	}
	if action == proposeAction {
		return identity.AuthorizeNamed(ctx, bc, tenant, actor, configure)
	}
	return identity.AuthorizeNamed(ctx, bc, tenant, actor, approve)
}

// AuthorizePartyAction covers ORG's party actions. The actions of customer onboarding (V2.102), "org:ProposeOnboarding"
// (the implementation consultant's proposal), "org:ConfirmOnboarding" (the customer's owner's confirmation) and
// "org:RecordOnboardingModule" (the platform's record of what each module made of it), are granted by name only: "org:*"
// and "*" grant none, so that a general grant never lets a person choose how a customer's controls are set, confirm that
// choice or write its progress. ORG's commands for them run only under the platform's mark (V2.102, batch E): the only way
// in is the bootstrap's onboarding, which checks the plan and the consultant, and which checks the person's permission by
// name before it calls ORG. The record of a module's progress is held only by the principal the onboarding builds, which
// carries a mark no token can carry, so no person's role holds it.
func (permissions) AuthorizePartyAction(ctx context.Context, tenant, actor, action, _ string) error {
	switch action {
	case orgapi.ActionReadPartyContacts, orgapi.ActionErasePartyContact, orgapi.ActionReadContactAccessLog:
		// a person's contact details, their erasure and the log of who read them (since V2.106): by name only, as a VGM
		// signatory's name, so that "org:*" and "*" include no one's personal data
		return identity.AuthorizeNamed(ctx, "org", tenant, actor, action)
	case orgapi.ActionRecordOnboardingModule, orgapi.ActionProposeOnboarding, orgapi.ActionConfirmOnboarding:
		if err := onlyThePlatform(ctx, "org", action); err != nil { // the principal the onboarding builds, not a person's token that carries the permission
			return err
		}
		return identity.AuthorizeNamed(ctx, "org", tenant, actor, action)
	}
	return identity.Authorize(ctx, "org", tenant, actor, action)
}

func (permissions) AuthorizeQuoteAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "rate", tenant, actor, action)
}

func (permissions) AuthorizeOrderAction(ctx context.Context, tenant, actor, action, _ string) error {
	switch action {
	case comapi.ActionReadOrderContacts, comapi.ActionReadRestrictedOrderContacts, comapi.ActionEraseOrderContacts, comapi.ActionReadContactAccessLog:
		// an order's contacts, their erasure and the log of who read them (since V2.107): by name only, as ORG's
		return identity.AuthorizeNamed(ctx, "com", tenant, actor, action)
	}
	return identity.Authorize(ctx, "com", tenant, actor, action)
}

func (permissions) AuthorizeShipmentAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "ful", tenant, actor, action)
}

func (permissions) AuthorizeConsolidationAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "phy", tenant, actor, action)
}

// AuthorizeDocAction covers DOC's commands and the two reads DOC authorizes itself (since V2.109, when DOC is first
// served). Approving one's own draft ("doc:ApproveOwnTransportDocument"), issuing over a credit hold
// ("doc:OverrideCreditCheck") and reading the records of contacts provided abroad ("doc:ReadProvisionRecords") are
// granted by name only: a general grant never lets a person relax a control or read whose contacts went abroad.
func (permissions) AuthorizeDocAction(ctx context.Context, tenant, actor, action, _ string) error {
	switch action {
	case docapi.ActionApproveOwnTransportDocument, docapi.ActionOverrideCreditCheck, docapi.ActionReadProvisionRecords:
		return identity.AuthorizeNamed(ctx, "doc", tenant, actor, action)
	}
	return identity.Authorize(ctx, "doc", tenant, actor, action)
}

// AuthorizeAirAction covers AIR's commands (since V2.108, when AIR is first served).
func (permissions) AuthorizeAirAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "air", tenant, actor, action)
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
		seaapi.ActionSuperviseCarrierConflicts, seaapi.ActionReviewSettingsExceptions: // the review of a self-approval (V2.102, batch F) is by name too
		return identity.AuthorizeNamed(ctx, "sea", tenant, actor, action)
	case seaapi.ActionProposeHeldSettings, seaapi.ActionApproveHeldSettings: // a change set held for an onboarding (V2.102, batch F)
		return authorizeHeld(ctx, "sea", tenant, actor, action, seaapi.ActionProposeHeldSettings, seaapi.ActionConfigureSettings, seaapi.ActionApproveSettings)
	case seaapi.ActionDeclareSingleAdministrator: // the platform's own (V2.102, batch E): by name, and only with the platform's mark
		if err := onlyThePlatform(ctx, "sea", action); err != nil {
			return err
		}
		return identity.AuthorizeNamed(ctx, "sea", tenant, actor, action)
	}
	return identity.Authorize(ctx, "sea", tenant, actor, action)
}

// AuthorizeFinAction covers FIN's commands. The two actions of the settings, "fin:ConfigureSettings" (propose and
// cancel a change) and "fin:ApproveSettings" (approve one), are granted by name only: "fin:*" and "*" grant neither, so
// that a general grant never lets a person change how FIN's controls work or approve the change, as with a VGM
// signatory's name above; so is "fin:DeclareSingleAdministrator" (V2.102), which only the platform's onboarding holds
// (it is granted only with the platform's mark, as "org:RecordOnboardingModule" is), and "fin:ReviewAfterTheFact" (V2.102, batch B), which marks a week of the after-the-fact review list reviewed, so
// that a general grant never lets a person sign off the weekly compensation of the LEAN profile. That the approver is
// not the proposer, and that the reviewer is neither the submitter nor the previewer of a line of the week, are FIN's
// own rules, not permissions.
func (permissions) AuthorizeFinAction(ctx context.Context, tenant, actor, action, _ string) error {
	if action == finapi.ActionProposeHeldSettings || action == finapi.ActionApproveHeldSettings { // a change set held for an onboarding (V2.102, batch F)
		return authorizeHeld(ctx, "fin", tenant, actor, action, finapi.ActionProposeHeldSettings, finapi.ActionConfigureSettings, finapi.ActionApproveSettings)
	}
	if action == finapi.ActionDeclareSingleAdministrator { // the platform's own (V2.102, batch E): by name, and only with the platform's mark
		if err := onlyThePlatform(ctx, "fin", action); err != nil {
			return err
		}
		return identity.AuthorizeNamed(ctx, "fin", tenant, actor, action)
	}
	if action == finapi.ActionConfigureSettings || action == finapi.ActionApproveSettings || action == finapi.ActionReviewAfterTheFact ||
		action == finapi.ActionReviewSettingsExceptions {
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
