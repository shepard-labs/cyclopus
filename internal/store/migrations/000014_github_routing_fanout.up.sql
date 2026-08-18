CREATE TABLE github_routing_fanout_manifests (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    delivery_id uuid NOT NULL,
    org_id uuid NOT NULL,
    repository_id bigint NOT NULL CHECK (repository_id > 0),
    repository_node_id text NOT NULL CHECK (btrim(repository_node_id) <> '' AND octet_length(repository_node_id) <= 255),
    installation_id bigint NOT NULL CHECK (installation_id > 0),
    ref text NOT NULL CHECK (ref ~ '^refs/heads/[^[:space:]]+$' AND octet_length(ref) BETWEEN 12 AND 1035),
    event_kind text NOT NULL CHECK (event_kind IN ('push','delete')),
    commit_sha text CHECK (commit_sha IS NULL OR commit_sha ~ '^[0-9a-f]{40}$'),
    routing_generation bigint NOT NULL CHECK (routing_generation > 0),
    repository_generation bigint NOT NULL CHECK (repository_generation > 0),
    ref_lifecycle_generation bigint NOT NULL CHECK (ref_lifecycle_generation >= 0),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','leased','completed','superseded','failed')),
    eligibility_cutoff_app_id uuid,
    cursor_app_id uuid,
    total_count integer NOT NULL CHECK (total_count BETWEEN 0 AND 10000),
    inserted_count integer NOT NULL DEFAULT 0 CHECK (inserted_count BETWEEN 0 AND 10000),
    production_delete_count integer NOT NULL DEFAULT 0 CHECK (production_delete_count BETWEEN 0 AND 10000),
    batch_size integer NOT NULL CHECK (batch_size BETWEEN 1 AND 10000),
    max_fanout integer NOT NULL CHECK (max_fanout BETWEEN 1 AND 10000),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 12),
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    lease_owner text,
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_expires_at timestamptz,
    error_code text,
    error_detail text,
    snapshotted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    terminal_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (delivery_id),
    UNIQUE (id,org_id),
    UNIQUE (delivery_id,repository_id,ref,routing_generation),
    FOREIGN KEY (delivery_id,org_id,repository_id,repository_node_id)
        REFERENCES github_webhook_deliveries(id,org_id,repository_id,repository_node_id) MATCH FULL ON DELETE RESTRICT,
    FOREIGN KEY (org_id,installation_id)
        REFERENCES github_installations(org_id,github_installation_id) ON DELETE RESTRICT,
    FOREIGN KEY (repository_id,repository_node_id)
        REFERENCES github_repository_routing_state(repository_id,repository_node_id) ON DELETE RESTRICT,
    FOREIGN KEY (repository_id,ref)
        REFERENCES github_ref_routing_state(repository_id,ref) ON DELETE RESTRICT,
    CHECK ((event_kind='push')=(commit_sha IS NOT NULL)),
    CHECK (total_count <= max_fanout),
    CHECK (inserted_count <= total_count),
    CHECK ((total_count=0)=(eligibility_cutoff_app_id IS NULL)),
    CHECK (cursor_app_id IS NULL OR eligibility_cutoff_app_id IS NOT NULL AND cursor_app_id <= eligibility_cutoff_app_id),
    CHECK ((status IN ('pending','leased'))=(terminal_at IS NULL)),
    CHECK ((status='leased')=(lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK (status='leased' OR lease_owner IS NULL AND lease_expires_at IS NULL),
    CHECK (status<>'failed' OR error_code IS NOT NULL),
    CHECK (status<>'completed' OR inserted_count=total_count),
    CHECK (status<>'completed' OR (total_count=0 AND cursor_app_id IS NULL) OR cursor_app_id=eligibility_cutoff_app_id)
);

ALTER TABLE github_webhook_deliveries DROP CONSTRAINT github_webhook_deliveries_result_code_check;
ALTER TABLE github_webhook_deliveries ADD CONSTRAINT github_webhook_deliveries_result_code_check
    CHECK (result_code IS NULL OR result_code IN ('processed','fanout_staged','ignored','stale_push','production_branch_deleted','exhausted'));

CREATE INDEX github_routing_fanout_manifests_ref_generation
    ON github_routing_fanout_manifests(repository_id,ref,routing_generation,status);
CREATE INDEX github_routing_fanout_manifests_pending
    ON github_routing_fanout_manifests(next_attempt_at,updated_at,id) WHERE status='pending';
CREATE INDEX github_routing_fanout_manifests_leased
    ON github_routing_fanout_manifests(lease_expires_at,id) WHERE status='leased';
CREATE INDEX github_routing_fanout_manifests_terminal
    ON github_routing_fanout_manifests(terminal_at,id) WHERE status IN ('completed','superseded','failed');

CREATE TABLE github_routing_fanout_candidates (
    manifest_id uuid NOT NULL,
    org_id uuid NOT NULL,
    app_id uuid NOT NULL,
    environment_id uuid,
    config_generation bigint NOT NULL CHECK (config_generation > 0),
    workflow_path text NOT NULL CHECK (octet_length(workflow_path) BETWEEN 1 AND 255),
    production_ref boolean NOT NULL,
    snapshotted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (manifest_id,app_id),
    FOREIGN KEY (manifest_id,org_id) REFERENCES github_routing_fanout_manifests(id,org_id) ON DELETE CASCADE,
    FOREIGN KEY (org_id,app_id) REFERENCES apps(org_id,id) ON DELETE RESTRICT,
    FOREIGN KEY (org_id,app_id,environment_id) REFERENCES environments(org_id,app_id,id) ON DELETE RESTRICT
);

CREATE INDEX github_routing_fanout_candidates_app_identity
    ON github_routing_fanout_candidates(org_id,app_id,environment_id);

CREATE FUNCTION assert_github_routing_fanout_v14_down_safe() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
	IF EXISTS (SELECT 1 FROM github_webhook_deliveries WHERE result_code='fanout_staged') THEN
		RAISE EXCEPTION '000014 rollback blocked: map or clean up GitHub webhook deliveries with result_code=fanout_staged before retrying'
			USING ERRCODE='check_violation';
	END IF;
	IF EXISTS (SELECT 1 FROM github_routing_fanout_manifests WHERE status IN ('pending','leased')) THEN
        RAISE EXCEPTION '000014 rollback blocked: complete, fail, or supersede active GitHub routing fanout manifests before retrying'
            USING ERRCODE='check_violation';
    END IF;
END
$$;

CREATE FUNCTION guard_github_routing_fanout_v14_down() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE version=14 AND dirty=false) THEN
        PERFORM assert_github_routing_fanout_v14_down_safe();
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER guard_github_routing_fanout_v14_down BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT EXECUTE FUNCTION guard_github_routing_fanout_v14_down();
