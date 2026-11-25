# FIN's API for EII: job profit and the cost review of a loss-making job (V2.99)

This document is the contract FIN offers EII (the Enterprise Interaction Infrastructure of the other IDP project). FIN defines
it; EII's own proposal in its EII-08 and JV-08 is input, not the contract. It is for an authorized test tenant: **the tokens are
development tokens until the IDP identity platform is connected.** They are signed with a key that whoever runs the server
holds, they name a tenant, an actor and permissions that nobody else vouches for, and they live 24 hours at most. Nothing here is
a production interface. The test tenant `eii-test` is seeded by `tools/demo/eii-test.sh`, which also starts the server at
`http://127.0.0.1:8095` and mints the tokens `alice` (`fin:ReadProfit`, `fin:RequestCostReview`) and `fin-reviewer`
(`fin:ReadProfit`, `fin:ResolveCostReview`).

## What the API does, and does not do

- It reads each freight job's revenue, cost and profit, as FIN computes them, with a **profit version**.
- It takes a **cost review request** for a job whose profit is below 0, and its **resolution** by another person.
- It never changes an amount. A request records the figures its requester saw; a resolution says whether finance corrected the
  costs through FIN's ordinary commands (`COST_CORRECTED`) or found them right (`NO_CHANGE`). Neither changes a revenue, a cost, a
  pool share or a profit.
- All amounts are integers in minor units of the job's functional currency (`CNY`: 100.01 yuan is `10001`), never decimals.

## Conventions

- Base path `/v1/fin/`. JSON, snake_case, UTF-8. A body is one object whose keys are unique and known: **an unknown key is
  refused** (`400 MALFORMED_REQUEST`, "unknown field"), at most 1 MiB, `Content-Type: application/json` (otherwise `415`).
- `Authorization: Bearer <token>` on every call (`401 UNAUTHENTICATED` without a valid token).
- **The tenant and the actor come only from the token.** A body that names a tenant or a person (`tenant_id`, `actor_id`,
  `requested_by`, `resolved_by`, ...) is refused as an unknown field, and so is a **query parameter** the route does not
  define (`?tenant_id=`, `?actor_id=`, anything else): `400 MALFORMED_REQUEST`, and `detail` names the parameter. What a call may do is decided by the token's
  permissions, by FIN's own authorizer, before anything is read or written:
  - `fin:ReadProfit`: the two profit routes and the two request reads;
  - `fin:RequestCostReview`: `POST cost-review-requests`;
  - `fin:ResolveCostReview`: `POST cost-review-requests/{id}/resolution`.

  `fin:*` and `*` grant all three. None is granted by name only: none changes an amount or how a control works, and the rule that
  matters, that the resolver is not the requester, is FIN's own and holds whatever a token says. (FIN's two settings
  permissions, `ConfigureSettings` and `ApproveSettings`, stay by name only and are not part of this API.) A call without the
  permission is `403 FORBIDDEN`.
- **Text.** A control character (a NUL byte, an escape, ...) in a field or a path value is refused with `400 MALFORMED_REQUEST` and
  `detail` names the field (`reason`, `note`, `job_id`, `profit_version`, `outcome`, or the path parameter). A line break and a tab are
  allowed in `reason` and `note` only. Bytes that are not UTF-8 are refused in the same way.
- Money: `{"currency": "CNY", "minor": 10001}`.
- Errors are RFC 9457 problems (`application/problem+json`): `type`, `title`, `status`, `detail`, `instance`, `code` and
  `correlation_id` (also the `X-Correlation-ID` response header; send your own, 8 to 64 characters, to trace a call).
  `code` is what a program reads; `detail` says what is wrong and, for a business refusal, the way out.
- Another tenant's rows are never visible: a job or request of another tenant is `404 NOT_FOUND`.

## Idempotency: the command id

`POST` needs `Idempotency-Key: <command id>` (1 to 128 letters, digits and `. _ : -`; `400 IDEMPOTENCY_KEY_REQUIRED` when it is
missing). The key is the **command id**.

- A command sent again with the same key and the same body by the same person returns **its first receipt**, with the same status
  (`201` or `200`), whether the first call was answered or its answer was lost. Nothing is written twice.
