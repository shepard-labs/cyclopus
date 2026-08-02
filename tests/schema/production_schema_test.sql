\set ON_ERROR_STOP on

DO $$
DECLARE
    version integer := current_setting('server_version_num')::integer;
BEGIN
    IF version < 180000 OR version >= 190000 THEN
        RAISE EXCEPTION 'PostgreSQL 18 required, found server_version_num %', version;
    END IF;
END;
$$;

INSERT INTO orgs (name, slug) VALUES ('Schema Org', 'schema-org') RETURNING id AS org_id \gset
SELECT set_config('cyclopus.org_id', :'org_id', false);
INSERT INTO git_connections (org_id, installation_id, repo_full_name, webhook_secret_ref)
VALUES (:'org_id', 501, 'acme/schema', 'k8s-secret://cyclopus/github#webhook') RETURNING id AS repository_id \gset
SELECT set_config('cyclopus.repository_id', :'repository_id', false);
INSERT INTO projects (org_id, name, slug, git_connection_id)
VALUES (:'org_id', 'Schema Project', 'schema-project', :'repository_id') RETURNING id AS project_id \gset
INSERT INTO apps (org_id, project_id, name, slug, hostname_label)
VALUES (:'org_id', :'project_id', 'Schema App', 'schema-app', 'schema-app') RETURNING id AS app_id \gset
SELECT set_config('cyclopus.app_id', :'app_id', false);
INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
VALUES (:'org_id', :'app_id', 'feature/schema', 'preview', 'schema-preview', 'schema-preview-ns', 'schema-preview.example.test') RETURNING id AS environment_id \gset
UPDATE environments SET torn_down_at = now() WHERE id = :'environment_id';

DO $$
DECLARE
    org_id uuid := current_setting('cyclopus.org_id')::uuid;
    app_id uuid := current_setting('cyclopus.app_id')::uuid;
BEGIN
    BEGIN
        INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
        VALUES (org_id, app_id, 'feature/schema', 'preview', 'schema-preview', 'new-schema-ns', 'new-schema.example.test');
        RAISE EXCEPTION 'preview_slug was reusable';
    EXCEPTION WHEN unique_violation THEN
        NULL;
    END;
    BEGIN
        INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
        VALUES (org_id, app_id, 'feature/namespace', 'preview', 'new-schema-preview', 'schema-preview-ns', 'new-schema-preview.example.test');
        RAISE EXCEPTION 'namespace was reusable';
    EXCEPTION WHEN unique_violation THEN
        NULL;
    END;
END;
$$;

INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
VALUES (:'org_id', :'app_id', 'feature/schema', 'preview', 'schema-preview-2', 'schema-preview-ns-2', 'schema-preview-2.example.test') RETURNING id AS active_environment_id \gset
SELECT set_config('cyclopus.environment_id', :'active_environment_id', false);
INSERT INTO env_var_revisions (org_id, environment_id, revision)
VALUES (:'org_id', :'active_environment_id', 1) RETURNING id AS env_var_revision_id \gset
SELECT set_config('cyclopus.env_var_revision_id', :'env_var_revision_id', false);
INSERT INTO env_var_revision_entries (org_id, environment_id, env_var_revision_id, key, secret_ref)
VALUES (:'org_id', :'active_environment_id', :'env_var_revision_id', 'DATABASE_URL', 'k8s-secret://cyclopus/runtime#database-url');
INSERT INTO env_vars (org_id, environment_id, key, secret_ref)
VALUES (:'org_id', :'active_environment_id', 'RUNTIME_TOKEN', 'k8s-secret://cyclopus/runtime#runtime-token');

BEGIN;
UPDATE environments
SET current_env_var_revision_id = :'env_var_revision_id'
WHERE id = :'active_environment_id';
INSERT INTO releases (
    org_id, environment_id, image_ref, kind, env_var_revision_id,
    config_snapshot, config_hash, status
) VALUES (
    :'org_id', :'active_environment_id',
    'ghcr.io/acme/schema@sha256:' || repeat('a', 64), 'env_change', :'env_var_revision_id',
    jsonb_build_object(
        'version', 2,
        'desired_state', jsonb_build_object(
            'version', 1,
            'deployment', jsonb '{}', 'service', jsonb '{}', 'ingress', jsonb '{}', 'scaling', jsonb '{}'
        ),
        'env_var_revision', jsonb_build_object('id', :'env_var_revision_id')
    ), repeat('b', 64), 'pending'
);
COMMIT;

