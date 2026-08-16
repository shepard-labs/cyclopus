-- BREAKING ALPHA SCHEMA EPOCH: incompatible GitHub/import/deployment data is
-- intentionally discarded. Identity and compatible platform definitions remain.
TRUNCATE TABLE git_connections, registries, projects CASCADE;

DROP TABLE orchestration_job_run_evidence;
DROP TABLE orchestration_requests;
DROP TRIGGER orchestration_jobs_active_scope ON orchestration_jobs;
DROP TABLE orchestration_jobs;
DROP FUNCTION reject_orchestration_request_mutation();
DROP FUNCTION enforce_orchestration_idempotency_namespace();

ALTER TABLE projects DROP CONSTRAINT projects_org_id_git_connection_id_fkey;
DROP TABLE git_connections;

ALTER TABLE apps DROP CONSTRAINT apps_org_id_registry_id_fkey;
DROP TABLE registries;

CREATE TABLE schema_epoch (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    epoch text NOT NULL CHECK (epoch ~ '^[a-z0-9][a-z0-9_-]{0,99}$'),
    fingerprint text NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_epoch (epoch, fingerprint)
VALUES ('self_hosted_github_import_v1', '95e56712029f59eb49b6b0bd939cfbb2dc62360772731fabec7bf2cb2d7d290e');

CREATE TABLE github_app (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    github_app_id bigint UNIQUE CHECK (github_app_id > 0),
    app_slug text UNIQUE CHECK (app_slug IS NULL OR app_slug ~ '^[A-Za-z0-9][A-Za-z0-9-]{0,99}$'),
    app_name text CHECK (app_name IS NULL OR (btrim(app_name) <> '' AND octet_length(app_name) <= 200)),
    app_url text CHECK (app_url IS NULL OR (app_url ~ '^https://github\.com/apps/[A-Za-z0-9-]+$' AND octet_length(app_url) <= 2048)),
    owner_id bigint CHECK (owner_id > 0),
    owner_login text CHECK (owner_login IS NULL OR (btrim(owner_login) <> '' AND octet_length(owner_login) <= 100)),
    owner_type text CHECK (owner_type IS NULL OR owner_type IN ('User', 'Organization')),
    credential_secret_ref text CHECK (credential_secret_ref IS NULL OR credential_secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    state text NOT NULL DEFAULT 'absent' CHECK (state IN ('absent', 'exchanging', 'secret_pending', 'ready', 'failed', 'orphaned', 'replacing')),
    checkpoint text CHECK (checkpoint IS NULL OR checkpoint IN ('state_validated', 'exchange_started', 'metadata_received', 'secret_write_started', 'secret_written', 'ready_committed', 'orphan_recorded')),
    setup_session_id uuid,
    failure_code text CHECK (failure_code IS NULL OR octet_length(failure_code) <= 200),
    failure_message text CHECK (failure_message IS NULL OR octet_length(failure_message) <= 8192),
    orphan_cleanup_url text CHECK (orphan_cleanup_url IS NULL OR (orphan_cleanup_url ~ '^https://github\.com/' AND octet_length(orphan_cleanup_url) <= 2048)),
    orphaned_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    ready_at timestamptz,
    CHECK (state <> 'ready' OR (github_app_id IS NOT NULL AND app_slug IS NOT NULL AND owner_id IS NOT NULL AND owner_login IS NOT NULL AND owner_type IS NOT NULL AND credential_secret_ref IS NOT NULL AND ready_at IS NOT NULL)),
    CHECK (state NOT IN ('failed', 'orphaned') OR failure_code IS NOT NULL),
    CHECK (state <> 'orphaned' OR (github_app_id IS NOT NULL AND orphaned_at IS NOT NULL)),
    CHECK ((owner_id IS NULL) = (owner_login IS NULL)),
    CHECK (ready_at IS NULL OR ready_at >= created_at)
);

CREATE TABLE github_setup_sessions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    state_hash text NOT NULL UNIQUE CHECK (state_hash ~ '^[0-9a-f]{64}$'),
    flow_kind text NOT NULL CHECK (flow_kind IN ('app_setup', 'installation')),
    initiating_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    initiating_session_hash text NOT NULL CHECK (initiating_session_hash ~ '^[0-9a-f]{64}$'),
    org_id uuid REFERENCES orgs(id) ON DELETE CASCADE,
    installation_purpose text CHECK (installation_purpose IS NULL OR installation_purpose IN ('initial_setup', 'additional_owner', 'replacement')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'consumed', 'failed', 'expired')),
    checkpoint text CHECK (checkpoint IS NULL OR checkpoint IN ('created', 'redirected', 'callback_validated', 'installation_verified', 'completed')),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    failure_code text CHECK (failure_code IS NULL OR octet_length(failure_code) <= 200),
    failure_message text CHECK (failure_message IS NULL OR octet_length(failure_message) <= 8192),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '15 minutes'),
    CHECK ((flow_kind = 'app_setup' AND org_id IS NULL AND installation_purpose IS NULL) OR (flow_kind = 'installation' AND org_id IS NOT NULL AND installation_purpose IS NOT NULL)),
    CHECK ((status = 'consumed') = (consumed_at IS NOT NULL)),
    CHECK (status <> 'failed' OR failure_code IS NOT NULL),
    FOREIGN KEY (org_id, initiating_user_id) REFERENCES users(org_id, id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX github_setup_sessions_one_active_app_setup
    ON github_setup_sessions ((true)) WHERE flow_kind = 'app_setup' AND status = 'active';
CREATE INDEX github_setup_sessions_expiry ON github_setup_sessions (expires_at) WHERE status = 'active';

ALTER TABLE github_app ADD CONSTRAINT github_app_setup_session_fk
    FOREIGN KEY (setup_session_id) REFERENCES github_setup_sessions(id) ON DELETE SET NULL;

CREATE TABLE github_installations (
    github_installation_id bigint PRIMARY KEY CHECK (github_installation_id > 0),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    account_id bigint NOT NULL CHECK (account_id > 0),
    account_login text NOT NULL CHECK (btrim(account_login) <> '' AND octet_length(account_login) <= 100),
    account_type text NOT NULL CHECK (account_type IN ('User', 'Organization')),
    repository_selection text NOT NULL CHECK (repository_selection IN ('all', 'selected')),
    access_state text NOT NULL DEFAULT 'active' CHECK (access_state IN ('active', 'suspended', 'deleted')),
    reconciliation_generation bigint NOT NULL DEFAULT 1 CHECK (reconciliation_generation > 0),
    last_api_confirmed_generation bigint CHECK (last_api_confirmed_generation > 0 AND last_api_confirmed_generation <= reconciliation_generation),
    suspended_at timestamptz,
    deleted_at timestamptz,
    last_reconciled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, github_installation_id),
    CHECK ((access_state = 'suspended') = (suspended_at IS NOT NULL)),
    CHECK ((access_state = 'deleted') = (deleted_at IS NOT NULL)),
    CHECK (deleted_at IS NULL OR deleted_at >= created_at)
);

CREATE FUNCTION protect_github_installation_claim() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'GitHub installation claims are retained as tombstones' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.github_installation_id IS DISTINCT FROM OLD.github_installation_id
       OR NEW.org_id IS DISTINCT FROM OLD.org_id
       OR NEW.account_id IS DISTINCT FROM OLD.account_id THEN
        RAISE EXCEPTION 'GitHub installation identity cannot be rebound' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.access_state = 'deleted' AND NEW.access_state <> 'deleted' THEN
        RAISE EXCEPTION 'deleted GitHub installation tombstones cannot be reactivated' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER github_installations_claim_immutable
