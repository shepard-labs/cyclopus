-- Phase 0 contract. The implementation migration may rename objects, but must
-- preserve these durability, scope single-flight, coalescing, lease, deadline,
-- status evidence, and dispatch/run reconciliation guarantees.
CREATE TABLE orchestration_jobs (
    id uuid PRIMARY KEY,
    repository_id uuid NOT NULL,
    workflow_id text NOT NULL,
    git_ref text NOT NULL,
    environment_id uuid NOT NULL,
    commit_sha text NOT NULL,
    idempotency_key text NOT NULL UNIQUE,
    pending_commit_sha text,
    pending_idempotency_key text UNIQUE,
    status text NOT NULL CHECK (status IN (
        'queued', 'leased', 'dispatch_reconciling', 'dispatched',
        'completed', 'failed', 'blocked'
    )),
    lease_owner text,
    lease_expires_at timestamptz,
    dispatch_requested_at timestamptz,
    reconcile_after timestamptz,
    dispatch_deadline_at timestamptz,
    run_deadline_at timestamptz,
    reconciliation_owner text,
    reconciliation_lease_expires_at timestamptz,
    github_dispatch_identity text,
    github_run_id bigint,
    github_run_status text,
    github_run_conclusion text,
    github_status_observed_at timestamptz,
    terminal_error_code text,
    terminal_error_message text,
    terminal_at timestamptz,
    attempt integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'leased') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK ((pending_commit_sha IS NULL) = (pending_idempotency_key IS NULL)),
    CHECK (status <> 'dispatch_reconciling' OR
           (dispatch_requested_at IS NOT NULL AND reconcile_after IS NOT NULL AND dispatch_deadline_at IS NOT NULL)),
    CHECK (status <> 'dispatched' OR
           (github_run_id IS NOT NULL AND run_deadline_at IS NOT NULL)),
    CHECK ((reconciliation_owner IS NULL) = (reconciliation_lease_expires_at IS NULL))
);

-- Immutable per-attempt evidence survives reuse of the job row when a pending
-- SHA is promoted after success or terminal failure.
CREATE TABLE orchestration_job_run_evidence (
    orchestration_job_id uuid NOT NULL REFERENCES orchestration_jobs(id),
    attempt integer NOT NULL,
    github_run_id bigint,
    github_run_status text NOT NULL,
    github_run_conclusion text,
    observed_at timestamptz NOT NULL,
    terminal_error_code text,
    terminal_error_message text,
    terminal_at timestamptz,
    PRIMARY KEY (orchestration_job_id, attempt, observed_at)
);

-- Exactly one active build/deploy may own a repository/workflow/ref/environment
-- scope. This partial unique index is also the conflict arbiter for enqueue.
CREATE UNIQUE INDEX orchestration_jobs_scope_single_flight
    ON orchestration_jobs (repository_id, workflow_id, git_ref, environment_id)
    WHERE status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched');

CREATE INDEX orchestration_jobs_claimable
    ON orchestration_jobs (created_at, id)
    WHERE status IN ('queued', 'leased');

CREATE INDEX orchestration_jobs_reconciliation_due
    ON orchestration_jobs (reconcile_after, run_deadline_at, id)
    WHERE status IN ('dispatch_reconciling', 'dispatched');

CREATE UNIQUE INDEX orchestration_jobs_github_run
    ON orchestration_jobs (github_run_id)
    WHERE github_run_id IS NOT NULL;