DO $$
DECLARE
    org_id uuid := current_setting('cyclopus.org_id')::uuid;
    environment_id uuid := current_setting('cyclopus.environment_id')::uuid;
    revision_id uuid := current_setting('cyclopus.env_var_revision_id')::uuid;
BEGIN
    BEGIN
        UPDATE env_var_revision_entries SET secret_ref = 'k8s-secret://cyclopus/runtime#changed'
        WHERE env_var_revision_id = revision_id;
        RAISE EXCEPTION 'env-var revision entry update succeeded';
    EXCEPTION WHEN integrity_constraint_violation THEN
        NULL;
    END;
    BEGIN
        INSERT INTO env_var_revision_entries (org_id, environment_id, env_var_revision_id, key, secret_ref)
        VALUES (org_id, environment_id, revision_id, 'LATE_VALUE', 'k8s-secret://cyclopus/runtime#late');
        RAISE EXCEPTION 'published env-var revision accepted a new entry';
    EXCEPTION WHEN integrity_constraint_violation THEN
        NULL;
    END;
    BEGIN
        DELETE FROM env_var_revisions WHERE id = revision_id;
        RAISE EXCEPTION 'env-var revision delete succeeded';
    EXCEPTION WHEN integrity_constraint_violation THEN
        NULL;
    END;
END;
$$;

INSERT INTO orchestration_jobs (
    org_id, repository_id, workflow_id, git_ref, environment_id, commit_sha, idempotency_key
) VALUES (
    :'org_id', :'repository_id', 'deploy.yml', 'refs/heads/feature/schema',
    :'active_environment_id', repeat('e', 40), 'job-key'
) RETURNING id AS orchestration_job_id \gset
SELECT set_config('cyclopus.orchestration_job_id', :'orchestration_job_id', false);

INSERT INTO orchestration_requests (
    org_id, idempotency_key, request_hash, orchestration_job_id,
    repository_id, workflow_id, git_ref, environment_id, commit_sha
) VALUES (
    :'org_id', 'request-key', repeat('f', 64), :'orchestration_job_id',
    :'repository_id', 'deploy.yml', 'refs/heads/feature/schema', :'active_environment_id', repeat('e', 40)
);

DO $$
DECLARE
    tenant_id uuid := current_setting('cyclopus.org_id')::uuid;
    repository_id uuid := current_setting('cyclopus.repository_id')::uuid;
    environment_id uuid := current_setting('cyclopus.environment_id')::uuid;
    job_id uuid := current_setting('cyclopus.orchestration_job_id')::uuid;
BEGIN
    BEGIN
        INSERT INTO orchestration_requests (
            org_id, idempotency_key, request_hash, orchestration_job_id,
            repository_id, workflow_id, git_ref, environment_id, commit_sha
        ) VALUES (
            tenant_id, 'request-key', repeat('0', 64), job_id,
            repository_id, 'deploy.yml', 'refs/heads/feature/schema', environment_id, repeat('e', 40)
        );
        RAISE EXCEPTION 'duplicate orchestration request key succeeded';
    EXCEPTION WHEN unique_violation THEN
        NULL;
    END;
    BEGIN
        UPDATE orchestration_requests SET request_hash = repeat('1', 64)
        WHERE org_id = tenant_id AND idempotency_key = 'request-key';
        RAISE EXCEPTION 'orchestration request update succeeded';
    EXCEPTION WHEN integrity_constraint_violation THEN
        NULL;
    END;
END;
$$;

INSERT INTO api_idempotency_records (
    org_id, scope, idempotency_key, request_hash, status, response_code,
    resource_type, resource_org_id, resource_id, expires_at
) VALUES (
    :'org_id', 'orchestration', 'typed-resource', repeat('2', 64), 'completed', 202,
    'orchestration_job', :'org_id', :'orchestration_job_id', now() + interval '1 hour'
);

