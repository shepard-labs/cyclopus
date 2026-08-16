\set ON_ERROR_STOP on

DO $$
BEGIN
    IF current_setting('server_version_num')::integer < 180000
       OR current_setting('server_version_num')::integer >= 190000 THEN
        RAISE EXCEPTION 'PostgreSQL 18 required';
    END IF;
    IF (SELECT epoch FROM schema_epoch) <> 'self_hosted_github_import_v1'
       OR (SELECT fingerprint FROM schema_epoch) <> '95e56712029f59eb49b6b0bd939cfbb2dc62360772731fabec7bf2cb2d7d290e' THEN
        RAISE EXCEPTION 'unexpected schema epoch';
    END IF;
END;
$$;

-- Keep the shell concurrency/idempotency/advisory-lock postlude independent
-- from the organization intentionally tombstoned below.
INSERT INTO orgs (name, slug) VALUES ('Postlude Schema Org', 'postlude-schema-org') RETURNING id AS postlude_org_id \gset
INSERT INTO github_installations (
    github_installation_id, org_id, account_id, account_login, account_type, repository_selection,
    last_api_confirmed_generation
) VALUES (502, :'postlude_org_id', 602, 'postlude-acme', 'Organization', 'selected', 1);
INSERT INTO github_repositories (
    github_repository_id, org_id, github_installation_id, node_id, owner_login,
    name, full_name, visibility, default_branch, last_api_confirmed_generation
) VALUES (703, :'postlude_org_id', 502, 'R_schema_postlude', 'postlude-acme',
    'schema-postlude', 'postlude-acme/schema-postlude', 'private', 'main', 1);
INSERT INTO git_connections (org_id, github_repository_id)
VALUES (:'postlude_org_id', 703) RETURNING id AS postlude_connection_id \gset
INSERT INTO projects (org_id, name, slug, git_connection_id)
VALUES (:'postlude_org_id', 'Postlude Schema Project', 'postlude-schema-project', :'postlude_connection_id')
RETURNING id AS postlude_project_id \gset
INSERT INTO apps (
    org_id, project_id, name, slug, hostname_label, image_repository,
    github_workflow_path, production_branch, image_visibility
) VALUES (
    :'postlude_org_id', :'postlude_project_id', 'Postlude Schema App', 'postlude-schema-app',
    'postlude-schema-app', 'ghcr.io/postlude-acme/schema-postlude',
    '.github/workflows/deploy.yml', 'main', 'public'
) RETURNING id AS postlude_app_id \gset
INSERT INTO environments (
    org_id, app_id, branch, kind, namespace
) VALUES (
    :'postlude_org_id', :'postlude_app_id', 'main', 'production', 'postlude-schema-production'
);

DO $$
DECLARE
    tombstone_definition text := pg_get_functiondef('enforce_tombstone_readiness()'::regprocedure);
BEGIN
    IF tombstone_definition NOT LIKE '%TG_TABLE_NAME = ''orgs''%'
       OR tombstone_definition NOT LIKE '%TG_TABLE_NAME = ''projects''%'
       OR tombstone_definition NOT LIKE '%resolving_image%'
       OR tombstone_definition NOT LIKE '%deploying%'
       OR tombstone_definition NOT LIKE '%deployment_claim_owner = NULL%'
       OR tombstone_definition NOT LIKE '%in_progress_release_id = NULL%'
       OR tombstone_definition LIKE '%pending_commit_sha%' THEN
        RAISE EXCEPTION 'replacement tombstone trigger does not cover org/project/app post-dispatch cleanup';
    END IF;
END;
$$;

INSERT INTO orgs (name, slug) VALUES ('Schema Org', 'schema-org') RETURNING id AS org_id \gset
INSERT INTO users (org_id, email, password_hash, operator)
VALUES (:'org_id', 'owner@example.test', 'test-hash', true) RETURNING id AS user_id \gset
SELECT set_config('cyclopus.org_id', :'org_id', false);
SELECT set_config('cyclopus.user_id', :'user_id', false);

INSERT INTO github_setup_sessions (
    state_hash, flow_kind, initiating_user_id, initiating_session_hash, expires_at
) VALUES (repeat('a', 64), 'app_setup', :'user_id', repeat('b', 64), now() + interval '15 minutes')
RETURNING id AS setup_id \gset

DO $$
BEGIN
    BEGIN
        INSERT INTO github_setup_sessions (state_hash, flow_kind, initiating_user_id, initiating_session_hash, expires_at)
        VALUES (repeat('c', 64), 'app_setup', current_setting('cyclopus.user_id')::uuid, repeat('d', 64), now() + interval '15 minutes');
        RAISE EXCEPTION 'second active app setup succeeded';
    EXCEPTION WHEN unique_violation THEN NULL;
    END;
END;
$$;

UPDATE github_setup_sessions SET status = 'consumed', consumed_at = now() WHERE id = :'setup_id';
INSERT INTO github_app (
    github_app_id, app_slug, app_name, app_url, owner_id, owner_login, owner_type,
    credential_secret_ref, state, checkpoint, setup_session_id, ready_at
) VALUES (
    7001, 'cyclopus-schema', 'Cyclopus Schema', 'https://github.com/apps/cyclopus-schema',
    9001, 'schema-owner', 'Organization', 'k8s-secret://cyclopus/github-app#credentials',
    'ready', 'ready_committed', :'setup_id', now()
);

INSERT INTO github_installations (
    github_installation_id, org_id, account_id, account_login, account_type, repository_selection,
    last_api_confirmed_generation
) VALUES (501, :'org_id', 601, 'acme', 'Organization', 'selected', 1);
INSERT INTO github_repositories (
    github_repository_id, org_id, github_installation_id, node_id, owner_login,
    name, full_name, visibility, default_branch, last_api_confirmed_generation
) VALUES (701, :'org_id', 501, 'R_schema', 'acme', 'schema', 'acme/schema', 'private', 'main', 1);
INSERT INTO git_connections (org_id, github_repository_id)
VALUES (:'org_id', 701) RETURNING id AS connection_id \gset
SELECT set_config('cyclopus.connection_id', :'connection_id', false);

DO $$
BEGIN
    BEGIN
        UPDATE github_installations SET org_id = uuidv7() WHERE github_installation_id = 501;
        RAISE EXCEPTION 'installation claim rebound';
    EXCEPTION WHEN integrity_constraint_violation THEN NULL;
    END;
    BEGIN
        DELETE FROM github_installations WHERE github_installation_id = 501;
        RAISE EXCEPTION 'installation tombstone deleted';
    EXCEPTION WHEN integrity_constraint_violation THEN NULL;
    END;
    BEGIN
        INSERT INTO git_connections (org_id, github_repository_id) VALUES (current_setting('cyclopus.org_id')::uuid, 701);
        RAISE EXCEPTION 'duplicate repository connection succeeded';
    EXCEPTION WHEN unique_violation THEN NULL;
    END;
END;
$$;

INSERT INTO orgs (name, slug) VALUES ('Other Schema Org', 'other-schema-org') RETURNING id AS other_org_id \gset
SELECT set_config('cyclopus.other_org_id', :'other_org_id', false);
DO $$
BEGIN
    BEGIN
        INSERT INTO github_setup_sessions (
            state_hash, flow_kind, initiating_user_id, initiating_session_hash, org_id,
            installation_purpose, expires_at
        ) VALUES (
            repeat('e', 64), 'installation', current_setting('cyclopus.user_id')::uuid,
            repeat('f', 64), current_setting('cyclopus.other_org_id')::uuid,
            'initial_setup', now() + interval '15 minutes'
        );
        RAISE EXCEPTION 'installation setup session crossed organization boundary';
    EXCEPTION WHEN foreign_key_violation THEN NULL;
    END;
END;
$$;

INSERT INTO registries (
    org_id, active_generation, active_credential_secret_ref, state, checkpoint
) VALUES (:'org_id', 1, 'k8s-secret://cyclopus/ghcr-pull#dockerconfigjson', 'ready', 'activated')
RETURNING id AS registry_id \gset
SELECT set_config('cyclopus.registry_id', :'registry_id', false);

DO $$
BEGIN
    BEGIN
        INSERT INTO registries (org_id) VALUES (current_setting('cyclopus.org_id')::uuid);
        RAISE EXCEPTION 'second GHCR credential succeeded';
    EXCEPTION WHEN unique_violation THEN NULL;
    END;
