# GitHub webhook operations

This runbook covers the Phase 5 GitHub webhook inbox, reconciliation, durable fan-out, routing intents, metrics, rollout, rollback, and operator verification. GitHub App creation and workflow requirements are in [github-app.md](github-app.md).

## Security rules

- Never print or log a webhook secret, payload-encryption key, signature input, raw webhook payload from a real repository, access token, or decrypted payload.
- Use synthetic payloads for manual verification. Do not place personal data, private repository names, commit messages, email addresses, or customer identifiers in fixtures.
- Do not pass a secret on a command line or in an environment variable. Both may be exposed through shell history, process inspection, crash reports, or debugging tools.
- Disable shell tracing before secret handling: `set +x`. Use a temporary directory with mode `0700`, secret files with mode `0600`, and a cleanup trap.
- Status queries must not select `verified_payload`, `payload_nonce`, key bytes, Secret data, or decrypted content.

## Payload-encryption keyring

`CYCLOPUS_WEBHOOK_PAYLOAD_ENCRYPTION_KEY_FILE` is required. The file is strict JSON with no duplicate or unknown fields:

```json
{
  "active_version": "<version>",
  "keys": {
    "<version>": "<standard-base64 AES key>",
    "<retained-version>": "<standard-base64 AES key>"
  }
}
```

Requirements:

- Standard padded base64 only; URL-safe base64 is rejected.
- Each decoded key is exactly 16, 24, or 32 bytes.
- `active_version` names an entry in `keys`.
- Key version names are nonblank after trimming.
- On Unix, the file must have no group or world permission bits. Use `0600`.
- Mount the file read-only from an existing Kubernetes Secret or external secret provider. The stock Helm chart does not generate it.

### Encryption-key rotation and retention

1. Add the new key under a new immutable version while retaining every old key still referenced by `github_webhook_deliveries.key_version`.
2. Change `active_version` to the new version and restart the single Cyclopus pod.
3. Verify `/readyz`, a signed ping, and a synthetic push before removing any old key.
4. Keep an old key until no retained payload references it:

   ```sql
   SELECT key_version, count(*)
   FROM github_webhook_deliveries
   WHERE verified_payload IS NOT NULL
   GROUP BY key_version
   ORDER BY key_version;
   ```

5. Remove an old key only when its count is zero and every backup that contains payload encrypted with it has expired or remains decryptable under the approved backup/key escrow policy.

Removing a referenced key does not break delivery-ID replay detection, which uses retained digest and canonical metadata, but it prevents processing of any retained raw payload encrypted with that version.

## Exact runtime configuration

Environment variables use the `CYCLOPUS_` prefix. The table lists the final code defaults from `internal/config/config.go`.

### Listener and ingress settings

| Environment variable | Default | Constraint or purpose |
|---|---:|---|
| `CYCLOPUS_HTTP_ADDRESS` | `127.0.0.1:8080` | Main HTTP listener. Stock Helm sets `0.0.0.0:8080` behind Traefik. |
| `CYCLOPUS_HTTP_READ_HEADER_TIMEOUT` | `5s` | Slow-header limit. |
| `CYCLOPUS_HTTP_READ_TIMEOUT` | `15s` | HTTP request read limit. |
| `CYCLOPUS_HTTP_WRITE_TIMEOUT` | `30s` | Response write limit. |
| `CYCLOPUS_HTTP_IDLE_TIMEOUT` | `60s` | Keep-alive idle limit. |
| `CYCLOPUS_HTTP_SHUTDOWN_TIMEOUT` | `10s` | HTTP, metrics, and gRPC server shutdown bound. |
| `CYCLOPUS_HTTP_MAX_BODY_BYTES` | `1048576` | General HTTP body limit. |
| `CYCLOPUS_HTTP_MAX_HEADER_BYTES` | `1048576` | Main and metrics server header limit. |
| `CYCLOPUS_WEBHOOK_GITHUB_BODY_LIMIT` | `1048576` | Raw webhook body limit. |
| `CYCLOPUS_WEBHOOK_GITHUB_MAX_CONCURRENT` | `4` | In-process webhook handler semaphore; allowed range 1–64. |
| `CYCLOPUS_WEBHOOK_GITHUB_BODY_READ_TIMEOUT` | `10s` | Per-webhook body-read deadline. |
| `CYCLOPUS_WEBHOOK_GITHUB_RATE_LIMIT_ENABLED` | `true` | Required by validation. |
| `CYCLOPUS_WEBHOOK_GITHUB_RATE_LIMIT_PER_MINUTE` | `120` | Per-source-IP token refill rate. |
| `CYCLOPUS_WEBHOOK_GITHUB_RATE_LIMIT_BURST` | `30` | Per-source-IP burst; must not exceed the per-minute rate. |

