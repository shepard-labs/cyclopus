-- Self-hosted GitHub import contract. The implementation migration must preserve
-- durability, target single-flight, latest-push coalescing, generation fencing,
-- leases/deadlines, dispatch ambiguity, immutable evidence, OCI resolution, and
-- deployment-state guarantees.
CREATE TABLE orchestration_jobs (
    id uuid PRIMARY KEY,
    org_id uuid NOT NULL,
    app_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    github_repository_id bigint NOT NULL,
    github_workflow_path text NOT NULL,
    workflow_revision text NOT NULL,
    dispatch_ref text NOT NULL,
    dispatch_ref_observed_sha text,
    desired_checkout_sha text NOT NULL,
    desired_generation bigint NOT NULL,
    idempotency_key text NOT NULL,
    pending_github_repository_id bigint,
    pending_github_workflow_path text,
    pending_workflow_revision text,
    pending_dispatch_ref text,
    pending_checkout_sha text,
    pending_desired_generation bigint,
    pending_idempotency_key text,
    status text NOT NULL CHECK (status IN (
        'queued', 'leased', 'dispatch_reconciling', 'dispatched',
        'resolving_image', 'deploying', 'completed', 'failed', 'blocked',
        'cancelled', 'superseded'
    )),
    lease_owner text,
    lease_expires_at timestamptz,
    dispatch_requested_at timestamptz,
    dispatch_ambiguity_state text NOT NULL DEFAULT 'none' CHECK (dispatch_ambiguity_state IN ('none', 'possible', 'matched', 'expired_no_match')),
    reconcile_after timestamptz,
    dispatch_deadline_at timestamptz,
    run_deadline_at timestamptz,
    reconciliation_owner text,
    reconciliation_lease_expires_at timestamptz,
    github_dispatch_identity text,
    github_run_id bigint,
    github_run_head_sha text,
    github_run_status text,
    github_run_conclusion text,
    github_status_observed_at timestamptz,
    cancellation_state text NOT NULL DEFAULT 'not_requested',
    cancellation_requested_at timestamptz,
    superseded_by_job_id uuid,
    superseded_at timestamptz,
    oci_resolution_state text NOT NULL DEFAULT 'pending' CHECK (oci_resolution_state IN ('pending', 'polling', 'resolved', 'failed', 'not_required')),
    image_tag_ref text,
    resolved_image_digest text,
    immutable_image_ref text,
    oci_media_type text,
    oci_resolved_at timestamptz,
    deployment_state text NOT NULL DEFAULT 'not_started' CHECK (deployment_state IN ('not_started', 'waiting_credential', 'ready', 'claimed', 'applying', 'live', 'failed', 'superseded', 'cancelled')),
    deployment_claim_owner text,
    deployment_claim_expires_at timestamptz,
    release_id uuid,
    terminal_error_code text,
    terminal_error_message text,
    terminal_at timestamptz,
    attempt integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, idempotency_key),
    UNIQUE (org_id, pending_idempotency_key),
    CHECK ((status = 'leased') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK ((pending_checkout_sha IS NULL) = (pending_github_repository_id IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_github_workflow_path IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_workflow_revision IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_dispatch_ref IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_desired_generation IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_idempotency_key IS NULL)),
    CHECK (status <> 'dispatch_reconciling' OR
           (dispatch_requested_at IS NOT NULL AND dispatch_ref_observed_sha IS NOT NULL AND reconcile_after IS NOT NULL AND dispatch_deadline_at IS NOT NULL AND dispatch_ambiguity_state IN ('possible', 'matched'))),
    CHECK (status <> 'dispatched' OR
           (github_run_id IS NOT NULL AND run_deadline_at IS NOT NULL)),
    CHECK ((reconciliation_owner IS NULL) = (reconciliation_lease_expires_at IS NULL)),
    CHECK ((deployment_claim_owner IS NULL) = (deployment_claim_expires_at IS NULL)),
    CHECK ((resolved_image_digest IS NULL) = (immutable_image_ref IS NULL)),
    CHECK (oci_resolution_state <> 'resolved' OR (image_tag_ref IS NOT NULL AND resolved_image_digest IS NOT NULL AND immutable_image_ref IS NOT NULL AND oci_media_type IS NOT NULL AND oci_resolved_at IS NOT NULL)),
    CHECK (status <> 'resolving_image' OR (github_run_status = 'completed' AND github_run_conclusion = 'success')),
    CHECK (status <> 'deploying' OR (oci_resolution_state = 'resolved' AND deployment_state IN ('ready', 'claimed', 'applying', 'live'))),
    CHECK (deployment_state NOT IN ('ready', 'claimed', 'applying', 'live') OR immutable_image_ref IS NOT NULL),
    CHECK (status <> 'completed' OR (oci_resolution_state = 'resolved' AND deployment_state = 'live' AND release_id IS NOT NULL)),
    CHECK ((status = 'superseded') = (superseded_at IS NOT NULL)),
    CHECK (cancellation_state = 'not_requested' OR cancellation_requested_at IS NOT NULL),
    CHECK (status NOT IN ('completed', 'failed', 'cancelled', 'superseded') OR terminal_at IS NOT NULL)
);

