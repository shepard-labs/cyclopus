ALTER TABLE registries
    DROP CONSTRAINT registries_state_check,
    DROP CONSTRAINT registries_checkpoint_check;
ALTER TABLE registries
    ADD COLUMN previous_generation bigint,
    ADD COLUMN previous_credential_secret_ref text,
    ADD COLUMN lease_owner text,
    ADD COLUMN lease_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN activated_at timestamptz,
    ADD CONSTRAINT registries_state_check CHECK (state IN ('absent','creating','ready','rotating','reconciling','failed','deleting')),
    ADD CONSTRAINT registries_checkpoint_check CHECK (checkpoint IS NULL OR checkpoint IN (
        'pending_recorded','source_secret_write_started','source_secret_written','targets_validated',
        'namespaces_reconciling','namespaces_ready','activated','rollback_reconciling','cleanup_pending',
        'secret_write_started','secret_written','verified')),
    ADD CONSTRAINT registries_previous_credential_pair CHECK ((previous_generation IS NULL) = (previous_credential_secret_ref IS NULL)),
    ADD CONSTRAINT registries_previous_generation_positive CHECK (previous_generation IS NULL OR previous_generation > 0),
    ADD CONSTRAINT registries_previous_secret_ref_check CHECK (previous_credential_secret_ref IS NULL OR previous_credential_secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    ADD CONSTRAINT registries_lease_pair CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    ADD CONSTRAINT registries_lease_owner_check CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    ADD CONSTRAINT registries_lease_epoch_check CHECK (lease_epoch >= 0),
    ADD CONSTRAINT registries_attempts_check CHECK (attempts BETWEEN 0 AND 12),
    ADD CONSTRAINT registries_state_checkpoint_consistent CHECK (
        state<>'failed' OR failure_code IS NOT NULL),
    ADD CONSTRAINT registries_activation_consistent CHECK ((active_generation = 0 AND activated_at IS NULL) OR active_generation > 0);

CREATE INDEX registries_operation_due
    ON registries(next_attempt_at,updated_at,id) WHERE state IN ('creating','rotating','reconciling','deleting') AND lease_owner IS NULL AND attempts<12;
CREATE INDEX registries_operation_leased
    ON registries(lease_expires_at,id) WHERE lease_owner IS NOT NULL AND attempts<12;
CREATE INDEX registries_operation_exhausted
    ON registries(updated_at,id) WHERE state IN ('creating','rotating','reconciling','deleting') AND attempts>=12;

CREATE FUNCTION registry_row_valid(value registries) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT value.reconciliation_generation <= value.active_generation
       AND (value.previous_generation IS NULL OR value.previous_generation<=value.active_generation)
       AND CASE value.state
        WHEN 'absent' THEN value.checkpoint IS NULL AND value.active_generation=0 AND value.pending_generation IS NULL
        WHEN 'creating' THEN value.checkpoint IN ('pending_recorded','source_secret_write_started','source_secret_written','targets_validated','namespaces_reconciling','namespaces_ready')
            AND value.active_generation=0 AND value.pending_generation IS NOT NULL
        WHEN 'ready' THEN value.checkpoint IN ('activated','cleanup_pending')
            AND value.active_generation>0 AND value.active_credential_secret_ref IS NOT NULL
            AND value.pending_generation IS NULL
        WHEN 'rotating' THEN value.checkpoint IN ('pending_recorded','source_secret_write_started','source_secret_written','targets_validated','namespaces_reconciling','namespaces_ready')
            AND value.pending_generation IS NOT NULL
        WHEN 'reconciling' THEN value.checkpoint IN ('namespaces_reconciling','namespaces_ready','rollback_reconciling')
            AND value.active_generation>0
        WHEN 'failed' THEN value.failure_code IN ('registry_auth_failed','credential_validation_failed','credential_operation_exhausted','image_pull_secret_reconcile_failed','kubernetes_ownership_conflict','kubernetes_unavailable','postgres_permanent')
        WHEN 'deleting' THEN value.checkpoint='cleanup_pending'
        ELSE false END
$$;

CREATE FUNCTION enforce_registry_checkpoint() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT registry_row_valid(NEW) THEN
        RAISE EXCEPTION 'registry state/checkpoint is incompatible with enabled Phase 6'
            USING ERRCODE='check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER registries_checkpoint_valid
    BEFORE INSERT OR UPDATE ON registries
    FOR EACH ROW EXECUTE FUNCTION enforce_registry_checkpoint();

CREATE TABLE registry_credential_generations (
    org_id uuid NOT NULL,
    registry_id uuid NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    secret_ref text NOT NULL CHECK (secret_ref ~ '^k8s-secret://[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?#[A-Za-z0-9._-]{1,253}$'),
    state text NOT NULL CHECK (state IN ('pending','active','retired','failed','garbage_collecting','garbage_collected')),
    operation_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    validated_at timestamptz,
    activated_at timestamptz,
    retired_at timestamptz,
    garbage_collected_at timestamptz,
    PRIMARY KEY (registry_id,generation),
    UNIQUE (org_id,registry_id,generation),
    UNIQUE (secret_ref),
    FOREIGN KEY (org_id,registry_id) REFERENCES registries(org_id,id) ON DELETE RESTRICT,
    CHECK (validated_at IS NULL OR validated_at >= created_at),
    CHECK (activated_at IS NULL OR validated_at IS NOT NULL AND activated_at >= validated_at),
    CHECK (retired_at IS NULL OR activated_at IS NOT NULL AND retired_at >= activated_at),
    CHECK ((state='active') = (activated_at IS NOT NULL AND retired_at IS NULL AND garbage_collected_at IS NULL)),
    CHECK (state NOT IN ('retired','garbage_collecting','garbage_collected') OR retired_at IS NOT NULL),
    CHECK ((state='garbage_collected') = (garbage_collected_at IS NOT NULL))
);
CREATE INDEX registry_credential_generations_registry_fk ON registry_credential_generations(org_id,registry_id);
CREATE INDEX registry_credential_generations_operation_fk ON registry_credential_generations(org_id,operation_id);
CREATE INDEX registry_credential_generations_cleanup
    ON registry_credential_generations(retired_at,registry_id,generation) WHERE state IN ('retired','garbage_collecting');
CREATE UNIQUE INDEX registry_credential_generations_one_active
    ON registry_credential_generations(org_id,registry_id) WHERE state='active';

CREATE FUNCTION protect_registry_credential_generation_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR NEW.org_id IS DISTINCT FROM OLD.org_id
       OR NEW.registry_id IS DISTINCT FROM OLD.registry_id
       OR NEW.generation IS DISTINCT FROM OLD.generation
       OR NEW.secret_ref IS DISTINCT FROM OLD.secret_ref
       OR NEW.operation_id IS DISTINCT FROM OLD.operation_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'registry credential generation identity is append-only'
            USING ERRCODE='integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER registry_credential_generations_identity_immutable
    BEFORE UPDATE OR DELETE ON registry_credential_generations
    FOR EACH ROW EXECUTE FUNCTION protect_registry_credential_generation_identity();

CREATE TABLE registry_credential_operations (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id uuid NOT NULL,
    registry_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('import_create','create','rotate','rollback')),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','leased','succeeded','failed','cancelled')),
    checkpoint text NOT NULL DEFAULT 'pending_recorded' CHECK (checkpoint IN (
        'pending_recorded','source_secret_write_started','source_secret_written','targets_validated',
        'namespaces_reconciling','namespaces_ready','activated','rollback_reconciling','cleanup_pending')),
    candidate_generation bigint NOT NULL CHECK (candidate_generation > 0),
    previous_generation bigint CHECK (previous_generation > 0),
    target_repository text CHECK (target_repository IS NULL OR valid_repository_path(target_repository,1)),
    idempotency_key text NOT NULL CHECK (btrim(idempotency_key) <> '' AND octet_length(idempotency_key) <= 255),
    lease_owner text CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_expires_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 12),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    failure_code text CHECK (failure_code IS NULL OR failure_code IN (
        'registry_auth_failed','credential_validation_failed','credential_operation_exhausted',
        'image_pull_secret_reconcile_failed','kubernetes_ownership_conflict','kubernetes_unavailable',
        'postgres_retryable','postgres_permanent','superseded','ancestor_tombstoned')),
    failure_message text CHECK (failure_message IS NULL OR octet_length(failure_message) <= 8192),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    terminal_at timestamptz,
    UNIQUE (org_id,id),
    UNIQUE (org_id,idempotency_key),
    UNIQUE (id,org_id,registry_id),
    UNIQUE (id,org_id,registry_id,candidate_generation),
    FOREIGN KEY (org_id,registry_id) REFERENCES registries(org_id,id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id,registry_id,candidate_generation) REFERENCES registry_credential_generations(org_id,registry_id,generation) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (org_id,registry_id,previous_generation) REFERENCES registry_credential_generations(org_id,registry_id,generation) DEFERRABLE INITIALLY DEFERRED,
    CHECK ((status='leased') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK (status='leased' OR lease_owner IS NULL AND lease_expires_at IS NULL),
    CHECK ((status IN ('succeeded','failed','cancelled')) = (terminal_at IS NOT NULL)),
    CHECK ((status='failed') = (failure_code IS NOT NULL)),
    CHECK (status='failed' OR failure_message IS NULL),
    CHECK (previous_generation IS NULL OR previous_generation < candidate_generation),
    CHECK (kind IN ('import_create','create') OR previous_generation IS NOT NULL),
    CHECK (status<>'succeeded' OR checkpoint IN ('activated','cleanup_pending'))
);
ALTER TABLE registry_credential_generations
    ADD CONSTRAINT registry_credential_generations_operation_fk
    FOREIGN KEY (operation_id,org_id,registry_id,generation)
    REFERENCES registry_credential_operations(id,org_id,registry_id,candidate_generation)
    DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX registry_credential_operations_registry_fk ON registry_credential_operations(org_id,registry_id);