BEFORE UPDATE OR DELETE ON github_installations
FOR EACH ROW EXECUTE FUNCTION protect_github_installation_claim();

CREATE TABLE github_repositories (
    github_repository_id bigint PRIMARY KEY CHECK (github_repository_id > 0),
    org_id uuid NOT NULL,
    github_installation_id bigint NOT NULL,
    node_id text NOT NULL UNIQUE CHECK (btrim(node_id) <> '' AND octet_length(node_id) <= 255),
    owner_login text NOT NULL CHECK (btrim(owner_login) <> '' AND octet_length(owner_login) <= 100),
    name text NOT NULL CHECK (btrim(name) <> '' AND name !~ '[/[:space:]]' AND octet_length(name) <= 100),
    full_name text NOT NULL CHECK (full_name ~ '^[^/[:space:]]{1,100}/[^/[:space:]]{1,100}$'),
    visibility text NOT NULL CHECK (visibility IN ('public', 'private', 'internal')),
    archived boolean NOT NULL DEFAULT false,
    default_branch text NOT NULL CHECK (btrim(default_branch) <> '' AND octet_length(default_branch) <= 255),
    access_state text NOT NULL DEFAULT 'active' CHECK (access_state IN ('active', 'inaccessible', 'deleted')),
    reconciliation_generation bigint NOT NULL DEFAULT 1 CHECK (reconciliation_generation > 0),
    last_api_confirmed_generation bigint CHECK (last_api_confirmed_generation > 0 AND last_api_confirmed_generation <= reconciliation_generation),
    last_reconciled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, github_repository_id),
    FOREIGN KEY (org_id, github_installation_id) REFERENCES github_installations(org_id, github_installation_id) ON DELETE RESTRICT,
    CHECK (lower(full_name) = lower(owner_login || '/' || name))
);

CREATE TABLE git_connections (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    github_repository_id bigint NOT NULL,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'disconnected')),
    access_state text NOT NULL DEFAULT 'active' CHECK (access_state IN ('active', 'suspended', 'inaccessible', 'deleted')),
    disconnected_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, github_repository_id),
    FOREIGN KEY (org_id, github_repository_id) REFERENCES github_repositories(org_id, github_repository_id) ON DELETE RESTRICT,
    CHECK ((state = 'disconnected') = (disconnected_at IS NOT NULL))
);

ALTER TABLE projects ADD CONSTRAINT projects_org_id_git_connection_id_fkey
    FOREIGN KEY (org_id, git_connection_id) REFERENCES git_connections(org_id, id) ON DELETE SET NULL (git_connection_id);

CREATE TABLE registries (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    singleton boolean NOT NULL DEFAULT true CHECK (singleton),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    provider text NOT NULL DEFAULT 'ghcr' CHECK (provider = 'ghcr'),
    registry_host text NOT NULL DEFAULT 'ghcr.io' CHECK (registry_host = 'ghcr.io'),
    active_generation bigint NOT NULL DEFAULT 0 CHECK (active_generation >= 0),
    active_credential_secret_ref text CHECK (active_credential_secret_ref IS NULL OR active_credential_secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    pending_generation bigint CHECK (pending_generation > 0),
    pending_credential_secret_ref text CHECK (pending_credential_secret_ref IS NULL OR pending_credential_secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    state text NOT NULL DEFAULT 'absent' CHECK (state IN ('absent', 'creating', 'ready', 'rotating', 'reconciling', 'failed', 'deleting')),
    checkpoint text CHECK (checkpoint IS NULL OR checkpoint IN ('pending_recorded', 'secret_write_started', 'secret_written', 'verified', 'activated', 'namespaces_reconciling', 'cleanup_pending')),
    operation_id uuid,
    reconciliation_generation bigint NOT NULL DEFAULT 0 CHECK (reconciliation_generation >= 0),
    failure_code text CHECK (failure_code IS NULL OR octet_length(failure_code) <= 200),
    failure_message text CHECK (failure_message IS NULL OR octet_length(failure_message) <= 8192),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (singleton),
    UNIQUE (org_id, id),
    CHECK ((active_generation = 0) = (active_credential_secret_ref IS NULL)),
    CHECK ((pending_generation IS NULL) = (pending_credential_secret_ref IS NULL)),
    CHECK (pending_generation IS NULL OR pending_generation > active_generation),
    CHECK (state <> 'ready' OR active_generation > 0),
    CHECK (state <> 'failed' OR failure_code IS NOT NULL)
);

DO $$
DECLARE
    constraint_name name;
BEGIN
    SELECT conname INTO constraint_name
    FROM pg_constraint
    WHERE conrelid = 'apps'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%(registry_id IS NULL) = (image_repository IS NULL)%';
    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE apps DROP CONSTRAINT %I', constraint_name);
    END IF;
END;
$$;

ALTER TABLE apps
    DROP COLUMN ci_workflow_id,
    ALTER COLUMN image_repository SET NOT NULL,
    ADD COLUMN github_workflow_path text NOT NULL CHECK (github_workflow_path ~ '^\.github/workflows/[^/[:space:]]+\.(yml|yaml)$' AND octet_length(github_workflow_path) <= 255),
    ADD COLUMN production_branch text NOT NULL CHECK (btrim(production_branch) <> '' AND octet_length(production_branch) <= 255),
    ADD COLUMN annotation_policy text NOT NULL DEFAULT 'validate_if_present' CHECK (annotation_policy IN ('validate_if_present')),
    ADD COLUMN image_visibility text NOT NULL DEFAULT 'public' CHECK (image_visibility IN ('public', 'private')),
    ADD COLUMN desired_generation bigint NOT NULL DEFAULT 1 CHECK (desired_generation > 0),
    ADD COLUMN cleanup_state text NOT NULL DEFAULT 'active' CHECK (cleanup_state IN ('active', 'cleanup_requested', 'cleaning', 'cleanup_failed', 'cleaned')),
    ADD COLUMN cleanup_checkpoint text CHECK (cleanup_checkpoint IS NULL OR octet_length(cleanup_checkpoint) <= 200),
    ADD COLUMN cleanup_error_code text CHECK (cleanup_error_code IS NULL OR octet_length(cleanup_error_code) <= 200),
    ADD COLUMN cleanup_error_message text CHECK (cleanup_error_message IS NULL OR octet_length(cleanup_error_message) <= 8192),
    ADD CONSTRAINT apps_image_repository_ghcr CHECK (image_repository ~ '^ghcr\.io/[a-z0-9._-]+(/[a-z0-9._-]+)+$' AND octet_length(image_repository) <= 255),
    ADD CONSTRAINT apps_private_registry_required CHECK ((image_visibility = 'private') = (registry_id IS NOT NULL)),
    ADD CONSTRAINT apps_org_id_registry_id_fkey FOREIGN KEY (org_id, registry_id) REFERENCES registries(org_id, id) ON DELETE RESTRICT;

DROP INDEX environments_active_branch;
CREATE UNIQUE INDEX environments_one_live_production
    ON environments (app_id) WHERE kind = 'production' AND torn_down_at IS NULL;
CREATE UNIQUE INDEX environments_one_live_preview_branch
    ON environments (app_id, branch) WHERE kind = 'preview' AND torn_down_at IS NULL;

ALTER TABLE environments
    ADD COLUMN desired_generation bigint NOT NULL DEFAULT 1 CHECK (desired_generation > 0),
    ADD COLUMN required_registry_credential_generation bigint CHECK (required_registry_credential_generation > 0),
    ADD COLUMN ready_registry_credential_generation bigint CHECK (ready_registry_credential_generation > 0),
    ADD COLUMN registry_credential_state text NOT NULL DEFAULT 'not_required' CHECK (registry_credential_state IN ('not_required', 'pending', 'ready', 'failed')),
    ADD COLUMN registry_credential_checkpoint text CHECK (registry_credential_checkpoint IS NULL OR registry_credential_checkpoint IN ('source_read', 'namespace_secret_written', 'namespace_secret_verified')),
    ADD COLUMN registry_credential_error_code text CHECK (registry_credential_error_code IS NULL OR octet_length(registry_credential_error_code) <= 200),
    ADD COLUMN registry_credential_error_message text CHECK (registry_credential_error_message IS NULL OR octet_length(registry_credential_error_message) <= 8192),
    ADD CONSTRAINT environments_registry_generation_consistent CHECK (
        (registry_credential_state = 'not_required' AND required_registry_credential_generation IS NULL AND ready_registry_credential_generation IS NULL)
        OR (registry_credential_state = 'pending' AND required_registry_credential_generation IS NOT NULL AND ready_registry_credential_generation IS NULL)
        OR (registry_credential_state = 'ready' AND required_registry_credential_generation IS NOT NULL AND ready_registry_credential_generation >= required_registry_credential_generation)
        OR (registry_credential_state = 'failed' AND required_registry_credential_generation IS NOT NULL AND registry_credential_error_code IS NOT NULL)
    );

CREATE TABLE import_operations (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (btrim(idempotency_key) <> '' AND octet_length(idempotency_key) <= 255),
    request_digest text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'credential_pending', 'committing', 'pending_dispatch', 'completed', 'failed', 'cancelled')),
    checkpoint text CHECK (checkpoint IS NULL OR checkpoint IN ('request_validated', 'credential_ready', 'resources_committed', 'job_enqueued', 'completed')),
    github_repository_id bigint NOT NULL,
    git_connection_id uuid,
    project_id uuid,
    app_id uuid,
    environment_id uuid,
    orchestration_job_id uuid,
    registry_id uuid,
    failure_code text CHECK (failure_code IS NULL OR octet_length(failure_code) <= 200),
    failure_message text CHECK (failure_message IS NULL OR octet_length(failure_message) <= 8192),
    lease_owner text CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    lease_expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    UNIQUE (org_id, id),
    UNIQUE (org_id, idempotency_key),
    FOREIGN KEY (org_id, github_repository_id) REFERENCES github_repositories(org_id, github_repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, git_connection_id) REFERENCES git_connections(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, project_id) REFERENCES projects(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, app_id) REFERENCES apps(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, environment_id) REFERENCES environments(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, registry_id) REFERENCES registries(org_id, id) ON DELETE RESTRICT,
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CHECK (state <> 'failed' OR failure_code IS NOT NULL),
    CHECK (state <> 'completed' OR (git_connection_id IS NOT NULL AND project_id IS NOT NULL AND app_id IS NOT NULL AND environment_id IS NOT NULL AND orchestration_job_id IS NOT NULL AND completed_at IS NOT NULL))
);

