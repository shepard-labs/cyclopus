LOCK TABLE schema_epoch, registries, registry_credential_operations, registry_credential_generations,
    registry_credential_validation_targets, environments, orchestration_jobs, releases IN SHARE ROW EXCLUSIVE MODE;
SELECT assert_oci_ghcr_v15_down_safe();

DROP TRIGGER guard_oci_ghcr_v15_down ON schema_migrations;
DROP FUNCTION guard_oci_ghcr_v15_down();
DROP FUNCTION assert_oci_ghcr_v15_down_safe();

DROP FUNCTION enable_phase6_schema_epoch();
DROP TRIGGER schema_epoch_phase6_enable_guard ON schema_epoch;
DROP FUNCTION guard_phase6_schema_epoch_update();

DROP TRIGGER releases_phase6_registry_evidence ON releases;
DROP TRIGGER orchestration_jobs_phase6_registry_evidence ON orchestration_jobs;
DROP FUNCTION enforce_phase6_release_registry_evidence();
DROP FUNCTION enforce_phase6_orchestration_registry_evidence();
DROP FUNCTION phase6_orchestration_valid(orchestration_jobs);

CREATE OR REPLACE FUNCTION reject_release_snapshot_mutation() RETURNS trigger
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

ALTER TABLE releases DROP CONSTRAINT releases_registry_credential_generation_check;
ALTER TABLE releases DROP CONSTRAINT releases_registry_credential_pair;
ALTER TABLE releases DROP CONSTRAINT releases_registry_credential_generation_fk;
ALTER TABLE releases DROP COLUMN registry_credential_generation;
ALTER TABLE releases DROP COLUMN registry_credential_registry_id;

DROP INDEX orchestration_jobs_oci_deadline;
DROP INDEX orchestration_jobs_oci_exhausted;
DROP INDEX orchestration_jobs_oci_leased;
DROP INDEX orchestration_jobs_oci_due;
ALTER TABLE orchestration_jobs
    DROP CONSTRAINT orchestration_jobs_oci_lease_owner_check,
    DROP CONSTRAINT orchestration_jobs_oci_lease_pair,
    DROP CONSTRAINT orchestration_jobs_oci_lease_epoch_check,
    DROP CONSTRAINT orchestration_jobs_oci_attempts_check,
    DROP CONSTRAINT orchestration_jobs_oci_error_code_check,
    DROP CONSTRAINT orchestration_jobs_oci_config_digest_check,
    DROP CONSTRAINT orchestration_jobs_oci_revision_check,
    DROP CONSTRAINT orchestration_jobs_oci_annotation_status_check,
    DROP CONSTRAINT orchestration_jobs_resolved_registry_generation_check,
    DROP CONSTRAINT orchestration_jobs_resolved_registry_pair,
    DROP CONSTRAINT orchestration_jobs_resolved_registry_generation_fk,
    DROP CONSTRAINT orchestration_jobs_oci_lease_state,
    DROP COLUMN oci_lease_owner,
    DROP COLUMN oci_lease_epoch,
    DROP COLUMN oci_lease_expires_at,
    DROP COLUMN oci_attempts,
    DROP COLUMN oci_next_attempt_at,
    DROP COLUMN oci_deadline_at,
    DROP COLUMN oci_error_code,
    DROP COLUMN resolved_registry_id,
    DROP COLUMN resolved_registry_generation,
    DROP COLUMN oci_config_digest,
    DROP COLUMN oci_config_media_type,
    DROP COLUMN oci_source,
    DROP COLUMN oci_revision,
    DROP COLUMN oci_annotation_status;

