CREATE TABLE orgs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    name text NOT NULL CHECK (btrim(name) <> '' AND octet_length(name) <= 200),
    slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    external_credentials_revoked_at timestamptz,
    deleted_at timestamptz,
    CHECK (deleted_at IS NULL OR deleted_at >= created_at),
    CHECK (external_credentials_revoked_at IS NULL OR external_credentials_revoked_at >= created_at),
    CHECK (deleted_at IS NULL OR external_credentials_revoked_at IS NOT NULL)
);

CREATE FUNCTION reject_org_physical_delete() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'organizations must be tombstoned' USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE TRIGGER orgs_tombstone_only
BEFORE DELETE ON orgs
FOR EACH ROW EXECUTE FUNCTION reject_org_physical_delete();

CREATE FUNCTION enforce_tombstone_readiness() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
        IF TG_TABLE_NAME = 'orgs' THEN
            PERFORM pg_advisory_xact_lock(hashtextextended(NEW.id::text, 1380270412));
        ELSE
            PERFORM pg_advisory_xact_lock(hashtextextended(NEW.org_id::text, 1380270412));
        END IF;
        IF NEW.external_credentials_revoked_at IS NULL THEN
            RAISE EXCEPTION 'external credential revocation must be confirmed before tombstone' USING ERRCODE = 'check_violation';
        END IF;
        IF TG_TABLE_NAME = 'orgs' THEN
            IF EXISTS (
                SELECT 1 FROM environments
                WHERE org_id = NEW.id
                  AND (in_progress_release_id IS NOT NULL OR lock_acquired_at IS NOT NULL)
            ) THEN
                RAISE EXCEPTION 'all descendant deploy leases must be externally compensated and cleared before organization tombstone' USING ERRCODE = 'check_violation';
            END IF;
            IF EXISTS (SELECT 1 FROM environments WHERE org_id = NEW.id AND torn_down_at IS NULL) THEN
                RAISE EXCEPTION 'all descendant environments must be torn down before organization tombstone' USING ERRCODE = 'check_violation';
            END IF;
            UPDATE orchestration_jobs
            SET status = 'cancelled', pending_commit_sha = NULL, pending_idempotency_key = NULL,
                lease_owner = NULL, lease_expires_at = NULL, reconcile_after = NULL,
                reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL,
                terminal_error_code = 'ancestor_tombstoned',
                terminal_error_message = 'organization was tombstoned', terminal_at = now(), updated_at = now()
            WHERE org_id = NEW.id AND status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched');
            UPDATE reconciliation_runs
            SET status = 'cancelled', lease_owner = NULL, lease_expires_at = NULL,
                error_code = 'ancestor_tombstoned', error_message = 'organization was tombstoned',
                completed_at = now()
            WHERE org_id = NEW.id AND status IN ('queued', 'leased', 'running');
        ELSIF TG_TABLE_NAME = 'projects' THEN
            IF EXISTS (
                SELECT 1 FROM environments environment
                JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
                WHERE app.org_id = NEW.org_id AND app.project_id = NEW.id
                  AND (environment.in_progress_release_id IS NOT NULL OR environment.lock_acquired_at IS NOT NULL)
            ) THEN
                RAISE EXCEPTION 'all descendant deploy leases must be externally compensated and cleared before project tombstone' USING ERRCODE = 'check_violation';
            END IF;
            IF EXISTS (
                SELECT 1 FROM environments environment
                JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
                WHERE app.org_id = NEW.org_id AND app.project_id = NEW.id AND environment.torn_down_at IS NULL
            ) THEN
                RAISE EXCEPTION 'all descendant environments must be torn down before project tombstone' USING ERRCODE = 'check_violation';
            END IF;
            UPDATE orchestration_jobs job
            SET status = 'cancelled', pending_commit_sha = NULL, pending_idempotency_key = NULL,
                lease_owner = NULL, lease_expires_at = NULL, reconcile_after = NULL,
                reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL,
                terminal_error_code = 'ancestor_tombstoned',
                terminal_error_message = 'project was tombstoned', terminal_at = now(), updated_at = now()
            FROM environments environment
            JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
            WHERE job.org_id = environment.org_id AND job.environment_id = environment.id
              AND app.org_id = NEW.org_id AND app.project_id = NEW.id
              AND job.status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched');
            UPDATE reconciliation_runs run
            SET status = 'cancelled', lease_owner = NULL, lease_expires_at = NULL,
                error_code = 'ancestor_tombstoned', error_message = 'project was tombstoned',
                completed_at = now()
            WHERE run.org_id = NEW.org_id
              AND (run.scope_kind = 'org' OR run.scope_app_id IN (
                  SELECT id FROM apps WHERE org_id = NEW.org_id AND project_id = NEW.id
              ))
              AND run.status IN ('queued', 'leased', 'running');
        ELSIF TG_TABLE_NAME = 'apps' THEN
            IF EXISTS (
                SELECT 1 FROM environments
                WHERE org_id = NEW.org_id AND app_id = NEW.id
                  AND (in_progress_release_id IS NOT NULL OR lock_acquired_at IS NOT NULL)
            ) THEN
                RAISE EXCEPTION 'all descendant deploy leases must be externally compensated and cleared before app tombstone' USING ERRCODE = 'check_violation';
            END IF;
            IF EXISTS (SELECT 1 FROM environments WHERE org_id = NEW.org_id AND app_id = NEW.id AND torn_down_at IS NULL) THEN
                RAISE EXCEPTION 'all descendant environments must be torn down before app tombstone' USING ERRCODE = 'check_violation';
            END IF;
            UPDATE orchestration_jobs job
            SET status = 'cancelled', pending_commit_sha = NULL, pending_idempotency_key = NULL,
                lease_owner = NULL, lease_expires_at = NULL, reconcile_after = NULL,
                reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL,
                terminal_error_code = 'ancestor_tombstoned',
                terminal_error_message = 'app was tombstoned', terminal_at = now(), updated_at = now()
            FROM environments environment
            WHERE job.org_id = environment.org_id AND job.environment_id = environment.id
              AND environment.org_id = NEW.org_id AND environment.app_id = NEW.id
              AND job.status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched');
            UPDATE reconciliation_runs
            SET status = 'cancelled', lease_owner = NULL, lease_expires_at = NULL,
                error_code = 'ancestor_tombstoned', error_message = 'app was tombstoned',
                completed_at = now()
            WHERE org_id = NEW.org_id AND (scope_kind = 'org' OR scope_app_id = NEW.id)
              AND status IN ('queued', 'leased', 'running');
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    email text NOT NULL UNIQUE CHECK (email = lower(email) AND btrim(email) <> '' AND octet_length(email) <= 320),
    password_hash text CHECK (password_hash IS NULL OR octet_length(password_hash) <= 1024),
    github_user_id bigint UNIQUE,
	operator boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((password_hash IS NOT NULL) OR (github_user_id IS NOT NULL)),
    UNIQUE (org_id, id)
);