CREATE TABLE github_deliveries (
    delivery_id text PRIMARY KEY CHECK (delivery_id ~ '^[0-9a-fA-F-]{1,100}$'),
    payload_hash text NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    raw_body bytea CHECK (raw_body IS NULL OR octet_length(raw_body) BETWEEN 2 AND 2097152),
    event text NOT NULL CHECK (btrim(event) <> '' AND octet_length(event) <= 100),
    action text CHECK (action IS NULL OR octet_length(action) <= 100),
    signature_256 text NOT NULL CHECK (signature_256 ~ '^sha256=[0-9a-f]{64}$'),
    hook_id bigint NOT NULL CHECK (hook_id > 0),
    target_type text CHECK (target_type IS NULL OR target_type IN ('repository', 'organization', 'app')),
    target_id bigint CHECK (target_id > 0),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'leased', 'processing', 'retryable', 'completed', 'rejected', 'failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt BETWEEN 0 AND 100),
    lease_owner text CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    lease_expires_at timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    terminal_at timestamptz,
    raw_body_purge_after timestamptz NOT NULL DEFAULT (now() + interval '7 days'),
    failure_code text CHECK (failure_code IS NULL OR octet_length(failure_code) <= 200),
    failure_message text CHECK (failure_message IS NULL OR octet_length(failure_message) <= 8192),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CHECK (state NOT IN ('leased', 'processing') OR lease_owner IS NOT NULL),
    CHECK (state NOT IN ('completed', 'rejected', 'failed') OR terminal_at IS NOT NULL),
    CHECK (raw_body IS NOT NULL OR terminal_at IS NOT NULL),
    CHECK (raw_body_purge_after > created_at)
);

CREATE INDEX github_deliveries_work_due ON github_deliveries (next_attempt_at, created_at, delivery_id)
    WHERE state IN ('pending', 'retryable');
CREATE INDEX github_deliveries_raw_body_retention ON github_deliveries (raw_body_purge_after)
    WHERE raw_body IS NOT NULL AND terminal_at IS NOT NULL;