INSERT INTO reconciliation_runs (
    org_id, scope_kind, scope_id, scope_app_id, scope_environment_id
) VALUES (
    :'org_id', 'environment', :'active_environment_id', :'app_id', :'active_environment_id'
) RETURNING id AS reconciliation_run_id \gset
SELECT set_config('cyclopus.reconciliation_run_id', :'reconciliation_run_id', false);
-- Single-flight (enforce_reconciliation_scope_single_flight) allows only one
-- active run per overlapping scope: close the prior run out before the next
-- environment-scoped run is created below.
UPDATE reconciliation_runs SET status = 'completed', completed_at = now() WHERE id = :'reconciliation_run_id';

DO $$
DECLARE
    tenant_id uuid := current_setting('cyclopus.org_id')::uuid;
    job_id uuid := current_setting('cyclopus.orchestration_job_id')::uuid;
BEGIN
    BEGIN
        INSERT INTO api_idempotency_records (
            org_id, scope, idempotency_key, request_hash, status, response_code,
            resource_type, resource_org_id, resource_id, expires_at
        ) VALUES (
            tenant_id, 'orchestration', 'partial-resource', repeat('3', 64), 'completed', 202,
            'orchestration_job', NULL, NULL, now() + interval '1 hour'
        );
        RAISE EXCEPTION 'partial idempotency resource identity succeeded';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
    BEGIN
        INSERT INTO api_idempotency_records (
            org_id, scope, idempotency_key, request_hash, status, response_code,
            resource_type, resource_org_id, resource_id, expires_at
        ) VALUES (
            tenant_id, 'orchestration', 'cross-tenant-resource', repeat('4', 64), 'completed', 202,
            'orchestration_job', uuidv7(), job_id, now() + interval '1 hour'
        );
        RAISE EXCEPTION 'cross-tenant idempotency resource identity succeeded';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END;
$$;

INSERT INTO reconciliation_runs (
    org_id, scope_kind, scope_id, scope_app_id, scope_environment_id, dry_run, prune
) VALUES (
    :'org_id', 'environment', :'active_environment_id', :'app_id', :'active_environment_id', false, true
) RETURNING id AS reconciliation_run_id \gset

UPDATE reconciliation_runs
SET status = 'running', plan = '{"actions":[{"action":"patch","object":"Deployment/schema-app"}]}',
    lease_owner = 'schema-reconciler', lease_expires_at = now() + interval '1 minute', started_at = now()
WHERE id = :'reconciliation_run_id';
INSERT INTO audit_events (org_id, action, target_type, target_id, after)
VALUES (
    :'org_id', 'reconciliation.invoked', 'reconciliation_run', :'reconciliation_run_id',
    '{"actions":[{"action":"patch","object":"Deployment/schema-app"}]}'
);
INSERT INTO audit_events (org_id, action, target_type, target_id, after)
VALUES (
    :'org_id', 'reconciliation.object_patched', 'reconciliation_object', :'reconciliation_run_id',
    '{"operation":"patch","object_kind":"Deployment","object_name":"schema-app"}'
);
SELECT set_config('cyclopus.reconciliation_run_id', :'reconciliation_run_id', false);

DO $$
DECLARE
    run_id uuid := current_setting('cyclopus.reconciliation_run_id')::uuid;
    invocation_audits integer;
    mutation_audits integer;
    invocation_plan jsonb;
BEGIN
    SELECT count(*) INTO invocation_audits
    FROM audit_events
    WHERE target_id = run_id AND action = 'reconciliation.invoked';
    SELECT after INTO invocation_plan
    FROM audit_events
    WHERE target_id = run_id AND action = 'reconciliation.invoked';
    SELECT count(*) INTO mutation_audits
    FROM audit_events
    WHERE target_id = run_id AND action LIKE 'reconciliation.object_%';
    IF invocation_audits <> 1 OR invocation_plan <> '{"actions":[{"action":"patch","object":"Deployment/schema-app"}]}'::jsonb THEN
        RAISE EXCEPTION 'reconciliation invocation audit must contain the full plan exactly once';
    END IF;
    IF mutation_audits <> 1 THEN
        RAISE EXCEPTION 'reconciliation mutation audit count = %, want 1', mutation_audits;
    END IF;
END;
$$;

DO $$
DECLARE
    tenant_id uuid := current_setting('cyclopus.org_id')::uuid;
BEGIN
    BEGIN
        UPDATE orgs SET external_credentials_revoked_at = now(), deleted_at = now() WHERE id = tenant_id;
        RAISE EXCEPTION 'org tombstone succeeded with active descendant environment';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END;
$$;

