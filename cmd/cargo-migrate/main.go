//go:build bento

// cargo-migrate is an OPERATOR-ONLY migration tool. Not linked into the API.
// It validates source digests and applies Bento migration assets first, then Cargo
// ORG migrations, recording SHA256 in a history table. Never rewrite applied SQL.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
)

type migration struct {
	ID       string
	SQL      string
	Checksum string
}

func checksum(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migration blocked:", err)
		os.Exit(1)
	}
}
func run() error {
	dir := flag.String("directory", "sql", "immutable ORG SQL migrations directory")
	apply := flag.Bool("apply", false, "explicitly apply to PostgreSQL as dedicated migrator role")
	flag.Parse()
	files, err := filepath.Glob(filepath.Join(*dir, "000*.sql"))
	if err != nil {
		return err
	}
	if len(files) < 6 {
		return fmt.Errorf("only %d migrations, expected original 000001-000005 + 000006", len(files))
	}
	sort.Strings(files)
	var items []migration
	for i, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".sql")
		if !strings.HasPrefix(id, fmt.Sprintf("%06d_", i+1)) {
			return fmt.Errorf("noncontiguous migration: %s", id)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if len(raw) == 0 {
			return fmt.Errorf("empty migration %s", id)
		}
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		items = append(items, migration{ID: id, SQL: body, Checksum: checksum(body)})
	}
	framework := bentopg.Migrations()
	for _, m := range framework {
		if _, err := bentopg.RenderMigration(m.ID, bentopg.DefaultSchema); err != nil {
			return err
		}
		fmt.Printf("FRAMEWORK %s %s\n", m.ID, m.Checksum)
	}
	for _, m := range items {
		fmt.Printf("CARGO     %s sha256:%s\n", m.ID, m.Checksum)
	}
	if !*apply {
		fmt.Println("DRY-RUN: no database connection or DDL executed")
		return nil
	}
	dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN")
	if dsn == "" {
		return errors.New("IDP_CARGO_MIGRATOR_DSN required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return err
	}
	// PostgreSQL Simple Protocol is needed for carefully versioned multi-statement DDL.
	// No user input or variables are interpolated into the migration SQL.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	var version int
	if err = pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&version); err != nil {
		return err
	}
	if version != 16 {
		return fmt.Errorf("PostgreSQL major %d is not supported by this candidate", version)
	}
	if _, err = pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS cargo_migrations;
      CREATE TABLE IF NOT EXISTS cargo_migrations.applied (
        id text PRIMARY KEY, sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	// Bento framework migrations assume the target schema already exists; creating it is the consumer's job.
	if _, err = pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{bentopg.DefaultSchema}.Sanitize()); err != nil {
		return err
	}
	for _, fm := range framework {
		sql, err := bentopg.RenderMigration(fm.ID, bentopg.DefaultSchema)
		if err != nil {
			return err
		}
		if err = applyMigration(ctx, pool, "framework:"+fm.ID, sql, strings.TrimPrefix(fm.Checksum, "sha256:")); err != nil {
			return err
		}
	}
	for _, cm := range items {
		if err = applyMigration(ctx, pool, "cargo:"+cm.ID, cm.SQL, cm.Checksum); err != nil {
			return err
		}
	}
	db, err := bentopg.NewDB(pool)
	if err != nil {
		return err
	}
	if err = db.CheckSchema(ctx); err != nil {
		return fmt.Errorf("Bento CheckSchema: %w", err)
	}
	fmt.Println("DONE: PostgreSQL 16 migration and Bento schema validation successful")
	return nil
}
func applyMigration(ctx context.Context, pool *pgxpool.Pool, id, sql, want string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(804081911, 610150)"); err != nil {
		return err
	}
	var exists string
	err = tx.QueryRow(ctx, "SELECT sha256 FROM cargo_migrations.applied WHERE id=$1", id).Scan(&exists)
	if err == nil {
		if exists != want {
			return fmt.Errorf("migration tampering %s: previous %s current %s", id, exists, want)
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%s DDL: %w", id, err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO cargo_migrations.applied(id,sha256) VALUES($1,$2)", id, want); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Println("APPLIED", id)
	return nil
}