-- Enqueue or coalesce a push. If dispatch has not started, replace the queued
-- SHA directly. Otherwise retain only the latest pending SHA; intermediate
-- pushes are deliberately superseded. $1..$7 are id, repository, workflow,
-- ref, environment, commit SHA, and the SHA-scoped idempotency key.
INSERT INTO orchestration_jobs (
    id, repository_id, workflow_id, git_ref, environment_id, commit_sha, idempotency_key
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (repository_id, workflow_id, git_ref, environment_id)
    WHERE status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched')
DO UPDATE
   SET commit_sha = CASE
           WHEN orchestration_jobs.status = 'queued' THEN EXCLUDED.commit_sha
           ELSE orchestration_jobs.commit_sha
       END,
       idempotency_key = CASE
           WHEN orchestration_jobs.status = 'queued' THEN EXCLUDED.idempotency_key
           ELSE orchestration_jobs.idempotency_key
       END,
       pending_commit_sha = CASE
           WHEN orchestration_jobs.status = 'queued' THEN NULL
           ELSE EXCLUDED.commit_sha
       END,
       pending_idempotency_key = CASE
           WHEN orchestration_jobs.status = 'queued' THEN NULL
           ELSE EXCLUDED.idempotency_key
       END,
       updated_at = now()
RETURNING id, status, commit_sha, pending_commit_sha;

-- Acquire before any workflow_dispatch request. Reconciliation jobs are never
-- reclaimed as ordinary leases: they follow the protocol below first.
WITH candidate AS (
    SELECT id
      FROM orchestration_jobs
     WHERE status = 'queued'
        OR (status = 'leased' AND lease_expires_at < now())
     ORDER BY created_at, id
     FOR UPDATE SKIP LOCKED
     LIMIT 1
)
UPDATE orchestration_jobs AS job
   SET status = 'leased',
       lease_owner = $1,
       lease_expires_at = now() + interval '120 seconds',
       github_run_status = NULL,
       github_run_conclusion = NULL,
       github_status_observed_at = NULL,
       terminal_error_code = NULL,
       terminal_error_message = NULL,
       terminal_at = NULL,
       attempt = attempt + 1,
       updated_at = now()
  FROM candidate
 WHERE job.id = candidate.id
RETURNING job.id, job.idempotency_key, job.repository_id, job.workflow_id,
          job.git_ref, job.environment_id, job.commit_sha, job.lease_expires_at;

-- Before sending workflow_dispatch, durably enter reconciliation and release
-- the lease. This closes the crash window between the external side effect and
-- recording its unknown outcome. The request MUST include
-- inputs.orchestration_job_id = job.id and inputs.commit_sha = job.commit_sha.
-- The customer workflow MUST expose the job ID in run-name, checkout the exact
-- commit_sha, and attest that HEAD equals commit_sha before building. Correlation
-- requires both the run-name identity and GitHub's head_sha to equal commit_sha.
-- Both a 2xx and
-- an ambiguous timeout remain in this state until a correlated run is observed.
-- The store, not its caller, establishes the exact five-minute observation deadline;
-- observation polls do not extend it forever and never redispatch.
UPDATE orchestration_jobs
   SET status = 'dispatch_reconciling',
       dispatch_requested_at = now(),
       reconcile_after = now() + interval '90 seconds',
       dispatch_deadline_at = COALESCE(dispatch_deadline_at, now() + interval '300 seconds'),
       lease_owner = NULL,
       lease_expires_at = NULL,
       updated_at = now()
 WHERE id = $1
   AND status = 'leased'
   AND lease_owner = $2
   AND lease_expires_at > now();

-- Reconciliation polls workflow runs created since dispatch_requested_at and
-- matches display_title/run-name `cyclopus:<orchestration_job_id>`. Persist the
-- observed run before leaving dispatch reconciliation and establish the
-- exact store-owned 45-minute run deadline.
UPDATE orchestration_jobs
   SET status = 'dispatched',
       github_dispatch_identity = $2,
       github_run_id = $3,
       github_run_status = $4,
       github_run_conclusion = NULL,
       github_status_observed_at = now(),
       reconcile_after = now() + interval '30 seconds',
       run_deadline_at = now() + interval '2700 seconds',
       reconciliation_owner = NULL,
       reconciliation_lease_expires_at = NULL,
       updated_at = now()
 WHERE id = $1
   AND status = 'dispatch_reconciling'
   AND github_run_id IS NULL;

-- Startup and every 30-second periodic sweep claim all due nonterminal GitHub
-- work. A separate reconciliation lease prevents concurrent API replicas from
-- polling or resolving the same job. Expired leases are reclaimable.
WITH candidate AS (
    SELECT id
      FROM orchestration_jobs
     WHERE status IN ('dispatch_reconciling', 'dispatched')
       AND (reconciliation_lease_expires_at IS NULL OR reconciliation_lease_expires_at < now())
       AND (reconcile_after <= now() OR dispatch_deadline_at <= now() OR run_deadline_at <= now())
     ORDER BY COALESCE(reconcile_after, run_deadline_at), id
     FOR UPDATE SKIP LOCKED
     LIMIT 1
)
UPDATE orchestration_jobs AS job
   SET reconciliation_owner = $1,
       reconciliation_lease_expires_at = now() + interval '60 seconds',
       updated_at = now()
  FROM candidate
 WHERE job.id = candidate.id
RETURNING job.*;

-- Retryable observation failures are bounded by the deadline for the current
-- state. Never use dispatch_deadline_at for a dispatched run: that deadline is
-- already past and would hot-loop. The WHERE guard prevents persisting a past
-- reconcile_after; an expired job must use the terminal resolver below.
UPDATE orchestration_jobs
   SET reconcile_after = LEAST(
           now() + interval '30 seconds',
           CASE status
               WHEN 'dispatch_reconciling' THEN dispatch_deadline_at
               WHEN 'dispatched' THEN run_deadline_at
           END),
       reconciliation_owner = NULL,
       reconciliation_lease_expires_at = NULL,
       updated_at = now()
 WHERE id = $1
   AND status IN ('dispatch_reconciling', 'dispatched')
   AND CASE status
           WHEN 'dispatch_reconciling' THEN dispatch_deadline_at
           WHEN 'dispatched' THEN run_deadline_at
       END > now()
   AND reconciliation_owner = $2
   AND reconciliation_lease_expires_at > now();

-- If no correlated run is observable before the hard dispatch deadline, retain
-- the same dispatch_reconciling job and schedule another observation. GitHub
-- offers no dispatch idempotency key, so an ambiguous workflow_dispatch is
-- never sent again automatically.
UPDATE orchestration_jobs
   SET reconcile_after = LEAST(now() + interval '90 seconds', dispatch_deadline_at),
       reconciliation_owner = NULL,
       reconciliation_lease_expires_at = NULL,
       updated_at = now()
 WHERE id = $1
   AND status = 'dispatch_reconciling'
   AND github_run_id IS NULL
   AND reconcile_after <= now()
   AND dispatch_deadline_at > now()
   AND reconciliation_owner = $2
   AND reconciliation_lease_expires_at > now();

-- Persist every observed nonterminal GitHub status before scheduling the next
-- poll. This statement is used by startup and periodic reconciliation.
WITH evidence AS (
    INSERT INTO orchestration_job_run_evidence (
        orchestration_job_id, attempt, github_run_id, github_run_status,
        github_run_conclusion, observed_at
    )
    SELECT id, attempt, github_run_id, $3, $4, now()
      FROM orchestration_jobs
     WHERE id = $1
       AND status = 'dispatched'
       AND reconciliation_owner = $2
       AND reconciliation_lease_expires_at > now()
    RETURNING orchestration_job_id
)
UPDATE orchestration_jobs AS job
   SET github_run_status = $3,
       github_run_conclusion = $4,
       github_status_observed_at = now(),
       reconcile_after = now() + interval '30 seconds',
       reconciliation_owner = NULL,
       reconciliation_lease_expires_at = NULL,
       updated_at = now()
  FROM evidence
 WHERE job.id = evidence.orchestration_job_id
   AND $3 IN ('queued', 'in_progress');

-- Resolve success or terminal failure in one transaction. Callers use
-- github_run_missing after dispatch_deadline_at, github_run_timed_out after
-- run_deadline_at, and github_run_failed/cancelled/timed_out for the matching
-- GitHub conclusions. Immutable evidence is inserted first, then the latest
-- pending SHA is atomically promoted to queued in the same statement. Without
-- pending work, the row becomes completed or failed and leaves single-flight.
-- A missing run is valid only with github_run_missing after the hard dispatch
-- deadline. A pending or later push is the retry; reconciliation never is.
WITH terminal_evidence AS (
    INSERT INTO orchestration_job_run_evidence (
        orchestration_job_id, attempt, github_run_id, github_run_status,
        github_run_conclusion, observed_at, terminal_error_code,
        terminal_error_message, terminal_at
    )
    SELECT id, attempt, github_run_id, $3, $4, now(), $5, $6, now()
      FROM orchestration_jobs
     WHERE id = $1
       AND status IN ('dispatch_reconciling', 'dispatched')
       AND reconciliation_owner = $2
       AND reconciliation_lease_expires_at > now()
       AND (
           $4 IN ('success', 'failure', 'cancelled', 'timed_out')
           OR (status = 'dispatch_reconciling' AND github_run_id IS NULL
               AND dispatch_deadline_at <= now() AND $5 = 'github_run_missing')
           OR (status = 'dispatched' AND run_deadline_at <= now())
       )
    RETURNING orchestration_job_id
)
UPDATE orchestration_jobs AS job
   SET status = CASE
           WHEN pending_commit_sha IS NOT NULL THEN 'queued'
           WHEN $4 = 'success' THEN 'completed'
           ELSE 'failed'
       END,
       commit_sha = COALESCE(pending_commit_sha, commit_sha),
       idempotency_key = COALESCE(pending_idempotency_key, idempotency_key),
       pending_commit_sha = NULL,
       pending_idempotency_key = NULL,
       dispatch_requested_at = NULL,
       reconcile_after = NULL,
       dispatch_deadline_at = NULL,
       run_deadline_at = NULL,
       reconciliation_owner = NULL,
       reconciliation_lease_expires_at = NULL,
       github_dispatch_identity = NULL,
       github_run_id = NULL,
       github_run_status = $3,
       github_run_conclusion = $4,
       github_status_observed_at = now(),
       terminal_error_code = $5,
       terminal_error_message = $6,
       terminal_at = now(),
       updated_at = now()
  FROM terminal_evidence
 WHERE job.id = terminal_evidence.orchestration_job_id
RETURNING job.id, job.status, job.commit_sha, job.terminal_error_code;