-- Phase 0 deliberately permits exactly one user in one organization.
CREATE UNIQUE INDEX users_singleton ON users ((true));

CREATE FUNCTION claim_first_user(
    claimed_email text,
    claimed_password_hash text DEFAULT NULL,
    claimed_github_user_id bigint DEFAULT NULL
) RETURNS users
LANGUAGE plpgsql
AS $$
DECLARE
    claimed_org orgs;
    claimed_user users;
BEGIN
    PERFORM pg_advisory_xact_lock(1129599568, 1);
    IF EXISTS (SELECT 1 FROM users) THEN
        RAISE EXCEPTION 'cyclopus installation is already claimed' USING ERRCODE = 'unique_violation';
    END IF;

    INSERT INTO orgs (name, slug)
    VALUES ('My Organization', 'my-organization')
    RETURNING * INTO claimed_org;

    INSERT INTO users (org_id, email, password_hash, github_user_id, operator)
    VALUES (claimed_org.id, lower(claimed_email), claimed_password_hash, claimed_github_user_id, true)
    RETURNING * INTO claimed_user;

    RETURN claimed_user;
END;
$$;

CREATE FUNCTION valid_api_token_scopes(candidate_scopes text[]) RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
AS $$
    SELECT cardinality(candidate_scopes) BETWEEN 1 AND 64
       AND cardinality(candidate_scopes) = (
           SELECT count(DISTINCT candidate_scope) FROM unnest(candidate_scopes) candidate_scope
       )
       AND COALESCE((
           SELECT bool_and(candidate_scope ~ '^deploy:app:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
           FROM unnest(candidate_scopes) candidate_scope
       ), false);
$$;

CREATE TABLE api_tokens (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind text NOT NULL DEFAULT 'api' CHECK (kind IN ('session', 'api')),
    name text NOT NULL CHECK (btrim(name) <> '' AND octet_length(name) <= 100),
    token_prefix text NOT NULL UNIQUE CHECK (token_prefix ~ '^[A-Za-z0-9_-]{12}$'),
    token_hash text NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    scopes text[] NOT NULL CHECK (
        (kind = 'session' AND cardinality(scopes) = 0)
        OR (kind = 'api' AND valid_api_token_scopes(scopes) AND octet_length(array_to_string(scopes, ',')) <= 4096)
    ),
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz,
    CHECK (expires_at IS NULL OR expires_at > created_at),
    CHECK (last_used_at IS NULL OR last_used_at >= created_at),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

CREATE TABLE git_connections (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    provider text NOT NULL DEFAULT 'github' CHECK (provider = 'github'),
    installation_id bigint NOT NULL CHECK (installation_id > 0),
    repo_full_name text NOT NULL CHECK (repo_full_name ~ '^[^/[:space:]]{1,100}/[^/[:space:]]{1,100}$'),
    default_branch text NOT NULL DEFAULT 'main' CHECK (btrim(default_branch) <> '' AND octet_length(default_branch) <= 255),
    webhook_secret_ref text NOT NULL CHECK (webhook_secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, provider, installation_id, repo_full_name),
    UNIQUE (org_id, id)
);

CREATE TABLE registries (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    provider text NOT NULL CHECK (provider IN ('ghcr', 'gcr', 'ecr', 'other')),
    registry_url text NOT NULL CHECK (btrim(registry_url) <> '' AND octet_length(registry_url) <= 2048),
    credential_secret_ref text NOT NULL CHECK (credential_secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, registry_url),
    UNIQUE (org_id, id)
);

CREATE TABLE projects (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (btrim(name) <> '' AND octet_length(name) <= 200),
    slug text NOT NULL CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    git_connection_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    external_credentials_revoked_at timestamptz,
    deleted_at timestamptz,
    UNIQUE (org_id, slug),
    UNIQUE (org_id, id),
    FOREIGN KEY (org_id, git_connection_id) REFERENCES git_connections(org_id, id) ON DELETE SET NULL (git_connection_id),
    CHECK (deleted_at IS NULL OR deleted_at >= created_at),
    CHECK (external_credentials_revoked_at IS NULL OR external_credentials_revoked_at >= created_at),
    CHECK (deleted_at IS NULL OR external_credentials_revoked_at IS NOT NULL)
);

CREATE TABLE apps (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL CHECK (btrim(name) <> '' AND octet_length(name) <= 200),
    slug text NOT NULL CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    hostname_label text NOT NULL UNIQUE CHECK (hostname_label ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    registry_id uuid,
    image_repository text CHECK (image_repository IS NULL OR octet_length(image_repository) <= 255),
    container_port integer NOT NULL DEFAULT 8080 CHECK (container_port BETWEEN 1 AND 65535),
    health_check_path text NOT NULL DEFAULT '/' CHECK (health_check_path LIKE '/%' AND octet_length(health_check_path) <= 2048),
    scaling_mode text NOT NULL DEFAULT 'hpa' CHECK (scaling_mode IN ('hpa', 'keda_cron', 'keda_http')),
    min_replicas integer NOT NULL DEFAULT 1 CHECK (min_replicas >= 0),
    max_replicas integer NOT NULL DEFAULT 3 CHECK (max_replicas >= 1),
    hpa_target_cpu_percent integer NOT NULL DEFAULT 70 CHECK (hpa_target_cpu_percent BETWEEN 1 AND 100),
    cpu_request_millicores integer NOT NULL DEFAULT 100 CHECK (cpu_request_millicores > 0),
    cpu_limit_millicores integer NOT NULL DEFAULT 1000 CHECK (cpu_limit_millicores > 0),
    memory_request_mib integer NOT NULL DEFAULT 128 CHECK (memory_request_mib > 0),
    memory_limit_mib integer NOT NULL DEFAULT 512 CHECK (memory_limit_mib > 0),
    previews_enabled boolean NOT NULL DEFAULT true,
    max_preview_environments integer NOT NULL DEFAULT 10 CHECK (max_preview_environments >= 0),
    preview_idle_ttl_hours integer NOT NULL DEFAULT 168 CHECK (preview_idle_ttl_hours > 0),
    deploy_timeout_seconds integer NOT NULL DEFAULT 600 CHECK (deploy_timeout_seconds > 0),
    release_retention_count integer NOT NULL DEFAULT 5 CHECK (release_retention_count >= 1),
    runtime_class text NOT NULL DEFAULT 'gvisor' CHECK (runtime_class IN ('gvisor', 'runc')),
    ci_workflow_id text CHECK (ci_workflow_id IS NULL OR (btrim(ci_workflow_id) <> '' AND octet_length(ci_workflow_id) <= 255)),
    created_at timestamptz NOT NULL DEFAULT now(),
    external_credentials_revoked_at timestamptz,
    deleted_at timestamptz,
    UNIQUE (project_id, slug),
    UNIQUE (org_id, id),
    FOREIGN KEY (org_id, project_id) REFERENCES projects(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, registry_id) REFERENCES registries(org_id, id) ON DELETE RESTRICT,
    CHECK ((registry_id IS NULL) = (image_repository IS NULL)),
    CHECK (image_repository IS NULL OR btrim(image_repository) <> ''),
    CHECK (max_replicas >= GREATEST(min_replicas, 1)),
    CHECK (cpu_limit_millicores >= cpu_request_millicores),
    CHECK (memory_limit_mib >= memory_request_mib),
    CONSTRAINT hpa_requires_floor CHECK (scaling_mode <> 'hpa' OR min_replicas >= 1),
    CHECK (max_preview_environments <= 1000),
    CHECK (preview_idle_ttl_hours <= 8760),
    CHECK (deploy_timeout_seconds <= 86400),
    CHECK (release_retention_count <= 1000),
    CHECK (deleted_at IS NULL OR deleted_at >= created_at),
    CHECK (external_credentials_revoked_at IS NULL OR external_credentials_revoked_at >= created_at),
    CHECK (deleted_at IS NULL OR external_credentials_revoked_at IS NOT NULL)
);

CREATE TABLE environments (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    app_id uuid NOT NULL,
    branch text NOT NULL CHECK (btrim(branch) <> '' AND octet_length(branch) <= 255),
    kind text NOT NULL CHECK (kind IN ('production', 'preview')),
    preview_slug text,
    namespace text NOT NULL CHECK (namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
    planned_hostname text NOT NULL UNIQUE CHECK (planned_hostname = lower(planned_hostname) AND btrim(planned_hostname) <> '' AND octet_length(planned_hostname) <= 253),
    lock_acquired_at timestamptz,
    deploy_recovery_checkpoint text CHECK (deploy_recovery_checkpoint IS NULL OR deploy_recovery_checkpoint IN (
        'claimed', 'first_deployment_missing', 'late_ready_observed',
        'secret_revision_verified', 'deployment_restored', 'desired_state_restored'
    )),
    pending_commit_sha text CHECK (pending_commit_sha IS NULL OR pending_commit_sha ~ '^[0-9a-f]{40}$'),
    deploy_recovery_error_code text CHECK (deploy_recovery_error_code IS NULL OR octet_length(deploy_recovery_error_code) <= 200),
    deploy_recovery_error_message text CHECK (deploy_recovery_error_message IS NULL OR octet_length(deploy_recovery_error_message) <= 8192),
    deploy_recovery_quarantined_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    torn_down_at timestamptz,
    UNIQUE (org_id, id),
    UNIQUE (org_id, app_id, id),
    FOREIGN KEY (org_id, app_id) REFERENCES apps(org_id, id) ON DELETE RESTRICT,
    CHECK ((kind = 'preview') = (preview_slug IS NOT NULL)),
    CHECK ((deploy_recovery_quarantined_at IS NULL AND deploy_recovery_error_code IS NULL AND deploy_recovery_error_message IS NULL)
        OR (deploy_recovery_quarantined_at IS NOT NULL AND deploy_recovery_error_code IS NOT NULL AND deploy_recovery_error_message IS NOT NULL)),
    CHECK (preview_slug IS NULL OR preview_slug ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
    CHECK (torn_down_at IS NULL OR torn_down_at >= created_at)
);

CREATE TRIGGER orgs_tombstone_ready
BEFORE UPDATE OF deleted_at ON orgs
FOR EACH ROW EXECUTE FUNCTION enforce_tombstone_readiness();

CREATE TRIGGER projects_tombstone_ready
BEFORE UPDATE OF deleted_at ON projects
FOR EACH ROW EXECUTE FUNCTION enforce_tombstone_readiness();

CREATE TRIGGER apps_tombstone_ready
BEFORE UPDATE OF deleted_at ON apps
FOR EACH ROW EXECUTE FUNCTION enforce_tombstone_readiness();

CREATE TRIGGER projects_tombstone_only
BEFORE DELETE ON projects
FOR EACH ROW EXECUTE FUNCTION reject_org_physical_delete();

CREATE TRIGGER apps_tombstone_only
BEFORE DELETE ON apps
FOR EACH ROW EXECUTE FUNCTION reject_org_physical_delete();

CREATE UNIQUE INDEX environments_active_branch
    ON environments (app_id, branch)
    WHERE torn_down_at IS NULL;

CREATE UNIQUE INDEX environments_preview_slug
    ON environments (preview_slug)
    WHERE preview_slug IS NOT NULL;

CREATE UNIQUE INDEX environments_namespace
    ON environments (namespace);

CREATE TABLE env_var_revisions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    created_by_user_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (environment_id, revision),
    UNIQUE (id, environment_id),
    UNIQUE (org_id, environment_id, id),
    FOREIGN KEY (org_id, environment_id) REFERENCES environments(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, created_by_user_id) REFERENCES users(org_id, id) ON DELETE SET NULL (created_by_user_id)
);

CREATE TABLE env_var_revision_entries (
    org_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    env_var_revision_id uuid NOT NULL,
    key text NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]{0,252}$'),
    secret_ref text NOT NULL CHECK (secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    is_secret boolean NOT NULL DEFAULT true,
    PRIMARY KEY (env_var_revision_id, key),
    FOREIGN KEY (org_id, environment_id, env_var_revision_id)
        REFERENCES env_var_revisions(org_id, environment_id, id)
        ON DELETE RESTRICT
);

CREATE FUNCTION reject_env_var_revision_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'environment variable revisions are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE FUNCTION reject_published_env_var_revision_entry() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM environments
        WHERE current_env_var_revision_id = NEW.env_var_revision_id
    ) OR EXISTS (
        SELECT 1 FROM releases
        WHERE env_var_revision_id = NEW.env_var_revision_id
    ) THEN
        RAISE EXCEPTION 'published environment variable revisions are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER env_var_revisions_append_only
BEFORE UPDATE OR DELETE ON env_var_revisions
FOR EACH ROW EXECUTE FUNCTION reject_env_var_revision_mutation();

CREATE TRIGGER env_var_revision_entries_append_only
BEFORE UPDATE OR DELETE ON env_var_revision_entries
FOR EACH ROW EXECUTE FUNCTION reject_env_var_revision_mutation();

ALTER TABLE environments
    ADD COLUMN current_env_var_revision_id uuid,
    ADD CONSTRAINT environments_current_env_var_revision_fk
        FOREIGN KEY (current_env_var_revision_id, id)
        REFERENCES env_var_revisions(id, environment_id)
        ON DELETE RESTRICT;

CREATE TABLE releases (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    image_ref text NOT NULL CHECK (image_ref ~ '^.+@sha256:[0-9a-f]{64}$' AND octet_length(image_ref) <= 2120),
    git_commit_sha text CHECK (git_commit_sha IS NULL OR git_commit_sha ~ '^[0-9a-f]{40}$'),
    triggered_by_user_id uuid,
    kind text NOT NULL CHECK (kind IN ('deploy', 'env_change', 'rollback', 'reconcile')),
    env_var_revision_id uuid,
    config_snapshot jsonb NOT NULL CHECK (jsonb_typeof(config_snapshot) = 'object' AND octet_length(config_snapshot::text) <= 1048576),
    config_hash text NOT NULL CHECK (config_hash ~ '^[0-9a-f]{64}$'),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'deploying', 'live', 'failed', 'superseded')),
    failure_reason text CHECK (failure_reason IS NULL OR octet_length(failure_reason) <= 8192),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, environment_id),
    UNIQUE (org_id, id),
    UNIQUE (org_id, environment_id, id),
    FOREIGN KEY (org_id, environment_id) REFERENCES environments(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, environment_id, env_var_revision_id)
        REFERENCES env_var_revisions(org_id, environment_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, triggered_by_user_id) REFERENCES users(org_id, id) ON DELETE SET NULL (triggered_by_user_id),
    CHECK ((status = 'failed') OR failure_reason IS NULL)
);