-- Immutable per-attempt evidence survives reuse of the job row when a pending
-- SHA is promoted after success or terminal failure.
CREATE TABLE orchestration_job_run_evidence (
    orchestration_job_id uuid NOT NULL REFERENCES orchestration_jobs(id),
    attempt integer NOT NULL,
    dispatch_ref text NOT NULL,
    dispatch_ref_observed_sha text,
    github_workflow_path text NOT NULL,
    workflow_revision text NOT NULL,
    dispatch_requested_at timestamptz,
    dispatch_ambiguity_state text NOT NULL,
    github_dispatch_identity text,
    github_run_id bigint,
    github_run_status text NOT NULL,
    github_run_conclusion text,
    observed_head_sha text,
    observed_at timestamptz NOT NULL,
    terminal_error_code text,
    terminal_error_message text,
    terminal_at timestamptz,
    PRIMARY KEY (orchestration_job_id, attempt, observed_at)
);

-- A BEFORE INSERT trigger copies the dispatch/workflow/correlation columns from
-- the live job row into every evidence row. Archival statements may supply them
-- explicitly, but older insert call sites cannot accidentally omit this proof.

-- Exactly one active build/deploy may own an organization/application/environment
-- target. This partial unique index is also the exact conflict arbiter for enqueue.
CREATE UNIQUE INDEX orchestration_jobs_scope_single_flight
    ON orchestration_jobs (org_id, app_id, environment_id)
    WHERE status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying');

CREATE INDEX orchestration_jobs_claimable
    ON orchestration_jobs (created_at, id)
    WHERE status IN ('queued', 'leased');

CREATE INDEX orchestration_jobs_reconciliation_due
    ON orchestration_jobs (reconcile_after, run_deadline_at, id)
    WHERE status IN ('dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying');

CREATE UNIQUE INDEX orchestration_jobs_github_run
    ON orchestration_jobs (github_run_id)
    WHERE github_run_id IS NOT NULL;

-- The implementation trigger uses the same generation rule as this contract:
-- current pending work must equal the target environment generation; an already-active
-- observed attempt may retain its captured generation; artifact/deployment
-- evidence, pending promotion, and completion require the current generation.
-- Terminal/cancellation transitions are intentionally exempt from that CAS.

