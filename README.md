# IDP-Cargo ORG / Go + DDD + Bento-Go · V2.5 integration candidate (PG16 executed locally)

**This is a Cargo-owned vertical slice, not a fork or modification of idp-bento-go.**

- Product module: `go.idp.xyz/idp-cargo` (candidate until Cargo repo governance signs off).
- Framework module: `go.idp.xyz/idp-bento-go@v0.1.0-rc.2` (release candidate, not stable). The vanity import resolves to the public repository `github.com/idp-xyz/idp-bento-go`, tag `v0.1.0-rc.2`.
- Go version target: `1.26.5`; database: PostgreSQL 16.
- `internal/contexts/org/domain/`: pure Go DDD entities, value objects, invariants.
- `internal/contexts/org/application/`: authorized application command orchestration and receipts.
- `internal/contexts/org/adapters/postgres/`: actual Bento DB scoped Repository + eligibility, receipts, Outbox writer.
- `internal/platform/bento/`: transaction-wide tenant GUC + Bento Transaction API seam.
- `cmd/cargo-migrate`: operator-only migration application; creates the Bento schema, applies the framework migration, then Cargo migrations; checksum tracking.
- `sql/`: self-contained sequential migration files; `../sql` is the unchanged historical V2.3 snapshot plus additive 000006.
- `internal/integration`: real PG16 tests tagged `bento && integration`: 4 V2.4 cases (replay, rollback, RLS, concurrency) and 6 V2.5 hardening cases (Outbox row count and rollback, concurrent Outbox, RLS write paths, role-ID precision).
- `.github/workflows/org-pg16-contract.yml`: proposed CI. Every step was executed locally in V2.5; it has **not** been run on GitHub Actions.

## Status of evidence (V2.5)

Executed on 2026-11-06 on a local WSL2 Ubuntu 24.04 runner, following the workflow steps in order. Logs: `../validation/v2_5/`.

- Exact Go `1.26.5` toolchain; `go.idp.xyz/idp-bento-go@v0.1.0-rc.2` resolved with the published `h1:W9T1KsbXwuVs3lHTNwSUcEBY0Ids00hUH7I9KuBtCEo=`; `go mod verify` passed.
- PostgreSQL `16.12` from the CI-pinned image index digest `postgres:16.12-alpine@sha256:97ff59a4e30e08d1c11bdcd9455e7832368c0572b576c9092cde2df4ae5552a3`.
- `cargo-migrate -apply`: Bento framework migration and Cargo 000001–000006 applied, Bento `CheckSchema` passed, a second run was idempotent.
- The workflow's application-role grant script ran verbatim; `cargo_app` is NOSUPERUSER / NOBYPASSRLS and owns no tables.
- `CI=true go test -tags 'bento integration' -count=1 -v ./...`: 22 domain/application tests and 10 PG16 integration tests passed, none skipped.
- `go test -race` and `go vet` passed.

The run found three defects in the V2.4 candidate; the V2.4 → V2.5 patch is `../validation/v2_5/30_v2_4_to_v2_5_go_cargo.patch`:

1. `go.sum` held only the two framework lines, so every `-tags bento` build failed with `missing go.sum entry`. `go.mod` / `go.sum` are now the complete graph from Go 1.26.5 `go mod tidy`.
2. `cargo-migrate` did not create the Bento schema, so the first framework migration failed with `schema "bento" does not exist`. Bento migrations never run `CREATE SCHEMA`; that is the consumer's first migration step (bento-go `docs/design/postgres-adapter-contract.md`).
3. `PartyStore.Save` hashed nanosecond validity times into `role_id`, while `timestamptz` keeps microseconds. After a reload the ID changed and the re-insert of an existing role violated `party_role_approved_no_overlap` (23P01), so a second role approval on the same party failed whenever the first `ValidFrom` had sub-microsecond precision. The hash and the insert now use microsecond-truncated UTC times.

NOT executed:

- A protected GitHub Actions run with signed, retained evidence.
- OIDC / IAM, KMS and privacy controls, Inbox live delivery and crash recovery, `ErrCommitUncertain` recovery, Bento testkit consumer contracts, Cargo→COM business UAT.
- No Cargo production repository or branch has been created.

## Run in an authorized PG16 CI environment

1. Install Go 1.26.5 and resolve the immutable framework candidate; do not use Git `main` or a local `replace`.
2. Set `IDP_CARGO_MIGRATOR_DSN` to a dedicated migration role in PG16.
3. `go run -tags bento ./cmd/cargo-migrate -directory sql` to inspect; separately authorize `-apply`.
4. Create a `cargo_app` user with no owner/superuser/BYPASSRLS/DDL rights; grant only required DML.
5. Set `CI=true IDP_CARGO_POSTGRES_DSN=...`.
6. `go test -tags 'bento integration' -count=1 -v ./...`
7. `go test -tags 'bento integration' -race -count=1 ./...`
8. Review `../ci/RUNBOOK_PG16_BENTO_RC2.md` and collect signed, retained CI proof.

## Important boundaries

- The approval actor and tenant identity are **not trusted merely because they are inside a Go struct**. A production HTTP adapter must derive them from a verified authenticated identity and enforce business authority.
- `app.tenant_id` RLS protects against accidental missing/wrong query scopes. SQL injection that can execute arbitrary `set_config` is out of its security boundary.
- A missing or legacy `result_revision=0` receipt does **not** count as execution success.
- A revision conflict surfaces as `org.ErrConflict` (stale `ExpectedRevision`) or Bento `repository.ErrConflict` (lost the conditional UPDATE race); an HTTP adapter must map both to the same conflict response.
- No direct business writes across Cargo bounded contexts.
- `EventID` is not a substitute for CommandID deduplication. No Exactly-Once guarantee.
- The earliest ORG master data cannot be signed off until a Cargo business owner validates authoritative identity, legal status and partner role semantics.

## Dependency lock qualification

`go.sum` is the complete module graph produced by Go 1.26.5 `go mod tidy`; `go.idp.xyz/idp-bento-go` keeps the published RC2 sums that the workflow checks. A protected CI run must still execute `go mod verify` and reject unexpected graph changes before this becomes an approved supply-chain baseline.
