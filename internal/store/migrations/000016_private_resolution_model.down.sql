LOCK TABLE apps, orchestration_jobs IN SHARE ROW EXCLUSIVE MODE;
SELECT assert_private_resolution_v16_down_safe();

DROP TRIGGER guard_private_resolution_v16_down ON schema_migrations;
DROP FUNCTION guard_private_resolution_v16_down();
DROP FUNCTION assert_private_resolution_v16_down_safe();

CREATE OR REPLACE FUNCTION enforce_orchestration_target_and_artifact() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected_repository text;
    target_repository_id bigint;
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
    SELECT app.image_repository, connection.github_repository_id, environment.desired_generation
      INTO expected_repository, target_repository_id, environment_generation
      FROM apps app
      JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
      JOIN git_connections connection ON connection.org_id = project.org_id AND connection.id = project.git_connection_id
      JOIN environments environment ON environment.org_id = app.org_id AND environment.app_id = app.id
     WHERE app.org_id = NEW.org_id AND app.id = NEW.app_id AND environment.id = NEW.environment_id;
    IF NOT FOUND OR target_repository_id IS DISTINCT FROM NEW.github_repository_id THEN
        RAISE EXCEPTION 'orchestration target does not match app project repository' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.pending_desired_generation IS NOT NULL
       AND NEW.pending_desired_generation <> environment_generation THEN
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
       AND NEW.desired_generation <> environment_generation THEN
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

CREATE OR REPLACE FUNCTION orchestration_oci_valid(value orchestration_jobs) RETURNS boolean
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

ALTER TABLE orchestration_jobs
    DROP CONSTRAINT orchestration_jobs_check9,
    ADD CONSTRAINT orchestration_jobs_check9 CHECK (
        status <> 'deploying' OR (oci_resolution_state = 'resolved' AND deployment_state IN ('ready', 'claimed', 'applying', 'live'))
    );

ALTER TABLE apps
    DROP CONSTRAINT apps_registry_source_consistency,
    DROP CONSTRAINT apps_image_repository_registry,
    ADD CONSTRAINT apps_registry_source_consistency CHECK (
        (registry_id IS NULL AND github_workflow_path IS NOT NULL AND production_branch IS NOT NULL) OR
        (registry_id IS NOT NULL AND github_workflow_path IS NULL AND production_branch IS NULL)
    );

UPDATE apps SET image_repository='ghcr.io/'||image_repository WHERE registry_id IS NULL;

ALTER TABLE apps
    ADD CONSTRAINT apps_image_repository_registry CHECK (
        (registry_id IS NULL
            AND pg_catalog.octet_length(image_repository) <= 255
            AND pg_catalog.left(image_repository,8)='ghcr.io/'
            AND valid_repository_path(pg_catalog.substring(image_repository,9),2)) OR
        (registry_id IS NOT NULL AND valid_repository_path(image_repository,1))
    );
