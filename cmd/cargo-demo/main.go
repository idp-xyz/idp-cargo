// cargo-demo seeds the test tenant eii-test through the contexts' real commands (see bootstrap.SeedDemoTenant). It
// reads the database of the application role in IDP_CARGO_POSTGRES_DSN and prints what it seeded. Give the same -at to
// run again: the commands are the same, and the contexts answer them from their receipts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"go.idp.xyz/idp-cargo/internal/bootstrap"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "cargo-demo:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("cargo-demo", flag.ContinueOnError)
	tenant := fs.String("tenant", "eii-test", "the tenant to seed")
	atFlag := fs.String("at", "", "the day's start, RFC 3339 (hours of business time count from it); default: 48 hours ago, to the hour. Give the same value to run again")
	if err := fs.Parse(args); err != nil {
		return err
	}
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	if *atFlag != "" {
		t, err := time.Parse(time.RFC3339, *atFlag)
		if err != nil {
			return fmt.Errorf("-at: %w", err)
		}
		at = t
	}
	dsn := getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" {
		return errors.New("IDP_CARGO_POSTGRES_DSN is required (the application role of the demo database)")
	}
	return bootstrap.SeedDemoTenant(ctx, dsn, *tenant, at, out)
}
