// cargo-api serves Cargo's HTTP API (paths under /v1, RFC 9457 problems). Every /v1 request carries a bearer token;
// until the IDP identity platform is connected, tokens are HS256-signed with the key in IDP_CARGO_API_TOKEN_KEY
// (base64, 32 bytes or more), for development and tests only: whoever holds the key can sign any tenant, actor and
// permission, so the server starts only with -dev-tokens. -mint-token prints such a token and exits.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/bootstrap"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "cargo-api:", err)
		os.Exit(1)
	}
}

type options struct {
	addr, schema, issuer, audience string
	marginFloorBP, discountBP      int
	shutdown                       time.Duration
	mint, devTokens                bool
	mintTenant, mintActor          string
	mintPermissions                string
	mintTTL                        time.Duration
}

func parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("cargo-api", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.addr, "addr", "127.0.0.1:8080", "listen address (port 0: any free port, logged as serving)")
	fs.StringVar(&o.schema, "schema", bentopg.DefaultSchema, "Bento schema")
	fs.StringVar(&o.issuer, "token-issuer", "idp-cargo-dev", "issuer (iss) a token must name")
	fs.StringVar(&o.audience, "token-audience", "idp-cargo-api", "audience (aud) a token must name")
	fs.IntVar(&o.marginFloorBP, "margin-floor-bp", 2000, "RATE: a quote's margin floor, basis points of its sell total, for every legal entity")
	fs.IntVar(&o.discountBP, "rate-card-discount-bp", 0, "RATE: how far below the rated sell an overridden line may sell without approval, basis points")
	fs.DurationVar(&o.shutdown, "shutdown-timeout", 15*time.Second, "time for requests in flight on SIGTERM")
	fs.BoolVar(&o.devTokens, "dev-tokens", false, "accept development tokens (HS256, shared key): never in production")
	fs.BoolVar(&o.mint, "mint-token", false, "print a development token for -tenant, -actor and -permissions and exit")
	fs.StringVar(&o.mintTenant, "tenant", "", "-mint-token: the tenant")
	fs.StringVar(&o.mintActor, "actor", "", "-mint-token: the actor (subject)")
	fs.StringVar(&o.mintPermissions, "permissions", "", "-mint-token: comma-separated permissions, such as rate:Read,rate:MaintainChargeCodes")
	fs.DurationVar(&o.mintTTL, "ttl", time.Hour, "-mint-token: lifetime")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	return o, nil
}

func tokenKey(getenv func(string) string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(getenv("IDP_CARGO_API_TOKEN_KEY"))
	if err != nil || len(key) < 32 {
		return nil, errors.New("IDP_CARGO_API_TOKEN_KEY must be base64 of 32 bytes or more")
	}
	return key, nil
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	o, err := parse(args)
	if err != nil {
		return err
	}
	key, err := tokenKey(getenv)
	if err != nil {
		return err
	}
	cfg := bootstrap.APIConfig{TokenKey: key, TokenIssuer: o.issuer, TokenAudience: o.audience, MarginFloorBP: o.marginFloorBP,
		RateCardDiscountBP: o.discountBP}
	if o.mint {
		var perms []string
		if o.mintPermissions != "" {
			perms = strings.Split(o.mintPermissions, ",")
		}
		token, err := bootstrap.MintDevToken(cfg, o.mintTenant, o.mintActor, perms, o.mintTTL)
		if err != nil {
			return fmt.Errorf("the token would not verify: %w", err)
		}
		_, err = fmt.Fprintln(stdout, token)
		return err
	}
	if !o.devTokens {
		return errors.New("until the IDP identity platform is connected, cargo-api accepts only development tokens: start it with -dev-tokens, never in production")
	}
	dsn := getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" {
		return errors.New("IDP_CARGO_POSTGRES_DSN is required")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: dsn, BentoSchema: o.schema})
	if err != nil {
		return err
	}
	defer platform.Close()
	cfg.Logger = logger
	handler, err := platform.API(cfg)
	if err != nil {
		return err
	}
	base, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	srv := bootstrap.APIServer(o.addr, handler, logger, base)
	ln, err := net.Listen("tcp", o.addr)
	if err != nil {
		return err
	}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	logger.Info("serving", "addr", ln.Addr().String(), "token_issuer", o.issuer, "token_audience", o.audience)
	select {
	case err = <-failed:
		return err
	case <-ctx.Done():
	}
	stop() // a second signal ends the process at once
	logger.Info("shutting down", "timeout", o.shutdown)
	shutdown, cancel := context.WithTimeout(context.Background(), o.shutdown)
	defer cancel()
	if err = srv.Shutdown(shutdown); err != nil {
		// out of time: cancel the requests still running, so their database work ends and the pool can close
		cancelRequests()
		_ = srv.Close()
		logger.Warn("requests cancelled at shutdown", "error", err)
	}
	if err = <-failed; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("stopped")
	return nil
}