-- The sole enqueue/coalescing primitive acquires the organization tombstone
-- advisory lock before target rows, validates active repository/workflow scope,
-- records durable request identity before advancing the environment generation,
-- and returns the complete resulting job projection plus request-specific
-- disposition without requiring a same-statement join to the inserted row.
-- Exact replays return the recorded result without advancing the generation;
-- conflicting reuse of a key is rejected. A first push creates fresh queued work;
-- every later active push preserves the captured attempt and replaces only pending.
-- The function has a migration-captured trusted search_path, uses explicitly
-- qualified catalog functions, and is not executable by PUBLIC. The owning
-- migration/application role retains EXECUTE; a deployment with separate roles
-- grants EXECUTE to its already-created application role outside this portable SQL.
SELECT * FROM enqueue_orchestration_push(
    $1, $2, $3, $4, $5, $6, $7, $8, $9
);

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
RETURNING job.id, job.idempotency_key, job.github_repository_id, job.github_workflow_path,
          job.workflow_revision, job.dispatch_ref, job.environment_id,
          job.desired_checkout_sha, job.desired_generation, job.lease_expires_at;

-- Before sending workflow_dispatch, durably enter reconciliation and release
-- the lease. This closes the crash window between the external side effect and
-- recording its unknown outcome. The request MUST include
-- inputs.orchestration_job_id = job.id and inputs.commit_sha = job.desired_checkout_sha.
-- The customer workflow MUST expose the job ID in run-name, checkout the exact
-- desired_checkout_sha, and attest that HEAD equals it before building. Correlation
-- records the run-name identity and observed head SHA separately from checkout proof.
-- Both a 2xx and
-- an ambiguous timeout remain in this state until a correlated run is observed.
-- The store, not its caller, establishes the exact five-minute observation deadline;
-- observation polls do not extend it forever and never redispatch. Parameters are
-- organization ID, job ID, lease owner, and the ref SHA observed before dispatch.
UPDATE orchestration_jobs
   SET status = 'dispatch_reconciling',
        dispatch_requested_at = now(),
        dispatch_ref_observed_sha = $4,
        dispatch_ambiguity_state = 'possible',
       reconcile_after = now() + interval '90 seconds',
       dispatch_deadline_at = now() + interval '300 seconds',
       lease_owner = NULL,
       lease_expires_at = NULL,
       updated_at = now()
 WHERE org_id = $1
   AND id = $2
   AND status = 'leased'
   AND lease_owner = $3
   AND lease_expires_at > now()
   AND $4 ~ '^[0-9a-f]{40}$';

-- Reconciliation polls workflow runs created since dispatch_requested_at and
-- matches display_title/run-name `cyclopus:<orchestration_job_id>`. Persist the
-- observed run before leaving dispatch reconciliation and establish the
-- exact store-owned 45-minute run deadline. Parameters are organization ID,
-- job ID, dispatch identity, run ID, run status, and observed run head SHA.
UPDATE orchestration_jobs
   SET status = 'dispatched',
       github_dispatch_identity = $3,
       github_run_id = $4,
       github_run_head_sha = $6,
        github_run_status = $5,
        github_run_conclusion = NULL,
        dispatch_ambiguity_state = 'matched',
       github_status_observed_at = now(),
       reconcile_after = now() + interval '30 seconds',
       run_deadline_at = now() + interval '2700 seconds',
       reconciliation_owner = NULL,
       reconciliation_lease_expires_at = NULL,
       updated_at = now()
 WHERE org_id = $1
   AND id = $2
   AND status = 'dispatch_reconciling'
   AND github_run_id IS NULL
   AND $6 ~ '^[0-9a-f]{40}$';

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
 WHERE org_id = $1
   AND id = $2
   AND status IN ('dispatch_reconciling', 'dispatched')
   AND CASE status
           WHEN 'dispatch_reconciling' THEN dispatch_deadline_at
           WHEN 'dispatched' THEN run_deadline_at
       END > now()
   AND reconciliation_owner = $3
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
 WHERE org_id = $1
   AND id = $2
   AND status = 'dispatch_reconciling'
   AND github_run_id IS NULL
   AND reconcile_after <= now()
   AND dispatch_deadline_at > now()
   AND reconciliation_owner = $3
   AND reconciliation_lease_expires_at > now();