END;
$$;

INSERT INTO projects (org_id, name, slug, git_connection_id)
VALUES (:'org_id', 'Schema Project', 'schema-project', :'connection_id') RETURNING id AS project_id \gset
SELECT set_config('cyclopus.project_id', :'project_id', false);
INSERT INTO apps (
    org_id, project_id, name, slug, hostname_label, registry_id, image_repository,
    github_workflow_path, production_branch, image_visibility
) VALUES (
    :'org_id', :'project_id', 'Schema App', 'schema-app', 'schema-app', :'registry_id',
    'ghcr.io/acme/schema', '.github/workflows/deploy.yml', 'main', 'private'
) RETURNING id AS app_id \gset
SELECT set_config('cyclopus.app_id', :'app_id', false);
INSERT INTO environments (
    org_id, app_id, branch, kind, namespace, required_registry_credential_generation,
    ready_registry_credential_generation, registry_credential_state, registry_credential_checkpoint
) VALUES (
    :'org_id', :'app_id', 'main', 'production', 'schema-production', 1, 1, 'ready', 'namespace_secret_verified'
) RETURNING id AS production_environment_id \gset
SELECT set_config('cyclopus.production_environment_id', :'production_environment_id', false);

CREATE FUNCTION seed_active_orchestration_states(target_app uuid, key_prefix text) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    active_state text;
    target_environment uuid;
    target_job uuid;
    checkout_sha text;
BEGIN
    FOREACH active_state IN ARRAY ARRAY['queued','leased','dispatch_reconciling','dispatched','resolving_image','deploying'] LOOP
        checkout_sha := substr(md5(key_prefix || active_state) || md5(active_state || key_prefix), 1, 40);
        INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
        SELECT app.org_id, app.id, 'fixture/' || key_prefix || '/' || active_state, 'preview',
            substr('fixture-' || md5(key_prefix || active_state), 1, 63),
            substr('fixture-' || md5(active_state || key_prefix), 1, 63),
            substr(md5(key_prefix || active_state), 1, 24) || '.example.test'
        FROM apps app WHERE app.id = target_app
        RETURNING id INTO target_environment;
        INSERT INTO orchestration_jobs (org_id, app_id, environment_id, github_repository_id,
            github_workflow_path, workflow_revision, dispatch_ref, desired_checkout_sha,
            desired_generation, idempotency_key)
        SELECT app.org_id, app.id, target_environment, connection.github_repository_id,
            app.github_workflow_path, repeat('4', 40), 'refs/heads/fixture/' || active_state,
            checkout_sha, 1, key_prefix || '-' || active_state
        FROM apps app
        JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
        JOIN git_connections connection ON connection.org_id=project.org_id AND connection.id=project.git_connection_id
        WHERE app.id=target_app RETURNING id INTO target_job;
        IF active_state = 'leased' THEN
            UPDATE orchestration_jobs SET status='leased',lease_owner='fixture-worker',
                lease_expires_at=now()+interval '5 minutes' WHERE id=target_job;
        ELSIF active_state = 'dispatch_reconciling' THEN
            UPDATE orchestration_jobs SET status='dispatch_reconciling',attempt=1,
                dispatch_requested_at=now(),dispatch_ref_observed_sha=checkout_sha,
                dispatch_ambiguity_state='possible',reconcile_after=now()+interval '30 seconds',
                dispatch_deadline_at=now()+interval '5 minutes' WHERE id=target_job;
        ELSIF active_state = 'dispatched' THEN
            UPDATE orchestration_jobs SET status='dispatched',attempt=1,
                dispatch_requested_at=now(),dispatch_ref_observed_sha=checkout_sha,
                dispatch_ambiguity_state='matched',github_dispatch_identity='cyclopus:'||id,
                github_run_id=abs(hashtextextended(key_prefix || active_state, 0)),github_run_status='in_progress',
                run_deadline_at=now()+interval '45 minutes' WHERE id=target_job;
        ELSIF active_state = 'resolving_image' THEN
            UPDATE orchestration_jobs SET status='resolving_image',attempt=1,
                github_run_status='completed',github_run_conclusion='success',github_run_head_sha=checkout_sha
                WHERE id=target_job;
        ELSIF active_state = 'deploying' THEN
            UPDATE orchestration_jobs SET status='deploying',attempt=1,
                github_run_status='completed',github_run_conclusion='success',github_run_head_sha=checkout_sha,
                image_tag_ref=app.image_repository || ':' || checkout_sha,
                resolved_image_digest='sha256:' || repeat('9',64),
                immutable_image_ref=app.image_repository || '@sha256:' || repeat('9',64),
                oci_media_type='application/vnd.oci.image.manifest.v1+json',
                oci_resolution_state='resolved',oci_resolved_at=now(),deployment_state='ready'
            FROM apps app WHERE orchestration_jobs.id=target_job AND app.id=target_app;
        END IF;
    END LOOP;
END;
$$;

-- Rapid pushes may leave the observed primary attempt on its captured generation
-- while coalescing exactly-current pending work. Promotion is a fresh attempt.
SELECT orchestration_job_id AS rapid_job_id
FROM enqueue_orchestration_push(
    :'org_id', :'app_id', :'production_environment_id', 701,
    '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main',
    repeat('b', 40), 'rapid-primary'
) \gset
SELECT set_config('rapid.job_id', :'rapid_job_id', false);
SELECT accepted_generation AS replay_generation, enqueue_result AS replay_result
FROM enqueue_orchestration_push(
    :'org_id', :'app_id', :'production_environment_id', 701,
    '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main',
    repeat('b', 40), 'rapid-primary'
) \gset
SELECT set_config('rapid.replay_generation', :'replay_generation', false);
SELECT set_config('rapid.replay_result', :'replay_result', false);
DO $$ BEGIN
    IF current_setting('rapid.replay_generation')::bigint <> 2
       OR current_setting('rapid.replay_result') <> 'queued'
        OR NOT EXISTS (SELECT 1 FROM environments environment
           WHERE environment.id = current_setting('cyclopus.production_environment_id')::uuid
             AND environment.desired_generation = 2) THEN
        RAISE EXCEPTION 'exact enqueue replay advanced generation or changed its stable result';
    END IF;
    BEGIN
        PERFORM * FROM enqueue_orchestration_push(
            current_setting('cyclopus.org_id')::uuid,
            current_setting('cyclopus.app_id')::uuid,
            current_setting('cyclopus.production_environment_id')::uuid, 701,
            '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main',
            repeat('f', 40), 'rapid-primary');
        RAISE EXCEPTION 'conflicting orchestration idempotency request succeeded';
    EXCEPTION WHEN unique_violation THEN NULL;
    END;
END $$;
UPDATE orchestration_jobs SET status = 'leased', lease_owner = 'rapid-worker',
    lease_expires_at = now() + interval '2 minutes', attempt = 1 WHERE id = :'rapid_job_id';
UPDATE orchestration_jobs SET status = 'dispatch_reconciling', lease_owner = NULL, lease_expires_at = NULL,
    dispatch_requested_at = now() - interval '2 minutes', dispatch_ref_observed_sha = repeat('b', 40),
    dispatch_ambiguity_state = 'possible', reconcile_after = now(), dispatch_deadline_at = now() + interval '3 minutes'
WHERE id = :'rapid_job_id';
UPDATE orchestration_jobs SET status = 'dispatched', github_dispatch_identity = 'cyclopus:' || id,
    github_run_id = 8101, github_run_head_sha = repeat('b', 40), github_run_status = 'in_progress',
    dispatch_ambiguity_state = 'matched', github_status_observed_at = now(),
    run_deadline_at = now() + interval '30 minutes', reconciliation_owner = 'old-owner',
    reconciliation_lease_expires_at = now() - interval '1 second' WHERE id = :'rapid_job_id';