UPDATE environments SET torn_down_at = now() WHERE org_id = :'org_id' AND torn_down_at IS NULL;
INSERT INTO audit_events (org_id, action, target_type, target_id)
VALUES (:'org_id', 'org.deleted', 'org', :'org_id');
UPDATE apps SET external_credentials_revoked_at = now(), deleted_at = now() WHERE id = :'app_id';
UPDATE projects SET external_credentials_revoked_at = now(), deleted_at = now() WHERE id = :'project_id';
UPDATE orgs SET external_credentials_revoked_at = now(), deleted_at = now() WHERE id = :'org_id';

DO $$
DECLARE
    tenant_id uuid := current_setting('cyclopus.org_id')::uuid;
    retained_releases integer;
    retained_audits integer;
    retained_projects integer;
    retained_apps integer;
    job_status text;
    run_status text;
BEGIN
    SELECT count(*) INTO retained_releases FROM releases WHERE org_id = tenant_id;
    SELECT count(*) INTO retained_audits FROM audit_events WHERE org_id = tenant_id;
    SELECT count(*) INTO retained_projects FROM projects WHERE org_id = tenant_id AND deleted_at IS NOT NULL;
    SELECT count(*) INTO retained_apps FROM apps WHERE org_id = tenant_id AND deleted_at IS NOT NULL;
    SELECT status INTO job_status FROM orchestration_jobs
    WHERE id = current_setting('cyclopus.orchestration_job_id')::uuid;
    SELECT status INTO run_status FROM reconciliation_runs
    WHERE id = current_setting('cyclopus.reconciliation_run_id')::uuid;
    IF retained_releases = 0 OR retained_audits = 0 OR retained_projects = 0 OR retained_apps = 0 THEN
        RAISE EXCEPTION 'org tombstone did not retain release/audit history';
    END IF;
    IF job_status <> 'cancelled' OR run_status <> 'cancelled' THEN
        RAISE EXCEPTION 'org tombstone did not terminalize work: job %, run %', job_status, run_status;
    END IF;
    BEGIN
        INSERT INTO orchestration_jobs (
            org_id, repository_id, workflow_id, git_ref, environment_id, commit_sha, idempotency_key
        ) VALUES (
            tenant_id, current_setting('cyclopus.repository_id')::uuid, 'deploy.yml',
            'refs/heads/feature/schema', current_setting('cyclopus.environment_id')::uuid,
            repeat('9', 40), 'post-tombstone-job'
        );
        RAISE EXCEPTION 'orchestration work was created after tombstone';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
    BEGIN
        INSERT INTO reconciliation_runs (org_id, scope_kind, scope_id)
        VALUES (tenant_id, 'org', tenant_id);
        RAISE EXCEPTION 'reconciliation work was created after tombstone';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
    BEGIN
        INSERT INTO releases (
            org_id, environment_id, image_ref, kind, config_snapshot, config_hash, status
        ) VALUES (
            tenant_id, current_setting('cyclopus.environment_id')::uuid,
            'ghcr.io/acme/schema@sha256:' || repeat('a', 64), 'deploy',
            '{"version":2,"desired_state":{"version":1,"deployment":{},"service":{},"ingress":{},"scaling":{}}}', repeat('b', 64), 'failed'
        );
        RAISE EXCEPTION 'release was created after tombstone';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
    BEGIN
        UPDATE env_vars SET secret_ref = 'k8s-secret://cyclopus/runtime#post-tombstone'
        WHERE org_id = tenant_id AND environment_id = current_setting('cyclopus.environment_id')::uuid;
        RAISE EXCEPTION 'environment variable was mutated after tombstone';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
    BEGIN
        DELETE FROM apps WHERE org_id = tenant_id;
        RAISE EXCEPTION 'physical app deletion succeeded';
    EXCEPTION WHEN integrity_constraint_violation THEN
        NULL;
    END;
    BEGIN
        DELETE FROM projects WHERE org_id = tenant_id;
        RAISE EXCEPTION 'physical project deletion succeeded';
    EXCEPTION WHEN integrity_constraint_violation THEN
        NULL;
    END;
    BEGIN
        DELETE FROM orgs WHERE id = tenant_id;
        RAISE EXCEPTION 'physical org deletion succeeded';
    EXCEPTION WHEN integrity_constraint_violation THEN
        NULL;
    END;
END;
$$;