CREATE TABLE orchestration_jobs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    app_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    github_repository_id bigint NOT NULL,
    github_workflow_path text NOT NULL CHECK (github_workflow_path ~ '^\.github/workflows/[^/[:space:]]+\.(yml|yaml)$' AND octet_length(github_workflow_path) <= 255),
    workflow_revision text NOT NULL CHECK (workflow_revision ~ '^[0-9a-f]{40}$'),
    dispatch_ref text NOT NULL CHECK (btrim(dispatch_ref) <> '' AND octet_length(dispatch_ref) <= 1024),
    dispatch_ref_observed_sha text CHECK (dispatch_ref_observed_sha IS NULL OR dispatch_ref_observed_sha ~ '^[0-9a-f]{40}$'),
    desired_checkout_sha text NOT NULL CHECK (desired_checkout_sha ~ '^[0-9a-f]{40}$'),
    desired_generation bigint NOT NULL CHECK (desired_generation > 0),
    idempotency_key text NOT NULL CHECK (btrim(idempotency_key) <> '' AND octet_length(idempotency_key) <= 255),
    pending_github_repository_id bigint CHECK (pending_github_repository_id > 0),
    pending_github_workflow_path text CHECK (pending_github_workflow_path IS NULL OR (pending_github_workflow_path ~ '^\.github/workflows/[^/[:space:]]+\.(yml|yaml)$' AND octet_length(pending_github_workflow_path) <= 255)),
    pending_workflow_revision text CHECK (pending_workflow_revision IS NULL OR pending_workflow_revision ~ '^[0-9a-f]{40}$'),
    pending_dispatch_ref text CHECK (pending_dispatch_ref IS NULL OR (btrim(pending_dispatch_ref) <> '' AND octet_length(pending_dispatch_ref) <= 1024)),
    pending_checkout_sha text CHECK (pending_checkout_sha IS NULL OR pending_checkout_sha ~ '^[0-9a-f]{40}$'),
    pending_desired_generation bigint CHECK (pending_desired_generation > 0),
    pending_idempotency_key text CHECK (pending_idempotency_key IS NULL OR (btrim(pending_idempotency_key) <> '' AND octet_length(pending_idempotency_key) <= 255)),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying', 'completed', 'failed', 'blocked', 'cancelled', 'superseded')),
    lease_owner text CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    lease_expires_at timestamptz,
    dispatch_requested_at timestamptz,
    dispatch_ambiguity_state text NOT NULL DEFAULT 'none' CHECK (dispatch_ambiguity_state IN ('none', 'possible', 'matched', 'expired_no_match')),
    reconcile_after timestamptz,
    dispatch_deadline_at timestamptz,
    run_deadline_at timestamptz,
    reconciliation_owner text CHECK (reconciliation_owner IS NULL OR (btrim(reconciliation_owner) <> '' AND octet_length(reconciliation_owner) <= 255)),
    reconciliation_lease_expires_at timestamptz,
    github_dispatch_identity text CHECK (github_dispatch_identity IS NULL OR octet_length(github_dispatch_identity) <= 255),
    github_run_id bigint CHECK (github_run_id > 0),
    github_run_head_sha text CHECK (github_run_head_sha IS NULL OR github_run_head_sha ~ '^[0-9a-f]{40}$'),
    github_run_status text CHECK (github_run_status IS NULL OR github_run_status IN ('queued', 'in_progress', 'completed')),
    github_run_conclusion text CHECK (github_run_conclusion IS NULL OR github_run_conclusion IN ('success', 'failure', 'cancelled', 'timed_out', 'action_required', 'neutral', 'skipped', 'stale')),
    github_status_observed_at timestamptz,
    cancellation_state text NOT NULL DEFAULT 'not_requested' CHECK (cancellation_state IN ('not_requested', 'requested', 'accepted', 'failed', 'not_supported')),
    cancellation_requested_at timestamptz,
    superseded_by_job_id uuid,
    superseded_at timestamptz,
    oci_resolution_state text NOT NULL DEFAULT 'pending' CHECK (oci_resolution_state IN ('pending', 'polling', 'resolved', 'failed', 'not_required')),
    image_tag_ref text CHECK (image_tag_ref IS NULL OR image_tag_ref ~ '^ghcr\.io/.+:[0-9a-f]{40}$'),
    resolved_image_digest text CHECK (resolved_image_digest IS NULL OR resolved_image_digest ~ '^sha256:[0-9a-f]{64}$'),
    immutable_image_ref text CHECK (immutable_image_ref IS NULL OR (immutable_image_ref ~ '^ghcr\.io/.+@sha256:[0-9a-f]{64}$' AND octet_length(immutable_image_ref) <= 2120)),
    oci_media_type text CHECK (oci_media_type IS NULL OR octet_length(oci_media_type) <= 255),
    oci_resolved_at timestamptz,
    deployment_state text NOT NULL DEFAULT 'not_started' CHECK (deployment_state IN ('not_started', 'waiting_credential', 'ready', 'claimed', 'applying', 'live', 'failed', 'superseded', 'cancelled')),
    deployment_claim_owner text CHECK (deployment_claim_owner IS NULL OR (btrim(deployment_claim_owner) <> '' AND octet_length(deployment_claim_owner) <= 255)),
    deployment_claim_expires_at timestamptz,
    release_id uuid,
    terminal_error_code text CHECK (terminal_error_code IS NULL OR octet_length(terminal_error_code) <= 200),
    terminal_error_message text CHECK (terminal_error_message IS NULL OR octet_length(terminal_error_message) <= 8192),
    terminal_at timestamptz,
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, idempotency_key),
    UNIQUE (org_id, pending_idempotency_key),
    FOREIGN KEY (org_id, app_id) REFERENCES apps(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, app_id, environment_id) REFERENCES environments(org_id, app_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, github_repository_id) REFERENCES github_repositories(org_id, github_repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, pending_github_repository_id) REFERENCES github_repositories(org_id, github_repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, environment_id, release_id) REFERENCES releases(org_id, environment_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (superseded_by_job_id) REFERENCES orchestration_jobs(id) ON DELETE RESTRICT,
    CHECK ((status = 'leased') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK ((pending_checkout_sha IS NULL) = (pending_github_repository_id IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_github_workflow_path IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_workflow_revision IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_dispatch_ref IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_desired_generation IS NULL)
       AND (pending_checkout_sha IS NULL) = (pending_idempotency_key IS NULL)),
    CHECK (status <> 'dispatch_reconciling' OR (dispatch_requested_at IS NOT NULL AND dispatch_ref_observed_sha IS NOT NULL AND reconcile_after IS NOT NULL AND dispatch_deadline_at IS NOT NULL AND dispatch_ambiguity_state IN ('possible', 'matched'))),
    CHECK (status <> 'dispatched' OR (github_run_id IS NOT NULL AND run_deadline_at IS NOT NULL)),
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

ALTER TABLE import_operations ADD CONSTRAINT import_operations_orchestration_job_fk
    FOREIGN KEY (org_id, orchestration_job_id) REFERENCES orchestration_jobs(org_id, id) ON DELETE RESTRICT;

CREATE FUNCTION enforce_orchestration_idempotency_namespace() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.org_id::text, 1330205774));
    IF NEW.pending_idempotency_key = NEW.idempotency_key OR EXISTS (
        SELECT 1 FROM orchestration_jobs existing
        WHERE existing.org_id = NEW.org_id AND existing.id <> NEW.id
          AND (existing.idempotency_key IN (NEW.idempotency_key, NEW.pending_idempotency_key)
            OR existing.pending_idempotency_key IN (NEW.idempotency_key, NEW.pending_idempotency_key))
    ) THEN
        RAISE EXCEPTION 'orchestration idempotency key is already in use' USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER orchestration_jobs_idempotency_namespace
BEFORE INSERT OR UPDATE OF org_id, idempotency_key, pending_idempotency_key ON orchestration_jobs
FOR EACH ROW EXECUTE FUNCTION enforce_orchestration_idempotency_namespace();

CREATE FUNCTION enforce_orchestration_target_and_artifact() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected_repository text;
    target_repository_id bigint;
    target_generation bigint;
    environment_generation bigint;
    release_image_ref text;
    release_commit_sha text;
    release_status text;