CREATE FUNCTION validate_release_snapshot() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.config_snapshot->>'version' IS DISTINCT FROM '2'
       OR jsonb_typeof(NEW.config_snapshot->'desired_state') <> 'object'
       OR NEW.config_snapshot#>>'{desired_state,version}' IS DISTINCT FROM '1'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,deployment}') <> 'object'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,service}') <> 'object'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,ingress}') <> 'object'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,scaling}') <> 'object' THEN
        RAISE EXCEPTION 'release config_snapshot must be a v2 desired-state envelope'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.kind = 'env_change' AND (
        NEW.env_var_revision_id IS NULL
        OR jsonb_typeof(NEW.config_snapshot->'env_var_revision') <> 'object'
        OR NEW.config_snapshot#>>'{env_var_revision,id}' IS DISTINCT FROM NEW.env_var_revision_id::text
    ) THEN
        RAISE EXCEPTION 'env-change release must contain its env-var revision'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER releases_snapshot_valid
BEFORE INSERT ON releases
FOR EACH ROW EXECUTE FUNCTION validate_release_snapshot();

CREATE TRIGGER env_var_revision_entries_published_immutable
BEFORE INSERT ON env_var_revision_entries
FOR EACH ROW EXECUTE FUNCTION reject_published_env_var_revision_entry();

