ALTER TABLE releases
    ADD COLUMN desired_generation bigint,
    ADD COLUMN orchestration_job_id uuid,
    ADD COLUMN provenance text NOT NULL DEFAULT 'legacy_unverified',
    ADD CONSTRAINT releases_desired_generation_check CHECK (desired_generation IS NULL OR desired_generation > 0),
    ADD CONSTRAINT releases_provenance_check CHECK (
        (provenance='legacy_unverified' AND desired_generation IS NULL AND orchestration_job_id IS NULL)
        OR (provenance='contextual' AND desired_generation IS NOT NULL AND orchestration_job_id IS NULL)
        OR (provenance='orchestration' AND desired_generation IS NOT NULL AND orchestration_job_id IS NOT NULL)),
    ADD CONSTRAINT releases_orchestration_job_fk FOREIGN KEY (org_id,orchestration_job_id)
        REFERENCES orchestration_jobs(org_id,id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED;

CREATE OR REPLACE FUNCTION validate_release_snapshot() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF NEW.config_snapshot->>'version' NOT IN ('2','3','4')
       OR jsonb_typeof(NEW.config_snapshot->'desired_state') <> 'object'
       OR NEW.config_snapshot#>>'{desired_state,version}' IS DISTINCT FROM '1'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,deployment}') <> 'object'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,service}') <> 'object'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,ingress}') <> 'object'
       OR jsonb_typeof(NEW.config_snapshot#>'{desired_state,scaling}') <> 'object' THEN
        RAISE EXCEPTION 'release config_snapshot must be a v2, v3, or v4 desired-state envelope'
            USING ERRCODE='check_violation';
    END IF;
    IF NEW.kind='env_change' AND (NEW.env_var_revision_id IS NULL
       OR jsonb_typeof(NEW.config_snapshot->'env_var_revision') <> 'object'
       OR NEW.config_snapshot#>>'{env_var_revision,id}' IS DISTINCT FROM NEW.env_var_revision_id::text) THEN
        RAISE EXCEPTION 'env-change release must contain its env-var revision' USING ERRCODE='check_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION enforce_release_generation_provenance() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
	IF NEW.provenance='legacy_unverified' THEN NEW.provenance := 'contextual'; END IF;
	IF NEW.desired_generation IS NULL THEN
		SELECT environment.desired_generation INTO NEW.desired_generation
		FROM environments environment WHERE environment.org_id=NEW.org_id AND environment.id=NEW.environment_id;
	END IF;
	IF NEW.desired_generation IS NULL OR NEW.desired_generation <= 0 THEN
		RAISE EXCEPTION 'new releases require a positive desired generation' USING ERRCODE='check_violation';
	END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER releases_generation_provenance
    BEFORE INSERT ON releases FOR EACH ROW EXECUTE FUNCTION enforce_release_generation_provenance();

CREATE OR REPLACE FUNCTION reject_release_snapshot_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
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
       OR NEW.desired_generation IS DISTINCT FROM OLD.desired_generation
       OR NEW.orchestration_job_id IS DISTINCT FROM OLD.orchestration_job_id
       OR NEW.provenance IS DISTINCT FROM OLD.provenance
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'release snapshots are immutable' USING ERRCODE='integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION assert_release_generation_v21_down_safe() RETURNS void
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM releases
        WHERE provenance<>'legacy_unverified' OR desired_generation IS NOT NULL OR orchestration_job_id IS NOT NULL) THEN
        RAISE EXCEPTION '000021 downgrade would discard release generation provenance or evidence'
            USING ERRCODE='dependent_objects_still_exist';
    END IF;
END
$$;
CREATE FUNCTION guard_release_generation_v21_down() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE version=21 AND dirty=false) THEN
        PERFORM assert_release_generation_v21_down_safe();
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER guard_release_generation_v21_down
    BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT
    EXECUTE FUNCTION guard_release_generation_v21_down();