BEGIN
    IF TG_OP = 'UPDATE'
       AND OLD.pending_checkout_sha IS NOT NULL
       AND NEW.desired_checkout_sha = OLD.pending_checkout_sha
       AND NEW.desired_generation = OLD.pending_desired_generation
       AND NEW.idempotency_key = OLD.pending_idempotency_key
       AND NEW.pending_checkout_sha IS NULL THEN
        -- Promotion is a single request-tuple operation. This also protects
        -- callers written before routing fields became part of pending work.
        NEW.github_workflow_path := OLD.pending_github_workflow_path;
        NEW.workflow_revision := OLD.pending_workflow_revision;
        NEW.dispatch_ref := OLD.pending_dispatch_ref;
        NEW.github_repository_id := OLD.pending_github_repository_id;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.pending_checkout_sha IS NULL THEN
        NEW.pending_github_repository_id := NULL;
        NEW.pending_github_workflow_path := NULL;
        NEW.pending_workflow_revision := NULL;
        NEW.pending_dispatch_ref := NULL;
    END IF;
    SELECT app.image_repository, connection.github_repository_id, app.desired_generation, environment.desired_generation
      INTO expected_repository, target_repository_id, target_generation, environment_generation
      FROM apps app
      JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
      JOIN git_connections connection ON connection.org_id = project.org_id AND connection.id = project.git_connection_id
      JOIN environments environment ON environment.org_id = app.org_id AND environment.app_id = app.id
     WHERE app.org_id = NEW.org_id AND app.id = NEW.app_id AND environment.id = NEW.environment_id;
    IF NOT FOUND OR target_repository_id IS DISTINCT FROM NEW.github_repository_id THEN
        RAISE EXCEPTION 'orchestration target does not match app project repository' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.pending_desired_generation IS NOT NULL
       AND (NEW.pending_desired_generation <> target_generation OR NEW.pending_desired_generation <> environment_generation) THEN
        RAISE EXCEPTION 'pending orchestration desired generation is stale' USING ERRCODE = 'check_violation';
    END IF;
    IF (CASE WHEN TG_OP = 'INSERT' THEN true ELSE
            NEW.desired_generation IS DISTINCT FROM OLD.desired_generation
            OR (NEW.status NOT IN ('failed', 'blocked', 'cancelled', 'superseded') AND (
                NEW.image_tag_ref IS DISTINCT FROM OLD.image_tag_ref
                OR NEW.resolved_image_digest IS DISTINCT FROM OLD.resolved_image_digest
                 OR NEW.immutable_image_ref IS DISTINCT FROM OLD.immutable_image_ref
                 OR NEW.oci_resolution_state IS DISTINCT FROM OLD.oci_resolution_state
                 OR NEW.deployment_state IS DISTINCT FROM OLD.deployment_state
                 OR NEW.deployment_claim_owner IS DISTINCT FROM OLD.deployment_claim_owner
                 OR NEW.deployment_claim_expires_at IS DISTINCT FROM OLD.deployment_claim_expires_at
                 OR NEW.release_id IS DISTINCT FROM OLD.release_id
            ))
            OR (NEW.status = 'completed' AND OLD.status <> 'completed')
        END)
       AND (NEW.desired_generation <> target_generation OR NEW.desired_generation <> environment_generation) THEN
        RAISE EXCEPTION 'orchestration desired generation is stale' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.image_tag_ref IS NOT NULL AND NEW.image_tag_ref IS DISTINCT FROM expected_repository || ':' || NEW.desired_checkout_sha THEN
        RAISE EXCEPTION 'image tag must identify the app repository and desired checkout SHA' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.immutable_image_ref IS NOT NULL AND NEW.immutable_image_ref IS DISTINCT FROM expected_repository || '@' || NEW.resolved_image_digest THEN
        RAISE EXCEPTION 'immutable image reference must identify the app repository and resolved digest' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.status = 'completed' THEN
        SELECT image_ref, git_commit_sha, status
          INTO release_image_ref, release_commit_sha, release_status
          FROM releases
         WHERE org_id = NEW.org_id AND environment_id = NEW.environment_id AND id = NEW.release_id;
        IF NOT FOUND OR release_status <> 'live' OR release_image_ref IS DISTINCT FROM NEW.immutable_image_ref
           OR release_commit_sha IS DISTINCT FROM NEW.desired_checkout_sha THEN
            RAISE EXCEPTION 'completed orchestration requires matching live release evidence' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER orchestration_jobs_target_and_artifact
BEFORE INSERT OR UPDATE ON orchestration_jobs
FOR EACH ROW EXECUTE FUNCTION enforce_orchestration_target_and_artifact();

CREATE TABLE orchestration_job_run_evidence (
    orchestration_job_id uuid NOT NULL REFERENCES orchestration_jobs(id) ON DELETE CASCADE,
    attempt integer NOT NULL CHECK (attempt > 0),
    dispatch_ref text NOT NULL CHECK (btrim(dispatch_ref) <> '' AND octet_length(dispatch_ref) <= 1024),
    dispatch_ref_observed_sha text CHECK (dispatch_ref_observed_sha IS NULL OR dispatch_ref_observed_sha ~ '^[0-9a-f]{40}$'),
    github_workflow_path text NOT NULL CHECK (github_workflow_path ~ '^\.github/workflows/[^/[:space:]]+\.(yml|yaml)$' AND octet_length(github_workflow_path) <= 255),
    workflow_revision text NOT NULL CHECK (workflow_revision ~ '^[0-9a-f]{40}$'),
    dispatch_requested_at timestamptz,
    dispatch_ambiguity_state text NOT NULL CHECK (dispatch_ambiguity_state IN ('none', 'possible', 'matched', 'expired_no_match')),
    github_dispatch_identity text CHECK (github_dispatch_identity IS NULL OR octet_length(github_dispatch_identity) <= 255),
    github_run_id bigint CHECK (github_run_id > 0),
    github_run_status text NOT NULL CHECK (github_run_status IN ('queued', 'in_progress', 'completed')),
    github_run_conclusion text CHECK (github_run_conclusion IS NULL OR github_run_conclusion IN ('success', 'failure', 'cancelled', 'timed_out', 'action_required', 'neutral', 'skipped', 'stale')),
    observed_head_sha text CHECK (observed_head_sha IS NULL OR observed_head_sha ~ '^[0-9a-f]{40}$'),
    observed_at timestamptz NOT NULL,
    terminal_error_code text CHECK (terminal_error_code IS NULL OR octet_length(terminal_error_code) <= 200),
    terminal_error_message text CHECK (terminal_error_message IS NULL OR octet_length(terminal_error_message) <= 8192),
    terminal_at timestamptz,
    PRIMARY KEY (orchestration_job_id, attempt, observed_at)
);

CREATE FUNCTION complete_orchestration_run_evidence() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    SELECT dispatch_ref, dispatch_ref_observed_sha, github_workflow_path,
           workflow_revision, dispatch_requested_at, dispatch_ambiguity_state,
           github_dispatch_identity
      INTO NEW.dispatch_ref, NEW.dispatch_ref_observed_sha, NEW.github_workflow_path,
           NEW.workflow_revision, NEW.dispatch_requested_at, NEW.dispatch_ambiguity_state,
           NEW.github_dispatch_identity
      FROM orchestration_jobs WHERE id = NEW.orchestration_job_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'orchestration evidence job does not exist' USING ERRCODE = 'foreign_key_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER orchestration_job_run_evidence_complete
BEFORE INSERT ON orchestration_job_run_evidence
FOR EACH ROW EXECUTE FUNCTION complete_orchestration_run_evidence();

CREATE UNIQUE INDEX orchestration_jobs_scope_single_flight
    ON orchestration_jobs (org_id, app_id, environment_id)
    WHERE status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying');
CREATE INDEX orchestration_jobs_claimable ON orchestration_jobs (created_at, id)
    WHERE status IN ('queued', 'leased');
CREATE INDEX orchestration_jobs_reconciliation_due ON orchestration_jobs (reconcile_after, run_deadline_at, id)
    WHERE status IN ('dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying');
CREATE UNIQUE INDEX orchestration_jobs_github_run ON orchestration_jobs (github_run_id) WHERE github_run_id IS NOT NULL;

