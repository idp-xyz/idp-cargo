package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

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
// principal (identity.Authorize), and answers the handler: RATE's catalogue, with ORG for RATE's party checks.
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
	return transport.NewHandler(transport.Config{Verifier: cfg.verifier(), Logger: cfg.Logger, Ready: p.pool.Ping}, rate)
}

// APIServer is an http.Server for the API's handler with Cargo's timeouts.
func APIServer(addr string, h http.Handler, logger *slog.Logger) *http.Server {
	return transport.Server(addr, h, logger)
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

func (permissions) AuthorizePartyAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "org", tenant, actor, action)
}

func (permissions) AuthorizeQuoteAction(ctx context.Context, tenant, actor, action, _ string) error {
	return identity.Authorize(ctx, "rate", tenant, actor, action)
}

// margins is RATE's margin and discount policy, one for every legal entity.
type margins struct{ floor, discount int }

func (m margins) MarginFloorBP(context.Context, string, string) (int, error) { return m.floor, nil }
func (m margins) RateCardDiscountBP(context.Context, string, string) (int, error) {
	return m.discount, nil
}
