# IDP-Cargo · Go + DDD + Bento-Go · V2.11 integration candidate

**IDP-Cargo is one product repository and one Go module. Business code is organized by DDD bounded context; IDP-Bento-Go and IDP-Platform-Go stay separate repositories referenced as Go module versions.**

- Product module: `go.idp.xyz/idp-cargo` (candidate). The `go.idp.xyz` vanity host currently serves only `idp-bento-go`, so this path does not resolve outside the repository yet.
- Framework module: `go.idp.xyz/idp-bento-go@v0.1.0-rc.2` (release candidate, not stable), resolved from the public repository `github.com/idp-xyz/idp-bento-go`.
- Go `1.26.5`; PostgreSQL 16.

## Layout

```
cmd/cargo-migrate/              operator-only migrations: Bento schema, framework migration, then each context's migrations in manifest order
cmd/cargo-worker/               Outbox relay: claim, publish to an HTTP sink, settle; at least once
internal/contexts/org/          ORG bounded context
  module.go                     assembles ORG; the only constructor other code uses
  api/                          ORG's contract: commands, PartyRef v1 and the party-role query, published events, error kinds, Authorizer port
  internal/                     private to ORG (Go internal rule)
    domain/                     BusinessParty, LegalEntity, PartnerMergeCase
    application/                ApproveRole use case and its ports
    adapters/postgres/          tenant-scoped persistence, receipts, Outbox, Bento repository ports
    integration/                PG16 tests (build tag integration)
internal/contexts/rate/         RATE bounded context (Rate, Quote & Commercial Rules)
  module.go                     assembles RATE; ORG is passed in as its query port
  api/                          RATE's contract: quote commands, QuoteSnapshot v1, events, error kinds, Authorizer and MarginPolicy ports
  internal/domain/              Quote: margin floor, price approval, acceptance within validity
  internal/application/         CreateQuote, ApproveQuotePrice, AcceptQuote, GetQuoteSnapshot and their ports
  internal/adapters/postgres/   quotes, receipts and Outbox in cargo_rate
  internal/adapters/org/        customer check through ORG's api (anticorruption layer)
  internal/integration/         PG16 tests with a stand-in for ORG
internal/bootstrap/             composition root: one connection pool, a Bento transaction owner per context and for the relay
internal/kernel/                value objects shared by every context (ValidityInterval, Money); standard library only
internal/foundation/bento/      context-agnostic IDP-Bento-Go adaptation (tenant-bound transactions, clock)
internal/foundation/messaging/  reliable messaging: Outbox relay, HTTP webhook publisher
migrations/org/                 immutable ORG migrations 000001–000006
migrations/rate/                RATE migrations, starting with 000001_rate_quotes
migrations/manifest/            order in which the contexts' migrations run
tools/dependency-check/         package rules of the structure baseline; cross-context allowlist generated from the Context Map
.github/CODEOWNERS              an owner for every context directory and the kernel
```

- Other contexts, transports, cross-context processes and the AI capability layer import `internal/contexts/org/api` only; the compiler rejects imports of `internal/contexts/org/internal/...` from outside ORG, and `tools/dependency-check` rejects the rest of what the layering and the Context Map do not allow.
- Further contexts (COM, FUL, BOOK, …), `internal/processes/` and `internal/ai/` get directories when their first real code lands; no empty scaffolding.
- Design documents, review reports and evidence logs are kept in the IDP-Cargo master archive and are not published in this repository.

## Status of evidence

GitHub Actions, `ubuntu-24.04` runner, workflow `org-pg16-contract.yml`, on every push and pull request. `main` is protected: changes land through pull requests that pass `org-pg16-contract` and are up to date with `main`; administrators are included; force pushes and branch deletion are blocked. No approving review is required yet.

- Runs 42201396450 (`3973a51`, failed: torn read in `PartyStore.Load`, defect 4), 42202747912 and 42203416561 (32 tests), 42204649979, 42205465991, 42205477004 and 42205609990 (35 tests plus 13 Bento testkit contract scenarios).
- V2.8 moves ORG into `internal/contexts/org/{module.go, api, internal}` and the Bento seam into `internal/foundation/bento`, moves migrations to `migrations/org`, drops the obsolete `bento` build tag, and adds the ORG module with api error classification. Migration files and their checksums are unchanged.
- V2.9 adds `cmd/cargo-worker` and the Outbox `Relay` (in `internal/foundation/messaging`, as the repository structure baseline V1.1 places it). PG16 tests cover a failed publish retried with backoff, abandonment after the failure budget without blocking the partition, and a process-level crash drill: the worker is killed with SIGKILL while the sink holds a delivery, a second worker re-delivers it after the lease expires, and SIGTERM stops it cleanly. Each publish is bounded by the sink timeout and a batch must fit inside its lease, so a slow sink cannot push queued deliveries past the lease and have them re-claimed while still waiting.
- V2.10 starts S1. `internal/kernel` holds `ValidityInterval`. ORG's api gains PartyRef v1 and `ResolvePartyRole`, which confirms that an active party holds an approved role for a verified legal entity at an instant. `internal/bootstrap` gives every context and the relay its own Bento transaction owner; a PG16 test shows that ORG refuses commands and queries issued inside another context's transaction (AC4). `go run ./tools/dependency-check` runs as a step of the required check: it enforces rules D1–D6 (AC2, AC3) and CODEOWNERS coverage (AC7), and warns on D7.
- V2.11 adds RATE, step 2 of the S1 slice. A quote is priced in one currency. If its margin is below the floor that `MarginPolicy` supplies, someone other than its author must approve the price, with a reason. It is accepted within its validity, and ORG confirms the CUSTOMER role both when the quote is created and when it is accepted. `GetQuoteSnapshot` returns QuoteSnapshot v1 for accepted quotes, without costs. `cargo_rate` keeps accepted quotes and all quote lines immutable through triggers and FORCE RLS. `cargo-migrate` applies contexts in `migrations/manifest` order, and the allowlist gains `rate org` from Context Map V1.3. A PG16 test in `internal/bootstrap` runs ORG and RATE together.