UPDATE apps SET github_workflow_path = '.github/workflows/release.yaml' WHERE id = :'app_id';
SELECT enqueue_result AS pending_result, accepted_generation AS pending_result_generation
FROM enqueue_orchestration_push(
    :'org_id', :'app_id', :'production_environment_id', 701,
    '.github/workflows/release.yaml', repeat('5', 40), 'refs/tags/v2',
    repeat('c', 40), 'rapid-pending'
) \gset
SELECT set_config('rapid.pending_result', :'pending_result', false);
SELECT set_config('rapid.pending_result_generation', :'pending_result_generation', false);
DO $$ BEGIN
    IF current_setting('rapid.pending_result') <> 'pending'
       OR current_setting('rapid.pending_result_generation')::bigint <> 3 THEN
        RAISE EXCEPTION 'active enqueue did not report its stable pending result';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM environments environment
        WHERE environment.id = current_setting('cyclopus.production_environment_id')::uuid
          AND environment.desired_generation = 3) THEN
        RAISE EXCEPTION 'serial pushes did not atomically advance the target environment generation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE id = current_setting('rapid.job_id')::uuid
        AND desired_generation = 2 AND pending_desired_generation = 3
        AND desired_checkout_sha = repeat('b', 40) AND pending_checkout_sha = repeat('c', 40)
        AND pending_github_repository_id = 701
        AND github_workflow_path = '.github/workflows/deploy.yml'
        AND workflow_revision = repeat('4', 40) AND dispatch_ref = 'refs/heads/main'
        AND pending_github_workflow_path = '.github/workflows/release.yaml'
        AND pending_workflow_revision = repeat('5', 40) AND pending_dispatch_ref = 'refs/tags/v2') THEN
        RAISE EXCEPTION 'rapid coalescing did not retain old primary/current pending generation';
    END IF;
END $$;

WITH rejected AS (
    SELECT id FROM orchestration_jobs WHERE id = :'rapid_job_id' AND status = 'dispatched'
      AND reconciliation_owner = 'old-owner' AND reconciliation_lease_expires_at > now()
)
SELECT count(*) AS expired_resolution_count FROM rejected \gset
SELECT set_config('rapid.expired_resolution_count', :'expired_resolution_count', false);
DO $$ BEGIN
    IF current_setting('rapid.expired_resolution_count')::integer <> 0 THEN RAISE EXCEPTION 'expired lease resolved'; END IF;
END $$;

UPDATE orchestration_jobs SET reconciliation_owner = 'rapid-owner',
    reconciliation_lease_expires_at = now() + interval '1 minute' WHERE id = :'rapid_job_id';
WITH rejected AS (
    SELECT id FROM orchestration_jobs WHERE id = :'rapid_job_id' AND status = 'dispatched'
      AND reconciliation_owner = 'stale-owner' AND reconciliation_lease_expires_at > now()
)
SELECT count(*) AS stale_owner_resolution_count FROM rejected \gset
SELECT set_config('rapid.stale_owner_resolution_count', :'stale_owner_resolution_count', false);
DO $$ BEGIN
    IF current_setting('rapid.stale_owner_resolution_count')::integer <> 0 THEN RAISE EXCEPTION 'stale owner resolved'; END IF;
END $$;
WITH terminal_evidence AS (
    INSERT INTO orchestration_job_run_evidence (
        orchestration_job_id, attempt, dispatch_ref, dispatch_ref_observed_sha,
        github_workflow_path, workflow_revision, dispatch_requested_at,
        dispatch_ambiguity_state, github_dispatch_identity, github_run_id,
        github_run_status, github_run_conclusion, observed_head_sha, observed_at, terminal_at
    )
    SELECT id, attempt, dispatch_ref, dispatch_ref_observed_sha, github_workflow_path,
        workflow_revision, dispatch_requested_at, dispatch_ambiguity_state,
        github_dispatch_identity, github_run_id, 'completed', 'success', github_run_head_sha, now(), now()
    FROM orchestration_jobs WHERE id = :'rapid_job_id' AND status = 'dispatched'
      AND github_run_id IS NOT NULL AND reconciliation_owner = 'rapid-owner'
      AND reconciliation_lease_expires_at > now() AND pending_desired_generation = 3
    RETURNING orchestration_job_id
)
UPDATE orchestration_jobs AS job SET status = 'queued', desired_checkout_sha = pending_checkout_sha,
    desired_generation = pending_desired_generation, idempotency_key = pending_idempotency_key,
    pending_github_repository_id = NULL, pending_github_workflow_path = NULL,
    pending_workflow_revision = NULL, pending_dispatch_ref = NULL,
    pending_checkout_sha = NULL, pending_desired_generation = NULL, pending_idempotency_key = NULL,
    lease_owner = NULL, lease_expires_at = NULL, dispatch_requested_at = NULL,
    dispatch_ref_observed_sha = NULL, dispatch_ambiguity_state = 'none', reconcile_after = NULL,
    dispatch_deadline_at = NULL, run_deadline_at = NULL, reconciliation_owner = NULL,
    reconciliation_lease_expires_at = NULL, github_dispatch_identity = NULL, github_run_id = NULL,
    github_run_head_sha = NULL, github_run_status = NULL, github_run_conclusion = NULL,
    github_status_observed_at = NULL, cancellation_state = 'not_requested', cancellation_requested_at = NULL,
    superseded_by_job_id = NULL, superseded_at = NULL, oci_resolution_state = 'pending', image_tag_ref = NULL,
    resolved_image_digest = NULL, immutable_image_ref = NULL, oci_media_type = NULL, oci_resolved_at = NULL,
    deployment_state = 'not_started', deployment_claim_owner = NULL, deployment_claim_expires_at = NULL,
    release_id = NULL, terminal_error_code = NULL, terminal_error_message = NULL, terminal_at = NULL,
    attempt = attempt + 1, updated_at = now()
FROM terminal_evidence WHERE job.id = terminal_evidence.orchestration_job_id;
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE id = current_setting('rapid.job_id')::uuid
        AND status = 'queued' AND attempt = 2 AND desired_generation = 3
        AND desired_checkout_sha = repeat('c', 40) AND idempotency_key = 'rapid-pending'
        AND github_workflow_path = '.github/workflows/release.yaml'
        AND workflow_revision = repeat('5', 40) AND dispatch_ref = 'refs/tags/v2'
        AND pending_checkout_sha IS NULL AND dispatch_requested_at IS NULL
        AND dispatch_ref_observed_sha IS NULL AND dispatch_ambiguity_state = 'none'
        AND dispatch_deadline_at IS NULL AND run_deadline_at IS NULL AND github_run_id IS NULL
        AND github_run_status IS NULL AND terminal_at IS NULL AND oci_resolution_state = 'pending'
        AND deployment_state = 'not_started' AND release_id IS NULL) THEN
        RAISE EXCEPTION 'promoted attempt was not fully reset';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM orchestration_job_run_evidence
        WHERE orchestration_job_id = current_setting('rapid.job_id')::uuid AND attempt = 1
          AND dispatch_ref = 'refs/heads/main' AND dispatch_ref_observed_sha = repeat('b', 40)
          AND github_workflow_path = '.github/workflows/deploy.yml'
          AND workflow_revision = repeat('4', 40) AND dispatch_requested_at IS NOT NULL
          AND dispatch_ambiguity_state = 'matched'
          AND github_dispatch_identity = 'cyclopus:' || current_setting('rapid.job_id')
          AND github_run_id = 8101 AND observed_head_sha = repeat('b', 40)
          AND github_run_status = 'completed' AND github_run_conclusion = 'success') THEN
        RAISE EXCEPTION 'promoted attempt did not archive terminal evidence';
    END IF;
END $$;
UPDATE orchestration_jobs SET status = 'leased', lease_owner = 'fresh-worker',
    lease_expires_at = now() + interval '2 minutes' WHERE id = :'rapid_job_id';
UPDATE orchestration_jobs SET status = 'dispatch_reconciling', dispatch_requested_at = now(),
    dispatch_ref_observed_sha = repeat('c', 40), dispatch_ambiguity_state = 'possible',
    reconcile_after = now() + interval '90 seconds', dispatch_deadline_at = now() + interval '300 seconds',
    lease_owner = NULL, lease_expires_at = NULL WHERE id = :'rapid_job_id';
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE id = current_setting('rapid.job_id')::uuid
        AND dispatch_deadline_at > now() + interval '4 minutes') THEN
        RAISE EXCEPTION 'dispatch did not establish a fresh deadline';
    END IF;
END $$;
UPDATE environments SET desired_generation = 4 WHERE id = :'production_environment_id';
DO $$ BEGIN
    BEGIN
        UPDATE orchestration_jobs SET image_tag_ref = 'ghcr.io/acme/schema:' || repeat('c', 40)
        WHERE id = current_setting('rapid.job_id')::uuid;
        RAISE EXCEPTION 'stale generation recorded artifact evidence';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
