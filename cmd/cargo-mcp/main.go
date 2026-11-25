// cargo-mcp serves FIN's three read-only MCP tools (list_loss_jobs, get_job_profit, list_job_cost_reviews) for the IDP
// TELOS gateway (idp-mcp), which registers it as a target with identity forward. It knows a person only from the
// gateway's signed assertion (ES256, checked against the public key in the configuration), never from a development
// token, and a request without a valid assertion runs nothing. The configuration is a JSON file (-config); the database
// DSN comes from IDP_CARGO_POSTGRES_DSN, as for cargo-api. The server does not start with a configuration that is not
// valid, and has no switch that turns the verification off.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/bootstrap"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "cargo-mcp:", err)
		os.Exit(1)
	}
}

type options struct {
	addr, schema, config string
	shutdown             time.Duration
}

func parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("cargo-mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.addr, "addr", "127.0.0.1:8600", "listen address (port 0: any free port, logged as serving)")
	fs.StringVar(&o.schema, "schema", bentopg.DefaultSchema, "Bento schema")
	fs.StringVar(&o.config, "config", "", "the configuration file (JSON)")
	fs.DurationVar(&o.shutdown, "shutdown-timeout", 15*time.Second, "time for requests in flight on SIGTERM")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if o.config == "" {
		return options{}, errors.New("-config is required: the file that names the endpoint, the gateway, its public key and what each role may read")
	}
	return o, nil
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	o, err := parse(args)
	if err != nil {
		return err
	}
	cfg, err := bootstrap.LoadMCPConfig(o.config)
	if err != nil {
		return err
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
	mcp, err := platform.MCP(cfg, logger)
	if err != nil {
		return err
	}
	base, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	srv := bootstrap.APIServer(o.addr, mcp.Handler, logger, base)
	ln, err := net.Listen("tcp", o.addr)
	if err != nil {
		return err
	}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	logger.Info("serving", "addr", ln.Addr().String(), "audience", cfg.Audience, "gateway_id", cfg.GatewayID, "gateway_key_ids", mcp.KeyIDs)
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
