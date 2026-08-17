CREATE TABLE github_webhook_deliveries (
    id uuid PRIMARY KEY DEFAULT uuidv7(), delivery_id uuid NOT NULL UNIQUE,
    event text NOT NULL CHECK (event ~ '^[a-z][a-z0-9_]{0,63}$'), action text NOT NULL DEFAULT '' CHECK (action ~ '^[a-z0-9_]{0,63}$'),
    installation_id bigint CHECK (installation_id > 0), repository_id bigint CHECK (repository_id > 0),
    repository_node_id text CHECK (repository_node_id IS NULL OR (btrim(repository_node_id) <> '' AND octet_length(repository_node_id) <= 255)),
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT, payload_digest text NOT NULL CHECK (payload_digest ~ '^[0-9a-f]{64}$'),
    verified_payload bytea CHECK (verified_payload IS NULL OR octet_length(verified_payload) BETWEEN 18 AND 2097168),
    payload_nonce bytea CHECK (payload_nonce IS NULL OR octet_length(payload_nonce) = 12),
    key_version text NOT NULL CHECK (btrim(key_version) <> '' AND octet_length(key_version) <= 100),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','leased','completed','failed')),
    attempts smallint NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 12), next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    lease_owner text CHECK (lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)), lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0), lease_expires_at timestamptz,
    result_code text CHECK (result_code IS NULL OR result_code IN ('processed','ignored','stale_push','production_branch_deleted','exhausted')),
    error_code text CHECK (error_code IS NULL OR error_code IN ('database_unavailable','github_unavailable','github_rate_limited','permission_revoked','malformed_response','invalid_event','fanout_limit','internal','attempts_exhausted')),
    error_detail text CHECK (error_detail IS NULL OR octet_length(error_detail) <= 512), received_at timestamptz NOT NULL DEFAULT clock_timestamp(), last_received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    terminal_at timestamptz, payload_redacted_at timestamptz, retain_until timestamptz, updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((status = 'leased') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK (status IN ('pending','leased') OR terminal_at IS NOT NULL), CHECK (status NOT IN ('completed','failed') OR (lease_owner IS NULL AND lease_expires_at IS NULL)),
    CHECK (status <> 'completed' OR (result_code IS NOT NULL AND error_code IS NULL)), CHECK (status <> 'failed' OR error_code IS NOT NULL),
    CHECK ((verified_payload IS NULL) = (payload_nonce IS NULL)), CHECK ((verified_payload IS NULL) = (payload_redacted_at IS NOT NULL)), CHECK (payload_redacted_at IS NULL OR terminal_at IS NOT NULL), CHECK (retain_until IS NULL OR terminal_at IS NOT NULL),
    CHECK ((repository_id IS NULL AND repository_node_id IS NULL) OR (installation_id IS NOT NULL AND repository_id IS NOT NULL AND repository_node_id IS NOT NULL))
);
REVOKE SELECT (verified_payload) ON github_webhook_deliveries FROM PUBLIC;
CREATE INDEX github_webhook_deliveries_due ON github_webhook_deliveries(next_attempt_at,received_at,id) WHERE status='pending' AND attempts<12;
CREATE INDEX github_webhook_deliveries_expired_lease ON github_webhook_deliveries(lease_expires_at,received_at,id) WHERE status='leased' AND attempts<12;
CREATE INDEX github_webhook_deliveries_terminal_retention ON github_webhook_deliveries(retain_until,id) WHERE status IN ('completed','failed') AND verified_payload IS NOT NULL;

ALTER TABLE github_installations ADD CONSTRAINT github_installations_org_external_unique UNIQUE(org_id,github_installation_id);
ALTER TABLE github_repositories ADD CONSTRAINT github_repositories_id_node_unique UNIQUE(github_repository_id,node_id);
ALTER TABLE github_repositories ADD CONSTRAINT github_repositories_org_installation_id_node_unique UNIQUE(org_id,github_installation_id,github_repository_id,node_id);
ALTER TABLE github_webhook_deliveries ADD CONSTRAINT github_webhook_deliveries_installation_fk FOREIGN KEY(org_id,installation_id) REFERENCES github_installations(org_id,github_installation_id) ON DELETE RESTRICT;
ALTER TABLE github_webhook_deliveries ADD CONSTRAINT github_webhook_deliveries_repository_fk FOREIGN KEY(org_id,installation_id,repository_id,repository_node_id) REFERENCES github_repositories(org_id,github_installation_id,github_repository_id,node_id) ON DELETE RESTRICT;
ALTER TABLE github_webhook_deliveries ADD CONSTRAINT github_webhook_deliveries_identity_unique UNIQUE(id,org_id,repository_id,repository_node_id);
CREATE TABLE github_repository_routing_state (
 repository_id bigint PRIMARY KEY REFERENCES github_repositories(github_repository_id) ON DELETE RESTRICT, org_id uuid NOT NULL,
 repository_node_id text NOT NULL, repository_generation bigint NOT NULL CHECK(repository_generation>0), lifecycle_generation bigint NOT NULL DEFAULT 0 CHECK(lifecycle_generation>=0), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(org_id,repository_id), UNIQUE(repository_id,repository_node_id), FOREIGN KEY(org_id,repository_id) REFERENCES github_repositories(org_id,github_repository_id) ON DELETE RESTRICT,
 FOREIGN KEY(repository_id,repository_node_id) REFERENCES github_repositories(github_repository_id,node_id) ON DELETE RESTRICT
);
CREATE TABLE github_ref_routing_state (
 repository_id bigint NOT NULL REFERENCES github_repository_routing_state(repository_id) ON DELETE RESTRICT, ref text NOT NULL CHECK(ref ~ '^refs/heads/[^[:space:]]+$' AND octet_length(ref) BETWEEN 12 AND 1035),
 routing_generation bigint NOT NULL DEFAULT 0 CHECK(routing_generation>=0), lifecycle_generation bigint NOT NULL DEFAULT 0 CHECK(lifecycle_generation>=0), validated_sha text CHECK(validated_sha IS NULL OR validated_sha ~ '^[0-9a-f]{40}$'), ref_exists boolean NOT NULL DEFAULT true, updated_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(repository_id,ref)
);