END $$;
UPDATE orchestration_jobs SET status = 'cancelled', pending_checkout_sha = NULL,
    pending_github_repository_id = NULL, pending_github_workflow_path = NULL,
    pending_workflow_revision = NULL, pending_dispatch_ref = NULL,
    pending_desired_generation = NULL, pending_idempotency_key = NULL,
    reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL,
    cancellation_state = 'requested', cancellation_requested_at = now(),
    terminal_error_code = 'environment_teardown_requested',
    terminal_error_message = 'teardown cancelled active stale attempt', terminal_at = now()
WHERE id = :'rapid_job_id' AND status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying');
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE id = current_setting('rapid.job_id')::uuid
        AND status = 'cancelled' AND desired_generation = 3 AND terminal_at IS NOT NULL) THEN
        RAISE EXCEPTION 'teardown could not cancel stale-generation active job';
    END IF;
END $$;

DO $$
DECLARE
    definition text := pg_get_functiondef('enqueue_orchestration_push(uuid,uuid,uuid,bigint,text,text,text,text,text)'::regprocedure);
    observed_state text;
BEGIN
    IF definition NOT LIKE '%SET search_path TO%'
       OR EXISTS (
           SELECT 1
             FROM pg_proc function
             CROSS JOIN LATERAL aclexplode(COALESCE(function.proacl, acldefault('f', function.proowner))) privilege
            WHERE function.oid = 'enqueue_orchestration_push(uuid,uuid,uuid,bigint,text,text,text,text,text)'::regprocedure
              AND privilege.grantee = 0 AND privilege.privilege_type = 'EXECUTE'
       ) THEN
        RAISE EXCEPTION 'enqueue function search_path or PUBLIC privilege is unsafe';
    END IF;
    BEGIN
        PERFORM * FROM enqueue_orchestration_push(
            NULL, current_setting('cyclopus.app_id')::uuid,
            current_setting('cyclopus.production_environment_id')::uuid, 701,
            '.github/workflows/release.yaml', repeat('5', 40), 'refs/tags/v2',
            repeat('c', 40), 'invalid-null-org');
    EXCEPTION WHEN OTHERS THEN observed_state := SQLSTATE;
    END;
    IF observed_state <> '23514' THEN
        RAISE EXCEPTION 'malformed enqueue did not use invalid-argument SQLSTATE: %', observed_state;
    END IF;
    observed_state := NULL;
    BEGIN
        PERFORM * FROM enqueue_orchestration_push(
            current_setting('cyclopus.org_id')::uuid,
            current_setting('cyclopus.app_id')::uuid,
            current_setting('cyclopus.production_environment_id')::uuid, 701,
            '.github/workflows/release.yaml', repeat('5', 40), 'refs/tags/v2',
            repeat('d', 40), 'rapid-pending');
    EXCEPTION WHEN OTHERS THEN observed_state := SQLSTATE;
    END;
    IF observed_state <> '23505' THEN
        RAISE EXCEPTION 'idempotency mismatch did not use conflict SQLSTATE: %', observed_state;
    END IF;
    observed_state := NULL;
    UPDATE github_repositories SET access_state = 'inaccessible' WHERE github_repository_id = 701;
    BEGIN
        PERFORM * FROM enqueue_orchestration_push(
            current_setting('cyclopus.org_id')::uuid,
            current_setting('cyclopus.app_id')::uuid,
            current_setting('cyclopus.production_environment_id')::uuid, 701,
            '.github/workflows/release.yaml', repeat('5', 40), 'refs/tags/v2',
            repeat('e', 40), 'inactive-target-probe');
    EXCEPTION WHEN OTHERS THEN observed_state := SQLSTATE;
    END;
    UPDATE github_repositories SET access_state = 'active' WHERE github_repository_id = 701;
    IF observed_state <> '23503' THEN
        RAISE EXCEPTION 'inactive enqueue target did not use failed-precondition SQLSTATE: %', observed_state;
    END IF;
END;
$$;

UPDATE environments SET desired_generation = 9223372036854775807 WHERE id = :'production_environment_id';
DO $$
DECLARE
    observed_state text;
BEGIN
    BEGIN
        PERFORM * FROM enqueue_orchestration_push(
            current_setting('cyclopus.org_id')::uuid,
            current_setting('cyclopus.app_id')::uuid,
            current_setting('cyclopus.production_environment_id')::uuid, 701,
            '.github/workflows/release.yaml', repeat('5', 40), 'refs/tags/v2',
            repeat('e', 40), 'generation-overflow-probe');
    EXCEPTION WHEN OTHERS THEN observed_state := SQLSTATE;
    END;
    IF observed_state <> '23514' THEN
        RAISE EXCEPTION 'generation overflow did not use invalid-argument SQLSTATE: %', observed_state;
    END IF;
END;
$$;
UPDATE environments SET desired_generation = 4 WHERE id = :'production_environment_id';

-- Success is valid only from dispatched with a correlated run ID; recovery
-- terminals become eligible only at their state-specific hard deadline.
DO $$
DECLARE
    probe uuid;
BEGIN
    INSERT INTO orchestration_jobs (org_id, app_id, environment_id, github_repository_id,
        github_workflow_path, workflow_revision, dispatch_ref, desired_checkout_sha,
        desired_generation, idempotency_key)
    VALUES (current_setting('cyclopus.org_id')::uuid, current_setting('cyclopus.app_id')::uuid,
        current_setting('cyclopus.production_environment_id')::uuid, 701,
        '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main', repeat('d', 40), 4, 'resolution-probe')
    RETURNING id INTO probe;
    UPDATE orchestration_jobs SET status = 'leased', lease_owner = 'probe',
        lease_expires_at = now() + interval '1 minute', attempt = 1 WHERE id = probe;
    UPDATE orchestration_jobs SET status = 'dispatch_reconciling', lease_owner = NULL, lease_expires_at = NULL,
        dispatch_requested_at = now(), dispatch_ref_observed_sha = repeat('d', 40),
        dispatch_ambiguity_state = 'possible', reconcile_after = now(),
        dispatch_deadline_at = now() + interval '1 minute', reconciliation_owner = 'probe',
        reconciliation_lease_expires_at = now() + interval '1 minute' WHERE id = probe;
    IF EXISTS (SELECT 1 FROM orchestration_jobs WHERE id = probe
        AND status = 'dispatched' AND github_run_id IS NOT NULL
        AND reconciliation_owner = 'probe' AND reconciliation_lease_expires_at > now()) THEN
        RAISE EXCEPTION 'success accepted from dispatch_reconciling without run ID';
    END IF;
    IF EXISTS (SELECT 1 FROM orchestration_jobs WHERE id = probe AND status = 'dispatch_reconciling'
        AND github_run_id IS NULL AND dispatch_deadline_at <= now()) THEN
        RAISE EXCEPTION 'dispatch terminal became eligible before deadline';
    END IF;
    UPDATE orchestration_jobs SET dispatch_deadline_at = now() - interval '1 second' WHERE id = probe;
    IF NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE id = probe AND status = 'dispatch_reconciling'
        AND github_run_id IS NULL AND dispatch_deadline_at <= now()) THEN
        RAISE EXCEPTION 'dispatch terminal not eligible after deadline';
    END IF;
    UPDATE orchestration_jobs SET status = 'cancelled', reconciliation_owner = NULL,
        reconciliation_lease_expires_at = NULL, terminal_at = now() WHERE id = probe;
END;
$$;

-- A dispatched run timeout is gated by run_deadline_at, independently of the
-- already-expired dispatch deadline.
DO $$
DECLARE
    probe uuid;