CREATE UNIQUE INDEX releases_one_live_per_environment
    ON releases (environment_id)
    WHERE status = 'live';

CREATE FUNCTION reject_release_snapshot_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.org_id IS DISTINCT FROM OLD.org_id
       OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
       OR NEW.image_ref IS DISTINCT FROM OLD.image_ref
       OR NEW.git_commit_sha IS DISTINCT FROM OLD.git_commit_sha
       OR NEW.triggered_by_user_id IS DISTINCT FROM OLD.triggered_by_user_id
       OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.env_var_revision_id IS DISTINCT FROM OLD.env_var_revision_id
       OR NEW.config_snapshot IS DISTINCT FROM OLD.config_snapshot
       OR NEW.config_hash IS DISTINCT FROM OLD.config_hash
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'release snapshots are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER releases_snapshot_immutable
BEFORE UPDATE ON releases
FOR EACH ROW EXECUTE FUNCTION reject_release_snapshot_mutation();

ALTER TABLE environments
    ADD COLUMN in_progress_release_id uuid,
    ADD CONSTRAINT environments_in_progress_release_fk
        FOREIGN KEY (in_progress_release_id, id)
        REFERENCES releases(id, environment_id)
        ON DELETE SET NULL (in_progress_release_id),
    ADD CONSTRAINT environments_lease_consistent
    CHECK ((in_progress_release_id IS NULL) = (lock_acquired_at IS NULL));

