// cargo-migrate is an OPERATOR-ONLY migration tool. Not linked into the API.
// It validates source digests and applies Bento migration assets first, then the Cargo
// contexts' migrations in manifest order, recording SHA256 in a history table. Never rewrite applied SQL.
// A context the manifest marks "numbering" first gets idp-platform-go's numbering assets in its own schema (MD-28).
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
	numberingpg "go.idp.xyz/idp-platform-go/numbering/postgres"
)

type migration struct {
	ID       string
	SQL      string
	Checksum string
	Ledger   string // the history id: cargo:<id> or platform:<schema>:<id>
}

// entry is a manifest line: a context's migration directory and the platform capabilities it uses.
type entry struct {
	Dir       string
	Numbering bool
}

// platformNumbering renders idp-platform-go's numbering assets for a context's schema, cargo_<context>.
func platformNumbering(context string) ([]migration, error) {
	schema := "cargo_" + context
	assets, err := numberingpg.Migrations(schema)
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, a := range assets {
		id := schema + ":" + a.ID
		out = append(out, migration{ID: id, SQL: a.SQL, Checksum: checksum(a.SQL), Ledger: "platform:" + id})
	}
	return out, nil
}

func checksum(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migration blocked:", err)
		os.Exit(1)
	}
}
func run() error {
	manifest := flag.String("manifest", "", "file listing the contexts' migration directories in order, such as migrations/manifest/contexts.txt")
	dir := flag.String("directory", "migrations/org", "one context's migration directory, used when -manifest is not set")
	apply := flag.Bool("apply", false, "explicitly apply to PostgreSQL as dedicated migrator role")
	flag.Parse()
	entries := []entry{{Dir: *dir}}
	if *manifest != "" {
		var err error
		if entries, err = readManifest(*manifest); err != nil {
			return err
		}
	}
	items, err := load(entries)
	if err != nil {
		return err
	}
	framework := bentopg.Migrations()
	for _, m := range framework {
		if _, err := bentopg.RenderMigration(m.ID, bentopg.DefaultSchema); err != nil {
			return err
		}
		fmt.Printf("FRAMEWORK %s %s\n", m.ID, m.Checksum)
	}
	for _, m := range items {
		fmt.Printf("%-9s %s sha256:%s\n", strings.ToUpper(strings.SplitN(m.Ledger, ":", 2)[0]), m.ID, m.Checksum)
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
		if err = applyMigration(ctx, pool, cm.Ledger, cm.SQL, cm.Checksum); err != nil {
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

// load reads the entries' migrations in order: a context's platform assets before its own migrations.
func load(entries []entry) ([]migration, error) {
	var items []migration
	for _, e := range entries {
		if e.Numbering {
			assets, err := platformNumbering(filepath.Base(e.Dir))
			if err != nil {
				return nil, err
			}
			items = append(items, assets...)
		}
		loaded, err := loadDirectory(e.Dir)
		if err != nil {
			return nil, err
		}
		items = append(items, loaded...)
	}
	return items, nil
}

// readManifest returns the migration directories a manifest lists, in order, with the platform capabilities each
// uses ("numbering"). Entries name directories under the migrations root, which is the parent of the manifest's
// directory.
func readManifest(path string) ([]entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(filepath.Dir(path))
	seen := map[string]bool{}
	var entries []entry
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		name := fields[0]
		if seen[name] || strings.ContainsAny(name, `/\.`) {
			return nil, fmt.Errorf("manifest %s: invalid or repeated entry %q", path, name)
		}
		seen[name] = true
		e := entry{Dir: filepath.Join(root, name)}
		for _, capability := range fields[1:] {
			if capability != "numbering" || e.Numbering {
				return nil, fmt.Errorf("manifest %s: entry %q names an unknown or repeated capability %q", path, name, capability)
			}
			e.Numbering = true
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("manifest %s lists no migration directories", path)
	}
	return entries, nil
}

// loadDirectory reads one context's migrations. Files are numbered from 000001 without gaps and named
// NNNNNN_<context>_<topic>.sql, which keeps ledger IDs unique across contexts.
func loadDirectory(dir string) ([]migration, error) {
	bc := filepath.Base(dir)
	files, err := filepath.Glob(filepath.Join(dir, "000*.sql"))
	if err != nil {
		return nil, err
	}
	if bc == "org" && len(files) < 6 {
		return nil, fmt.Errorf("only %d migrations, expected original 000001-000005 + 000006", len(files))
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no migrations in %s", dir)
	}
	sort.Strings(files)
	var items []migration
	for i, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".sql")
		if !strings.HasPrefix(id, fmt.Sprintf("%06d_%s_", i+1, bc)) {
			return nil, fmt.Errorf("noncontiguous or misnamed migration: %s", id)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("empty migration %s", id)
		}
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		items = append(items, migration{ID: id, SQL: body, Checksum: checksum(body), Ledger: "cargo:" + id})
	}
	return items, nil
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