BEGIN
    INSERT INTO orchestration_jobs (org_id, app_id, environment_id, github_repository_id,
        github_workflow_path, workflow_revision, dispatch_ref, desired_checkout_sha,
        desired_generation, idempotency_key)
    VALUES (current_setting('cyclopus.org_id')::uuid, current_setting('cyclopus.app_id')::uuid,
        current_setting('cyclopus.production_environment_id')::uuid, 701,
        '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main', repeat('e', 40), 4, 'timeout-probe')
    RETURNING id INTO probe;
    UPDATE orchestration_jobs SET status = 'leased', lease_owner = 'timeout-probe',
        lease_expires_at = now() + interval '1 minute', attempt = 1 WHERE id = probe;
    UPDATE orchestration_jobs SET status = 'dispatch_reconciling', lease_owner = NULL, lease_expires_at = NULL,
        dispatch_requested_at = now() - interval '10 minutes', dispatch_ref_observed_sha = repeat('e', 40),
        dispatch_ambiguity_state = 'possible', reconcile_after = now(),
        dispatch_deadline_at = now() - interval '5 minutes' WHERE id = probe;
    UPDATE orchestration_jobs SET status = 'dispatched', github_run_id = 8102,
        github_dispatch_identity = 'cyclopus:' || id, github_run_head_sha = repeat('e', 40),
        github_run_status = 'in_progress', dispatch_ambiguity_state = 'matched',
        run_deadline_at = now() + interval '1 minute' WHERE id = probe;
    IF EXISTS (SELECT 1 FROM orchestration_jobs WHERE id = probe AND status = 'dispatched'
        AND github_run_id IS NOT NULL AND run_deadline_at <= now()) THEN
        RAISE EXCEPTION 'run timeout eligible before run deadline';
    END IF;
    UPDATE orchestration_jobs SET run_deadline_at = now() - interval '1 second' WHERE id = probe;
    IF NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE id = probe AND status = 'dispatched'
        AND github_run_id IS NOT NULL AND run_deadline_at <= now()) THEN
        RAISE EXCEPTION 'run timeout not eligible after run deadline';
    END IF;
    UPDATE orchestration_jobs SET status = 'cancelled', terminal_at = now() WHERE id = probe;
END;
$$;

UPDATE apps SET github_workflow_path = '.github/workflows/deploy.yml' WHERE id = :'app_id';
UPDATE environments SET desired_generation = 1 WHERE id = :'production_environment_id';

DO $$
BEGIN
    BEGIN
        INSERT INTO environments (org_id, app_id, branch, kind, namespace)
        VALUES (current_setting('cyclopus.org_id')::uuid, current_setting('cyclopus.app_id')::uuid, 'release', 'production', 'schema-production-two');
        RAISE EXCEPTION 'second live production environment succeeded';
    EXCEPTION WHEN unique_violation THEN NULL;
    END;
END;
$$;

INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
VALUES (:'org_id', :'app_id', 'feature/schema', 'preview', 'schema-preview', 'schema-preview', 'schema-preview.example.test')
RETURNING id AS preview_a_environment_id \gset
SELECT set_config('cyclopus.preview_a_environment_id', :'preview_a_environment_id', false);
INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
VALUES (:'org_id', :'app_id', 'feature/schema-b', 'preview', 'schema-preview-b', 'schema-preview-b', 'schema-preview-b.example.test')
RETURNING id AS preview_b_environment_id \gset
SELECT set_config('cyclopus.preview_b_environment_id', :'preview_b_environment_id', false);
DO $$
BEGIN
    BEGIN
        INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
        VALUES (current_setting('cyclopus.org_id')::uuid, current_setting('cyclopus.app_id')::uuid, 'feature/schema', 'preview', 'schema-preview-two', 'schema-preview-two', 'schema-preview-two.example.test');
        RAISE EXCEPTION 'duplicate live preview branch succeeded';
    EXCEPTION WHEN unique_violation THEN NULL;
    END;
END;
$$;

-- Environment generations are independent fences for one app. Alternate pushes
-- across production and two previews and verify each target's monotonic sequence.
SELECT accepted_generation AS alternating_production_generation
FROM enqueue_orchestration_push(:'org_id', :'app_id', :'production_environment_id', 701,
    '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main', repeat('1', 40), 'alternating-production') \gset
SELECT set_config('alternating.production_generation', :'alternating_production_generation', false);
SELECT accepted_generation AS alternating_preview_a_generation
FROM enqueue_orchestration_push(:'org_id', :'app_id', :'preview_a_environment_id', 701,
    '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/feature/schema', repeat('2', 40), 'alternating-preview-a') \gset
SELECT set_config('alternating.preview_a_generation', :'alternating_preview_a_generation', false);
DO $$ BEGIN
    IF (SELECT desired_generation FROM environments WHERE id = current_setting('cyclopus.production_environment_id')::uuid) <> 2
       OR NOT EXISTS (SELECT 1 FROM orchestration_jobs
           WHERE environment_id = current_setting('cyclopus.production_environment_id')::uuid AND desired_generation = 2) THEN
        RAISE EXCEPTION 'push to a different environment incorrectly fenced production';
    END IF;
END $$;
SELECT accepted_generation AS alternating_production_second_generation
FROM enqueue_orchestration_push(:'org_id', :'app_id', :'production_environment_id', 701,
    '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main', repeat('3', 40), 'alternating-production-second') \gset
SELECT set_config('alternating.production_second_generation', :'alternating_production_second_generation', false);
SELECT accepted_generation AS alternating_preview_b_generation
FROM enqueue_orchestration_push(:'org_id', :'app_id', :'preview_b_environment_id', 701,
    '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/feature/schema-b', repeat('4', 40), 'alternating-preview-b') \gset
SELECT set_config('alternating.preview_b_generation', :'alternating_preview_b_generation', false);
SELECT accepted_generation AS alternating_preview_a_second_generation
FROM enqueue_orchestration_push(:'org_id', :'app_id', :'preview_a_environment_id', 701,
    '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/feature/schema', repeat('5', 40), 'alternating-preview-a-second') \gset
SELECT set_config('alternating.preview_a_second_generation', :'alternating_preview_a_second_generation', false);
DO $$ BEGIN
    IF current_setting('alternating.production_generation')::bigint <> 2
       OR current_setting('alternating.production_second_generation')::bigint <> 3
       OR current_setting('alternating.preview_a_generation')::bigint <> 2
       OR current_setting('alternating.preview_a_second_generation')::bigint <> 3
       OR current_setting('alternating.preview_b_generation')::bigint <> 2 THEN
        RAISE EXCEPTION 'alternating environment generations were not independent';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE environment_id = current_setting('cyclopus.production_environment_id')::uuid
        AND desired_generation = 2 AND pending_desired_generation = 3)
       OR NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE environment_id = current_setting('cyclopus.preview_a_environment_id')::uuid
        AND desired_generation = 2 AND pending_desired_generation = 3)
       OR NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE environment_id = current_setting('cyclopus.preview_b_environment_id')::uuid
        AND desired_generation = 2 AND pending_desired_generation IS NULL) THEN
        RAISE EXCEPTION 'alternating pushes did not assign target generations to queued/pending work';
    END IF;
END $$;

-- A caller-controlled schema earlier in search_path cannot shadow application
-- objects used by the security-sensitive enqueue function.
SELECT set_config('cyclopus.application_schema', current_schema(), false);
SELECT set_config('cyclopus.attacker_schema', :'attacker_schema', false);
CREATE ROLE :"attacker_role" NOLOGIN;
CREATE ROLE :"public_probe_role" NOLOGIN;
CREATE SCHEMA :"attacker_schema" AUTHORIZATION :"attacker_role";
GRANT USAGE ON SCHEMA :"application_schema" TO :"public_probe_role";
SET ROLE :"attacker_role";
CREATE TABLE :"attacker_schema".apps (desired_generation bigint);
CREATE TABLE :"attacker_schema".environments (desired_generation bigint);
CREATE TABLE :"attacker_schema".orchestration_jobs (id uuid);
CREATE TABLE :"attacker_schema".orchestration_push_requests (idempotency_key text);
CREATE FUNCTION :"attacker_schema".enqueue_orchestration_push(
    uuid, uuid, uuid, bigint, text, text, text, text, text
) RETURNS TABLE (accepted_generation bigint)
LANGUAGE sql AS 'SELECT -1::bigint';
RESET ROLE;
SELECT set_config('search_path', format('%I,%I,pg_catalog', :'attacker_schema', :'application_schema'), false);
DO $$
DECLARE
    application_function regprocedure := to_regprocedure(format(
        '%I.enqueue_orchestration_push(uuid,uuid,uuid,bigint,text,text,text,text,text)',
        current_setting('cyclopus.application_schema')));
    definition text;
    owner_generation bigint;