-- Persist every observed nonterminal GitHub status before scheduling the next
-- poll. This statement is used by startup and periodic reconciliation.
WITH evidence AS (
    INSERT INTO orchestration_job_run_evidence (
        orchestration_job_id, attempt, dispatch_ref, dispatch_ref_observed_sha,
        github_workflow_path, workflow_revision, dispatch_requested_at,
        dispatch_ambiguity_state, github_dispatch_identity, github_run_id,
        github_run_status, github_run_conclusion, observed_head_sha, observed_at
    )
	SELECT id, attempt, dispatch_ref, dispatch_ref_observed_sha, github_workflow_path,
           workflow_revision, dispatch_requested_at, dispatch_ambiguity_state,
           github_dispatch_identity, github_run_id, $4, $5, github_run_head_sha, now()
      FROM orchestration_jobs
     WHERE org_id = $1
       AND id = $2
       AND status = 'dispatched'
       AND reconciliation_owner = $3
       AND reconciliation_lease_expires_at > now()
    RETURNING orchestration_job_id
)
UPDATE orchestration_jobs AS job
   SET github_run_status = $4,
       github_run_conclusion = $5,
       github_status_observed_at = now(),
       reconcile_after = now() + interval '30 seconds',
       reconciliation_owner = NULL,
       reconciliation_lease_expires_at = NULL,
       updated_at = now()
  FROM evidence
 WHERE job.id = evidence.orchestration_job_id
   AND $4 IN ('queued', 'in_progress');

