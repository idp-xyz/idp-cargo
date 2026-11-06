// cargo-worker relays committed Outbox events to an HTTP sink. Delivery is at least once: consumers
// must deduplicate by event ID.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/foundation/messaging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cargo-worker:", err)
		os.Exit(1)
	}
}

func run() error {
	schema := flag.String("schema", bentopg.DefaultSchema, "Bento schema that holds the outbox")
	sink := flag.String("sink", "", "HTTP endpoint that receives each event envelope as a JSON POST")
	batch := flag.Int("batch", 50, "deliveries claimed per round")
	lease := flag.Duration("lease", 30*time.Second, "lease on a claimed delivery; an unsettled delivery is re-claimed after it expires")
	poll := flag.Duration("poll", time.Second, "wait between claims while the outbox is idle")
	maxAttempts := flag.Uint("max-attempts", 20, "claims of one delivery before it is abandoned")
	maxFailures := flag.Uint("max-failures", 10, "recorded publish failures before a delivery is abandoned")
	retryBase := flag.Duration("retry-base", time.Second, "delay after the first publish failure, doubled per failure")
	retryMax := flag.Duration("retry-max", 5*time.Minute, "upper bound for the retry delay")
	sinkTimeout := flag.Duration("sink-timeout", 10*time.Second, "timeout for one sink request")
	flag.Parse()
	dsn := os.Getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" || *sink == "" {
		return errors.New("IDP_CARGO_POSTGRES_DSN and -sink are required")
	}
	// A lease shorter than one sink request lets a second worker claim a delivery that is still in flight.
	if *lease <= *sinkTimeout {
		return errors.New("-lease must be longer than -sink-timeout")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	db, err := bentopg.NewDB(pool, bentopg.WithSchema(*schema))
	if err != nil {
		return err
	}
	if err = db.CheckSchema(ctx); err != nil {
		return err
	}
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		return err
	}
	relay := bento.Relay{
		Outbox:      store,
		Publisher:   messaging.Webhook{URL: *sink, Client: &http.Client{Timeout: *sinkTimeout}},
		Clock:       bento.SystemClock{},
		Batch:       *batch,
		LeaseFor:    *lease,
		MaxAttempts: uint32(*maxAttempts),
		MaxFailures: uint32(*maxFailures),
		RetryBase:   *retryBase,
		RetryMax:    *retryMax,
		IdleWait:    *poll,
		Logger:      logger,
	}
	logger.Info("relaying outbox", "schema", *schema, "sink", *sink, "lease", *lease)
	if err = relay.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("stopped")
	return nil
}
