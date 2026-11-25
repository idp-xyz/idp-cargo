# FIN's MCP server for the IDP TELOS gateway: three read-only tools (V2.101)

This document is for the operators of the IDP TELOS platform who register `cargo-mcp` as a target of the idp-mcp gateway. It says what to register, which tools there are, which role grants Cargo recommends, how the gateway's public key reaches the server and how the server is configured. The platform's own guide, `idp-mcp/docs/guides/upstream-mcp-server.md`, says what the gateway needs of an upstream; this server follows it, with the answers the platform gave on 2026-11-25 (its section 9).

## What the server is, and is not

- It offers **three read-only tools** of FIN and **no write tool**. A cost review request is not asked for through it: the platform decides later (idp-host D63-9) whether that goes through an MCP write tool or through EER.
- It knows a person **only from the gateway's signed caller assertion** (`_meta["io.idp.telos/caller-assertion"]`, ES256). It never reads the unsigned `io.idp.telos/caller`, and it never accepts a development token: there is no code path from a token to a caller. The assertion is checked as the guide's section 3.3 says, on the raw JSON-RPC body, before the SDK sees the request, with one addition the platform asked for: the issuer `iss` must be `idp-mcp-gateway:<the gateway id in the configuration>`.
- A request without a valid assertion is answered `403`, runs nothing, and its reason goes to the log only. A body that is not one JSON-RPC call (a batch, an object that repeats a key at any depth, `method` or `params` spelled in another case) is answered `400` before anything else, so that the digest the gateway signed and the SDK's reading of the body do not differ. After the assertion is verified the body is read a second time with the library the SDK reads it with (`github.com/segmentio/encoding/json`, as v1.7.0 does), and the method, the tool's name and its arguments (as canonical JSON) must equal what was verified; otherwise the answer is `400` and the reason goes to the log. This is what is done and tested; it is not a proof for every possible body. `tools/list` and `server/discover` pass without an assertion (the gateway checks the catalogue without a caller); every other method needs one. There is no switch that turns the check off, and the server does not start without a valid P-256 public key.
- It is a stateless Streamable HTTP server of the official Go SDK (`github.com/modelcontextprotocol/go-sdk` v1.7.0): `POST /mcp` only, protocol 2026-07-28, JSON responses.

## What to register

| Item | Value |
|---|---|
| `targetName` | `fin` (the tools are then `fin__list_loss_jobs`, `fin__get_job_profit` and `fin__list_job_cost_reviews`) |
| `endpoint` | the address the gateway reaches the server at, ending in `/mcp`, for example `http://fin-mcp:8600/mcp`. It is also the assertion's `aud`: the server's configuration holds the same string, and the two must be equal character for character |
| `identity` | `forward` (FIN has to know who asks; it cannot be changed after registration) |
| `class` | `internal` |
| network | the exact host in the gateway's `IDP_PRIVATE_UPSTREAM_HOSTS` for an internal address. The hop may be plain HTTP; the assertion defends against forgery and replay, not eavesdropping, so use https and `IDP_UPSTREAM_CA_FILE` where that matters |
| gateway key | the gateway must run with `IDP_GATEWAY_CALLER_KEY_FILE` (without it the gateway signs nothing and the server refuses every call); its gateway id is the one in the server's configuration |
| tools and grants | below |
| result sensitivity | `confidential` for the target (below) |

## The tools

All three have `readOnlyHint: true` and `openWorldHint: false`, a closed input schema (`additionalProperties: false`, every field typed and bounded), an `outputSchema`, and no tenant or person field in an input or in a result. Amounts are an integer `minor` and a `currency`, as FIN gives them. `profit_version` is passed through and is compared for equality only. A result is a short text and `structuredContent`.

| Tool | Input | What it answers |
|---|---|---|
| `list_loss_jobs` | `legal_entity_id`; `limit` 1 to 50 (default 20); `cursor` | the jobs of the legal entity whose profit is below zero: job, shipment, state, revenue, cost, profit, profit basis and profit version; FIN's `as_of`; `next_cursor`; `truncated` (the call stopped after 5 FIN pages with more to read); `row_cap_reached` (a chain of cursors delivered its 200 rows and no more is given) |
| `get_job_profit` | `job_id` | one job's profit with its parts (invoiced part, matched and accrued cost, pool cost, re-routing losses, the figures by charge code and its shares of pools), the profit basis and the profit version |
| `list_job_cost_reviews` | `job_id`; `state` (`OPEN` or `RESOLVED`); `limit` 1 to 50 (default 20); `cursor` | the job's cost review requests in the order made, each with the figures its requester saw and its resolution when it has one; no person's name. The reason and the note are text people wrote |

