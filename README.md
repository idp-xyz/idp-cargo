# IDP-Cargo ORG / Go + DDD + Bento-Go · V2.7 integration candidate

**This is a Cargo-owned vertical slice, not a fork or modification of idp-bento-go.**

- Product module: `go.idp.xyz/idp-cargo` (candidate until Cargo repo governance signs off). Source repository: `github.com/idp-xyz/idp-cargo`.
- Framework module: `go.idp.xyz/idp-bento-go@v0.1.0-rc.2` (release candidate, not stable). The vanity import resolves to the public repository `github.com/idp-xyz/idp-bento-go`, tag `v0.1.0-rc.2`.
- Go version target: `1.26.5`; database: PostgreSQL 16.
- `internal/contexts/org/domain/`: pure Go DDD entities, value objects, invariants.
- `internal/contexts/org/application/`: authorized application command orchestration and receipts.
- `internal/contexts/org/adapters/postgres/`: Bento DB scoped persistence (`PartyStore`, exposed through Bento's repository ports by `PartyRepository`), legal-entity eligibility, command receipts, Outbox writer and the `ORGEnvelope` event mapping.
- `internal/platform/bento/`: transaction-wide tenant GUC + Bento Transaction API seam.
- `cmd/cargo-migrate`: operator-only migration application; creates the Bento schema, applies the framework migration, then Cargo migrations; checksum tracking.
- `sql/`: sequential migration files 000001–000006; never rewrite an applied file.
- `internal/integration`: real PG16 tests tagged `bento && integration`: 4 V2.4 cases (replay, rollback, RLS, concurrency), 6 V2.5 hardening cases (Outbox row count and rollback, concurrent Outbox, RLS write paths, role-ID precision) and the Bento testkit Repository, Outbox and Inbox contracts added in V2.7.
- `.github/workflows/org-pg16-contract.yml`: runs on every push and pull request. `main` is protected and requires this check.

Design documents, review reports and evidence logs are kept in the IDP-Cargo master archive and are not published in this repository.

## Status of evidence (V2.7)

GitHub Actions, `ubuntu-24.04` runner, workflow `org-pg16-contract.yml`:

- Run 42201396450 on `3973a51`: failed, 31 of 32 tests passed. `TestConcurrentCommandsSingleOutboxRowAndRevisionConflict` caught a torn read in `PartyStore.Load` (defect 4 below).
- Runs 42202747912 on `9fdbe63` and 42203416561 on `f5ae770`: success, 32 tests.
- Run 42204649979 on `81b68cf`: success, 35 tests plus 13 Bento testkit contract scenarios, none skipped; `-race` and `go vet` passed. Go 1.26.5, PostgreSQL 16.12 from the pinned image digest, `go.idp.xyz/idp-bento-go@v0.1.0-rc.2` fetched with `GOPROXY=direct` and its sum checked.
- `main` is protected: changes land through pull requests that pass `org-pg16-contract` and are up to date with `main`; administrators are included; force pushes and branch deletion are blocked. No approving review is required yet.

Bento testkit contracts:

- `RunRepositoryContract` runs `PartyRepository` against the `cargo_org` tables with tenant-bound transactions. Cargo binds one tenant per transaction, so the contract's "other scope" is a second party of the same tenant; cross-tenant isolation is covered by the RLS tests.
- `RunOutboxContract` and `RunInboxContract` run Bento's PostgreSQL adapters in per-scenario schemas that the migrator role provisions with the workflow's grants and the application role exercises. Outbox envelopes come from `ORGEnvelope`.

Defects found by real runs and fixed:

1. `go.sum` held only the two framework lines, so every `-tags bento` build failed with `missing go.sum entry`. `go.mod` / `go.sum` are now the complete graph from Go 1.26.5 `go mod tidy`.
2. `cargo-migrate` did not create the Bento schema, so the first framework migration failed with `schema "bento" does not exist`. Bento migrations never run `CREATE SCHEMA`; that is the consumer's first migration step (bento-go `docs/design/postgres-adapter-contract.md`).
3. `PartyStore.Save` hashed nanosecond validity times into `role_id`, while `timestamptz` keeps microseconds. After a reload the ID changed and the re-insert of an existing role violated `party_role_approved_no_overlap` (23P01). The hash and the insert now use microsecond-truncated UTC times.
4. `PartyStore.Load` read the party row and its roles in two statements. Under READ COMMITTED each statement takes a new snapshot, so a command committing in between produced a torn aggregate (old revision, new role) and a losing handler failed with `ErrRoleOverlap` instead of a revision conflict; locally 4 of 300 stress runs. `Load` now locks the root row with `FOR NO KEY UPDATE`; 300 of 300 runs pass and every losing handler returns `org.ErrConflict`.

NOT executed:

- Required approving review; signed build provenance or attestations.
- OIDC / IAM, KMS and privacy controls, `ErrCommitUncertain` recovery, an Outbox relay process and Inbox consumers in Cargo, Cargo→COM business UAT.

## Run in an authorized PG16 CI environment

1. Install Go 1.26.5 and resolve the immutable framework candidate; do not use Git `main` or a local `replace`.
2. Set `IDP_CARGO_MIGRATOR_DSN` to a dedicated migration role in PG16.
3. `go run -tags bento ./cmd/cargo-migrate -directory sql` to inspect; separately authorize `-apply`.
4. Create a `cargo_app` user with no owner/superuser/BYPASSRLS/DDL rights; grant only required DML.
5. Set `CI=true IDP_CARGO_POSTGRES_DSN=...`. The Bento contract tests also need `IDP_CARGO_MIGRATOR_DSN` to provision their throwaway schemas.
6. `go test -tags 'bento integration' -count=1 -v ./...`
7. `go test -tags 'bento integration' -race -count=1 ./...`
8. Collect signed, retained CI proof.

## Important boundaries

- The approval actor and tenant identity are **not trusted merely because they are inside a Go struct**. A production HTTP adapter must derive them from a verified authenticated identity and enforce business authority.
- `app.tenant_id` RLS protects against accidental missing/wrong query scopes. SQL injection that can execute arbitrary `set_config` is out of its security boundary.
- A missing or legacy `result_revision=0` receipt does **not** count as execution success.
- A stale `ExpectedRevision` surfaces as `org.ErrConflict`. With the root-row lock in `Load`, the conditional UPDATE in `Save` should not lose a race inside the handler, but an HTTP adapter must still map Bento `repository.ErrConflict` to the same conflict response.
- No direct business writes across Cargo bounded contexts.
- `EventID` is not a substitute for CommandID deduplication. No Exactly-Once guarantee.
- The earliest ORG master data cannot be signed off until a Cargo business owner validates authoritative identity, legal status and partner role semantics.

## Dependency lock qualification

`go.sum` is the complete module graph produced by Go 1.26.5 `go mod tidy`. On GitHub Actions it resolved with `GOPROXY=direct` and `go mod verify` passed. Promotion to an approved supply-chain baseline still needs review.