CREATE TABLE orchestration_push_requests (
    org_id uuid NOT NULL,
    idempotency_key text NOT NULL CHECK (btrim(idempotency_key) <> '' AND octet_length(idempotency_key) <= 255),
    app_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    github_repository_id bigint NOT NULL,
    github_workflow_path text NOT NULL CHECK (github_workflow_path ~ '^\.github/workflows/[^/[:space:]]+\.(yml|yaml)$' AND octet_length(github_workflow_path) <= 255),
    workflow_revision text NOT NULL CHECK (workflow_revision ~ '^[0-9a-f]{40}$'),
    dispatch_ref text NOT NULL CHECK (btrim(dispatch_ref) <> '' AND octet_length(dispatch_ref) <= 1024),
    desired_checkout_sha text NOT NULL CHECK (desired_checkout_sha ~ '^[0-9a-f]{40}$'),
    orchestration_job_id uuid NOT NULL,
    accepted_generation bigint NOT NULL CHECK (accepted_generation > 0),
    enqueue_result text NOT NULL CHECK (enqueue_result IN ('queued', 'pending')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, idempotency_key),
    FOREIGN KEY (org_id, app_id) REFERENCES apps(org_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, app_id, environment_id) REFERENCES environments(org_id, app_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, github_repository_id) REFERENCES github_repositories(org_id, github_repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id, orchestration_job_id) REFERENCES orchestration_jobs(org_id, id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED
);

CREATE FUNCTION enqueue_orchestration_push(
    target_org_id uuid, target_app_id uuid, target_environment_id uuid,
    target_repository_id bigint, target_workflow_path text,
    target_workflow_revision text, target_dispatch_ref text,
    target_checkout_sha text, target_idempotency_key text
) RETURNS TABLE (
    orchestration_job_id uuid, org_id uuid, app_id uuid, environment_id uuid,
    github_repository_id bigint, github_workflow_path text, workflow_revision text,
    dispatch_ref text, desired_checkout_sha text, desired_generation bigint,
    idempotency_key text, pending_checkout_sha text, pending_desired_generation bigint,
    pending_idempotency_key text, orchestration_status text,
    accepted_generation bigint, enqueue_result text
)
LANGUAGE plpgsql
SET search_path FROM CURRENT
AS $$
DECLARE
    app_generation bigint;
    environment_generation bigint;
    target_project_id uuid;
    target_connection_id uuid;
    target_installation_id bigint;
    active_job_id uuid;
    request_job_id uuid;
    request_generation bigint;
    request_result text;
    request_app_id uuid;
    request_environment_id uuid;
    request_repository_id bigint;
    request_workflow_path text;
    request_workflow_revision text;
    request_dispatch_ref text;
    request_checkout_sha text;
BEGIN
    IF target_org_id IS NULL OR target_app_id IS NULL OR target_environment_id IS NULL
       OR target_repository_id IS NULL OR target_repository_id <= 0
       OR target_workflow_path IS NULL OR target_workflow_path !~ '^\.github/workflows/[^/[:space:]]+\.(yml|yaml)$'
       OR pg_catalog.octet_length(target_workflow_path) > 255
       OR target_workflow_revision IS NULL OR target_workflow_revision !~ '^[0-9a-f]{40}$'
       OR target_dispatch_ref IS NULL OR pg_catalog.btrim(target_dispatch_ref) = ''
       OR pg_catalog.octet_length(target_dispatch_ref) > 1024
       OR target_checkout_sha IS NULL OR target_checkout_sha !~ '^[0-9a-f]{40}$'
       OR target_idempotency_key IS NULL OR pg_catalog.btrim(target_idempotency_key) = ''
       OR pg_catalog.octet_length(target_idempotency_key) > 255 THEN
        RAISE EXCEPTION 'invalid orchestration enqueue request' USING ERRCODE = 'check_violation';
    END IF;

    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(target_org_id::text, 1380270412));

    SELECT app.project_id INTO target_project_id FROM apps app
     WHERE app.org_id = target_org_id AND app.id = target_app_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'orchestration application target is inaccessible' USING ERRCODE = 'foreign_key_violation';
    END IF;

    PERFORM 1 FROM orgs org WHERE org.id = target_org_id AND org.deleted_at IS NULL FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'orchestration organization scope is inactive' USING ERRCODE = 'foreign_key_violation';
    END IF;
    SELECT project.git_connection_id INTO target_connection_id FROM projects project
     WHERE project.org_id = target_org_id AND project.id = target_project_id
       AND project.deleted_at IS NULL FOR KEY SHARE;
    IF NOT FOUND OR target_connection_id IS NULL THEN
        RAISE EXCEPTION 'orchestration project scope is inactive' USING ERRCODE = 'foreign_key_violation';
    END IF;
    SELECT app.desired_generation INTO app_generation FROM apps app
     WHERE app.org_id = target_org_id AND app.id = target_app_id AND app.project_id = target_project_id
       AND app.deleted_at IS NULL AND app.github_workflow_path = target_workflow_path FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'orchestration application or workflow scope is inactive' USING ERRCODE = 'foreign_key_violation';
    END IF;
    SELECT environment.desired_generation INTO environment_generation FROM environments environment
     WHERE environment.org_id = target_org_id AND environment.app_id = target_app_id
       AND environment.id = target_environment_id AND environment.torn_down_at IS NULL
       AND environment.lifecycle_status NOT IN ('teardown_requested', 'torn_down', 'failed') FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'orchestration environment scope is inactive' USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF app_generation <> environment_generation THEN
        RAISE EXCEPTION 'application and environment desired generations are incoherent' USING ERRCODE = 'foreign_key_violation';
    END IF;

    SELECT connection.github_repository_id INTO request_repository_id FROM git_connections connection
     WHERE connection.org_id = target_org_id AND connection.id = target_connection_id
       AND connection.state = 'active' AND connection.access_state = 'active' FOR KEY SHARE;
    IF NOT FOUND OR request_repository_id IS DISTINCT FROM target_repository_id THEN
        RAISE EXCEPTION 'orchestration repository does not match the active project connection' USING ERRCODE = 'foreign_key_violation';
    END IF;
    SELECT repository.github_installation_id INTO target_installation_id FROM github_repositories repository
     WHERE repository.org_id = target_org_id AND repository.github_repository_id = target_repository_id
       AND repository.access_state = 'active' AND NOT repository.archived
       AND repository.last_api_confirmed_generation = repository.reconciliation_generation FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'orchestration repository access is inactive or stale' USING ERRCODE = 'foreign_key_violation';
    END IF;
    PERFORM 1 FROM github_installations installation
     WHERE installation.org_id = target_org_id AND installation.github_installation_id = target_installation_id
       AND installation.access_state = 'active'
       AND installation.last_api_confirmed_generation = installation.reconciliation_generation FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'orchestration installation access is inactive or stale' USING ERRCODE = 'foreign_key_violation';
    END IF;

    SELECT job.id INTO active_job_id FROM orchestration_jobs job
     WHERE job.org_id = target_org_id AND job.app_id = target_app_id
       AND job.environment_id = target_environment_id
       AND job.status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying')
     FOR UPDATE;

    IF app_generation = 9223372036854775807 THEN
        RAISE EXCEPTION 'orchestration desired generation cannot exceed bigint maximum' USING ERRCODE = 'check_violation';
    END IF;
    request_job_id := COALESCE(active_job_id, uuidv7());
    request_generation := app_generation + 1;
    request_result := CASE WHEN active_job_id IS NULL THEN 'queued' ELSE 'pending' END;
    INSERT INTO orchestration_push_requests (
        org_id, idempotency_key, app_id, environment_id, github_repository_id,
        github_workflow_path, workflow_revision, dispatch_ref, desired_checkout_sha,
        orchestration_job_id, accepted_generation, enqueue_result
    ) VALUES (
        target_org_id, target_idempotency_key, target_app_id, target_environment_id,
        target_repository_id, target_workflow_path, target_workflow_revision,
        target_dispatch_ref, target_checkout_sha, request_job_id, request_generation, request_result
    ) ON CONFLICT ON CONSTRAINT orchestration_push_requests_pkey DO NOTHING;

    IF NOT FOUND THEN
        SELECT request.app_id, request.environment_id, request.github_repository_id,
               request.github_workflow_path, request.workflow_revision, request.dispatch_ref,
               request.desired_checkout_sha, request.orchestration_job_id,
               request.accepted_generation, request.enqueue_result
          INTO request_app_id, request_environment_id, request_repository_id,
               request_workflow_path, request_workflow_revision, request_dispatch_ref,
               request_checkout_sha, request_job_id, request_generation, request_result
          FROM orchestration_push_requests request
         WHERE request.org_id = target_org_id AND request.idempotency_key = target_idempotency_key
         FOR UPDATE;
        IF request_app_id IS DISTINCT FROM target_app_id
           OR request_environment_id IS DISTINCT FROM target_environment_id
           OR request_repository_id IS DISTINCT FROM target_repository_id
           OR request_workflow_path IS DISTINCT FROM target_workflow_path
           OR request_workflow_revision IS DISTINCT FROM target_workflow_revision
           OR request_dispatch_ref IS DISTINCT FROM target_dispatch_ref
           OR request_checkout_sha IS DISTINCT FROM target_checkout_sha THEN
            RAISE EXCEPTION 'orchestration idempotency key was used for a different request' USING ERRCODE = 'unique_violation';
        END IF;
        RETURN QUERY
        SELECT job.id, job.org_id, job.app_id, job.environment_id, job.github_repository_id,
               job.github_workflow_path, job.workflow_revision, job.dispatch_ref,
               job.desired_checkout_sha, job.desired_generation, job.idempotency_key,
               job.pending_checkout_sha, job.pending_desired_generation,
               job.pending_idempotency_key, job.status, request_generation, request_result
          FROM orchestration_jobs job WHERE job.org_id = target_org_id AND job.id = request_job_id;
        RETURN;
    END IF;

    app_generation := request_generation;
    UPDATE apps app SET desired_generation = app_generation
     WHERE app.org_id = target_org_id AND app.id = target_app_id
       AND app.desired_generation = app_generation - 1;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'application desired generation changed while enqueueing' USING ERRCODE = 'serialization_failure';
    END IF;
    UPDATE environments environment SET desired_generation = app_generation
     WHERE environment.org_id = target_org_id AND environment.app_id = target_app_id
       AND environment.id = target_environment_id
       AND environment.desired_generation = app_generation - 1;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'environment desired generation changed while enqueueing' USING ERRCODE = 'serialization_failure';
    END IF;

    IF active_job_id IS NULL THEN
        INSERT INTO orchestration_jobs (
            id, org_id, app_id, environment_id, github_repository_id, github_workflow_path,
            workflow_revision, dispatch_ref, desired_checkout_sha, desired_generation, idempotency_key
        ) VALUES (
            request_job_id, target_org_id, target_app_id, target_environment_id, target_repository_id,
            target_workflow_path, target_workflow_revision, target_dispatch_ref,
            target_checkout_sha, app_generation, target_idempotency_key
        );
    ELSE
        UPDATE orchestration_jobs job SET
            pending_github_repository_id = target_repository_id,
            pending_github_workflow_path = target_workflow_path,
            pending_workflow_revision = target_workflow_revision,
            pending_dispatch_ref = target_dispatch_ref,
            pending_checkout_sha = target_checkout_sha,
            pending_desired_generation = app_generation,
            pending_idempotency_key = target_idempotency_key,
            updated_at = now()
         WHERE job.org_id = target_org_id AND job.id = active_job_id;
    END IF;

    RETURN QUERY
    SELECT job.id, job.org_id, job.app_id, job.environment_id, job.github_repository_id,
           job.github_workflow_path, job.workflow_revision, job.dispatch_ref,
           job.desired_checkout_sha, job.desired_generation, job.idempotency_key,
           job.pending_checkout_sha, job.pending_desired_generation,
           job.pending_idempotency_key, job.status, request_generation, request_result
      FROM orchestration_jobs job WHERE job.org_id = target_org_id AND job.id = request_job_id;