- A cursor is opaque. It holds FIN's next position, the rows delivered, the legal entity (or job and state) it is for and an expiry of 15 minutes, and is bound by an HMAC to the caller (the CargoPai tenant and the subject). Another member's cursor, an expired one, a changed one and one for another request are refused with the way out: start again without a cursor.
- Errors are tool errors with fixed sentences, none carrying FIN's text, a statement, a stack or an address: `no such job or request`, `you are not allowed to do this in FIN`, `the input is not valid; check the arguments against the tool's schema and call again`, `FIN is not available now; try again later`, `too many requests; try again in a minute`, and for anything else `the FIN tool failed to complete the request (reference <id>)`, whose reference is in the server's log with the detail.
- A call may take 20 seconds at most (FIN's queries included), well under the gateway's 60.
- Each member may make 60 calls a minute and 4 at once (configurable), counted in memory by this process.
- **The catalogue is pinned.** Two digests are held by unit tests of the server. `sha256:737616495fa3f18b9cb4a31a685ce67a893243ce60978aa27f339d15a1ad16d3` is idp-mcp's canonical digest of the `tools` array of `tools/list`. `sha256:e9e09cce5005e0705147680702c9306720822d0616d684d21d91ed99297d15ab` is the gateway's own form, `release.Catalog.Digest` over `{tools, prompts, resources, resourceTemplates}` with the last three empty: **an operator compares this one with the target's `snapshotDigest`.** (It was computed with the same canonical JSON as idp-mcp's `release.Digest`; it has not been compared with a running gateway.) A change of any name, description, schema or annotation changes it and, once approved, the gateway blocks the target as drifted until the catalogue is reviewed again. The server's operators must be told before a declaration changes.

### Recommended role grants

The gateway's roles decide only that a tool may be called at all (a Release's `grants`: tenant, role and tool names). Which legal entities a member may see is CargoPai's domain rule, and is set in the server's configuration (below). Cargo recommends:

| Gateway role | Tools |
|---|---|
| a finance role (for example `finance-analyst`) | `fin__list_loss_jobs`, `fin__get_job_profit`, `fin__list_job_cost_reviews` |
| any other role | none |

Give the three tools to the same roles, and grant the roles in the server's configuration the legal entities they may see. Neither list is a substitute for the other: a role granted in the gateway and not named in the server's configuration is refused by the server, and the other way round the gateway never forwards the call.

## The listen address

`cargo-mcp -addr` must be an address the gateway can reach (for example `0.0.0.0:8600` in a container, not the default `127.0.0.1:8600`). The registered endpoint, which is also the configured `audience`, is what the gateway sends to and what every assertion names; it need not equal `-addr`. When the server listens on loopback the SDK refuses a request whose `Host` header is not a loopback one (protection against DNS rebinding), so a gateway on another host cannot reach a loopback listener. Example for a container network: the server runs as service `fin-mcp` with `-addr 0.0.0.0:8600`, the configuration has `"audience": "http://fin-mcp:8600/mcp"`, and the target is registered with the endpoint `http://fin-mcp:8600/mcp`.

## The gateway's public key

The operators give the server the gateway's **public** key: a PEM file of a PKIX public key on P-256 (`openssl ec -in caller.key -pubout`). The server reads it at start, never from the gateway at run time (the gateway has no JWKS endpoint). The file may hold several keys, and the server picks one by the assertion's `kid`, the RFC 7638 thumbprint of the key; at start it logs the `kid` of each key it holds, to compare with the gateway's. To rotate: add the new public key to the file and restart the server, then change the gateway's private key, then remove the old public key. The file is read strictly: nothing but whitespace may stand outside the PEM blocks, each block must be a `PUBLIC KEY` that parses as a P-256 key, and a truncated block is refused; the messages never print what the file holds. A file that is missing, cannot be read, holds no key or holds a key that is not P-256 stops the server from starting.

The keys the demo script makes (`tools/demo/fin-mcp.sh`) are for local testing only: whoever holds the private key can sign a caller of any tenant and role.

## The configuration

A JSON file, whose path is the flag `-config` of `cargo-mcp`. The server reads it at start and does not start if it is not valid (an unknown key included). The database is given by `IDP_CARGO_POSTGRES_DSN`, as for `cargo-api`.

