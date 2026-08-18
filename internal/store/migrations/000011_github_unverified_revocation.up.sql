ALTER TABLE github_repository_routing_state ADD COLUMN installation_id bigint;
ALTER TABLE github_routing_intents ADD COLUMN installation_id bigint;
DROP TRIGGER IF EXISTS guard_github_webhook_v10_down ON schema_migrations;
DROP FUNCTION guard_github_webhook_v10_down();

LOCK TABLE github_installations, github_repositories, git_connections, github_webhook_deliveries,
    github_repository_routing_state, github_routing_intents IN SHARE ROW EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM github_repository_routing_state state
        LEFT JOIN github_repositories repository
          ON repository.org_id = state.org_id
         AND repository.github_repository_id = state.repository_id
         AND repository.node_id = state.repository_node_id
        WHERE repository.github_installation_id IS NULL
    ) THEN
        RAISE EXCEPTION '000011 installation backfill blocked: a routing-state repository identity has no unique authoritative github_repositories row; repair org/repository/node identity before retrying'
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM github_routing_intents intent
        LEFT JOIN github_webhook_deliveries delivery
          ON delivery.id = intent.delivery_id
         AND delivery.org_id = intent.org_id
         AND delivery.repository_id = intent.repository_id
         AND delivery.repository_node_id = intent.repository_node_id
        WHERE delivery.installation_id IS NULL
    ) THEN
        RAISE EXCEPTION '000011 installation backfill blocked: a routing intent has no authoritative installation identity on its webhook delivery; repair delivery/intent identity before retrying'
            USING ERRCODE = 'foreign_key_violation';
    END IF;
END
$$;

ALTER TABLE github_installations DROP CONSTRAINT github_installations_access_state_check;
ALTER TABLE github_installations ADD CONSTRAINT github_installations_access_state_check
    CHECK (access_state IN ('active', 'suspended', 'deleted', 'unverified_revocation'));

ALTER TABLE github_repositories DROP CONSTRAINT github_repositories_access_state_check;
ALTER TABLE github_repositories ADD CONSTRAINT github_repositories_access_state_check
    CHECK (access_state IN ('active', 'inaccessible', 'deleted', 'unverified_revocation'));

ALTER TABLE git_connections DROP CONSTRAINT git_connections_access_state_check;
ALTER TABLE git_connections ADD CONSTRAINT git_connections_access_state_check
    CHECK (access_state IN ('active', 'suspended', 'inaccessible', 'deleted', 'unverified_revocation'));

CREATE OR REPLACE FUNCTION protect_github_installation_claim() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'GitHub installation claims are retained as tombstones' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.github_installation_id IS DISTINCT FROM OLD.github_installation_id
       OR NEW.org_id IS DISTINCT FROM OLD.org_id
       OR NEW.account_id IS DISTINCT FROM OLD.account_id THEN
        RAISE EXCEPTION 'GitHub installation identity cannot be rebound' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.access_state = 'deleted' AND NEW.access_state <> 'deleted'
       AND current_setting('cyclopus.github_authoritative_reconcile', true) IS DISTINCT FROM 'on' THEN
        RAISE EXCEPTION 'deleted GitHub installation tombstones cannot be reactivated' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

UPDATE github_repository_routing_state state
SET installation_id = repository.github_installation_id
FROM github_repositories repository
WHERE repository.org_id = state.org_id
  AND repository.github_repository_id = state.repository_id
  AND repository.node_id = state.repository_node_id;

UPDATE github_routing_intents intent
SET installation_id = delivery.installation_id
FROM github_webhook_deliveries delivery
WHERE delivery.id = intent.delivery_id
  AND delivery.org_id = intent.org_id
  AND delivery.repository_id = intent.repository_id
  AND delivery.repository_node_id = intent.repository_node_id;

ALTER TABLE github_repository_routing_state
    ALTER COLUMN installation_id SET NOT NULL,
    ADD CONSTRAINT github_repository_routing_state_installation_id_check CHECK (installation_id > 0),
    ADD CONSTRAINT github_repository_routing_state_installation_fk FOREIGN KEY(org_id,installation_id) REFERENCES github_installations(org_id,github_installation_id) ON DELETE RESTRICT;
ALTER TABLE github_routing_intents
    ALTER COLUMN installation_id SET NOT NULL,
    ADD CONSTRAINT github_routing_intents_installation_id_check CHECK (installation_id > 0),
    ADD CONSTRAINT github_routing_intents_installation_fk FOREIGN KEY(org_id,installation_id) REFERENCES github_installations(org_id,github_installation_id) ON DELETE RESTRICT;

ALTER TABLE github_ref_routing_state
    ADD COLUMN last_delivery_id uuid,
    ADD COLUMN last_observed_received_at timestamptz,
    ADD COLUMN last_observation_sequence bigint;

ALTER TABLE github_webhook_deliveries ADD COLUMN ingestion_sequence bigserial;
WITH ordered AS (
    SELECT id, row_number() OVER (ORDER BY received_at, id) AS sequence
    FROM github_webhook_deliveries
)
UPDATE github_webhook_deliveries delivery
SET ingestion_sequence = ordered.sequence
FROM ordered
WHERE delivery.id = ordered.id;
SELECT setval(pg_get_serial_sequence('github_webhook_deliveries','ingestion_sequence'),
    COALESCE((SELECT max(ingestion_sequence) FROM github_webhook_deliveries), 1),
    EXISTS (SELECT 1 FROM github_webhook_deliveries));
ALTER TABLE github_webhook_deliveries ALTER COLUMN ingestion_sequence SET NOT NULL;
ALTER TABLE github_webhook_deliveries ADD CONSTRAINT github_webhook_deliveries_ingestion_sequence_unique UNIQUE(ingestion_sequence);
ALTER TABLE github_webhook_deliveries ADD CONSTRAINT github_webhook_deliveries_observation_identity_unique UNIQUE(id,received_at,ingestion_sequence);