-- Resolve success or terminal failure in one transaction. Callers use
-- github_run_missing/github_run_ambiguous after dispatch_deadline_at,
-- github_run_timed_out after run_deadline_at, and GitHub conclusions for the matching
-- GitHub conclusions. Immutable evidence is inserted first, then the latest
-- pending SHA is atomically promoted to queued in the same statement. Without
-- pending work, a successful GitHub run enters OCI resolution; it never skips
-- immutable-image verification or deployment and therefore cannot complete here.
-- A missing run is valid only with github_run_missing after the hard dispatch
-- deadline. A pending or later push is the retry; reconciliation never is.
WITH terminal_evidence AS (
    INSERT INTO orchestration_job_run_evidence (
        orchestration_job_id, attempt, dispatch_ref, dispatch_ref_observed_sha,
        github_workflow_path, workflow_revision, dispatch_requested_at,
        dispatch_ambiguity_state, github_dispatch_identity, github_run_id,
		github_run_status, github_run_conclusion, observed_head_sha, observed_at, terminal_error_code,
        terminal_error_message, terminal_at
    )
	SELECT id, attempt, dispatch_ref, dispatch_ref_observed_sha, github_workflow_path,
           workflow_revision, dispatch_requested_at, dispatch_ambiguity_state,
           github_dispatch_identity, github_run_id, $4, $5, github_run_head_sha,
           now(), $6, $7, now()
      FROM orchestration_jobs
     WHERE org_id = $1
       AND id = $2
       AND status IN ('dispatch_reconciling', 'dispatched')
        AND reconciliation_owner = $3
        AND reconciliation_lease_expires_at > now()
        AND (pending_desired_generation IS NULL OR
            pending_desired_generation = (SELECT desired_generation FROM environments WHERE org_id = $1 AND id = orchestration_jobs.environment_id))
        AND (
            (status = 'dispatched' AND github_run_id IS NOT NULL
             AND $4 = 'completed'
             AND $5 IN ('success', 'failure', 'cancelled', 'timed_out', 'action_required', 'neutral', 'skipped', 'stale')
             AND $6 IS NULL)
            OR (status = 'dispatch_reconciling' AND github_run_id IS NULL
                AND dispatch_deadline_at <= now() AND $4 = 'completed' AND $5 = ''
                AND $6 IN ('github_run_missing', 'github_run_ambiguous'))
            OR (status = 'dispatched' AND github_run_id IS NOT NULL
                AND run_deadline_at <= now() AND $4 IN ('queued', 'in_progress')
                AND $5 = '' AND $6 = 'github_run_timed_out')
        )
    RETURNING orchestration_job_id
)
UPDATE orchestration_jobs AS job
    SET status = CASE
            WHEN pending_checkout_sha IS NOT NULL THEN 'queued'
            WHEN $5 = 'success' THEN 'resolving_image'
           ELSE 'failed'
       END,
         github_repository_id = COALESCE(pending_github_repository_id, github_repository_id),
         github_workflow_path = COALESCE(pending_github_workflow_path, github_workflow_path),
         workflow_revision = COALESCE(pending_workflow_revision, workflow_revision),
         dispatch_ref = COALESCE(pending_dispatch_ref, dispatch_ref),
         desired_checkout_sha = COALESCE(pending_checkout_sha, desired_checkout_sha),
        desired_generation = COALESCE(pending_desired_generation, desired_generation),
       idempotency_key = COALESCE(pending_idempotency_key, idempotency_key),
         pending_github_repository_id = NULL,
         pending_github_workflow_path = NULL,
         pending_workflow_revision = NULL,
         pending_dispatch_ref = NULL,
         pending_checkout_sha = NULL,
        pending_desired_generation = NULL,
       pending_idempotency_key = NULL,
        lease_owner = NULL,
        lease_expires_at = NULL,
        dispatch_requested_at = NULL,
        dispatch_ref_observed_sha = NULL,
        dispatch_ambiguity_state = CASE WHEN pending_checkout_sha IS NOT NULL THEN 'none' ELSE dispatch_ambiguity_state END,
        reconcile_after = NULL,
        dispatch_deadline_at = NULL,
        run_deadline_at = NULL,
        reconciliation_owner = NULL,
        reconciliation_lease_expires_at = NULL,
        github_dispatch_identity = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE github_dispatch_identity END,
        github_run_id = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE github_run_id END,
        github_run_head_sha = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE github_run_head_sha END,
        github_run_status = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE $4 END,
        github_run_conclusion = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE NULLIF($5, '') END,
        github_status_observed_at = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE now() END,
        cancellation_state = CASE WHEN pending_checkout_sha IS NOT NULL THEN 'not_requested' ELSE cancellation_state END,
        cancellation_requested_at = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE cancellation_requested_at END,
        superseded_by_job_id = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE superseded_by_job_id END,
        superseded_at = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE superseded_at END,
        oci_resolution_state = CASE WHEN pending_checkout_sha IS NOT NULL THEN 'pending' ELSE oci_resolution_state END,
        image_tag_ref = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE image_tag_ref END,
        resolved_image_digest = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE resolved_image_digest END,
        immutable_image_ref = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE immutable_image_ref END,
        oci_media_type = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE oci_media_type END,
        oci_resolved_at = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE oci_resolved_at END,
        deployment_state = CASE WHEN pending_checkout_sha IS NOT NULL THEN 'not_started' ELSE deployment_state END,
        deployment_claim_owner = NULL,
        deployment_claim_expires_at = NULL,
        release_id = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE release_id END,
        terminal_error_code = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE $6 END,
        terminal_error_message = CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE $7 END,
        terminal_at = CASE WHEN pending_checkout_sha IS NULL AND $5 <> 'success' THEN now() ELSE NULL END,
        attempt = attempt + CASE WHEN pending_checkout_sha IS NOT NULL THEN 1 ELSE 0 END,
       updated_at = now()
  FROM terminal_evidence
 WHERE job.id = terminal_evidence.orchestration_job_id
RETURNING job.id, job.status, job.desired_checkout_sha, job.desired_generation, job.terminal_error_code;