```json
{
  "audience": "http://fin-mcp:8600/mcp",
  "gateway_id": "g1",
  "public_key_file": "gateway.pub.pem",
  "cursor_key_file": "cursor.key",
  "tenants": {
    "eii-test": {
      "cargo_tenant": "eii-test",
      "roles": {
        "finance-analyst": {"permissions": ["fin:ReadProfit"], "legal_entities": ["LE"]}
      }
    }
  },
  "limits": {"calls_per_minute": 60, "concurrent": 4}
}
```

- `audience`: the endpoint exactly as registered, ending in `/mcp`.
- `gateway_id`: the gateway whose assertions are accepted; `iss` must be `idp-mcp-gateway:<gateway_id>`.
- `public_key_file`, `cursor_key_file`: relative to the configuration file unless absolute. The cursor key is an HMAC key as base64 text of 32 bytes or more; it stays in the deployment and is not shared with the platform.
- `tenants`: for each tenant of the gateway, the CargoPai tenant it stands for and, for each role, the FIN permissions (only `fin:ReadProfit` for now; any other permission stops the server from starting) and the legal entities the role may see: a list, or `"*"` for all of the tenant's. A member's permissions and legal entities are the union of those of the roles the configuration names for them.
- A member's subject must be of the form FIN takes for an actor: it starts with a letter or digit and holds only letters, digits and `. _ : @ / -`, at most 128 characters. Any other subject (an address with `+`, for example) is refused as not allowed; the audit line names the cause.
- `tools/list` and `server/discover` need no assertion, so this server does not limit them; limiting them is the gateway's side.
- Default deny: a tenant or a role that is not named, a member whose roles add no permission, and a legal entity that is not allowed are refused with `you are not allowed to do this in FIN`. A job (or request) of a legal entity the member may not see is answered `no such job or request`, as if it did not exist; the job's legal entity is read before anything else about it.
- `limits`: calls a minute and calls at once for each member; zero or absent means the defaults, 60 and 4.

The server puts the CargoPai tenant and the assertion's `sub` on the request as FIN's principal and calls FIN in process; FIN authorizes as it always does, so the actor FIN sees is the member, not a service account. The assertion's `client` and `actor` go to the audit log only.

## Audit

One structured log line per call: the tool, the outcome (`ok`, `not_found` (no such job), `hidden` (a job of a legal entity the caller may not see; the answer is the same bytes as `not_found`, the log lets operators see probing), `not_allowed` (with a `cause`: the tenant is not in the policy, the subject is not of the right form, or no role grants `fin:ReadProfit`), `invalid_input`, `bad_cursor`, `rate_limited`, `unavailable`, `failed`, `protocol_error`), the time it took and a short hash of the caller (the first 4 bytes of the SHA-256 of the gateway tenant and the subject, in hex). No argument and no amount is logged.

## The result sensitivity of the target

**Cargo's recommendation, for the owner to confirm: `confidential`** for the whole target. Profit, revenue and cost are financial data; no tool returns a customer's personal or banking data, so nothing needs `restricted`. A default that is too low could send financial figures to an external model; one that is too high keeps the model from reading them, which is the safer way to be wrong. This is the guide's recommendation too (its section 5), and the owner of the data confirms it per target at registration. **It is not implemented yet:** idp-mcp D54 and idp-host D64 are rules without a field in the registration interface today, so there is nothing to set; the server sets no `_meta["io.idp.telos/resultSensitivity"]` of its own. When the field exists, register `confidential`.

## What this server does not do

- No write tool, and no tool that changes a cost review request: a request and its resolution stay in FIN's HTTP API (`docs/api/fin-eii.md`) until the platform decides how EII asks for them.
- No development token, anywhere: it takes no bearer token and has no flag for one. The only identity is the gateway's assertion.
- No development key outside local testing. The key pair of `tools/demo/fin-mcp.sh` is for a laptop; the server in production holds the public key the operators give it.
- No limit shared between copies: the rate and concurrency limits and the table of assertion ids that have been seen are in memory, one for each process. After a restart an assertion can be presented once more for the minute it lives, and with several copies a replay can reach another copy (the guide's section 3.6).
- No second review of the catalogue by itself: nothing in the server tells the gateway that a declaration changed except the gateway's own check.
- It does not run the idp-mcp stack, and has not been registered with one: that is the platform's.