At Traefik or another edge:

- terminate TLS;
- route only `POST /webhooks/github` to Cyclopus;
- preserve exactly one `X-GitHub-Delivery`, `X-GitHub-Event`, and `X-GitHub-Signature-256` field value;
- do not decompress requests; Cyclopus rejects any `Content-Encoding`;
- set the edge body limit no higher than the application limit;
- set slow-header and body timeouts consistent with the 5s and 10s application bounds;
- keep the upstream timeout below the 30s write timeout; and
- apply an edge request-rate/concurrency limit in addition to the in-process per-IP limiter.

Cyclopus returns empty fixed bodies with these statuses: `202` accepted or replayed, `400` malformed payload, `401` invalid authentication headers/signature, `405` non-POST, `409` delivery-ID conflict, `413` oversized body, `415` unsupported media type/encoding, `429` overload or rate limit, and `503` secret, body-read, identity, or persistence unavailability.

### GitHub event worker settings

| Environment variable | Default | Constraint or purpose |
|---|---:|---|
| `CYCLOPUS_GITHUB_EVENT_WORKER_SCAN_INTERVAL` | `1s` | Due-work and lease-recovery scan. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_DELIVERY_CONCURRENCY` | `4` | Delivery processors; range 1–100. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_FANOUT_CONCURRENCY` | `2` | Fan-out manifest processors; range 1–100. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_INTENT_CONCURRENCY` | `8` | Routing-intent processors; range 1–100. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_LEASE_DURATION` | `60s` | Delivery, manifest, and intent lease. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_PROCESSING_TIMEOUT` | `20s` | Per-processing context; must be shorter than the lease. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_MAX_ATTEMPTS` | `12` | Fixed by database constraints; no attempt 13. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_INITIAL_RETRY` | `5s` | Initial persisted retry delay. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_MAX_RETRY` | `15m` | Maximum retry delay and validation ceiling. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_PAYLOAD_RETENTION` | `24h` | Terminal raw-payload retention; `0` permits immediate cleanup, maximum `720h`. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_CLEANUP_INTERVAL` | `1m` | Payload-redaction scan interval. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_CLEANUP_BATCH` | `100` | Redaction batch; range 1–1000. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_SHUTDOWN_GRACE` | `30s` | Worker processing/write drain bound. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_MAX_FANOUT` | `10000` | Maximum eligible apps snapshotted for one delivery; range 1–10000. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_FANOUT_BATCH` | `100` | Candidate-to-intent materialization batch; 1 through `MAX_FANOUT`. |
| `CYCLOPUS_GITHUB_EVENT_WORKER_MAX_INTENTS` | `1000` | Legacy compatibility limit; if explicitly set without `MAX_FANOUT`, it also sets `MAX_FANOUT`. |
| `CYCLOPUS_GITHUB_REQUEST_TIMEOUT` | `20s` | GitHub API request timeout. It plus the 5s call-pin margin must fit within the 60s event lease. |

Claims use bounded `FOR UPDATE SKIP LOCKED` queries. A claim increments `attempts`; renewals and retry writes do not. Attempts 1–11 can retry. A retryable failure at claim 12 becomes terminal `attempts_exhausted`.

### Metrics listener

| Environment variable | Default | Constraint or purpose |
|---|---:|---|
| `CYCLOPUS_METRICS_ADDRESS` | `127.0.0.1:9090` | Separate HTTP listener serving only `/metrics`. |
| `CYCLOPUS_METRICS_ALLOW_PUBLIC` | `false` | Must be `true` before binding metrics to a non-loopback address. |