Bento testkit contracts:

- `RunRepositoryContract` runs `PartyRepository` against the `cargo_org` tables with tenant-bound transactions. Cargo binds one tenant per transaction, so the contract's "other scope" is a second party of the same tenant; cross-tenant isolation is covered by the RLS tests.
- `RunOutboxContract` and `RunInboxContract` run Bento's PostgreSQL adapters in per-scenario schemas that the migrator role provisions with the workflow's grants and the application role exercises. Outbox envelopes come from `ORGEnvelope`.

Defects found by real runs and fixed:

1. `go.sum` held only the two framework lines, so every Bento build failed with `missing go.sum entry`. `go.mod` / `go.sum` are now the complete graph from Go 1.26.5 `go mod tidy`.
2. `cargo-migrate` did not create the Bento schema, so the first framework migration failed with `schema "bento" does not exist`. Bento migrations never run `CREATE SCHEMA`; that is the consumer's first migration step (bento-go `docs/design/postgres-adapter-contract.md`).
3. `PartyStore.Save` hashed nanosecond validity times into `role_id`, while `timestamptz` keeps microseconds. After a reload the ID changed and the re-insert of an existing role violated `party_role_approved_no_overlap` (23P01). The hash and the insert now use microsecond-truncated UTC times.
4. `PartyStore.Load` read the party row and its roles in two statements. Under READ COMMITTED each statement takes a new snapshot, so a command committing in between produced a torn aggregate (old revision, new role) and a losing handler failed with `ErrRoleOverlap` instead of a revision conflict. `Load` now locks the root row with `FOR NO KEY UPDATE`.

NOT executed:

- Required approving review; signed build provenance or attestations.
- OIDC / IAM, KMS and privacy controls, `ErrCommitUncertain` recovery, Inbox consumers in Cargo, Cargo→COM business UAT.
- A production event transport: the HTTP webhook is a replaceable adapter, not a broker decision.

## Run against PostgreSQL 16

1. Install Go 1.26.5 and resolve the immutable framework candidate; do not use Git `main` or a local `replace`.
2. Set `IDP_CARGO_MIGRATOR_DSN` to a dedicated migration role in PG16.
3. `go run ./cmd/cargo-migrate -manifest migrations/manifest/contexts.txt` to inspect; separately authorize `-apply`.
4. Create a `cargo_app` user with no owner/superuser/BYPASSRLS/DDL rights; grant only required DML (for `cargo_rate`: SELECT, INSERT, UPDATE on `quote`; SELECT, INSERT on `quote_line` and `command_receipt`).
5. Set `CI=true IDP_CARGO_POSTGRES_DSN=...`. The Bento contract tests also need `IDP_CARGO_MIGRATOR_DSN` to provision their throwaway schemas.
6. `go test -tags integration -count=1 -v ./...`
7. `go test -tags integration -race -count=1 ./...`
8. Collect signed, retained CI proof.
9. Relay events: `IDP_CARGO_POSTGRES_DSN=... go run ./cmd/cargo-worker -sink https://...`. A claimed batch is sent one delivery at a time under one lease, so `-lease` must exceed `-batch` × `-sink-timeout` (defaults 10 × 5s under 1m); the worker refuses to start otherwise. `-max-attempts` (50) stays five times `-max-failures` (10), as Bento recommends, so the claim limit only ends publishers that crash before settling.
10. Package rules: `go run ./tools/dependency-check`. After the Context Map changes or a context is added, regenerate the allowlist with `-generate <Context Map JSON>` from the master archive.

## Important boundaries

- The approval actor and tenant identity are **not trusted merely because they are inside a Go struct**. A production HTTP adapter must derive them from a verified authenticated identity and enforce business authority.
- `app.tenant_id` RLS protects against accidental missing/wrong query scopes. SQL injection that can execute arbitrary `set_config` is out of its security boundary.
- A missing or legacy `result_revision=0` receipt does **not** count as execution success.
- Callers branch on the `api` error kinds (`ErrConflict`, `ErrRejected`, `ErrUnauthorized`, …), never on ORG or Bento causes; `org.ErrConflict` and Bento `repository.ErrConflict` both classify as `api.ErrConflict`.
- No direct business writes across Cargo bounded contexts.
- `internal/bootstrap` gives every context its own Bento transaction owner. A context that calls another context inside its own transaction gets `application.ErrTransactionConflict`, so cross-context queries run before the caller opens its transaction.
- `EventID` is not a substitute for CommandID deduplication. No Exactly-Once guarantee.
- The relay delivers at least once: a crash between publish and settle re-delivers after the lease expires. Sinks and consumers deduplicate by event ID, which the webhook sends as `Idempotency-Key`. Leases are wall-clock timestamps, so worker hosts need synchronized clocks.
- RATE keeps customers as ORG party references only; legal names stay in ORG. Quote costs and margins never leave RATE.
- RATE's margin floor, independent price approval and acceptance rules are a candidate design pending business review; the floor itself is supplied by `MarginPolicy`, not hard-coded.
- The earliest ORG master data cannot be signed off until a Cargo business owner validates authoritative identity, legal status and partner role semantics.

## Dependency lock qualification

`go.sum` is the complete module graph produced by Go 1.26.5 `go mod tidy`. On GitHub Actions it resolved with `GOPROXY=direct` and `go mod verify` passed. Promotion to an approved supply-chain baseline still needs review.