- The same key with another body, or from another person, is `409 IDEMPOTENCY_MISMATCH`. Use a new key for a new command.
- Two identical commands at once answer one receipt.
- A call that ends in `503 COMMIT_UNCERTAIN` (or whose response was lost) is **sent again with the same key**, never a new one: the
  answer is the receipt if the command committed, or the command runs if it did not. Do not retry with a new key before you have
  read the request back (`GET cost-review-requests/{id}`, or the job's list) and know it was not made.
- A refusal that was a business rule is not stored: after the cause is gone the same key may be used again with the same body.
- A business time is not part of the body: FIN records its own clock.

## The profit version

`profit_version` is an **opaque text**, the SHA-256 in hex (64 characters) of what a job's profit is computed from and of the
figures: the job's revision, the revision of each consolidation cost pool the job has a share in, the version of the settings in
force for its legal entity, and the revenue, cost, pool cost, profit (and its parts by charge code). Equal figures from the same
sources give the same version; a change of any of them gives another: a charge revised, a share apportioned, a pool's line
revised, a setting changed. **Clients compare it for equality only**: do not parse it, order it or derive anything from it. It is
not a hash of a response and not a revision number.

A request for a cost review carries the version its requester read. If the profit has changed since, FIN refuses the request as
stale (`FIN_REVIEW_STALE`): read the profit again, show it to the person, and ask again with the new version.

## Routes

### GET /v1/fin/legal-entities/{legalEntityId}/job-profits

The freight jobs of a legal entity that are not cancelled, in job id order, each with its profit. Permission `fin:ReadProfit`.

Query: `loss_only` (`true` or `false`, default `false`; `true` keeps the jobs whose profit is below 0), `limit` (1 to 500,
default 100), `after` (the `next_after` of the page before: a job id). Any other parameter, a `loss_only` that is not `true` or
`false`, a `limit` outside 1 to 500 or not a number, and an `after` that is not a job id are `400 MALFORMED_REQUEST`.

```json
{
  "legal_entity_id": "LE",
  "as_of": "2026-11-25T13:33:06.752086Z",
  "rows": [
    {"job_id": "JOB-H03", "shipment_id": "SH-H03", "customer_party_id": "cust-orient", "state": "OPEN", "currency": "CNY",
     "revenue": {"currency": "CNY", "minor": 12000}, "cost": {"currency": "CNY", "minor": 20001}, "profit": {"currency": "CNY", "minor": -8001},
     "profit_basis": "SEPARATE", "settings_version": 0, "profit_version": "6c3bfbcf69cd2b7739b5a2e6e51ad0ae9f0d82d12beac46b2b29938ead62a19e"}
  ],
  "next_after": "JOB-H03"
}
```

- `cost` is the job's cost and what it bears of consolidation cost pools together: `revenue - cost = profit` always. The parts
  are in `GET jobs/{jobId}/profit`.
- `profit_basis` is `SEPARATE` or `INCLUDED_MARKED` (the legal entity's setting F6, on how a departed house's former share, pending
  finance's confirmation, is told). Figures under the two bases are not to be compared. `settings_version` is the version of FIN's
  settings the row was read under.
- `as_of` is FIN's clock when the rows were read. All rows of a page are read in one read-only snapshot of FIN's data. For a job
  that has a share in a consolidation's pool, FIN asks PHY (the physical operations) which houses left the consolidation, just
  after the snapshot and not inside it, once for each consolidation of the page: a house that left between the two is told as PHY
  tells it at that moment. PHY's answer is part of the figures, so it is part of the version.
- `next_after` is the `after` of the next page, and empty after the last. With `loss_only=true` FIN looks at no more than 2,000
  jobs for one page, so a page may hold fewer than `limit` rows and still have a next page, and a next page may be empty:
  **continue while `next_after` is not empty**, not until a page is short.
- Status: `200`; `400`; `401`; `403`; `422 BUSINESS_RULE_VIOLATION` for a page FIN refuses (not seen with the checks above).

### GET /v1/fin/jobs/{jobId}/profit

One job's profit with its parts. Permission `fin:ReadProfit`. No query parameter (one is `400 MALFORMED_REQUEST`). `404 NOT_FOUND`
for an unknown job.

```json
{"job_id": "JOB-H03", "revision": 2, "profit_version": "6c3bfbcf...a19e",
 "revenue": {"currency": "CNY", "minor": 12000}, "invoiced": {"currency": "CNY", "minor": 0},
 "cost": {"currency": "CNY", "minor": 0}, "cost_matched": {"currency": "CNY", "minor": 0}, "cost_accrued": {"currency": "CNY", "minor": 0},
 "pool_cost": {"currency": "CNY", "minor": 20001}, "profit": {"currency": "CNY", "minor": -8001},
 "profit_basis": "SEPARATE", "settings_version": 0,
 "pending_in_profit": {"currency": "CNY", "minor": 0}, "former_shares_pending": {"currency": "CNY", "minor": 0},
 "rerouting_loss": {"currency": "CNY", "minor": 10000},
 "by_charge_code": [{"charge_code": "OCEAN_FREIGHT", "revenue": {"currency": "CNY", "minor": 12000}, "cost": {"currency": "CNY", "minor": 0}, "profit": {"currency": "CNY", "minor": 12000}}],
 "pool_shares": [{"pool_id": "P-2", "line_no": 1, "charge_code": "OCEAN_FREIGHT", "method": "CHARGEABLE", "local": {"currency": "CNY", "minor": 10001}, "pending": false}],
 "rerouting_losses": [{"pool_id": "P-1", "line_no": 1, "local": {"currency": "CNY", "minor": 10000}, "voucher": "CLAIM-1"}]}
```

`profit = revenue - cost - pool_cost`. `pool_cost` includes the re-routing losses the job bears (`rerouting_loss`), and, under
`INCLUDED_MARKED`, the former shares pending (`pending_in_profit`); under `SEPARATE` a former share pending
(`former_shares_pending`) is in neither `pool_cost` nor `profit`. `profit_version` is the one a request must carry.
`pool_shares` lists the shares borne and, marked `pending: true`, the former shares.

### POST /v1/fin/cost-review-requests

Asks finance to review the costs of a loss-making job. Permission `fin:RequestCostReview`. `Idempotency-Key` required.

```json
{"job_id": "JOB-H03", "profit_version": "6c3bfbcf...a19e", "reason": "The trucking and terminal costs look too high"}
```

`reason` is 1 to 500 characters. In one transaction FIN locks the job, reads the settings in force, computes the profit again and
decides. The requester is the token's actor.

`201 Created`, `Location: /v1/fin/cost-review-requests/CRR-1`, `ETag: "1"`:

```json
{"command_id": "e2e-1", "request_id": "CRR-1", "new_revision": 1, "request_no": 1}
```

`request_id` is `CRR-<n>`, numbered per tenant from 1 without a gap (a refused request takes no number).

| Status | `code` | When, and the way out |
|---|---|---|
| 400 | `MALFORMED_REQUEST` | not JSON, an unknown field, a repeated key, a key that is not snake_case |
| 400 | `IDEMPOTENCY_KEY_REQUIRED` | no `Idempotency-Key`: send one |
| 401 | `UNAUTHENTICATED` | no valid token |
| 403 | `FORBIDDEN` | the token lacks `fin:RequestCostReview` |
| 404 | `NOT_FOUND` | no such job (of this tenant) |
| 409 | `IDEMPOTENCY_MISMATCH` | the key was used for another body or by another person: use a new key |
| 415 | `UNSUPPORTED_MEDIA_TYPE` | send `application/json` |
| 422 | `FIN_REVIEW_JOB_NOT_OPEN` | the job is cancelled: ask about the shipment's open job |
| 422 | `FIN_REVIEW_NOT_A_LOSS` | the job's profit is not below 0: no review is needed |
| 422 | `FIN_REVIEW_STALE` | the profit changed since the version you saw: read it again (`GET jobs/{jobId}/profit`) and ask with its version |
| 422 | `FIN_REVIEW_OPEN` | the job has a request not yet resolved; `detail` names it ("request CRR-1 is open: wait for its resolution") |
| 422 | `BUSINESS_RULE_VIOLATION` | an invalid body that is none of the above (an empty or too long `reason`); `detail` says which |
| 503 | `COMMIT_UNCERTAIN` | send the same request again with the same key |

The checks are made in this order: the job is open, its profit is below 0, the version is the current one, no request is open.

Refusal example (`422`):

```json
{"type": "urn:idp-cargo:problem:fin_review_open", "title": "Unprocessable Entity", "status": 422,
 "detail": "fin: rejected by business rules: fin: FIN_REVIEW_OPEN: the job already has a cost review request: request CRR-1 is open: wait for its resolution",
 "instance": "/v1/fin/cost-review-requests", "code": "FIN_REVIEW_OPEN", "correlation_id": "..."}
```

Two different requests for one job at once: one is made, the other is refused as `FIN_REVIEW_OPEN`, naming it. Two identical ones
answer one receipt.

### GET /v1/fin/cost-review-requests/{requestId}

A request with its resolution. Permission `fin:ReadProfit`. No query parameter (one is `400 MALFORMED_REQUEST`). `404 NOT_FOUND` for an
unknown request.

```json
{"request_id": "CRR-1", "job_id": "JOB-H03", "shipment_id": "SH-H03", "legal_entity_id": "LE", "state": "RESOLVED",
 "requested_by": "alice", "requested_at": "2026-11-25T13:32:30.536873Z", "reason": "The trucking and terminal costs look too high",
 "revenue": {"currency": "CNY", "minor": 12000}, "cost": {"currency": "CNY", "minor": 20001}, "profit": {"currency": "CNY", "minor": -8001},
 "profit_basis": "SEPARATE", "settings_version": 0, "profit_version": "6c3bfbcf...a19e",
 "resolution": {"resolved_by": "fin-reviewer", "resolved_at": "2026-11-25T13:32:30.587793Z", "outcome": "NO_CHANGE", "note": "The costs are right"}}
```

`state` is `OPEN` (`resolution` is `null`) or `RESOLVED`. The figures are those the requester saw (cost includes pool cost); they
are not updated when the job later changes. Times are FIN's clock.

### GET /v1/fin/jobs/{jobId}/cost-review-requests

The requests of a job in the order made. Permission `fin:ReadProfit`. Query: `state` (`OPEN` or `RESOLVED`; both when absent),
`limit` (1 to 500, default 100) and `after` (a request id `CRR-<n>` without leading zeros, the `next_after` of the page before). Any other
parameter is refused too.

```json
{"job_id": "JOB-H03", "requests": [ { "...": "as GET cost-review-requests/{requestId}" } ], "next_after": ""}
```

`400 MALFORMED_REQUEST` for a bad `state`, `limit` or `after`, and for any other parameter. An unknown job answers an empty list.

### POST /v1/fin/cost-review-requests/{requestId}/resolution

Ends a request. Permission `fin:ResolveCostReview`. `Idempotency-Key` required.

```json
{"outcome": "NO_CHANGE", "note": "The costs were checked against the invoices and are right"}
```

`outcome` is `COST_CORRECTED` (finance corrected the costs through the ordinary commands; the resolution does not do it) or
`NO_CHANGE` (the costs are right). `note` is 1 to 500 characters. The resolver is the token's actor.

`200 OK`, `ETag: "2"`:

```json
{"command_id": "e2e-3", "request_id": "CRR-1", "new_revision": 2, "request_no": 1}
```

| Status | `code` | When, and the way out |
|---|---|---|
| 400, 401, 403, 409, 415, 503 | as above | `403` when the token lacks `fin:ResolveCostReview` |
| 404 | `NOT_FOUND` | no such request (of this tenant) |
| 422 | `FIN_REVIEW_SAME_ACTOR` | the person who requested does not resolve it: have another person with the permission resolve it |
| 422 | `FIN_REVIEW_CLOCK_BEHIND` | the resolution would be stamped earlier than its request, because the servers' clocks are apart: synchronise the servers' clocks (for example with NTP) and try again |
| 422 | `FIN_REVIEW_RESOLVED` | the request is resolved already: read it; a new review is a new request |
| 422 | `BUSINESS_RULE_VIOLATION` | an outcome that is none, or an empty or too long `note` |

After a resolution the job may be asked about again (a new request, with the version then current).

## Events

FIN publishes `CostReviewRequested.v1` and `CostReviewResolved.v1` through its outbox, in the same transaction as the request or
the resolution: subject the request id, the request number as version, and the job and the shipment. Like every FIN event they
carry identifiers and versions only, never an amount or a name.

## Limits of this step

- Development tokens only (above). No rate limit. No OpenAPI document yet.
- A page of job profits asks PHY once for each consolidation the page's jobs have a share in. Measured on the test tenant (30 jobs, 3
  of them with shares in 2 consolidations, FIN and PHY on one machine, 10 calls with `limit=500`): 32 to 45 ms a page (median 33 ms),
  against 4 ms for one job's profit. How much of it is PHY was not separated. A page of 500 jobs with many consolidations has not
  been measured and may be slower; FIN does not optimize it in this step.
- A job's profit is read from FIN's data and PHY's answer at one moment; PHY's answer is not part of FIN's snapshot (above).
- The server answers over HTTP on the loopback interface only; there is no TLS here.