The metrics listener is separate from the public HTTP and gRPC listeners and must use a distinct address. Keep it loopback-only and scrape through a local agent or sidecar. Public binding is an explicit opt-in and should also have network policy and authentication at the surrounding infrastructure layer; the built-in endpoint has no application authentication.

Exposed series:

- `cyclopus_github_webhook_ingress_total{event,action,result}`
- `cyclopus_github_delivery_total{event,action,result}`
- `cyclopus_github_fanout_total{event,result}`
- `cyclopus_github_intent_total{event,result}`
- `cyclopus_github_delivery_processing_seconds{event,action,result}`
- `cyclopus_github_fanout_processing_seconds{event,result}`
- `cyclopus_github_intent_processing_seconds{event,result}`
- `cyclopus_github_delivery_fanout{event,action}`
- `cyclopus_github_leases{kind,state}`, where `kind` is `delivery`, `fanout`, or `intent`, and `state` is `active` or `delayed`

Metric labels are allowlisted and normalize unknown values to `other`; they never contain delivery IDs, repository names, refs, SHAs, app/environment IDs, secrets, signatures, payloads, or raw external errors.

## Readiness and shutdown

`GET /healthz` reports process health. `GET /readyz` returns `200` only after:

- migrations and schema-epoch validation complete;
- the database pool connects;
- the active GitHub webhook Secret resolves to a non-empty value;
- GitHub App credential reconciliation completes;
- the recovery worker completes its startup scan; and
- the GitHub event worker performs startup payload cleanup, exhausted-lease handling, and an initial claim/lease inspection.

The active-secret readiness probe is cached for 30 seconds and bounded by a 2-second resolver timeout. The server refreshes HTTP/gRPC readiness once per second.

On SIGINT, SIGTERM, server failure, or worker failure, Cyclopus clears readiness and cancels the shared lifecycle context. The event worker stops claiming, cancels processors, waits up to `CYCLOPUS_GITHUB_EVENT_WORKER_SHUTDOWN_GRACE`, stops terminal writes, and leaves unfinished leases to expire for recovery. The server then gracefully stops HTTP, metrics, and gRPC under `CYCLOPUS_HTTP_SHUTDOWN_TIMEOUT`, forcing gRPC stop only if graceful shutdown exceeds that bound.

## Rollout: migrations 10 through 14

Migrations are embedded and run at process start under a PostgreSQL advisory lock. Back up Postgres before deployment.

Apply and verify in this order:

1. `000010_github_webhook_routing` — delivery inbox, routing state/intents, claim and retention indexes.
2. `000011_github_unverified_revocation` — fail-closed revocation state, installation identity on routing rows, ingestion ordering, ref-transition observations.
3. `000012_github_webhook_signature_key_version` — matched HMAC version metadata.
4. `000013_github_webhook_conflict_error` — `github_conflict` terminal error code.
5. `000014_github_routing_fanout` — durable manifests/candidates and `fanout_staged` delivery result.

Procedure:

1. Take and verify an encrypted Postgres backup.
2. Prepare the payload keyring and active HMAC Secret reference.
3. Deploy the Phase 5 binary. Do not enable GitHub delivery against a partially migrated database.
4. Wait for the Deployment rollout and `/readyz`.
5. Verify `schema_migrations.version = 14` and `dirty = false`.
6. Verify the metrics listener, then run signed ping, replay/conflict, production push, preview push, and deletion checks below.
7. Enable or retain GitHub webhook delivery only after all checks pass.

## Rollback guards

Stop webhook ingress and workers before preparing a schema rollback. Never bypass a guard or manually drop its trigger.

### Migration 14 to 13

Rollback is blocked while:

- any delivery has `result_code='fanout_staged'`; or
- any fan-out manifest is `pending` or `leased`.

Drain active manifests. Then map each terminal `fanout_staged` delivery to the v13-compatible meaning, normally `processed` after confirming its manifest/intents reached a durable terminal outcome. Remove candidate rows only through manifest deletion/cascade after preserving required audit evidence. Confirm no active manifest and no `fanout_staged` delivery remains before retrying the down migration.