END;
$$;

-- The migration owner (normally the application role) retains owner EXECUTE;
-- deployments using a distinct application role may grant EXECUTE to that
-- existing role after role creation, without this migration naming a role.
REVOKE EXECUTE ON FUNCTION enqueue_orchestration_push(uuid, uuid, uuid, bigint, text, text, text, text, text) FROM PUBLIC;
DO $$
DECLARE
    table_owner name;
BEGIN
    SELECT owner.rolname INTO table_owner
      FROM pg_catalog.pg_class relation
      JOIN pg_catalog.pg_roles owner ON owner.oid = relation.relowner
     WHERE relation.oid = 'orchestration_jobs'::regclass;
    IF table_owner IS DISTINCT FROM current_user THEN
        EXECUTE pg_catalog.format(
            'GRANT EXECUTE ON FUNCTION %I.enqueue_orchestration_push(uuid, uuid, uuid, bigint, text, text, text, text, text) TO %I',
            current_schema(), table_owner
        );
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_active_work_scope() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    scoped_org_id uuid;
    scoped_app_id uuid;
    scoped_environment_id uuid;
BEGIN
    IF TG_TABLE_NAME = 'orchestration_jobs' THEN
        IF NEW.status NOT IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying') THEN
            RETURN NEW;
        END IF;
        PERFORM 1
          FROM environments environment
          JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
          JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
          JOIN git_connections connection ON connection.org_id = project.org_id AND connection.id = project.git_connection_id
          JOIN github_repositories repository ON repository.org_id = connection.org_id AND repository.github_repository_id = connection.github_repository_id
          JOIN github_installations installation ON installation.org_id = repository.org_id AND installation.github_installation_id = repository.github_installation_id
          JOIN orgs org ON org.id = environment.org_id
         WHERE environment.org_id = NEW.org_id AND environment.id = NEW.environment_id
           AND environment.app_id = NEW.app_id AND repository.github_repository_id = NEW.github_repository_id
            AND environment.torn_down_at IS NULL AND environment.lifecycle_status NOT IN ('teardown_requested', 'torn_down', 'failed')
           AND app.deleted_at IS NULL AND project.deleted_at IS NULL AND org.deleted_at IS NULL
           AND connection.state = 'active' AND connection.access_state = 'active'
           AND repository.access_state = 'active' AND NOT repository.archived
           AND installation.access_state = 'active'
            AND repository.last_api_confirmed_generation = repository.reconciliation_generation
            AND installation.last_api_confirmed_generation = installation.reconciliation_generation
             AND (CASE WHEN TG_OP = 'UPDATE'
                  THEN OLD.status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying')
                  ELSE NEW.desired_generation = app.desired_generation AND NEW.desired_generation = environment.desired_generation
                  END)
         FOR KEY SHARE OF environment, app, project, connection, repository, installation, org;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'orchestration work scope is inactive, incoherent, or stale' USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    ELSIF TG_TABLE_NAME = 'reconciliation_runs' THEN
        IF TG_OP <> 'INSERT' AND NEW.status NOT IN ('queued', 'leased', 'running') THEN RETURN NEW; END IF;
        scoped_org_id := NEW.org_id; scoped_app_id := NEW.scope_app_id; scoped_environment_id := NEW.scope_environment_id;
    ELSIF TG_TABLE_NAME = 'releases' THEN
        IF TG_OP <> 'INSERT' AND NEW.status NOT IN ('pending', 'deploying') THEN RETURN NEW; END IF;
        scoped_org_id := NEW.org_id; scoped_environment_id := NEW.environment_id;
    ELSIF TG_TABLE_NAME IN ('env_vars', 'env_var_revisions') THEN
        scoped_org_id := NEW.org_id; scoped_environment_id := NEW.environment_id;
    ELSE
        scoped_org_id := NEW.org_id; scoped_environment_id := NEW.id;
    END IF;
    PERFORM 1 FROM orgs WHERE id = scoped_org_id AND deleted_at IS NULL FOR KEY SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'organization is tombstoned' USING ERRCODE = 'check_violation'; END IF;
    IF scoped_environment_id IS NOT NULL THEN
        PERFORM 1 FROM environments environment
        JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
        JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
        WHERE environment.org_id = scoped_org_id AND environment.id = scoped_environment_id
          AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL AND project.deleted_at IS NULL
        FOR KEY SHARE OF environment, app, project;
        IF NOT FOUND THEN RAISE EXCEPTION 'work scope has a tombstoned ancestor or torn-down environment' USING ERRCODE = 'check_violation'; END IF;
    ELSIF scoped_app_id IS NOT NULL THEN
        PERFORM 1 FROM apps app JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
        WHERE app.org_id = scoped_org_id AND app.id = scoped_app_id AND app.deleted_at IS NULL AND project.deleted_at IS NULL
        FOR KEY SHARE OF app, project;
        IF NOT FOUND THEN RAISE EXCEPTION 'work scope has a tombstoned ancestor' USING ERRCODE = 'check_violation'; END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER orchestration_jobs_active_scope