CREATE TABLE aliases (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    hostname text NOT NULL UNIQUE CHECK (hostname = lower(hostname) AND btrim(hostname) <> '' AND octet_length(hostname) <= 253),
    release_id uuid NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (environment_id),
    FOREIGN KEY (org_id, environment_id)
        REFERENCES environments(org_id, id)
        ON DELETE RESTRICT,
    FOREIGN KEY (org_id, environment_id, release_id)
        REFERENCES releases(org_id, environment_id, id)
        ON DELETE RESTRICT
);

CREATE TABLE domains (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    app_id uuid NOT NULL,
    hostname text NOT NULL UNIQUE CHECK (hostname = lower(hostname) AND btrim(hostname) <> '' AND octet_length(hostname) <= 253),
    verification_method text NOT NULL CHECK (verification_method IN ('dns_txt', 'cname')),
    verification_token text CHECK (verification_token IS NULL OR octet_length(verification_token) <= 1024),
    verified_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, app_id) REFERENCES apps(org_id, id) ON DELETE RESTRICT,
    CHECK ((verification_method = 'dns_txt') = (verification_token IS NOT NULL))
);

CREATE TABLE env_vars (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    key text NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]{0,252}$'),
    secret_ref text NOT NULL CHECK (secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    is_secret boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (environment_id, key),
    FOREIGN KEY (org_id, environment_id) REFERENCES environments(org_id, id) ON DELETE RESTRICT
);

CREATE TABLE storage_destinations (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    purpose text NOT NULL CHECK (purpose IN ('postgres_backup', 'log_drain', 'observability_backing')),
    endpoint text NOT NULL CHECK (btrim(endpoint) <> '' AND octet_length(endpoint) <= 2048),
    bucket text NOT NULL CHECK (btrim(bucket) <> '' AND octet_length(bucket) <= 255),
    credential_secret_ref text NOT NULL CHECK (credential_secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, purpose)
);

CREATE TABLE webhooks (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    target_url text NOT NULL CHECK (target_url ~ '^https://[^[:space:]]+$' AND octet_length(target_url) <= 2047),
    events text[] NOT NULL DEFAULT '{}' CHECK (cardinality(events) <= 64 AND octet_length(array_to_string(events, ',')) <= 4096),
    secret_ref text NOT NULL CHECK (secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    disabled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    webhook_id uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event text NOT NULL CHECK (btrim(event) <> '' AND octet_length(event) <= 200),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 1048576),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt BETWEEN 0 AND 6),
    next_attempt_at timestamptz,
    last_status_code integer CHECK (last_status_code BETWEEN 100 AND 599),
    last_error text CHECK (last_error IS NULL OR octet_length(last_error) <= 8192),
    delivered_at timestamptz,
    abandoned_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(delivered_at, abandoned_at) <= 1)
);

CREATE INDEX webhook_deliveries_pending
    ON webhook_deliveries (next_attempt_at)
    WHERE delivered_at IS NULL AND abandoned_at IS NULL;

CREATE TABLE audit_events (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    actor_user_id uuid,
    action text NOT NULL CHECK (btrim(action) <> '' AND octet_length(action) <= 200),
    target_type text NOT NULL CHECK (btrim(target_type) <> '' AND octet_length(target_type) <= 100),
    target_id uuid NOT NULL,
    before jsonb,
    after jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, actor_user_id) REFERENCES users(org_id, id) ON DELETE SET NULL (actor_user_id),
    CHECK (before IS NULL OR octet_length(before::text) <= 1048576),
    CHECK (after IS NULL OR octet_length(after::text) <= 1048576),
    CHECK (before IS NULL OR before::text !~ 'cyp_v1_[A-Za-z0-9_-]{12}_[A-Za-z0-9_-]{43}'),
    CHECK (after IS NULL OR after::text !~ 'cyp_v1_[A-Za-z0-9_-]{12}_[A-Za-z0-9_-]{43}')
);

CREATE INDEX audit_events_org_created ON audit_events (org_id, created_at DESC, id DESC);

CREATE FUNCTION reject_audit_event_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'audit events are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE TRIGGER audit_events_append_only
BEFORE UPDATE OR DELETE ON audit_events
FOR EACH ROW EXECUTE FUNCTION reject_audit_event_mutation();