### Migration 13 to 12

Rollback is blocked while any delivery has `error_code='github_conflict'`. Retain v13 or archive/resolve those rows and map them to a v12-compatible terminal code under an approved incident record.

### Migration 12 to 11

Rollback is blocked while any delivery has `signature_key_version <> 'legacy'`. Retain v12, or archive the version metadata and map all rows to `legacy` only if losing matched-key provenance is explicitly approved.

### Migration 11 to 10

Rollback is blocked until all of these are true:

- no installation, repository, or git connection is `unverified_revocation`;
- `github_ref_transition_observations` is empty;
- ref-routing observation checkpoints are cleared;
- retained deliveries do not refer to repositories transferred to another installation;
- repository-routing and intent installation identities are compatible with v10;
- deliveries and intents have no `pending` or `leased` rows; and
- no delivery retains `verified_payload`.

Reconcile revocations first. Archive required transition evidence outside the production schema, clear checkpoints only under a reviewed change, repair or remove incompatible identity rows, drain work, and run payload redaction.

### Migration 10 to 9

Rollback is blocked while any delivery or intent is `pending`/`leased`, or any delivery retains `verified_payload`. Stop ingress, drain or terminalize work, wait for retention cleanup or run an approved cleanup, and confirm all counts are zero.

## Backup and retention policy

- WAL-G sends Postgres backups over TLS to the configured S3-compatible destination. Require provider-side encryption at rest (SSE-S3, SSE-KMS, or equivalent), bucket access logging, versioning/object-lock policy appropriate to the organization, and least-privilege credentials.
- The database stores webhook payloads as AES-GCM ciphertext with authenticated delivery metadata. Backups contain that ciphertext, not plaintext, but remain sensitive because retained keyrings can decrypt it.
- The effective raw-payload retention is the longer of the live database retention and the backup/PITR retention. If policy requires deletion after `CYCLOPUS_GITHUB_EVENT_WORKER_PAYLOAD_RETENTION`, backup/PITR retention must not exceed that duration, or backup sets containing the ciphertext and corresponding keys must be cryptographically expired on the same schedule.
- Retain payload-encryption keys for every nonexpired backup that may contain their ciphertext. Store backup credentials and key escrow separately.
- Test restore and decryption with synthetic data. Never restore production webhook payloads into an unapproved development environment.
- See `infra/backup/README.md` for WAL-G wiring, the 60-second RPO, restore, DR, and upgrade scripts.

## Verification procedures

The following shell setup writes the HMAC secret to a private temporary file without printing it or placing it in an argument or environment variable. Run it in a shell with history disabled for the session if your shell records commands containing identifying placeholders. The commands themselves contain no secret value.

```sh
set +x
set +o history 2>/dev/null || true
umask 077
VERIFY_DIR=$(mktemp -d)
trap 'rm -rf "$VERIFY_DIR"' EXIT HUP INT TERM

: "${CYCLOPUS_URL:?set to https://cyclopus.example}"
: "${CYCLOPUS_NAMESPACE:?set to the control-plane namespace}"
: "${CYCLOPUS_WEBHOOK_SECRET_NAME:?set to the Kubernetes Secret name}"
: "${CYCLOPUS_WEBHOOK_SECRET_KEY:=webhook-secret}"

kubectl -n "$CYCLOPUS_NAMESPACE" get secret "$CYCLOPUS_WEBHOOK_SECRET_NAME" \
  -o "jsonpath={.data.${CYCLOPUS_WEBHOOK_SECRET_KEY}}" \
  | base64 -d >"$VERIFY_DIR/hmac.key"
chmod 0600 "$VERIFY_DIR/hmac.key"
test -s "$VERIFY_DIR/hmac.key"
```

Define a signer/sender. Python reads the key and payload files directly; the key is never printed.