ALTER TABLE github_ref_routing_state
    ADD CONSTRAINT github_ref_routing_state_last_observation_fk
    FOREIGN KEY(last_delivery_id,last_observed_received_at,last_observation_sequence)
    REFERENCES github_webhook_deliveries(id,received_at,ingestion_sequence) MATCH FULL ON DELETE RESTRICT;

CREATE TABLE github_ref_transition_observations (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    repository_id bigint NOT NULL,
    ref text NOT NULL,
    before_sha text NOT NULL CHECK (before_sha ~ '^[0-9a-f]{40}$'),
    after_sha text NOT NULL CHECK (after_sha ~ '^[0-9a-f]{40}$'),
    event_kind text NOT NULL CHECK (event_kind IN ('push','delete')),
    delivery_id uuid NOT NULL UNIQUE,
    received_at timestamptz NOT NULL,
    observation_sequence bigint NOT NULL,
    observation_kind text NOT NULL CHECK (observation_kind IN ('validated','resynchronized_gap')),
    transition_fingerprint text GENERATED ALWAYS AS (md5(repository_id::text || chr(31) || ref || chr(31) || before_sha || chr(31) || after_sha || chr(31) || event_kind)) STORED,
    FOREIGN KEY (repository_id, ref) REFERENCES github_ref_routing_state(repository_id, ref) ON DELETE RESTRICT,
    FOREIGN KEY (delivery_id, received_at, observation_sequence) REFERENCES github_webhook_deliveries(id,received_at,ingestion_sequence) ON DELETE RESTRICT
);
CREATE INDEX github_ref_transition_observations_transition ON github_ref_transition_observations(repository_id,ref,before_sha,after_sha,event_kind);
COMMENT ON COLUMN github_ref_transition_observations.transition_fingerprint IS 'Diagnostic transition correlation only; routing staleness is fenced by delivery identity, ingestion sequence, authoritative ref evidence, and current routing state.';

ALTER TABLE github_webhook_deliveries DROP CONSTRAINT github_webhook_deliveries_repository_fk;
ALTER TABLE github_repositories ADD CONSTRAINT github_repositories_org_id_node_unique UNIQUE(org_id,github_repository_id,node_id);
ALTER TABLE github_webhook_deliveries ADD CONSTRAINT github_webhook_deliveries_repository_fk
    FOREIGN KEY(org_id,repository_id,repository_node_id) REFERENCES github_repositories(org_id,github_repository_id,node_id) ON DELETE RESTRICT;

CREATE FUNCTION assert_github_webhook_v11_down_safe() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM github_installations WHERE access_state = 'unverified_revocation')
       OR EXISTS (SELECT 1 FROM github_repositories WHERE access_state = 'unverified_revocation')
       OR EXISTS (SELECT 1 FROM git_connections WHERE access_state = 'unverified_revocation') THEN
        RAISE EXCEPTION '000011 rollback blocked: reconcile unverified GitHub revocations before retrying' USING ERRCODE = 'check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM github_ref_transition_observations)
       OR EXISTS (SELECT 1 FROM github_ref_routing_state WHERE last_delivery_id IS NOT NULL OR last_observed_received_at IS NOT NULL OR last_observation_sequence IS NOT NULL) THEN
        RAISE EXCEPTION '000011 rollback blocked: ref transition observations exist; archive or remove observations and clear ref observation checkpoints before retrying' USING ERRCODE = 'check_violation';
    END IF;
    IF EXISTS (
        SELECT 1 FROM github_webhook_deliveries delivery
        JOIN github_repositories repository
          ON repository.org_id=delivery.org_id
         AND repository.github_repository_id=delivery.repository_id
         AND repository.node_id=delivery.repository_node_id
        WHERE delivery.repository_id IS NOT NULL
          AND delivery.installation_id IS DISTINCT FROM repository.github_installation_id
    ) THEN
        RAISE EXCEPTION '000011 rollback blocked: retained GitHub deliveries reference repositories transferred to another installation; remove those deliveries or restore compatible installation identity before retrying' USING ERRCODE = 'check_violation';
    END IF;
    IF EXISTS (
        SELECT 1 FROM github_repository_routing_state state
        JOIN github_repositories repository
          ON repository.org_id=state.org_id
         AND repository.github_repository_id=state.repository_id
         AND repository.node_id=state.repository_node_id
        WHERE state.installation_id<>repository.github_installation_id
    ) OR EXISTS (
        SELECT 1 FROM github_routing_intents intent
        JOIN github_webhook_deliveries delivery ON delivery.id=intent.delivery_id
        WHERE intent.installation_id IS DISTINCT FROM delivery.installation_id
    ) THEN
        RAISE EXCEPTION '000011 rollback blocked: routing installation identities are incompatible with the 000010 delivery/repository identities; reconcile or remove incompatible routing rows before retrying' USING ERRCODE = 'check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM github_webhook_deliveries WHERE status IN ('pending','leased') OR verified_payload IS NOT NULL)
       OR EXISTS (SELECT 1 FROM github_routing_intents WHERE status IN ('pending','leased')) THEN
        RAISE EXCEPTION '000011 rollback blocked: drain webhook deliveries/intents and redact retained payloads before retrying' USING ERRCODE = 'check_violation';
    END IF;
END
$$;

CREATE FUNCTION guard_github_webhook_v11_down() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE version=11 AND dirty=false) THEN
        PERFORM assert_github_webhook_v11_down_safe();
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER guard_github_webhook_v11_down BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT EXECUTE FUNCTION guard_github_webhook_v11_down();