CREATE TABLE api_idempotency_records (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    scope text NOT NULL CHECK (btrim(scope) <> '' AND octet_length(scope) <= 200),
    idempotency_key text NOT NULL CHECK (btrim(idempotency_key) <> '' AND octet_length(idempotency_key) <= 255),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    status text NOT NULL DEFAULT 'in_progress' CHECK (status IN ('in_progress', 'completed', 'failed')),
    response_code integer CHECK (response_code BETWEEN 100 AND 599),
    response_body jsonb,
    resource_type text CHECK (resource_type IS NULL OR resource_type IN (
        'org', 'user', 'api_token', 'git_connection', 'registry', 'project', 'app',
        'environment', 'release', 'alias', 'domain', 'env_var', 'env_var_revision',
        'storage_destination', 'webhook', 'reconciliation_run', 'orchestration_job'
    )),
    resource_org_id uuid,
    resource_id uuid,
    error_code text CHECK (error_code IS NULL OR octet_length(error_code) <= 200),
    lease_owner text CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    lease_expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    UNIQUE (org_id, id),
    UNIQUE (org_id, scope, idempotency_key),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CHECK (status <> 'in_progress' OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK (status = 'in_progress' OR response_code IS NOT NULL),
    CHECK (num_nonnulls(resource_type, resource_id) IN (0, 2)),
    CHECK (resource_org_id IS NULL OR resource_org_id = org_id),
    CHECK (expires_at > created_at),
    CHECK (response_body IS NULL OR response_body::text !~ 'cyp_v1_[A-Za-z0-9_-]{12}_[A-Za-z0-9_-]{43}')
);

CREATE FUNCTION bind_idempotency_resource_tenant() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.resource_type IS NULL THEN
        NEW.resource_org_id := NULL;
    ELSIF NEW.resource_org_id IS NULL THEN
        NEW.resource_org_id := NEW.org_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER api_idempotency_resource_tenant
BEFORE INSERT OR UPDATE OF org_id, resource_type, resource_org_id, resource_id
ON api_idempotency_records
FOR EACH ROW EXECUTE FUNCTION bind_idempotency_resource_tenant();

CREATE INDEX api_idempotency_records_expiry ON api_idempotency_records (expires_at);

CREATE FUNCTION valid_reconciliation_categories(candidate_categories text[]) RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
AS $$
    SELECT cardinality(candidate_categories) <= 9
       AND cardinality(candidate_categories) = (
           SELECT count(DISTINCT category) FROM unnest(candidate_categories) category
       )
       AND COALESCE((
           SELECT bool_and(category = ANY (ARRAY[
               'database','namespace','workload','network_policy','ingress','tls','scaling',
               'environment_variables','orchestration'
           ]::text[])) FROM unnest(candidate_categories) category
       ), true);
$$;

CREATE TABLE reconciliation_runs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    actor_user_id uuid,
    scope_kind text NOT NULL CHECK (scope_kind IN ('environment', 'app', 'org')),
    scope_id uuid NOT NULL,
    scope_app_id uuid,
    scope_environment_id uuid,
    dry_run boolean NOT NULL DEFAULT true,
    prune boolean NOT NULL DEFAULT false,
    categories text[] NOT NULL DEFAULT '{}' CHECK (valid_reconciliation_categories(categories)),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'leased', 'running', 'completed', 'failed', 'cancelled')),
    plan jsonb CHECK (plan IS NULL OR octet_length(plan::text) <= 1048576),
    report jsonb CHECK (report IS NULL OR octet_length(report::text) <= 1048576),
    lease_owner text CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    lease_expires_at timestamptz,
    error_code text CHECK (error_code IS NULL OR octet_length(error_code) <= 200),
    error_message text CHECK (error_message IS NULL OR octet_length(error_message) <= 8192),
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    completed_at timestamptz,
    FOREIGN KEY (org_id, actor_user_id) REFERENCES users(org_id, id) ON DELETE SET NULL (actor_user_id),
    FOREIGN KEY (org_id, scope_app_id) REFERENCES apps(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, scope_app_id, scope_environment_id) REFERENCES environments(org_id, app_id, id) ON DELETE RESTRICT,
    CHECK ((scope_kind = 'org' AND scope_id = org_id AND scope_app_id IS NULL AND scope_environment_id IS NULL)
        OR (scope_kind = 'app' AND scope_id = scope_app_id AND scope_app_id IS NOT NULL AND scope_environment_id IS NULL)
        OR (scope_kind = 'environment' AND scope_id = scope_environment_id AND scope_app_id IS NOT NULL AND scope_environment_id IS NOT NULL)),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CHECK (status NOT IN ('leased', 'running') OR lease_owner IS NOT NULL),
    CHECK (status NOT IN ('completed', 'failed', 'cancelled') OR completed_at IS NOT NULL)
);

CREATE INDEX reconciliation_runs_active_scope
    ON reconciliation_runs (org_id, scope_kind, scope_id)
    WHERE status IN ('queued', 'leased', 'running');

CREATE FUNCTION enforce_reconciliation_scope_single_flight() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.status NOT IN ('queued', 'leased', 'running') THEN
        RETURN NEW;
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.org_id::text, 1380270412));

    IF EXISTS (
        SELECT 1
        FROM reconciliation_runs existing
        WHERE existing.org_id = NEW.org_id
          AND existing.id <> NEW.id
          AND existing.status IN ('queued', 'leased', 'running')
          AND (
              existing.scope_kind = 'org'
              OR NEW.scope_kind = 'org'
              OR (existing.scope_app_id = NEW.scope_app_id AND (
                  existing.scope_kind = 'app'
                  OR NEW.scope_kind = 'app'
                  OR existing.scope_environment_id = NEW.scope_environment_id
              ))
          )
    ) THEN
        RAISE EXCEPTION 'overlapping reconciliation scope is already active' USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER reconciliation_runs_scope_single_flight
BEFORE INSERT OR UPDATE OF org_id, scope_kind, scope_id, scope_app_id, scope_environment_id, status
ON reconciliation_runs
FOR EACH ROW EXECUTE FUNCTION enforce_reconciliation_scope_single_flight();

CREATE TABLE reconciliation_evidence (
    reconciliation_run_id uuid NOT NULL REFERENCES reconciliation_runs(id) ON DELETE CASCADE,
    sequence bigint GENERATED ALWAYS AS IDENTITY,
    evidence_kind text NOT NULL CHECK (btrim(evidence_kind) <> '' AND octet_length(evidence_kind) <= 100),
    object_kind text CHECK (object_kind IS NULL OR octet_length(object_kind) <= 100),
    object_namespace text CHECK (object_namespace IS NULL OR octet_length(object_namespace) <= 63),
    object_name text CHECK (object_name IS NULL OR octet_length(object_name) <= 253),
    action text CHECK (action IS NULL OR octet_length(action) <= 100),
    desired_hash text,
    observed_hash text,
    details jsonb NOT NULL DEFAULT '{}' CHECK (octet_length(details::text) <= 1048576),
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (reconciliation_run_id, sequence)
);

