# Phase 0 Decisions

These decisions are implementation constraints for all later phases.

1. Deploy execution is asynchronous and durable. The deploy worker runs in the
   same Go binary as the API, while durable jobs, leases, attempts, and recovery
   state live in Postgres.
2. Postgres is the authoritative saga and source of truth. Kubernetes resources
   are derived artifacts reconstructed from Postgres.
3. No transaction spans Kubernetes and Postgres. Cross-system changes use
   ordered, idempotent saga steps with persisted checkpoints and compensation.
4. Taking a queued commit SHA is one atomic take-and-clear statement that
   returns the pre-update value. PostgreSQL 18's `OLD` row syntax is the
   normative contract:

   ```sql
   UPDATE environments
      SET pending_commit_sha = NULL
    WHERE id = $1
   RETURNING WITH (OLD AS old_row)
             old_row.pending_commit_sha;
   ```

   The implementation must preserve the pre-update value; a separate read and
   write is forbidden because it can lose a concurrent push. The executable
   PostgreSQL 18 contract is `tests/workflow/fixtures/pending_sha_pg18.sql`.
5. v1 has one user in one organization. The schema and API keep organization
   boundaries, but granular membership and RBAC are not implemented in v1.
6. Mutating API operations are idempotent. Callers provide or receive stable
   idempotency keys, repeated requests return the original logical result, and
   workers may safely retry persisted steps.
7. Environment-variable changes create immutable release snapshots. Applying a
   snapshot updates the Secret hash on the pod template and follows the normal
   durable deploy, readiness, failure, and rollback saga.
8. v1 uses one mutable Kubernetes Deployment per environment with
   `maxUnavailable=0`. Temporary mixed traffic between the old and new ReplicaSets
   during a rolling update is accepted.
9. `sslip.io` is the recommended default hostname suffix. It is not on the
   Public Suffix List (PSL), which means Let's Encrypt certificate quota is
   shared globally across all sslip.io users rather than per-install. For
   solo developers and small teams this is acceptable in practice. Operators
   running production workloads should configure their own wildcard domain
   (e.g. `apps.example.com` with a `*.apps.example.com → ingress-ip` record)
   to get per-install quota isolation. A startup log warning is emitted when
   sslip.io or nip.io is detected. The V3 pre-launch validation gate is
   resolved: PSL status confirmed absent, resolution documented, warning added.
10. Hostname generation uses `SuffixHostnameProvider`, which serves both
    preview (`{slug}.{suffix}`) and production (`{hostname_label}.{suffix}`)
    hostnames from a single configured DNS suffix. The `HostnameProvider`
    interface decouples the adapter from any specific service, allowing
    replacement without changing the release or environment model.
11. A push never calls GitHub `workflow_dispatch` directly. It first inserts or
    coalesces into the sole active durable orchestration job for its repository,
    workflow, ref, and environment, then atomically acquires that job's expiring
    lease in Postgres. Only the current lease owner may dispatch. Every dispatch
    carries required `orchestration_job_id` and immutable `commit_sha` workflow
    inputs. The workflow exposes the job ID through `run-name`, checks out the
    exact SHA, and attests `HEAD` equals that SHA before building. Correlation
    validates both the run-name identity and GitHub `head_sha`, allowing an ambiguous HTTP outcome to enter durable
    `dispatch_reconciling` state and search observable runs without mutation.
    Because GitHub offers no dispatch idempotency key, Cyclopus never
    automatically redispatches an ambiguous request. It polls until the hard
    deadline, then `ResolveOrchestration` records terminal
    `github_run_missing`; a pending or later push is the retry. The SQL contract
    is `workflow/orchestration-job.sql`.
12. v1 targets any VPS with nested virtualization, not a specific cloud
    provider. The install model is bring-your-own-node (Coolify/Dokploy-style):
    the operator supplies any SSH-accessible 64-bit Linux VPS whose app-workload
    nodes have nested virtualization enabled (required for the gVisor `runsc`
    runtime), and installs via a bootstrap script plus Helm — there is no
    cloud-API provisioning. The prior OVH-specific selection is retired; the
    constrained profile is now a generic `byo_vps` capacity gate (vcpu 2,
    memory 2048 MiB, disk 20 GiB). Execution remains blocked until the target
    host's capabilities — nested virtualization, capacity, and required OS
    baseline — are capability-probed and recorded as gate evidence.
13. Correlation does not make a GitHub run live forever. The first dispatch
    persists a store-owned, exact five-minute observation deadline; a correlated
    run persists a store-owned, exact 45-minute run deadline. Callers cannot
    override either hard deadline. Startup and 30-second periodic reconciliation claim
    every nonterminal dispatch/run with a 60-second Postgres reconciliation
    lease and persist status, conclusion, and error evidence. Missing after the
    dispatch deadline, still nonterminal after the run deadline, and GitHub
    `failure`, `cancelled`, or `timed_out` are terminal failures. Terminal
    resolution atomically promotes the latest pending SHA to `queued`, or leaves
    the scope as `completed`/`failed` when no pending SHA exists. Retry scheduling
    uses the dispatch deadline only for `dispatch_reconciling` and the run
    deadline only for `dispatched`; expired work resolves without another API
    call rather than persisting a past schedule and hot-looping.
14. **Phase 0 deletion erratum:** org, project, and app deletion is an audited
    tombstone lifecycle, superseding §4.6 wording about removing the top-level
    row and cascading descendants. The store may set `deleted_at` only after all
    descendant environments have `torn_down_at` and the caller explicitly
    confirms external API tokens, repository secrets, and provider credentials
    were revoked. Physical deletion is forbidden so release, reconciliation,
    environment, and audit history remains queryable.