CREATE UNIQUE INDEX registry_credential_operations_active
    ON registry_credential_operations(org_id,registry_id) WHERE status IN ('pending','leased');
CREATE INDEX registry_credential_operations_due
    ON registry_credential_operations(next_attempt_at,created_at,id) WHERE status='pending' AND attempts<12;
CREATE INDEX registry_credential_operations_leased
    ON registry_credential_operations(lease_expires_at,id) WHERE status='leased' AND attempts<12;
CREATE INDEX registry_credential_operations_exhausted
    ON registry_credential_operations(updated_at,id) WHERE status IN ('pending','leased') AND attempts>=12;
CREATE INDEX registry_credential_operations_terminal_cleanup
    ON registry_credential_operations(terminal_at,id) WHERE status IN ('succeeded','failed','cancelled');

CREATE FUNCTION enforce_registry_credential_references() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.active_generation>0 AND NOT EXISTS (SELECT 1 FROM registry_credential_generations WHERE org_id=NEW.org_id AND registry_id=NEW.id AND generation=NEW.active_generation) THEN
        RAISE EXCEPTION 'active registry generation is not tenant-local' USING ERRCODE='foreign_key_violation';
    END IF;
    IF NEW.pending_generation IS NOT NULL AND NOT EXISTS (SELECT 1 FROM registry_credential_generations WHERE org_id=NEW.org_id AND registry_id=NEW.id AND generation=NEW.pending_generation) THEN
        RAISE EXCEPTION 'pending registry generation is not tenant-local' USING ERRCODE='foreign_key_violation';
    END IF;
    IF NEW.previous_generation IS NOT NULL AND NOT EXISTS (SELECT 1 FROM registry_credential_generations WHERE org_id=NEW.org_id AND registry_id=NEW.id AND generation=NEW.previous_generation) THEN
        RAISE EXCEPTION 'previous registry generation is not tenant-local' USING ERRCODE='foreign_key_violation';
    END IF;
    IF NEW.operation_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM registry_credential_operations WHERE id=NEW.operation_id AND org_id=NEW.org_id AND registry_id=NEW.id) THEN
        RAISE EXCEPTION 'registry operation is not tenant-local' USING ERRCODE='foreign_key_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER registries_credential_references
    BEFORE INSERT OR UPDATE OF org_id,id,active_generation,pending_generation,previous_generation,operation_id ON registries
    FOR EACH ROW EXECUTE FUNCTION enforce_registry_credential_references();