```sh
send_signed_webhook() {
  event=$1
  delivery=$2
  payload_file=$3
  signature=$(python3 - "$VERIFY_DIR/hmac.key" "$payload_file" <<'PY'
import hashlib, hmac, pathlib, sys
key = pathlib.Path(sys.argv[1]).read_bytes()
payload = pathlib.Path(sys.argv[2]).read_bytes()
print("sha256=" + hmac.new(key, payload, hashlib.sha256).hexdigest())
PY
  )
  curl --silent --show-error --output /dev/null --write-out '%{http_code}\n' \
    --request POST "$CYCLOPUS_URL/webhooks/github" \
    --header 'Content-Type: application/json' \
    --header "X-GitHub-Delivery: $delivery" \
    --header "X-GitHub-Event: $event" \
    --header "X-GitHub-Signature-256: $signature" \
    --data-binary "@$payload_file"
  unset signature
}
```

### Signed stateless ping

```sh
printf '%s' '{"zen":"synthetic connectivity check","hook_id":1}' >"$VERIFY_DIR/ping.json"
PING_DELIVERY=$(python3 -c 'import uuid; print(uuid.uuid4())')
send_signed_webhook ping "$PING_DELIVERY" "$VERIFY_DIR/ping.json"
```

Expected: HTTP `202`; no delivery row for `PING_DELIVERY`; `cyclopus_github_webhook_ingress_total{event="ping",action="",result="accepted"}` increases.

### Signed replay and conflict

Use synthetic IDs that exist in the target instance. Do not use a real payload copied from GitHub.

```sh
: "${TEST_INSTALLATION_ID:?set a non-sensitive test installation ID}"
: "${TEST_REPOSITORY_ID:?set a non-sensitive test repository ID}"
: "${TEST_REPOSITORY_NODE_ID:?set a non-sensitive test repository node ID}"
REPLAY_DELIVERY=$(python3 -c 'import uuid; print(uuid.uuid4())')

python3 - "$TEST_INSTALLATION_ID" "$TEST_REPOSITORY_ID" "$TEST_REPOSITORY_NODE_ID" >"$VERIFY_DIR/replay.json" <<'PY'
import json, sys
json.dump({
  "installation": {"id": int(sys.argv[1])},
  "repository": {"id": int(sys.argv[2]), "node_id": sys.argv[3]},
  "ref": "refs/heads/cyclopus-doc-verification",
  "before": "1" * 40,
  "after": "2" * 40,
  "deleted": False
}, sys.stdout, separators=(",", ":"))
PY

send_signed_webhook push "$REPLAY_DELIVERY" "$VERIFY_DIR/replay.json"
send_signed_webhook push "$REPLAY_DELIVERY" "$VERIFY_DIR/replay.json"

python3 - "$VERIFY_DIR/replay.json" >"$VERIFY_DIR/conflict.json" <<'PY'
import json, pathlib, sys
value = json.loads(pathlib.Path(sys.argv[1]).read_text())
value["after"] = "3" * 40
json.dump(value, sys.stdout, separators=(",", ":"))
PY
send_signed_webhook push "$REPLAY_DELIVERY" "$VERIFY_DIR/conflict.json"
```

Expected statuses: first `202`, replay `202`, conflict `409`. The database contains one delivery row; its original `payload_digest` remains unchanged. The conflict does not create another intent or orchestration side effect.

### Production push

Prefer an ordinary push to a dedicated test repository's configured production branch so GitHub supplies authoritative branch state. Record the GitHub delivery ID from the App's delivery view. Expected progression:

1. delivery `pending` → `leased` → `completed` with `result_code` `fanout_staged` or `processed`;
2. fan-out manifest `pending`/`leased` → `completed` when fan-out is nonzero;
3. one production routing intent per eligible app → `completed`/`processed`;
4. one queued or coalesced `orchestration_jobs` row with the stored workflow path and pushed SHA;
5. `github_push.snapshotted` audit evidence; and
6. delivery, fan-out, and intent metrics increase.

### Preview push

Push a new non-production branch to a test repository for an app with previews enabled. Expected: completed delivery/manifest/intent, one preview environment for that app and branch, one queued/coalesced orchestration job, and normal environment/deploy audit events. An app at its preview cap may terminalize only its own intent; sibling apps continue.

### Branch deletion