CREATE TABLE github_routing_intents (
 id uuid PRIMARY KEY DEFAULT uuidv7(), delivery_id uuid NOT NULL REFERENCES github_webhook_deliveries(id) ON DELETE RESTRICT, org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
 repository_id bigint NOT NULL, repository_node_id text NOT NULL, app_id uuid NOT NULL, environment_id uuid, ref text NOT NULL, commit_sha text CHECK(commit_sha IS NULL OR commit_sha ~ '^[0-9a-f]{40}$'),
 event_kind text NOT NULL CHECK(event_kind IN ('push','delete')), routing_generation bigint NOT NULL CHECK(routing_generation>0), repository_generation bigint NOT NULL CHECK(repository_generation>0), ref_lifecycle_generation bigint NOT NULL CHECK(ref_lifecycle_generation>=0), config_generation bigint NOT NULL CHECK(config_generation>0), workflow_path text NOT NULL CHECK(octet_length(workflow_path) BETWEEN 1 AND 255), production_ref boolean NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','leased','completed','failed','dropped')), attempts smallint NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 12), next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_owner text CHECK(lease_owner IS NULL OR (btrim(lease_owner)<>'' AND octet_length(lease_owner)<=255)), lease_epoch bigint NOT NULL DEFAULT 0 CHECK(lease_epoch>=0), lease_expires_at timestamptz,
 result_code text CHECK(result_code IS NULL OR result_code IN ('processed','ignored','stale','superseded','exhausted')), error_code text CHECK(error_code IS NULL OR error_code IN ('database_unavailable','github_unavailable','github_rate_limited','permission_revoked','invalid_snapshot','internal','attempts_exhausted')), error_detail text CHECK(error_detail IS NULL OR octet_length(error_detail)<=512),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), terminal_at timestamptz, updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(delivery_id,app_id,ref,event_kind,routing_generation), FOREIGN KEY(org_id,app_id) REFERENCES apps(org_id,id) ON DELETE RESTRICT,
 FOREIGN KEY(delivery_id,org_id,repository_id,repository_node_id) REFERENCES github_webhook_deliveries(id,org_id,repository_id,repository_node_id) MATCH FULL ON DELETE RESTRICT, FOREIGN KEY(org_id,repository_id) REFERENCES github_repositories(org_id,github_repository_id) ON DELETE RESTRICT, FOREIGN KEY(repository_id,repository_node_id) REFERENCES github_repository_routing_state(repository_id,repository_node_id) ON DELETE RESTRICT, FOREIGN KEY(repository_id,ref) REFERENCES github_ref_routing_state(repository_id,ref) ON DELETE RESTRICT,
 FOREIGN KEY(org_id,app_id,environment_id) REFERENCES environments(org_id,app_id,id) ON DELETE RESTRICT,
 CHECK((status='leased')=(lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)), CHECK(status IN ('pending','leased') OR terminal_at IS NOT NULL),
 CHECK(status NOT IN ('completed','failed','dropped') OR (lease_owner IS NULL AND lease_expires_at IS NULL)), CHECK(status<>'completed' OR (result_code IS NOT NULL AND error_code IS NULL)), CHECK(status<>'failed' OR error_code IS NOT NULL), CHECK((event_kind='push')=(commit_sha IS NOT NULL))
);
CREATE INDEX github_routing_intents_due ON github_routing_intents(next_attempt_at,created_at,id) WHERE status='pending' AND attempts<12;
CREATE INDEX github_routing_intents_expired_lease ON github_routing_intents(lease_expires_at,created_at,id) WHERE status='leased' AND attempts<12;

CREATE FUNCTION guard_github_webhook_v10_down() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF EXISTS(SELECT 1 FROM github_webhook_deliveries WHERE status IN('pending','leased') OR verified_payload IS NOT NULL) OR EXISTS(SELECT 1 FROM github_routing_intents WHERE status IN('pending','leased')) THEN RAISE EXCEPTION '000010 rollback blocked: drain webhook deliveries/intents and redact retained payloads before retrying' USING ERRCODE='check_violation'; END IF; RETURN NULL; END $$;
CREATE TRIGGER guard_github_webhook_v10_down BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT EXECUTE FUNCTION guard_github_webhook_v10_down();