CREATE INDEX registries_active_generation_ref ON registries(org_id,id,active_generation) WHERE active_generation>0;
CREATE INDEX registries_pending_generation_ref ON registries(org_id,id,pending_generation) WHERE pending_generation IS NOT NULL;
CREATE INDEX registries_previous_generation_ref ON registries(org_id,id,previous_generation) WHERE previous_generation IS NOT NULL;
CREATE INDEX registries_operation_ref ON registries(operation_id,org_id,id) WHERE operation_id IS NOT NULL;

CREATE FUNCTION assert_registry_generation_consistency(target_org uuid,target_registry uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE registry_row registries;
BEGIN
    SELECT * INTO registry_row FROM registries WHERE org_id=target_org AND id=target_registry;
    IF NOT FOUND THEN RETURN; END IF;
    IF registry_row.active_generation=0 THEN
        IF registry_row.active_credential_secret_ref IS NOT NULL OR EXISTS (
            SELECT 1 FROM registry_credential_generations WHERE org_id=target_org AND registry_id=target_registry AND state='active') THEN
            RAISE EXCEPTION 'zero active registry generation has active credential state' USING ERRCODE='check_violation';
        END IF;
    ELSIF NOT EXISTS (SELECT 1 FROM registry_credential_generations generation
        WHERE generation.org_id=target_org AND generation.registry_id=target_registry
          AND generation.generation=registry_row.active_generation AND generation.state='active'
          AND generation.secret_ref=registry_row.active_credential_secret_ref) THEN
        RAISE EXCEPTION 'registry active generation and credential row disagree' USING ERRCODE='check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM registry_credential_generations generation
        WHERE generation.org_id=target_org AND generation.registry_id=target_registry AND generation.state='active'
          AND generation.generation<>registry_row.active_generation) THEN
        RAISE EXCEPTION 'active credential generation does not equal registry pointer' USING ERRCODE='check_violation';
    END IF;
    IF registry_row.pending_generation IS NULL THEN
        IF registry_row.pending_credential_secret_ref IS NOT NULL THEN
            RAISE EXCEPTION 'pending credential reference has no generation' USING ERRCODE='check_violation';
        END IF;
    ELSIF NOT EXISTS (SELECT 1 FROM registry_credential_generations generation
        JOIN registry_credential_operations operation ON operation.id=generation.operation_id
         AND operation.org_id=generation.org_id AND operation.registry_id=generation.registry_id
         AND operation.candidate_generation=generation.generation
        WHERE generation.org_id=target_org AND generation.registry_id=target_registry
          AND generation.generation=registry_row.pending_generation AND generation.state='pending'
          AND generation.secret_ref=registry_row.pending_credential_secret_ref
          AND operation.status IN ('pending','leased') AND operation.id=registry_row.operation_id) THEN
        RAISE EXCEPTION 'registry pending generation, operation, and credential row disagree' USING ERRCODE='check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM registry_credential_generations generation
        JOIN registry_credential_operations operation ON operation.id=generation.operation_id
         AND operation.org_id=generation.org_id AND operation.registry_id=generation.registry_id
        WHERE generation.org_id=target_org AND generation.registry_id=target_registry AND generation.state='pending'
          AND (operation.status NOT IN ('pending','leased') OR operation.candidate_generation<>generation.generation)) THEN
        RAISE EXCEPTION 'pending credential generation is not tied to a pending operation' USING ERRCODE='check_violation';
    END IF;