DROP INDEX environments_registry_credential_leased;
DROP INDEX environments_registry_credential_exhausted;
DROP INDEX environments_registry_credential_due;
DROP TRIGGER registry_generations_phase6_environments ON registry_credential_generations;
DROP TRIGGER registries_phase6_environments ON registries;
DROP TRIGGER apps_phase6_environments ON apps;
DROP FUNCTION enforce_phase6_generation_environments();
DROP FUNCTION enforce_phase6_registry_environments();
DROP FUNCTION enforce_phase6_app_environments();
DROP FUNCTION assert_phase6_dependent_environments(uuid,uuid);
DROP TRIGGER environments_phase6_credential ON environments;
DROP FUNCTION enforce_phase6_environment_credential();
DROP FUNCTION phase6_environment_credential_row_valid(environments);
ALTER TABLE environments
    DROP CONSTRAINT environments_registry_generation_consistent,
    DROP CONSTRAINT environments_registry_credential_attempts_check,
    DROP CONSTRAINT environments_registry_credential_lease_epoch_check,
    DROP CONSTRAINT environments_registry_credential_lease_owner_check,
    DROP CONSTRAINT environments_registry_credential_lease_pair,
    DROP CONSTRAINT environments_registry_credential_lease_state,
    DROP COLUMN registry_credential_attempts,
    DROP COLUMN registry_credential_next_attempt_at,
    DROP COLUMN registry_credential_lease_owner,
    DROP COLUMN registry_credential_lease_epoch,
    DROP COLUMN registry_credential_lease_expires_at,
    ADD CONSTRAINT environments_registry_generation_consistent CHECK (
        (registry_credential_state='not_required' AND required_registry_credential_generation IS NULL AND ready_registry_credential_generation IS NULL)
        OR (registry_credential_state='pending' AND required_registry_credential_generation IS NOT NULL AND ready_registry_credential_generation IS NULL)
        OR (registry_credential_state='ready' AND required_registry_credential_generation IS NOT NULL AND ready_registry_credential_generation>=required_registry_credential_generation)
        OR (registry_credential_state='failed' AND required_registry_credential_generation IS NOT NULL AND registry_credential_error_code IS NOT NULL));

DROP TABLE registry_credential_validation_targets;
DROP TRIGGER registries_credential_references ON registries;
DROP FUNCTION enforce_registry_credential_references();
DROP TRIGGER registry_operations_generation_consistency ON registry_credential_operations;
DROP TRIGGER registry_generations_registry_consistency ON registry_credential_generations;
DROP TRIGGER registries_generation_consistency ON registries;
DROP FUNCTION enforce_phase6_registry_generation_consistency();
DROP FUNCTION assert_phase6_registry_generation_consistency(uuid,uuid);
DROP INDEX registries_operation_ref;
DROP INDEX registries_previous_generation_ref;
DROP INDEX registries_pending_generation_ref;
DROP INDEX registries_active_generation_ref;
DROP INDEX registry_credential_generations_one_active;
ALTER TABLE registry_credential_generations DROP CONSTRAINT registry_credential_generations_operation_fk;
DROP TRIGGER registry_credential_generations_identity_immutable ON registry_credential_generations;
DROP FUNCTION protect_registry_credential_generation_identity();
DROP TABLE registry_credential_operations;
DROP TABLE registry_credential_generations;

DROP TRIGGER registries_phase6_checkpoint ON registries;
DROP FUNCTION enforce_phase6_registry_checkpoint();
DROP FUNCTION phase6_registry_row_valid(registries);
DROP INDEX registries_operation_leased;
DROP INDEX registries_operation_exhausted;
DROP INDEX registries_operation_due;
ALTER TABLE registries
    DROP CONSTRAINT registries_state_check,
    DROP CONSTRAINT registries_checkpoint_check,
    DROP CONSTRAINT registries_previous_credential_pair,
    DROP CONSTRAINT registries_previous_generation_positive,
    DROP CONSTRAINT registries_previous_secret_ref_check,
    DROP CONSTRAINT registries_lease_pair,
    DROP CONSTRAINT registries_lease_owner_check,
    DROP CONSTRAINT registries_lease_epoch_check,
    DROP CONSTRAINT registries_attempts_check,
    DROP CONSTRAINT registries_state_checkpoint_consistent,
    DROP CONSTRAINT registries_activation_consistent,
    DROP COLUMN previous_generation,
    DROP COLUMN previous_credential_secret_ref,
    DROP COLUMN lease_owner,
    DROP COLUMN lease_epoch,
    DROP COLUMN lease_expires_at,
    DROP COLUMN attempts,
    DROP COLUMN next_attempt_at,
    DROP COLUMN activated_at;
UPDATE registries SET checkpoint=CASE checkpoint
    WHEN 'source_secret_write_started' THEN 'secret_write_started'
    WHEN 'source_secret_written' THEN 'secret_written'
    WHEN 'targets_validated' THEN 'verified'
    ELSE checkpoint END;
ALTER TABLE registries
    ADD CONSTRAINT registries_state_check CHECK (state IN ('absent','creating','ready','rotating','reconciling','failed','deleting')),
    ADD CONSTRAINT registries_checkpoint_check CHECK (checkpoint IS NULL OR checkpoint IN ('pending_recorded','secret_write_started','secret_written','verified','activated','namespaces_reconciling','cleanup_pending'));

ALTER TABLE schema_epoch DROP COLUMN phase6_enabled;
