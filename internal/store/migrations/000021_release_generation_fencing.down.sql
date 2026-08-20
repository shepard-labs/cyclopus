LOCK TABLE releases IN SHARE ROW EXCLUSIVE MODE;
SELECT assert_release_generation_v21_down_safe();

DROP TRIGGER guard_release_generation_v21_down ON schema_migrations;
DROP FUNCTION guard_release_generation_v21_down();
DROP FUNCTION assert_release_generation_v21_down_safe();

DROP TRIGGER releases_generation_provenance ON releases;
DROP FUNCTION enforce_release_generation_provenance();

ALTER TABLE releases
    DROP CONSTRAINT releases_orchestration_job_fk,
    DROP CONSTRAINT releases_provenance_check,
    DROP CONSTRAINT releases_desired_generation_check,
    DROP COLUMN provenance,
    DROP COLUMN orchestration_job_id,
    DROP COLUMN desired_generation;

CREATE OR REPLACE FUNCTION validate_release_snapshot() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.config_snapshot->>'version' NOT IN ('2', '3')
       OR jsonb_typeof(NEW.config_snapshot->'desired_state') <> 'object'
       OR NEW.config_snapshot#>>'{desired_state,version}' IS DISTINCT FROM '1'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,deployment}') <> 'object'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,service}') <> 'object'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,ingress}') <> 'object'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,scaling}') <> 'object' THEN
        RAISE EXCEPTION 'release config_snapshot must be a v2 or v3 desired-state envelope'
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