END
$$;
CREATE FUNCTION enforce_registry_generation_consistency() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE target_org uuid; target_registry uuid;
BEGIN
    IF TG_TABLE_NAME='registries' THEN
        target_org:=COALESCE(NEW.org_id,OLD.org_id); target_registry:=COALESCE(NEW.id,OLD.id);
    ELSE
        target_org:=COALESCE(NEW.org_id,OLD.org_id); target_registry:=COALESCE(NEW.registry_id,OLD.registry_id);
    END IF;
    PERFORM assert_registry_generation_consistency(target_org,target_registry);
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER registries_generation_consistency
    AFTER INSERT OR UPDATE ON registries DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_registry_generation_consistency();
CREATE CONSTRAINT TRIGGER registry_generations_registry_consistency
    AFTER INSERT OR UPDATE OR DELETE ON registry_credential_generations DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_registry_generation_consistency();
CREATE CONSTRAINT TRIGGER registry_operations_generation_consistency
    AFTER INSERT OR UPDATE OR DELETE ON registry_credential_operations DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_registry_generation_consistency();

CREATE TABLE registry_credential_validation_targets (
    operation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    registry_id uuid NOT NULL,
    repository text NOT NULL CHECK (valid_repository_path(repository,1)),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','validated','failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 12),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    failure_code text CHECK (failure_code IS NULL OR failure_code IN (
        'registry_auth_failed','registry_forbidden','registry_unavailable','registry_response_invalid',
        'credential_validation_failed','credential_operation_exhausted','postgres_retryable','postgres_permanent')),
    validated_at timestamptz,
    PRIMARY KEY (operation_id,repository),
    FOREIGN KEY (operation_id,org_id,registry_id) REFERENCES registry_credential_operations(id,org_id,registry_id) ON DELETE CASCADE,
    CHECK ((status='validated') = (validated_at IS NOT NULL)),
    CHECK ((status='failed') = (failure_code IS NOT NULL)),
    CHECK (status='failed' OR failure_code IS NULL)
);
CREATE INDEX registry_credential_validation_targets_operation_fk
    ON registry_credential_validation_targets(operation_id,org_id,registry_id);
CREATE INDEX registry_credential_validation_targets_due
    ON registry_credential_validation_targets(next_attempt_at,operation_id,repository) WHERE status='pending' AND attempts<12;
CREATE INDEX registry_credential_validation_targets_exhausted
    ON registry_credential_validation_targets(operation_id,repository) WHERE status='pending' AND attempts>=12;

ALTER TABLE environments
    DROP CONSTRAINT environments_registry_generation_consistent,
    ADD COLUMN registry_credential_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN registry_credential_next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN registry_credential_lease_owner text,
    ADD COLUMN registry_credential_lease_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN registry_credential_lease_expires_at timestamptz,
    ADD CONSTRAINT environments_registry_credential_attempts_check CHECK (registry_credential_attempts BETWEEN 0 AND 12),
    ADD CONSTRAINT environments_registry_credential_lease_epoch_check CHECK (registry_credential_lease_epoch >= 0),
    ADD CONSTRAINT environments_registry_credential_lease_owner_check CHECK (registry_credential_lease_owner IS NULL OR (btrim(registry_credential_lease_owner) <> '' AND octet_length(registry_credential_lease_owner) <= 255)),
    ADD CONSTRAINT environments_registry_credential_lease_pair CHECK ((registry_credential_lease_owner IS NULL) = (registry_credential_lease_expires_at IS NULL)),
    ADD CONSTRAINT environments_registry_generation_consistent CHECK (
        (registry_credential_state='not_required' AND required_registry_credential_generation IS NULL AND ready_registry_credential_generation IS NULL)
        OR (registry_credential_state='pending' AND required_registry_credential_generation IS NOT NULL AND ready_registry_credential_generation IS NULL AND registry_credential_error_code IS NULL AND registry_credential_error_message IS NULL)
        OR (registry_credential_state='ready' AND required_registry_credential_generation IS NOT NULL AND ready_registry_credential_generation>=required_registry_credential_generation)
        OR (registry_credential_state='failed' AND required_registry_credential_generation IS NOT NULL AND registry_credential_error_code IS NOT NULL)),
    ADD CONSTRAINT environments_registry_credential_lease_state CHECK (registry_credential_lease_owner IS NULL OR registry_credential_state='pending');