CREATE FUNCTION reject_reconciliation_evidence_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation evidence is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE TRIGGER reconciliation_evidence_append_only
BEFORE UPDATE OR DELETE ON reconciliation_evidence
FOR EACH ROW EXECUTE FUNCTION reject_reconciliation_evidence_mutation();

CREATE TABLE orchestration_jobs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    repository_id uuid NOT NULL,
    workflow_id text NOT NULL CHECK (btrim(workflow_id) <> '' AND octet_length(workflow_id) <= 255),
    git_ref text NOT NULL CHECK (btrim(git_ref) <> '' AND octet_length(git_ref) <= 1024),
    environment_id uuid NOT NULL,
    commit_sha text NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    idempotency_key text NOT NULL CHECK (btrim(idempotency_key) <> '' AND octet_length(idempotency_key) <= 255),
    pending_commit_sha text CHECK (pending_commit_sha IS NULL OR pending_commit_sha ~ '^[0-9a-f]{40}$'),
    pending_idempotency_key text CHECK (pending_idempotency_key IS NULL OR (btrim(pending_idempotency_key) <> '' AND octet_length(pending_idempotency_key) <= 255)),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'completed', 'failed', 'blocked', 'cancelled')),
    lease_owner text CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    lease_expires_at timestamptz,
    dispatch_requested_at timestamptz,
    reconcile_after timestamptz,
    dispatch_deadline_at timestamptz,
    run_deadline_at timestamptz,
    reconciliation_owner text CHECK (reconciliation_owner IS NULL OR (btrim(reconciliation_owner) <> '' AND octet_length(reconciliation_owner) <= 255)),
    reconciliation_lease_expires_at timestamptz,
    github_dispatch_identity text CHECK (github_dispatch_identity IS NULL OR octet_length(github_dispatch_identity) <= 255),
    github_run_id bigint,
    github_run_status text CHECK (github_run_status IS NULL OR octet_length(github_run_status) <= 100),
    github_run_conclusion text CHECK (github_run_conclusion IS NULL OR octet_length(github_run_conclusion) <= 100),
    github_status_observed_at timestamptz,
    terminal_error_code text CHECK (terminal_error_code IS NULL OR octet_length(terminal_error_code) <= 200),
    terminal_error_message text CHECK (terminal_error_message IS NULL OR octet_length(terminal_error_message) <= 8192),
    terminal_at timestamptz,
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, idempotency_key),
    UNIQUE (org_id, pending_idempotency_key),
    FOREIGN KEY (org_id, repository_id) REFERENCES git_connections(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, environment_id) REFERENCES environments(org_id, id) ON DELETE RESTRICT,
    CHECK ((status = 'leased') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK ((pending_commit_sha IS NULL) = (pending_idempotency_key IS NULL)),
    CHECK (status <> 'dispatch_reconciling' OR (dispatch_requested_at IS NOT NULL AND reconcile_after IS NOT NULL AND dispatch_deadline_at IS NOT NULL)),
    CHECK (status <> 'dispatched' OR (github_run_id IS NOT NULL AND run_deadline_at IS NOT NULL)),
    CHECK ((reconciliation_owner IS NULL) = (reconciliation_lease_expires_at IS NULL))
);

CREATE TABLE orchestration_requests (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    idempotency_key text NOT NULL CHECK (btrim(idempotency_key) <> '' AND octet_length(idempotency_key) <= 255),
    request_hash text CHECK (request_hash IS NULL OR request_hash ~ '^[0-9a-f]{64}$'),
    orchestration_job_id uuid NOT NULL,
    repository_id uuid NOT NULL,
    workflow_id text NOT NULL CHECK (btrim(workflow_id) <> '' AND octet_length(workflow_id) <= 255),
    git_ref text NOT NULL CHECK (btrim(git_ref) <> '' AND octet_length(git_ref) <= 1024),
    environment_id uuid NOT NULL,
    commit_sha text NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, idempotency_key),
    FOREIGN KEY (org_id, orchestration_job_id) REFERENCES orchestration_jobs(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, repository_id) REFERENCES git_connections(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, environment_id) REFERENCES environments(org_id, id) ON DELETE RESTRICT
);

CREATE FUNCTION reject_orchestration_request_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'orchestration requests are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE TRIGGER orchestration_requests_append_only
BEFORE UPDATE OR DELETE ON orchestration_requests
FOR EACH ROW EXECUTE FUNCTION reject_orchestration_request_mutation();

CREATE FUNCTION enforce_orchestration_idempotency_namespace() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.org_id::text, 1330205774));

    IF NEW.pending_idempotency_key = NEW.idempotency_key OR EXISTS (
        SELECT 1
        FROM orchestration_jobs existing
        WHERE existing.org_id = NEW.org_id
          AND existing.id <> NEW.id
          AND (
              existing.idempotency_key IN (NEW.idempotency_key, NEW.pending_idempotency_key)
              OR existing.pending_idempotency_key IN (NEW.idempotency_key, NEW.pending_idempotency_key)
          )
    ) THEN
        RAISE EXCEPTION 'orchestration idempotency key is already in use' USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER orchestration_jobs_idempotency_namespace
BEFORE INSERT OR UPDATE OF org_id, idempotency_key, pending_idempotency_key
ON orchestration_jobs
FOR EACH ROW EXECUTE FUNCTION enforce_orchestration_idempotency_namespace();

CREATE TABLE orchestration_job_run_evidence (
    orchestration_job_id uuid NOT NULL REFERENCES orchestration_jobs(id) ON DELETE CASCADE,
    attempt integer NOT NULL CHECK (attempt > 0),
    github_run_id bigint,
    github_run_status text NOT NULL CHECK (btrim(github_run_status) <> ''),
    github_run_conclusion text,
    observed_at timestamptz NOT NULL,
    terminal_error_code text,
    terminal_error_message text,
    terminal_at timestamptz,
    PRIMARY KEY (orchestration_job_id, attempt, observed_at)
);