BEGIN
    definition := pg_get_functiondef(application_function);
    IF definition LIKE '%SET search_path FROM CURRENT%'
       OR definition NOT LIKE '%SET search_path TO%pg_catalog%'
       OR NOT has_function_privilege(current_user, application_function, 'EXECUTE') THEN
        RAISE EXCEPTION 'enqueue function schema context or owner execution privilege is unsafe';
    END IF;
    EXECUTE format('SELECT accepted_generation FROM %I.enqueue_orchestration_push($1,$2,$3,$4,$5,$6,$7,$8,$9)',
        current_setting('cyclopus.application_schema'))
       INTO owner_generation
       USING current_setting('cyclopus.org_id')::uuid, current_setting('cyclopus.app_id')::uuid,
             current_setting('cyclopus.preview_b_environment_id', true)::uuid, 701::bigint,
             '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/feature/schema-b',
             repeat('6', 40), 'shadow-owner-push';
    IF owner_generation <> 3 THEN
        RAISE EXCEPTION 'enqueue function resolved attacker-controlled shadow objects';
    END IF;
END $$;
SET ROLE :"public_probe_role";
DO $$
DECLARE
    observed_state text;
BEGIN
    BEGIN
        EXECUTE format('SELECT * FROM %I.enqueue_orchestration_push($1,$2,$3,$4,$5,$6,$7,$8,$9)',
            current_setting('cyclopus.application_schema'))
        USING current_setting('cyclopus.org_id')::uuid, current_setting('cyclopus.app_id')::uuid,
              current_setting('cyclopus.preview_b_environment_id', true)::uuid, 701::bigint,
              '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/feature/schema-b',
              repeat('6', 40), 'shadow-owner-push';
    EXCEPTION WHEN OTHERS THEN observed_state := SQLSTATE;
    END;
    IF observed_state <> '42501' THEN
        RAISE EXCEPTION 'PUBLIC-derived role executed enqueue: SQLSTATE %', observed_state;
    END IF;
END $$;
RESET ROLE;
SELECT set_config('search_path', format('%I,pg_catalog', current_setting('cyclopus.application_schema')), false);
DO $$ BEGIN
    IF (SELECT desired_generation FROM environments WHERE id = current_setting('cyclopus.preview_b_environment_id')::uuid) <> 3
       OR NOT EXISTS (SELECT 1 FROM orchestration_jobs
           WHERE environment_id = current_setting('cyclopus.preview_b_environment_id')::uuid
             AND desired_generation = 2 AND pending_desired_generation = 3
             AND pending_checkout_sha = repeat('6', 40)
             AND pending_idempotency_key = 'shadow-owner-push')
       OR NOT EXISTS (SELECT 1 FROM orchestration_push_requests
           WHERE environment_id = current_setting('cyclopus.preview_b_environment_id')::uuid
             AND idempotency_key = 'shadow-owner-push' AND accepted_generation = 3)
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_stat_user_tables
           WHERE schemaname = current_setting('cyclopus.attacker_schema') AND n_live_tup <> 0) THEN
        RAISE EXCEPTION 'shadow attack changed the wrong objects or failed to change intended app objects';
    END IF;
END $$;
DO $$ BEGIN EXECUTE format('DROP SCHEMA %I CASCADE', current_setting('cyclopus.attacker_schema')); END $$;
DROP ROLE :"attacker_role";
REVOKE USAGE ON SCHEMA :"application_schema" FROM :"public_probe_role";
DROP ROLE :"public_probe_role";
SELECT seed_active_orchestration_states(:'app_id', 'disconnect-active');
UPDATE git_connections SET state = 'disconnected', disconnected_at = now() WHERE id = :'connection_id';
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM orchestration_jobs WHERE idempotency_key LIKE 'alternating-%'
        AND status IN ('queued','leased','dispatch_reconciling','dispatched','resolving_image','deploying'))
       OR (SELECT desired_generation FROM environments WHERE id = current_setting('cyclopus.production_environment_id')::uuid) <> 4
       OR (SELECT desired_generation FROM environments WHERE id = current_setting('cyclopus.preview_a_environment_id')::uuid) <> 4
       OR (SELECT desired_generation FROM environments WHERE id = current_setting('cyclopus.preview_b_environment_id')::uuid) <> 4 THEN
        RAISE EXCEPTION 'disconnect did not fence all affected environments and terminalize active jobs';
    END IF;
    IF (SELECT count(*) FROM orchestration_jobs WHERE idempotency_key LIKE 'disconnect-active-%'
            AND status='cancelled' AND terminal_error_code='git_connection_disconnected'
            AND terminal_at IS NOT NULL AND cancellation_state='requested') <> 6
       OR (SELECT count(DISTINCT environment_id) FROM orchestration_jobs
            WHERE idempotency_key LIKE 'disconnect-active-%') <> 6 THEN
        RAISE EXCEPTION 'disconnect did not terminalize every active state across multiple environments';
    END IF;
END $$;
UPDATE git_connections SET state = 'active', disconnected_at = NULL WHERE id = :'connection_id';
UPDATE environments SET desired_generation = 1
WHERE id IN (:'production_environment_id', :'preview_a_environment_id', :'preview_b_environment_id');

INSERT INTO github_deliveries (
    delivery_id, payload_hash, raw_body, event, action, signature_256, hook_id
) VALUES (
    '11111111-1111-1111-1111-111111111111', repeat('1', 64), convert_to('{"ref":"refs/heads/main"}', 'UTF8'),
    'push', NULL, 'sha256=' || repeat('2', 64), 88
);
DO $$
BEGIN
    BEGIN
        INSERT INTO github_deliveries (delivery_id, payload_hash, raw_body, event, signature_256, hook_id)
        VALUES ('11111111-1111-1111-1111-111111111111', repeat('3', 64), convert_to('{}', 'UTF8'), 'push', 'sha256=' || repeat('4', 64), 88);
        RAISE EXCEPTION 'conflicting delivery identity succeeded';
    EXCEPTION WHEN unique_violation THEN NULL;
    END;
END;
$$;

-- App tombstone iterates every descendant environment fence and terminalizes
-- active work, including multiple environments of the same app.
DO $$
DECLARE
    tombstone_app uuid;
    first_environment uuid;
    second_environment uuid;
BEGIN
    INSERT INTO apps (org_id, project_id, name, slug, hostname_label, registry_id, image_repository,
        github_workflow_path, production_branch, image_visibility)
    VALUES (current_setting('cyclopus.org_id')::uuid, current_setting('cyclopus.project_id', true)::uuid,
        'Tombstone App', 'tombstone-app', 'tombstone-app', current_setting('cyclopus.registry_id')::uuid,
        'ghcr.io/acme/schema', '.github/workflows/deploy.yml', 'main', 'private') RETURNING id INTO tombstone_app;
    PERFORM seed_active_orchestration_states(tombstone_app, 'app-tombstone-active');
    UPDATE environments SET lifecycle_status = 'torn_down', torn_down_at = now() WHERE app_id=tombstone_app;
    UPDATE apps SET external_credentials_revoked_at = now(), deleted_at = now() WHERE id = tombstone_app;
    IF (SELECT count(*) FROM environments WHERE app_id = tombstone_app AND desired_generation = 2) <> 6
       OR (SELECT count(*) FROM orchestration_jobs WHERE app_id = tombstone_app AND status = 'cancelled'
            AND terminal_error_code='ancestor_tombstoned' AND terminal_at IS NOT NULL) <> 6 THEN
        RAISE EXCEPTION 'app tombstone did not fence and terminalize every descendant environment';
    END IF;
END;
$$;

SELECT orchestration_job_id
FROM enqueue_orchestration_push(
    :'org_id', :'app_id', :'production_environment_id', 701,
    '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main',
    repeat('6', 40), 'job-1'
) \gset
SELECT set_config('cyclopus.orchestration_job_id', :'orchestration_job_id', false);

UPDATE orchestration_jobs
   SET status = 'leased', lease_owner = 'schema-worker', lease_expires_at = now() + interval '2 minutes', attempt = attempt + 1
 WHERE org_id = :'org_id' AND id = :'orchestration_job_id';
UPDATE orchestration_jobs
   SET status = 'dispatch_reconciling', dispatch_requested_at = now(),
       dispatch_ref_observed_sha = repeat('5', 40), dispatch_ambiguity_state = 'possible',
       reconcile_after = now() + interval '90 seconds', dispatch_deadline_at = now() + interval '5 minutes',
       lease_owner = NULL, lease_expires_at = NULL
 WHERE org_id = :'org_id' AND id = :'orchestration_job_id' AND status = 'leased';