CREATE INDEX environments_registry_credential_due
    ON environments(registry_credential_next_attempt_at,created_at,id)
    WHERE registry_credential_state='pending' AND registry_credential_lease_owner IS NULL AND torn_down_at IS NULL AND registry_credential_attempts<12;
CREATE INDEX environments_registry_credential_leased
    ON environments(registry_credential_lease_expires_at,id) WHERE registry_credential_lease_owner IS NOT NULL AND registry_credential_attempts<12;
CREATE INDEX environments_registry_credential_exhausted
    ON environments(created_at,id) WHERE registry_credential_state='pending' AND torn_down_at IS NULL AND registry_credential_attempts>=12;

CREATE FUNCTION environment_credential_row_valid(value environments) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT CASE value.registry_credential_state
        WHEN 'not_required' THEN value.required_registry_credential_generation IS NULL
            AND value.ready_registry_credential_generation IS NULL
            AND value.registry_credential_checkpoint IS NULL
            AND value.registry_credential_error_code IS NULL
            AND value.registry_credential_error_message IS NULL
        WHEN 'pending' THEN value.required_registry_credential_generation IS NOT NULL
            AND value.ready_registry_credential_generation IS NULL
            AND (value.registry_credential_checkpoint IS NULL OR value.registry_credential_checkpoint IN ('source_read','namespace_secret_written'))
            AND value.registry_credential_error_code IS NULL
            AND value.registry_credential_error_message IS NULL
        WHEN 'ready' THEN value.required_registry_credential_generation IS NOT NULL
            AND value.ready_registry_credential_generation=value.required_registry_credential_generation
            AND value.registry_credential_checkpoint='namespace_secret_verified'
            AND value.registry_credential_error_code IS NULL
            AND value.registry_credential_error_message IS NULL
        WHEN 'failed' THEN value.required_registry_credential_generation IS NOT NULL
            AND (value.ready_registry_credential_generation IS NULL
                 OR value.ready_registry_credential_generation<=value.required_registry_credential_generation)
            AND value.registry_credential_error_code IN ('image_pull_secret_reconcile_failed','kubernetes_ownership_conflict','kubernetes_unavailable','postgres_permanent')
        ELSE false END
       AND CASE app.image_visibility
        WHEN 'public' THEN value.registry_credential_state='not_required'
        WHEN 'private' THEN app.deleted_at IS NULL AND registry.id IS NOT NULL
            AND value.required_registry_credential_generation>0
            AND (value.ready_registry_credential_generation IS NULL OR value.ready_registry_credential_generation<=value.required_registry_credential_generation)
            AND EXISTS (SELECT 1 FROM registry_credential_generations generation
                WHERE generation.org_id=registry.org_id AND generation.registry_id=registry.id
                  AND generation.generation=value.required_registry_credential_generation
                  AND generation.state IN ('active','retired'))
        ELSE false END
    FROM apps app
    LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
    WHERE app.org_id=value.org_id AND app.id=value.app_id
$$;

CREATE FUNCTION enforce_environment_credential() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT environment_credential_row_valid(NEW) THEN
        RAISE EXCEPTION 'environment credential state is incompatible with enabled Phase 6' USING ERRCODE='check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER environments_credential_valid
    BEFORE INSERT OR UPDATE ON environments
    FOR EACH ROW EXECUTE FUNCTION enforce_environment_credential();
CREATE FUNCTION assert_dependent_environments(target_org uuid,target_app uuid) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM environments environment WHERE environment.org_id=target_org AND environment.app_id=target_app
           AND environment.torn_down_at IS NULL AND NOT environment_credential_row_valid(environment)) THEN
        RAISE EXCEPTION 'application or registry mutation invalidates a live environment credential snapshot' USING ERRCODE='check_violation';
    END IF;
END
$$;
CREATE FUNCTION enforce_app_environments() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM assert_dependent_environments(NEW.org_id,NEW.id); RETURN NEW; END $$;
CREATE FUNCTION enforce_registry_environments() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE app_row record;
BEGIN
    FOR app_row IN SELECT org_id,id FROM apps WHERE org_id=NEW.org_id AND registry_id=NEW.id LOOP
        PERFORM assert_dependent_environments(app_row.org_id,app_row.id);
    END LOOP;
    RETURN NEW;
END $$;
CREATE FUNCTION enforce_generation_environments() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE app_row record;
BEGIN
    FOR app_row IN SELECT org_id,id FROM apps WHERE org_id=COALESCE(NEW.org_id,OLD.org_id) AND registry_id=COALESCE(NEW.registry_id,OLD.registry_id) LOOP
        PERFORM assert_dependent_environments(app_row.org_id,app_row.id);
    END LOOP;
    RETURN NULL;
END $$;
CREATE TRIGGER apps_environments_valid AFTER UPDATE OF image_visibility,registry_id,deleted_at ON apps
    FOR EACH ROW EXECUTE FUNCTION enforce_app_environments();
CREATE TRIGGER registries_environments_valid AFTER UPDATE OF state,active_generation,active_credential_secret_ref,pending_generation,pending_credential_secret_ref ON registries
    FOR EACH ROW EXECUTE FUNCTION enforce_registry_environments();