BEFORE INSERT OR UPDATE OF status, org_id, app_id, environment_id, github_repository_id, desired_generation ON orchestration_jobs
FOR EACH ROW EXECUTE FUNCTION enforce_active_work_scope();

CREATE OR REPLACE FUNCTION enforce_tombstone_readiness() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    affected_app_ids uuid[];
    scope_org_id uuid;
BEGIN
    IF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
        IF NEW.external_credentials_revoked_at IS NULL THEN
            RAISE EXCEPTION 'external credential revocation must be confirmed before tombstone' USING ERRCODE = 'check_violation';
        END IF;
        scope_org_id := CASE WHEN TG_TABLE_NAME = 'orgs' THEN NEW.id ELSE NEW.org_id END;
        PERFORM pg_advisory_xact_lock(hashtextextended(scope_org_id::text, 1380270412));

        IF TG_TABLE_NAME = 'orgs' THEN
            IF EXISTS (SELECT 1 FROM environments WHERE org_id = NEW.id AND torn_down_at IS NULL) THEN
                RAISE EXCEPTION 'all descendant environments must be torn down before organization tombstone' USING ERRCODE = 'check_violation';
            END IF;
            SELECT COALESCE(array_agg(id), '{}'::uuid[]) INTO affected_app_ids FROM apps WHERE org_id = NEW.id;
            UPDATE apps SET desired_generation = desired_generation + 1 WHERE org_id = NEW.id;
            UPDATE environments SET desired_generation = desired_generation + 1,
                in_progress_release_id = NULL, lock_acquired_at = NULL WHERE org_id = NEW.id;
        ELSIF TG_TABLE_NAME = 'projects' THEN
            IF EXISTS (SELECT 1 FROM environments environment JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
                WHERE app.org_id = NEW.org_id AND app.project_id = NEW.id AND environment.torn_down_at IS NULL) THEN
                RAISE EXCEPTION 'all descendant environments must be torn down before project tombstone' USING ERRCODE = 'check_violation';
            END IF;
            SELECT COALESCE(array_agg(id), '{}'::uuid[]) INTO affected_app_ids FROM apps WHERE org_id = NEW.org_id AND project_id = NEW.id;
            UPDATE apps SET desired_generation = desired_generation + 1 WHERE org_id = NEW.org_id AND project_id = NEW.id;
            UPDATE environments SET desired_generation = desired_generation + 1,
                in_progress_release_id = NULL, lock_acquired_at = NULL
             WHERE org_id = NEW.org_id AND app_id = ANY (affected_app_ids);
        ELSE
            IF EXISTS (SELECT 1 FROM environments WHERE org_id = NEW.org_id AND app_id = NEW.id AND torn_down_at IS NULL) THEN
                RAISE EXCEPTION 'all descendant environments must be torn down before app tombstone' USING ERRCODE = 'check_violation';
            END IF;
            affected_app_ids := ARRAY[NEW.id];
            NEW.desired_generation := OLD.desired_generation + 1;
            UPDATE environments SET desired_generation = desired_generation + 1,
                in_progress_release_id = NULL, lock_acquired_at = NULL
             WHERE org_id = NEW.org_id AND app_id = NEW.id;
        END IF;

        UPDATE orchestration_jobs
        SET status = 'cancelled', pending_github_repository_id = NULL,
            pending_github_workflow_path = NULL,
            pending_workflow_revision = NULL, pending_dispatch_ref = NULL,
            pending_checkout_sha = NULL, pending_desired_generation = NULL,
            pending_idempotency_key = NULL, lease_owner = NULL, lease_expires_at = NULL,
            reconcile_after = NULL, dispatch_deadline_at = NULL, run_deadline_at = NULL,
            reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL,
            deployment_claim_owner = NULL, deployment_claim_expires_at = NULL,
            cancellation_state = 'requested', cancellation_requested_at = COALESCE(cancellation_requested_at, now()),
            deployment_state = CASE WHEN deployment_state IN ('waiting_credential', 'ready', 'claimed', 'applying') THEN 'cancelled' ELSE deployment_state END,
            terminal_error_code = 'ancestor_tombstoned',
            terminal_error_message = TG_TABLE_NAME || ' was tombstoned', terminal_at = now(), updated_at = now()
        WHERE org_id = scope_org_id AND app_id = ANY (affected_app_ids)
          AND status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying');

        UPDATE reconciliation_runs
        SET status = 'cancelled', lease_owner = NULL, lease_expires_at = NULL,
            error_code = 'ancestor_tombstoned', error_message = TG_TABLE_NAME || ' was tombstoned', completed_at = now()
        WHERE org_id = scope_org_id
          AND (TG_TABLE_NAME = 'orgs' OR scope_kind = 'org' OR scope_app_id = ANY (affected_app_ids))
          AND status IN ('queued', 'leased', 'running');
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE cleanup_jobs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid REFERENCES orgs(id) ON DELETE RESTRICT,
    resource_kind text NOT NULL CHECK (resource_kind IN ('github_app_secret', 'ghcr_source_secret', 'environment_pull_secret', 'git_connection', 'app', 'environment', 'project')),
    resource_identity text NOT NULL CHECK (btrim(resource_identity) <> '' AND octet_length(resource_identity) <= 255),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'leased', 'blocked', 'completed', 'failed')),
    checkpoint text CHECK (checkpoint IS NULL OR octet_length(checkpoint) <= 200),
    lease_owner text CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    lease_expires_at timestamptz,
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    not_before timestamptz NOT NULL DEFAULT now(),
    error_code text CHECK (error_code IS NULL OR octet_length(error_code) <= 200),
    error_message text CHECK (error_message IS NULL OR octet_length(error_message) <= 8192),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    CHECK ((state = 'leased') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK (state NOT IN ('completed', 'failed') OR completed_at IS NOT NULL)
);

CREATE UNIQUE INDEX cleanup_jobs_single_flight ON cleanup_jobs (resource_kind, resource_identity)
    WHERE state IN ('queued', 'leased', 'blocked');
CREATE INDEX cleanup_jobs_claimable ON cleanup_jobs (not_before, created_at, id)
    WHERE state IN ('queued', 'leased');

ALTER TABLE api_idempotency_records DROP CONSTRAINT api_idempotency_records_resource_type_check;
ALTER TABLE api_idempotency_records ADD CONSTRAINT api_idempotency_records_resource_type_check CHECK (resource_type IS NULL OR resource_type IN (
    'org', 'user', 'api_token', 'github_app', 'github_installation', 'github_repository', 'git_connection', 'registry', 'import_operation',
    'project', 'app', 'environment', 'release', 'alias', 'domain', 'env_var', 'env_var_revision',
    'storage_destination', 'webhook', 'reconciliation_run', 'orchestration_job'
));