UPDATE orchestration_jobs
   SET status = 'dispatched', github_dispatch_identity = 'cyclopus:' || id,
       github_run_id = 8001, github_run_status = 'queued', dispatch_ambiguity_state = 'matched',
       github_status_observed_at = now(), reconcile_after = now() + interval '30 seconds',
       run_deadline_at = now() + interval '45 minutes'
 WHERE org_id = :'org_id' AND id = :'orchestration_job_id' AND status = 'dispatch_reconciling';
UPDATE orchestration_jobs
   SET reconciliation_owner = 'schema-reconciler', reconciliation_lease_expires_at = now() + interval '1 minute'
 WHERE org_id = :'org_id' AND id = :'orchestration_job_id';
WITH terminal_evidence AS (
    INSERT INTO orchestration_job_run_evidence (
        orchestration_job_id, attempt, dispatch_ref, dispatch_ref_observed_sha,
        github_workflow_path, workflow_revision, dispatch_requested_at,
        dispatch_ambiguity_state, github_dispatch_identity, github_run_id,
        github_run_status, github_run_conclusion, observed_head_sha, observed_at, terminal_at
    )
    SELECT id, attempt, dispatch_ref, dispatch_ref_observed_sha, github_workflow_path,
        workflow_revision, dispatch_requested_at, dispatch_ambiguity_state,
        github_dispatch_identity, github_run_id, 'completed', 'success', desired_checkout_sha, now(), now()
      FROM orchestration_jobs
     WHERE org_id = :'org_id' AND id = :'orchestration_job_id'
       AND status = 'dispatched' AND reconciliation_owner = 'schema-reconciler'
    RETURNING orchestration_job_id
)
UPDATE orchestration_jobs AS job
   SET status = 'resolving_image', github_run_status = 'completed', github_run_conclusion = 'success',
       github_run_head_sha = desired_checkout_sha, github_status_observed_at = now(),
       reconcile_after = NULL, dispatch_deadline_at = NULL, run_deadline_at = NULL,
       reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL
  FROM terminal_evidence WHERE job.id = terminal_evidence.orchestration_job_id;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM orchestration_job_run_evidence
        WHERE orchestration_job_id = current_setting('cyclopus.orchestration_job_id')::uuid
          AND dispatch_ref = 'refs/heads/main' AND dispatch_ref_observed_sha = repeat('5', 40)
          AND github_workflow_path = '.github/workflows/deploy.yml'
          AND workflow_revision = repeat('4', 40) AND dispatch_requested_at IS NOT NULL
          AND dispatch_ambiguity_state = 'matched'
          AND github_dispatch_identity = 'cyclopus:' || current_setting('cyclopus.orchestration_job_id')
          AND github_run_id = 8001 AND observed_head_sha = repeat('6', 40)
          AND github_run_status = 'completed' AND github_run_conclusion = 'success') THEN
        RAISE EXCEPTION 'successful dispatch evidence was not durable in resolving_image';
    END IF;
END $$;

INSERT INTO import_operations (
    org_id, idempotency_key, request_digest, state, checkpoint, github_repository_id,
    git_connection_id, project_id, app_id, environment_id, orchestration_job_id, registry_id, completed_at
) VALUES (
    :'org_id', 'import-1', repeat('7', 64), 'completed', 'completed', 701,
    :'connection_id', :'project_id', :'app_id', :'production_environment_id', :'orchestration_job_id', :'registry_id', now()
);

DO $$
BEGIN
    BEGIN
        INSERT INTO orchestration_jobs (
            org_id, app_id, environment_id, github_repository_id, github_workflow_path,
            workflow_revision, dispatch_ref, desired_checkout_sha, desired_generation, idempotency_key
        ) VALUES (
            current_setting('cyclopus.org_id')::uuid, current_setting('cyclopus.app_id')::uuid,
            current_setting('cyclopus.production_environment_id')::uuid, 701, '.github/workflows/deploy.yml',
            repeat('A', 40), 'refs/heads/main', repeat('8', 40), 2, 'bad-sha'
        );
        RAISE EXCEPTION 'uppercase SHA succeeded';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
        UPDATE orchestration_jobs SET image_tag_ref = 'ghcr.io/acme/schema:' || repeat('8', 40)
        WHERE id = current_setting('cyclopus.orchestration_job_id')::uuid;
        RAISE EXCEPTION 'mismatched SHA image tag succeeded';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
        UPDATE orchestration_jobs SET image_tag_ref = 'ghcr.io/other/schema:' || repeat('6', 40)
        WHERE id = current_setting('cyclopus.orchestration_job_id')::uuid;
        RAISE EXCEPTION 'mismatched image repository succeeded';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
        UPDATE orchestration_jobs SET image_tag_ref = 'ghcr.io/acme/schema:' || repeat('6', 40),
            resolved_image_digest = 'sha256:' || repeat('9', 64),
            immutable_image_ref = 'ghcr.io/acme/schema@sha256:' || repeat('8', 64),
            oci_resolved_at = now(), oci_resolution_state = 'resolved';
        RAISE EXCEPTION 'mismatched digest and immutable reference succeeded';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
        UPDATE orchestration_jobs SET status = 'completed', terminal_at = now()
        WHERE id = current_setting('cyclopus.orchestration_job_id')::uuid;
        RAISE EXCEPTION 'completion without deployment succeeded';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
        INSERT INTO import_operations (org_id, idempotency_key, request_digest, github_repository_id)
        VALUES (current_setting('cyclopus.org_id')::uuid, 'import-1', repeat('8', 64), 701);
        RAISE EXCEPTION 'duplicate import idempotency key succeeded';
    EXCEPTION WHEN unique_violation THEN NULL;
    END;
END;
$$;

UPDATE orchestration_jobs
   SET image_tag_ref = 'ghcr.io/acme/schema:' || repeat('6', 40),
       resolved_image_digest = 'sha256:' || repeat('9', 64),
       immutable_image_ref = 'ghcr.io/acme/schema@sha256:' || repeat('9', 64),
       oci_media_type = 'application/vnd.oci.image.manifest.v1+json', oci_resolved_at = now(),
       oci_resolution_state = 'resolved', deployment_state = 'ready', status = 'deploying'
 WHERE org_id = :'org_id' AND id = :'orchestration_job_id' AND status = 'resolving_image';

INSERT INTO releases (
    org_id, environment_id, image_ref, git_commit_sha, kind, config_snapshot, config_hash, status
) VALUES (
    :'org_id', :'production_environment_id', 'ghcr.io/acme/schema@sha256:' || repeat('9', 64), repeat('6', 40),
    'deploy', '{"version":2,"desired_state":{"version":1,"deployment":{},"service":{},"ingress":{},"scaling":{}}}',
    repeat('a', 64), 'deploying'
) RETURNING id AS release_id \gset
UPDATE orchestration_jobs SET release_id = :'release_id', deployment_state = 'applying'
 WHERE org_id = :'org_id' AND id = :'orchestration_job_id' AND status = 'deploying';

DO $$
BEGIN
    BEGIN
        UPDATE orchestration_jobs SET status = 'completed', deployment_state = 'live', terminal_at = now()
        WHERE id = current_setting('cyclopus.orchestration_job_id')::uuid;
        RAISE EXCEPTION 'completion with non-live release succeeded';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
END;
$$;
UPDATE releases SET status = 'live' WHERE id = :'release_id';
UPDATE orchestration_jobs SET status = 'completed', deployment_state = 'live', terminal_at = now()
 WHERE org_id = :'org_id' AND id = :'orchestration_job_id' AND status = 'deploying';

DO $$
DECLARE
    other_project uuid;
    other_app uuid;
    other_environment uuid;
    other_job uuid;
    other_release uuid;