CREATE UNIQUE INDEX orchestration_jobs_scope_single_flight
    ON orchestration_jobs (org_id, repository_id, workflow_id, git_ref, environment_id)
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

CREATE FUNCTION enforce_active_work_scope() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    scoped_org_id uuid;
    scoped_app_id uuid;
    scoped_environment_id uuid;
BEGIN
    IF TG_TABLE_NAME = 'orchestration_jobs' THEN
        IF TG_OP <> 'INSERT' AND NEW.status NOT IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched') THEN
            RETURN NEW;
        END IF;
        scoped_org_id := NEW.org_id;
        scoped_environment_id := NEW.environment_id;
    ELSIF TG_TABLE_NAME = 'reconciliation_runs' THEN
        IF TG_OP <> 'INSERT' AND NEW.status NOT IN ('queued', 'leased', 'running') THEN
            RETURN NEW;
        END IF;
        scoped_org_id := NEW.org_id;
        scoped_app_id := NEW.scope_app_id;
        scoped_environment_id := NEW.scope_environment_id;
    ELSIF TG_TABLE_NAME = 'releases' THEN
        IF TG_OP <> 'INSERT' AND NEW.status NOT IN ('pending', 'deploying') THEN
            RETURN NEW;
        END IF;
        scoped_org_id := NEW.org_id;
        scoped_environment_id := NEW.environment_id;
    ELSIF TG_TABLE_NAME IN ('env_vars', 'env_var_revisions') THEN
        scoped_org_id := NEW.org_id;
        scoped_environment_id := NEW.environment_id;
    ELSE
        scoped_org_id := NEW.org_id;
        scoped_environment_id := NEW.id;
    END IF;

    PERFORM 1 FROM orgs WHERE id = scoped_org_id AND deleted_at IS NULL FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'organization is tombstoned' USING ERRCODE = 'check_violation';
    END IF;

    IF scoped_environment_id IS NOT NULL THEN
        PERFORM 1
        FROM environments environment
        JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
        JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
        WHERE environment.org_id = scoped_org_id AND environment.id = scoped_environment_id
          AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL AND project.deleted_at IS NULL
        FOR KEY SHARE OF environment, app, project;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'work scope has a tombstoned ancestor or torn-down environment' USING ERRCODE = 'check_violation';
        END IF;
    ELSIF scoped_app_id IS NOT NULL THEN
        PERFORM 1
        FROM apps app
        JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
        WHERE app.org_id = scoped_org_id AND app.id = scoped_app_id
          AND app.deleted_at IS NULL AND project.deleted_at IS NULL
        FOR KEY SHARE OF app, project;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'work scope has a tombstoned ancestor' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER orchestration_jobs_active_scope
BEFORE INSERT OR UPDATE OF status, org_id, environment_id ON orchestration_jobs
FOR EACH ROW EXECUTE FUNCTION enforce_active_work_scope();

CREATE TRIGGER reconciliation_runs_active_scope
BEFORE INSERT OR UPDATE OF status, org_id, scope_app_id, scope_environment_id ON reconciliation_runs
FOR EACH ROW EXECUTE FUNCTION enforce_active_work_scope();

CREATE TRIGGER releases_active_scope
BEFORE INSERT OR UPDATE OF status, org_id, environment_id ON releases
FOR EACH ROW EXECUTE FUNCTION enforce_active_work_scope();

CREATE TRIGGER environments_deploy_lease_active_scope
BEFORE UPDATE OF in_progress_release_id, lock_acquired_at ON environments
FOR EACH ROW
WHEN (NEW.in_progress_release_id IS NOT NULL OR NEW.lock_acquired_at IS NOT NULL)
EXECUTE FUNCTION enforce_active_work_scope();

CREATE FUNCTION enforce_environment_teardown_readiness() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.torn_down_at IS NULL AND NEW.torn_down_at IS NOT NULL
       AND (OLD.in_progress_release_id IS NOT NULL OR OLD.lock_acquired_at IS NOT NULL) THEN
        RAISE EXCEPTION 'deploy lease must be externally compensated and cleared before environment teardown' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER environments_teardown_ready
BEFORE UPDATE OF torn_down_at ON environments
FOR EACH ROW EXECUTE FUNCTION enforce_environment_teardown_readiness();

CREATE TRIGGER env_vars_active_scope
BEFORE INSERT OR UPDATE ON env_vars
FOR EACH ROW EXECUTE FUNCTION enforce_active_work_scope();

CREATE TRIGGER env_var_revisions_active_scope
BEFORE INSERT ON env_var_revisions
FOR EACH ROW EXECUTE FUNCTION enforce_active_work_scope();

CREATE FUNCTION enforce_active_env_var_revision_entry_scope() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM 1
    FROM env_var_revisions revision
    JOIN environments environment ON environment.org_id = revision.org_id AND environment.id = revision.environment_id
    JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
    JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
    JOIN orgs org ON org.id = revision.org_id
    WHERE revision.id = NEW.env_var_revision_id AND revision.org_id = NEW.org_id
      AND revision.environment_id = NEW.environment_id AND environment.torn_down_at IS NULL
      AND app.deleted_at IS NULL AND project.deleted_at IS NULL AND org.deleted_at IS NULL
    FOR KEY SHARE OF environment, app, project, org;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'environment variable scope has a tombstoned ancestor or torn-down environment' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER env_var_revision_entries_active_scope
BEFORE INSERT ON env_var_revision_entries
FOR EACH ROW EXECUTE FUNCTION enforce_active_env_var_revision_entry_scope();

CREATE TRIGGER environments_env_revision_active_scope
BEFORE UPDATE OF current_env_var_revision_id ON environments
FOR EACH ROW
WHEN (NEW.current_env_var_revision_id IS NOT NULL)
EXECUTE FUNCTION enforce_active_work_scope();