Delete the test preview branch through GitHub. Expected: a completed delete routing intent and an idempotent preview teardown/lifecycle job. Repeating the deletion converges successfully. Deleting the configured production branch must not tear down production; the delivery records `production_branch_deleted`, and audit evidence records the production-delete count/outcome.

## Safe status queries

Set a local `psql` variable rather than interpolating delivery IDs into SQL:

```sql
SELECT version, dirty FROM schema_migrations;

SELECT status, count(*)
FROM github_webhook_deliveries
GROUP BY status ORDER BY status;

SELECT event, action, result_code, error_code, count(*)
FROM github_webhook_deliveries
GROUP BY event, action, result_code, error_code
ORDER BY event, action, result_code, error_code;

SELECT status, count(*)
FROM github_routing_fanout_manifests
GROUP BY status ORDER BY status;

SELECT status, result_code, error_code, count(*)
FROM github_routing_intents
GROUP BY status, result_code, error_code
ORDER BY status, result_code, error_code;

SELECT count(*) AS retained_payload_count
FROM github_webhook_deliveries
WHERE verified_payload IS NOT NULL;

SELECT key_version, count(*)
FROM github_webhook_deliveries
WHERE verified_payload IS NOT NULL
GROUP BY key_version ORDER BY key_version;

SELECT access_state, count(*)
FROM github_installations
GROUP BY access_state ORDER BY access_state;

SELECT access_state, count(*)
FROM github_repositories
GROUP BY access_state ORDER BY access_state;

SELECT status, count(*)
FROM orchestration_jobs
GROUP BY status ORDER BY status;

SELECT action, target_type, count(*)
FROM audit_events
WHERE action LIKE 'github_%' OR action LIKE 'github.%'
GROUP BY action, target_type
ORDER BY action, target_type;
```

For one delivery, use `psql -v delivery_id='<uuid>'` and metadata-only columns:

```sql
SELECT id, delivery_id, event, action, status, attempts,
       signature_key_version, key_version, result_code, error_code,
       received_at, terminal_at, payload_redacted_at
FROM github_webhook_deliveries
WHERE delivery_id = :'delivery_id'::uuid;
```

## Query-plan and index checks

Run these in a staging copy with representative cardinality. Keep `enable_seqscan` enabled; inspect the natural plan. The partial indexes are:

- `github_webhook_deliveries_due`
- `github_webhook_deliveries_expired_lease`
- `github_webhook_deliveries_terminal_retention`
- `github_routing_intents_due`
- `github_routing_intents_expired_lease`
- `github_routing_fanout_manifests_pending`
- `github_routing_fanout_manifests_leased`
- `github_routing_fanout_manifests_terminal`
- `github_routing_fanout_manifests_ref_generation`
- `github_routing_fanout_candidates_app_identity`
- `github_ref_transition_observations_transition`

```sql
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT id
FROM github_webhook_deliveries
WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp()
ORDER BY next_attempt_at, received_at, id
LIMIT 100;

EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT id
FROM github_routing_fanout_manifests
WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp()
ORDER BY next_attempt_at, updated_at, id
LIMIT 100;

EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT id
FROM github_routing_intents
WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp()
ORDER BY next_attempt_at, created_at, id
LIMIT 100;

EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT id
FROM github_webhook_deliveries
WHERE status IN ('completed','failed')
  AND verified_payload IS NOT NULL
  AND retain_until<=clock_timestamp()
ORDER BY retain_until, id
LIMIT 100;
```

Expected: the appropriate partial index participates, row estimates remain bounded, and the query does not sort or scan the full table. Repository integration coverage for plans and concurrency is in `internal/store/github_webhooks_integration_test.go`, `internal/store/github_fanout_integration_test.go`, and `internal/worker/github_events_integration_test.go`.

## Alert catalogue

Tune durations and rates to normal traffic. Prefer rate/increase alerts over a counter's absolute value.