BEGIN
    INSERT INTO projects (org_id, name, slug, git_connection_id)
    VALUES (current_setting('cyclopus.org_id')::uuid, 'Other Project', 'other-project', current_setting('cyclopus.connection_id')::uuid) RETURNING id INTO other_project;
    INSERT INTO apps (org_id, project_id, name, slug, hostname_label, registry_id, image_repository,
                      github_workflow_path, production_branch, image_visibility)
    VALUES (current_setting('cyclopus.org_id')::uuid, other_project, 'Other App', 'other-app', 'other-app', current_setting('cyclopus.registry_id')::uuid,
            'ghcr.io/acme/schema', '.github/workflows/deploy.yml', 'main', 'private') RETURNING id INTO other_app;
    INSERT INTO environments (org_id, app_id, branch, kind, namespace, desired_generation,
                              required_registry_credential_generation, ready_registry_credential_generation, registry_credential_state)
    VALUES (current_setting('cyclopus.org_id')::uuid, other_app, 'main', 'production', 'other-production', 1, 1, 1, 'ready')
    RETURNING id INTO other_environment;
    INSERT INTO github_repositories (
        github_repository_id, org_id, github_installation_id, node_id, owner_login,
        name, full_name, visibility, default_branch, last_api_confirmed_generation
    ) VALUES (
        702, current_setting('cyclopus.org_id')::uuid, 501, 'R_other', 'acme',
        'other', 'acme/other', 'private', 'main', 1
    );
    INSERT INTO orchestration_jobs (org_id, app_id, environment_id, github_repository_id, github_workflow_path,
        workflow_revision, dispatch_ref, desired_checkout_sha, desired_generation, idempotency_key)
    VALUES (current_setting('cyclopus.org_id')::uuid, other_app, other_environment, 701,
        '.github/workflows/deploy.yml', repeat('4',40), 'refs/heads/main', repeat('1',40), 1, 'other-current-generation')
    RETURNING id INTO other_job;
    INSERT INTO releases (org_id, environment_id, image_ref, git_commit_sha, kind, config_snapshot, config_hash, status)
    VALUES (current_setting('cyclopus.org_id')::uuid, other_environment,
        'ghcr.io/acme/schema@sha256:' || repeat('7',64), repeat('1',40), 'deploy',
        '{"version":2,"desired_state":{"version":1,"deployment":{},"service":{},"ingress":{},"scaling":{}}}',
        repeat('b',64), 'pending') RETURNING id INTO other_release;
    UPDATE orchestration_jobs SET image_tag_ref='ghcr.io/acme/schema:' || repeat('1',40),
        deployment_state='waiting_credential', release_id=other_release WHERE id=other_job;
    IF NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE id=other_job
        AND image_tag_ref='ghcr.io/acme/schema:' || repeat('1',40)
        AND deployment_state='waiting_credential' AND release_id=other_release) THEN
        RAISE EXCEPTION 'generation change on another target interfered with current artifact/release/deploy writes';
    END IF;
    UPDATE environments SET desired_generation = 2 WHERE id = other_environment;
    BEGIN
        UPDATE orchestration_jobs SET image_tag_ref=NULL WHERE id=other_job;
        RAISE EXCEPTION 'stale generation changed artifact evidence';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
        UPDATE orchestration_jobs SET release_id=NULL WHERE id=other_job;
        RAISE EXCEPTION 'stale generation changed release evidence';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
        UPDATE orchestration_jobs SET deployment_state='not_started' WHERE id=other_job;
        RAISE EXCEPTION 'stale generation changed deployment state';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    UPDATE orchestration_jobs SET status='cancelled', terminal_at=now() WHERE id=other_job;
    BEGIN
        INSERT INTO orchestration_jobs (org_id, app_id, environment_id, github_repository_id, github_workflow_path,
            workflow_revision, dispatch_ref, desired_checkout_sha, desired_generation, idempotency_key)
        VALUES (current_setting('cyclopus.org_id')::uuid, current_setting('cyclopus.app_id')::uuid, other_environment, 701,
            '.github/workflows/deploy.yml', repeat('4',40), 'refs/heads/main', repeat('1',40), 1, 'cross-target');
        RAISE EXCEPTION 'cross-app environment target succeeded';
    EXCEPTION WHEN foreign_key_violation OR check_violation THEN NULL;
    END;
    BEGIN
        INSERT INTO orchestration_jobs (org_id, app_id, environment_id, github_repository_id, github_workflow_path,
            workflow_revision, dispatch_ref, desired_checkout_sha, desired_generation, idempotency_key)
        VALUES (current_setting('cyclopus.org_id')::uuid, other_app, other_environment, 702,
            '.github/workflows/deploy.yml', repeat('4',40), 'refs/heads/main', repeat('1',40), 1, 'wrong-repository');
        RAISE EXCEPTION 'job repository differed from app project repository';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
        UPDATE git_connections SET state = 'disconnected', disconnected_at = now() WHERE id = current_setting('cyclopus.connection_id')::uuid;
        INSERT INTO orchestration_jobs (org_id, app_id, environment_id, github_repository_id, github_workflow_path,
            workflow_revision, dispatch_ref, desired_checkout_sha, desired_generation, idempotency_key)
        VALUES (current_setting('cyclopus.org_id')::uuid, other_app, other_environment, 701,
            '.github/workflows/deploy.yml', repeat('4',40), 'refs/heads/main', repeat('2',40), 1, 'inactive-scope');
        RAISE EXCEPTION 'inactive repository scope succeeded';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
    BEGIN
        INSERT INTO orchestration_jobs (org_id, app_id, environment_id, github_repository_id, github_workflow_path,
            workflow_revision, dispatch_ref, desired_checkout_sha, desired_generation, idempotency_key)
        VALUES (current_setting('cyclopus.org_id')::uuid, other_app, other_environment, 701,
            '.github/workflows/deploy.yml', repeat('4',40), 'refs/heads/main', repeat('3',40), 1, 'stale-generation');
        RAISE EXCEPTION 'stale generation succeeded';
    EXCEPTION WHEN check_violation THEN NULL;
    END;
END;
$$;

-- Project tombstones apply the same fence to every descendant environment and
-- every active orchestration state, not just queued jobs.
UPDATE git_connections SET state='active',disconnected_at=NULL WHERE id=:'connection_id';
DO $$
DECLARE
    tombstone_project uuid;
    tombstone_app uuid;
BEGIN
    INSERT INTO projects (org_id, name, slug, git_connection_id)
    VALUES (current_setting('cyclopus.org_id')::uuid, 'Tombstone Project', 'tombstone-project',
        current_setting('cyclopus.connection_id')::uuid) RETURNING id INTO tombstone_project;
    INSERT INTO apps (org_id, project_id, name, slug, hostname_label, registry_id, image_repository,
        github_workflow_path, production_branch, image_visibility)
    VALUES (current_setting('cyclopus.org_id')::uuid, tombstone_project, 'Project Tombstone App',
        'project-tombstone-app', 'project-tombstone-app', current_setting('cyclopus.registry_id')::uuid,
        'ghcr.io/acme/schema', '.github/workflows/deploy.yml', 'main', 'private') RETURNING id INTO tombstone_app;
    PERFORM seed_active_orchestration_states(tombstone_app, 'project-tombstone-active');
    UPDATE environments SET lifecycle_status='torn_down',torn_down_at=now() WHERE app_id=tombstone_app;
    UPDATE projects SET external_credentials_revoked_at=now(),deleted_at=now() WHERE id=tombstone_project;
    IF (SELECT count(*) FROM environments WHERE app_id=tombstone_app AND desired_generation=2) <> 6
       OR (SELECT count(*) FROM orchestration_jobs WHERE app_id=tombstone_app AND status='cancelled'
            AND terminal_error_code='ancestor_tombstoned' AND terminal_at IS NOT NULL) <> 6 THEN
        RAISE EXCEPTION 'project tombstone did not fence and terminalize every active descendant state';
    END IF;
END;
$$;

-- Organization tombstone is the widest boundary. Run it last so it can prove
-- all six active states are terminalized without weakening earlier live-scope tests.
DO $$
BEGIN
    PERFORM seed_active_orchestration_states(current_setting('cyclopus.app_id')::uuid, 'org-tombstone-active');
    UPDATE environments SET lifecycle_status='torn_down',torn_down_at=now()
     WHERE org_id=current_setting('cyclopus.org_id')::uuid AND torn_down_at IS NULL;
    UPDATE orgs SET external_credentials_revoked_at=now(),deleted_at=now()
     WHERE id=current_setting('cyclopus.org_id')::uuid;
    IF (SELECT count(*) FROM orchestration_jobs WHERE idempotency_key LIKE 'org-tombstone-active-%'
            AND status='cancelled' AND terminal_error_code='ancestor_tombstoned'
            AND terminal_at IS NOT NULL) <> 6
       OR (SELECT count(DISTINCT environment_id) FROM orchestration_jobs
            WHERE idempotency_key LIKE 'org-tombstone-active-%') <> 6 THEN
        RAISE EXCEPTION 'organization tombstone did not terminalize every active state across environments';
    END IF;
END;
$$;