-- Resolve the successful build tag to the exact immutable digest. The trigger
-- enforces image_tag_ref = app.image_repository:desired_checkout_sha and
-- immutable_image_ref = app.image_repository@resolved_image_digest.
UPDATE orchestration_jobs
   SET image_tag_ref = $2,
       resolved_image_digest = $3,
       immutable_image_ref = $4,
       oci_media_type = $5,
       oci_resolved_at = now(),
       oci_resolution_state = 'resolved',
       deployment_state = 'ready',
       status = 'deploying',
       updated_at = now()
 WHERE org_id = $1
   AND id = $6
   AND status = 'resolving_image'
   AND github_run_status = 'completed'
   AND github_run_conclusion = 'success'
   AND desired_generation = (SELECT desired_generation FROM environments WHERE org_id = $1 AND id = orchestration_jobs.environment_id);

-- Bind deployment to a release carrying the same immutable image and commit.
UPDATE orchestration_jobs AS job
   SET release_id = release.id,
       deployment_state = 'applying',
       updated_at = now()
  FROM releases release
 WHERE job.org_id = $1
   AND job.id = $2
   AND job.status = 'deploying'
    AND job.deployment_state IN ('ready', 'claimed')
    AND job.desired_generation = (SELECT desired_generation FROM environments WHERE org_id = job.org_id AND id = job.environment_id)
   AND release.org_id = job.org_id
   AND release.environment_id = job.environment_id
   AND release.image_ref = job.immutable_image_ref
   AND release.git_commit_sha = job.desired_checkout_sha
   AND release.status = 'deploying';

-- Completion is legal only after the matching release is live. The schema
-- trigger re-verifies repository, digest, commit, environment, and release state.
UPDATE orchestration_jobs AS job
   SET status = 'completed',
       deployment_state = 'live',
       terminal_at = now(),
       updated_at = now()
  FROM releases release
 WHERE job.org_id = $1
   AND job.id = $2
   AND job.status = 'deploying'
    AND job.release_id = release.id
    AND job.desired_generation = (SELECT desired_generation FROM environments WHERE org_id = job.org_id AND id = job.environment_id)
   AND release.org_id = job.org_id
   AND release.environment_id = job.environment_id
   AND release.image_ref = job.immutable_image_ref
   AND release.git_commit_sha = job.desired_checkout_sha
    AND release.status = 'live';

-- Scope teardown, disconnection, or generation-fence advancement may cancel any
-- active attempt even after its captured generation becomes stale. This path
-- records no artifact or deployment success and deliberately has no generation CAS.
UPDATE orchestration_jobs
   SET status = 'cancelled',
       pending_github_repository_id = NULL,
       pending_github_workflow_path = NULL,
       pending_workflow_revision = NULL,
       pending_dispatch_ref = NULL,
       pending_checkout_sha = NULL,
       pending_desired_generation = NULL,
       pending_idempotency_key = NULL,
       lease_owner = NULL,
       lease_expires_at = NULL,
       reconcile_after = NULL,
       reconciliation_owner = NULL,
       reconciliation_lease_expires_at = NULL,
       deployment_claim_owner = NULL,
       deployment_claim_expires_at = NULL,
       cancellation_state = 'requested',
       cancellation_requested_at = COALESCE(cancellation_requested_at, now()),
       deployment_state = CASE WHEN deployment_state IN ('ready', 'claimed', 'applying') THEN 'cancelled' ELSE deployment_state END,
       terminal_error_code = $3,
       terminal_error_message = $4,
       terminal_at = now(),
       updated_at = now()
 WHERE org_id = $1
   AND environment_id = $2
    AND status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying');

-- enforce_tombstone_readiness() is replaced after the epoch schema exists.
-- Organization/project/app tombstones advance affected environment fences,
-- cancel all six active orchestration states, clear orchestration,
-- reconciliation, deployment, and environment deploy claims, and retain the
-- immutable evidence above. It references no alpha pending SHA columns.