| Condition | Signal | Initial response |
|---|---|---|
| Invalid authentication | Increase in `cyclopus_github_webhook_ingress_total{result="auth_invalid"}` | Check GitHub secret rotation, duplicate proxies/headers, and hostile traffic. Never log supplied signatures. |
| Rate limited or overloaded | Increase in ingress `result="rate_limited"` or `result="overload"` | Check edge limits, source distribution, handler saturation, and body-read latency. |
| Delivery conflict | Increase in ingress `result="conflict"` or DB `error_code='github_conflict'` | Preserve the original row, identify delivery-ID reuse, and check proxy/body mutation. |
| Retry exhaustion | Increase in delivery/fan-out/intent `result="failed"` with `attempts_exhausted` rows | Inspect allowlisted error codes, GitHub/database health, and worker lease timing. |
| Terminal failed work | Rising `github_webhook_deliveries`, manifests, or intents in `failed` | Group by `error_code`; repair the dependency or reconcile access before redrive through normal new events. |
| Lease recovery/loss | Increase in delivery/fan-out/intent `result="lease_lost"` | Check processing time versus lease, restarts, DB latency, and competing workers. Occasional loss during shutdown is recoverable. |
| Delayed lease gauge | Sustained `cyclopus_github_leases{state="delayed"} > 0` | An unexpired foreign lease is blocking local capacity; verify owner health and wait for expiry rather than stealing it. |
| Fan-out failed | Increase in `cyclopus_github_fanout_total{result="failed"}` | Check manifest error codes, `MAX_FANOUT`, candidate integrity, and migration 14 health. |
| Backlog | Sustained growth of DB `pending` counts with flat processed counters | Check worker readiness, database/GitHub availability, concurrency, retry schedule, and per-item error codes. |
| Readiness unavailable | `/readyz` non-200 or gRPC health `NOT_SERVING` | Check startup migration, DB ping, active Secret resolution, App reconciliation, and startup worker scan. |
| Payload cleanup stalled | Retained-payload count grows beyond terminal rate plus retention window | Check cleanup interval/batch, worker errors, `retain_until`, and keyring availability. |

## Troubleshooting

### HTTP `401`

The required authentication header is missing, duplicated, comma-joined, malformed, or signed with an unavailable/nonaccepted key. Confirm the GitHub App URL and secret, active/previous references, overlap cutoff, and any proxy header rewriting. All such failures intentionally return the same empty response.

### HTTP `409`

The same `X-GitHub-Delivery` UUID arrived with different canonical identity or body digest. Preserve the original row. Check retries through body-transforming proxies, manual tests reusing a UUID, and possible delivery-ID collision or misuse.

### HTTP `413` or `415`

For `413`, compare edge and application body limits. For `415`, require `application/json` with optional UTF-8 charset only and no `Content-Encoding`.

### HTTP `429`

Distinguish metric result `rate_limited` from `overload`. Rate limiting is per source IP; overload means all handler semaphore slots are occupied. Check whether the edge collapses all clients to one source address and whether slow bodies hold slots.

### HTTP `503` or readiness failure

Check database connectivity, active HMAC Secret resolution, Kubernetes RBAC/Secret labels, body-read timeouts, and identity lookup. `/readyz` intentionally omits internal error text; inspect structured process logs without enabling payload logging.

### Pending or leased backlog

`pending` with a future `next_attempt_at` is a scheduled retry. Due `pending` work that does not move indicates a stopped/unready worker or claim-query problem. An unexpired `leased` row is not stolen; an expired row is reclaimable with a larger lease epoch. Compare processing timeout, GitHub request timeout, lease duration, and database latency.

### `unverified_revocation`

Routing fails closed while GitHub access is ambiguous after suspension, deletion, repository removal, transfer, or permission loss. Restore GitHub/API availability and let authoritative reconciliation converge. Do not manually mark the row active without reconciling its stable installation/repository identity.

### Fan-out stalled or failed

Inspect manifest status, counts, cursor/cutoff, attempt, and error code without selecting candidate payload-sensitive context. Ensure migration 14 is applied, `FANOUT_BATCH <= MAX_FANOUT`, and candidate foreign keys still resolve. One failed intent does not require rolling back successful siblings.

### Production branch deletion

This is intentionally non-destructive. Confirm the delivery result/audit evidence, restore the branch if accidental, and push a new authoritative SHA. Do not manually request production teardown as part of webhook recovery.