CREATE CONSTRAINT TRIGGER registry_generations_environments_valid AFTER UPDATE OF state ON registry_credential_generations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION enforce_generation_environments();

ALTER TABLE orchestration_jobs
    ADD COLUMN oci_lease_owner text,
    ADD COLUMN oci_lease_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN oci_lease_expires_at timestamptz,
    ADD COLUMN oci_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN oci_next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN oci_deadline_at timestamptz,
    ADD COLUMN oci_error_code text,
    ADD COLUMN resolved_registry_id uuid,
    ADD COLUMN resolved_registry_generation bigint,
    ADD COLUMN oci_config_digest text,
    ADD COLUMN oci_config_media_type text,
    ADD COLUMN oci_source text,
    ADD COLUMN oci_revision text,
    ADD COLUMN oci_annotation_status text NOT NULL DEFAULT 'pending',
    ADD CONSTRAINT orchestration_jobs_oci_lease_owner_check CHECK (oci_lease_owner IS NULL OR (btrim(oci_lease_owner) <> '' AND octet_length(oci_lease_owner) <= 255)),
    ADD CONSTRAINT orchestration_jobs_oci_lease_pair CHECK ((oci_lease_owner IS NULL) = (oci_lease_expires_at IS NULL)),
    ADD CONSTRAINT orchestration_jobs_oci_lease_epoch_check CHECK (oci_lease_epoch >= 0),
    ADD CONSTRAINT orchestration_jobs_oci_attempts_check CHECK (oci_attempts BETWEEN 0 AND 20),
    ADD CONSTRAINT orchestration_jobs_oci_error_code_check CHECK (oci_error_code IS NULL OR oci_error_code IN (
        'image_not_published','registry_auth_required','registry_auth_failed','registry_auth_challenge_invalid',
        'registry_forbidden','registry_rate_limited','registry_unavailable','registry_response_invalid',
        'registry_digest_invalid','registry_media_type_unsupported','registry_config_invalid',
        'oci_annotation_conflict','oci_source_mismatch','oci_revision_mismatch','postgres_retryable','postgres_permanent')),
    ADD CONSTRAINT orchestration_jobs_oci_config_digest_check CHECK (oci_config_digest IS NULL OR oci_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    ADD CONSTRAINT orchestration_jobs_oci_revision_check CHECK (oci_revision IS NULL OR oci_revision ~ '^[0-9a-f]{40}$'),
    ADD CONSTRAINT orchestration_jobs_oci_annotation_status_check CHECK (oci_annotation_status IN ('pending','matched','absent','mismatched','legacy')),
    ADD CONSTRAINT orchestration_jobs_resolved_registry_generation_check CHECK (resolved_registry_generation IS NULL OR resolved_registry_generation > 0),
    ADD CONSTRAINT orchestration_jobs_resolved_registry_pair CHECK ((resolved_registry_id IS NULL)=(resolved_registry_generation IS NULL)),
    ADD CONSTRAINT orchestration_jobs_oci_lease_state CHECK (oci_lease_owner IS NULL OR status='resolving_image' AND oci_resolution_state IN ('pending','polling'));
UPDATE orchestration_jobs SET oci_annotation_status='legacy'
    WHERE oci_resolution_state='resolved' OR status IN ('deploying','completed');
ALTER TABLE orchestration_jobs ADD CONSTRAINT orchestration_jobs_resolved_registry_generation_fk
    FOREIGN KEY (org_id,resolved_registry_id,resolved_registry_generation)
    REFERENCES registry_credential_generations(org_id,registry_id,generation) DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX orchestration_jobs_oci_due
    ON orchestration_jobs(oci_next_attempt_at,created_at,id)
    WHERE status='resolving_image' AND oci_resolution_state IN ('pending','polling') AND oci_lease_owner IS NULL AND oci_attempts<20;
CREATE INDEX orchestration_jobs_oci_leased
    ON orchestration_jobs(oci_lease_expires_at,id) WHERE oci_lease_owner IS NOT NULL AND oci_attempts<20;
CREATE INDEX orchestration_jobs_oci_deadline
    ON orchestration_jobs(oci_deadline_at,id) WHERE status='resolving_image' AND oci_deadline_at IS NOT NULL;
CREATE INDEX orchestration_jobs_oci_exhausted
    ON orchestration_jobs(created_at,id) WHERE status='resolving_image' AND oci_resolution_state IN ('pending','polling') AND oci_attempts>=20;

ALTER TABLE releases ADD COLUMN registry_credential_registry_id uuid;
ALTER TABLE releases ADD COLUMN registry_credential_generation bigint;
ALTER TABLE releases
    ADD CONSTRAINT releases_registry_credential_generation_check CHECK (registry_credential_generation IS NULL OR registry_credential_generation > 0),
    ADD CONSTRAINT releases_registry_credential_pair CHECK ((registry_credential_registry_id IS NULL)=(registry_credential_generation IS NULL)),
    ADD CONSTRAINT releases_registry_credential_generation_fk FOREIGN KEY (org_id,registry_credential_registry_id,registry_credential_generation)
        REFERENCES registry_credential_generations(org_id,registry_id,generation) DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION orchestration_oci_valid(value orchestration_jobs) RETURNS boolean
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
    SELECT CASE
      WHEN value.oci_annotation_status='legacy' THEN
        value.oci_resolution_state='resolved' AND value.status IN ('deploying','completed')
        AND value.oci_lease_owner IS NULL AND value.oci_lease_epoch=0 AND value.oci_lease_expires_at IS NULL
        AND value.oci_attempts=0 AND value.oci_deadline_at IS NULL AND value.oci_error_code IS NULL
        AND value.resolved_registry_id IS NULL AND value.resolved_registry_generation IS NULL
        AND value.oci_config_digest IS NULL AND value.oci_config_media_type IS NULL
        AND value.oci_source IS NULL AND value.oci_revision IS NULL
      ELSE
        ((value.oci_lease_owner IS NULL AND value.oci_lease_expires_at IS NULL)
          OR (value.status='resolving_image' AND value.oci_resolution_state IN ('pending','polling')
              AND value.oci_lease_owner IS NOT NULL AND value.oci_lease_expires_at IS NOT NULL))
        AND (value.oci_resolution_state<>'failed' OR value.oci_error_code IS NOT NULL)
        AND (value.oci_resolution_state NOT IN ('resolved','not_required') OR value.oci_error_code IS NULL)
        AND (value.oci_config_digest IS NULL)=(value.oci_config_media_type IS NULL)
        AND (value.oci_source IS NULL)=(value.oci_revision IS NULL)
        AND CASE value.oci_resolution_state
          WHEN 'pending' THEN value.oci_annotation_status='pending' AND value.oci_deadline_at IS NULL
          WHEN 'polling' THEN value.oci_annotation_status='pending' AND value.oci_deadline_at IS NOT NULL
          WHEN 'failed' THEN value.oci_annotation_status IN ('pending','mismatched') AND value.oci_deadline_at IS NOT NULL
          WHEN 'not_required' THEN value.oci_annotation_status='absent' AND value.oci_deadline_at IS NULL
            AND value.resolved_registry_id IS NULL AND value.resolved_registry_generation IS NULL
          WHEN 'resolved' THEN value.oci_annotation_status IN ('matched','absent') AND value.oci_deadline_at IS NOT NULL
            AND value.oci_config_digest IS NOT NULL
            AND (value.oci_annotation_status='matched')=(value.oci_source IS NOT NULL)
          ELSE false END
        AND EXISTS (SELECT 1 FROM apps app WHERE app.org_id=value.org_id AND app.id=value.app_id
          AND CASE WHEN value.oci_resolution_state='resolved' THEN
            (app.image_visibility='public' AND value.resolved_registry_id IS NULL AND value.resolved_registry_generation IS NULL)
            OR (app.image_visibility='private' AND value.resolved_registry_id=app.registry_id
              AND value.resolved_registry_generation IS NOT NULL
              AND EXISTS (SELECT 1 FROM registry_credential_generations generation
                WHERE generation.org_id=value.org_id AND generation.registry_id=value.resolved_registry_id
                  AND generation.generation=value.resolved_registry_generation))
          ELSE value.resolved_registry_id IS NULL AND value.resolved_registry_generation IS NULL END)
      END
$$;

CREATE FUNCTION enforce_orchestration_registry_evidence() RETURNS trigger LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
BEGIN
    IF NEW.oci_annotation_status='legacy' OR TG_OP='UPDATE' AND OLD.oci_annotation_status='legacy' THEN
        IF TG_OP='INSERT' OR OLD.oci_annotation_status IS DISTINCT FROM 'legacy'
           OR NEW.oci_annotation_status IS DISTINCT FROM 'legacy'
           OR NEW.oci_resolution_state IS DISTINCT FROM OLD.oci_resolution_state
           OR NEW.oci_lease_owner IS DISTINCT FROM OLD.oci_lease_owner
           OR NEW.oci_lease_epoch IS DISTINCT FROM OLD.oci_lease_epoch
           OR NEW.oci_lease_expires_at IS DISTINCT FROM OLD.oci_lease_expires_at
           OR NEW.oci_attempts IS DISTINCT FROM OLD.oci_attempts
           OR NEW.oci_next_attempt_at IS DISTINCT FROM OLD.oci_next_attempt_at
           OR NEW.resolved_registry_id IS DISTINCT FROM OLD.resolved_registry_id
           OR NEW.resolved_registry_generation IS DISTINCT FROM OLD.resolved_registry_generation
           OR NEW.oci_deadline_at IS DISTINCT FROM OLD.oci_deadline_at
           OR NEW.oci_config_digest IS DISTINCT FROM OLD.oci_config_digest
           OR NEW.oci_config_media_type IS DISTINCT FROM OLD.oci_config_media_type
           OR NEW.oci_source IS DISTINCT FROM OLD.oci_source OR NEW.oci_revision IS DISTINCT FROM OLD.oci_revision
           OR NEW.oci_error_code IS DISTINCT FROM OLD.oci_error_code
           OR NEW.oci_annotation_status IS DISTINCT FROM OLD.oci_annotation_status THEN
            RAISE EXCEPTION 'legacy OCI evidence is immutable after Phase 6 enablement' USING ERRCODE='check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT orchestration_oci_valid(NEW) THEN
        RAISE EXCEPTION 'orchestration OCI state is incompatible with enabled Phase 6' USING ERRCODE='check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE FUNCTION enforce_release_registry_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    visibility text;
    registry_id uuid;
BEGIN
    SELECT app.image_visibility,app.registry_id INTO visibility,registry_id
      FROM environments environment
      JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
      LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
     WHERE environment.org_id=NEW.org_id AND environment.id=NEW.environment_id;
    IF NOT FOUND OR (visibility='private' AND (NEW.registry_credential_registry_id IS DISTINCT FROM registry_id OR NEW.registry_credential_generation IS NULL)) OR (visibility='public' AND (NEW.registry_credential_registry_id IS NOT NULL OR NEW.registry_credential_generation IS NOT NULL)) THEN
        RAISE EXCEPTION 'release registry generation does not match tenant application visibility' USING ERRCODE='check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER orchestration_jobs_registry_evidence
    BEFORE INSERT OR UPDATE ON orchestration_jobs
    FOR EACH ROW EXECUTE FUNCTION enforce_orchestration_registry_evidence();
CREATE TRIGGER releases_registry_evidence
    BEFORE INSERT OR UPDATE OF org_id,environment_id,registry_credential_registry_id,registry_credential_generation ON releases
    FOR EACH ROW EXECUTE FUNCTION enforce_release_registry_evidence();

CREATE OR REPLACE FUNCTION reject_release_snapshot_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
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
       OR NEW.registry_credential_registry_id IS DISTINCT FROM OLD.registry_credential_registry_id
       OR NEW.registry_credential_generation IS DISTINCT FROM OLD.registry_credential_generation
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'release snapshots are immutable' USING ERRCODE='integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION assert_oci_ghcr_v15_down_safe() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    -- Lock order for rollback is schema_epoch, registries, operations, generations,
    -- validation targets, environments, orchestration jobs, then releases.
    IF EXISTS (SELECT 1 FROM registry_credential_operations WHERE status IN ('pending','leased'))
       OR EXISTS (SELECT 1 FROM registries WHERE lease_owner IS NOT NULL)
       OR EXISTS (SELECT 1 FROM environments WHERE registry_credential_lease_owner IS NOT NULL)
       OR EXISTS (SELECT 1 FROM orchestration_jobs WHERE oci_lease_owner IS NOT NULL) THEN
        RAISE EXCEPTION '000015 rollback blocked: nonterminal operations or leases remain' USING ERRCODE='check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM registry_credential_generations)
       OR EXISTS (SELECT 1 FROM registry_credential_operations)
       OR EXISTS (SELECT 1 FROM registry_credential_validation_targets)
        OR EXISTS (SELECT 1 FROM registries WHERE checkpoint IN ('namespaces_ready','rollback_reconciling')
            OR previous_generation IS NOT NULL OR operation_id IS NOT NULL OR lease_epoch<>0 OR attempts<>0 OR activated_at IS NOT NULL)
       OR EXISTS (SELECT 1 FROM environments WHERE required_registry_credential_generation IS NOT NULL OR ready_registry_credential_generation IS NOT NULL OR registry_credential_state<>'not_required' OR registry_credential_checkpoint IS NOT NULL OR registry_credential_error_code IS NOT NULL OR registry_credential_error_message IS NOT NULL OR registry_credential_attempts<>0 OR registry_credential_lease_epoch<>0)
        OR EXISTS (SELECT 1 FROM orchestration_jobs WHERE oci_lease_epoch<>0 OR oci_attempts<>0 OR oci_deadline_at IS NOT NULL OR oci_error_code IS NOT NULL OR resolved_registry_id IS NOT NULL OR resolved_registry_generation IS NOT NULL OR oci_config_digest IS NOT NULL OR oci_config_media_type IS NOT NULL OR oci_source IS NOT NULL OR oci_revision IS NOT NULL
            OR oci_annotation_status NOT IN ('pending','legacy')
            OR oci_annotation_status='legacy' AND NOT (oci_resolution_state='resolved' AND status IN ('deploying','completed')))
       OR EXISTS (SELECT 1 FROM releases WHERE registry_credential_registry_id IS NOT NULL OR registry_credential_generation IS NOT NULL) THEN
        RAISE EXCEPTION '000015 rollback blocked: Phase 6 credential, reconciliation, OCI, or release evidence remains' USING ERRCODE='check_violation';
    END IF;
END
$$;
CREATE FUNCTION guard_oci_ghcr_v15_down() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE version=15 AND dirty=false) THEN
        PERFORM assert_oci_ghcr_v15_down_safe();
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER guard_oci_ghcr_v15_down
    BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT EXECUTE FUNCTION guard_oci_ghcr_v15_down();
