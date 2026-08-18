LOCK TABLE github_installations, github_repositories, git_connections, github_webhook_deliveries,
    github_repository_routing_state, github_ref_routing_state, github_routing_intents,
    github_ref_transition_observations IN SHARE ROW EXCLUSIVE MODE;
SELECT assert_github_webhook_v11_down_safe();
DROP TRIGGER guard_github_webhook_v11_down ON schema_migrations;
DROP FUNCTION guard_github_webhook_v11_down();
DROP FUNCTION assert_github_webhook_v11_down_safe();

ALTER TABLE github_installations DROP CONSTRAINT github_installations_access_state_check;
ALTER TABLE github_installations ADD CONSTRAINT github_installations_access_state_check
    CHECK (access_state IN ('active', 'suspended', 'deleted'));

ALTER TABLE github_repositories DROP CONSTRAINT github_repositories_access_state_check;
ALTER TABLE github_repositories ADD CONSTRAINT github_repositories_access_state_check
    CHECK (access_state IN ('active', 'inaccessible', 'deleted'));

ALTER TABLE github_webhook_deliveries DROP CONSTRAINT github_webhook_deliveries_repository_fk;
ALTER TABLE github_webhook_deliveries ADD CONSTRAINT github_webhook_deliveries_repository_fk
    FOREIGN KEY(org_id,installation_id,repository_id,repository_node_id) REFERENCES github_repositories(org_id,github_installation_id,github_repository_id,node_id) ON DELETE RESTRICT;
ALTER TABLE github_repositories DROP CONSTRAINT github_repositories_org_id_node_unique;
DROP TABLE github_ref_transition_observations;
ALTER TABLE github_ref_routing_state DROP COLUMN last_observation_sequence, DROP COLUMN last_observed_received_at, DROP COLUMN last_delivery_id;
ALTER TABLE github_webhook_deliveries DROP CONSTRAINT github_webhook_deliveries_observation_identity_unique;
ALTER TABLE github_webhook_deliveries DROP COLUMN ingestion_sequence;
ALTER TABLE github_routing_intents DROP COLUMN installation_id;
ALTER TABLE github_repository_routing_state DROP COLUMN installation_id;

ALTER TABLE git_connections DROP CONSTRAINT git_connections_access_state_check;
ALTER TABLE git_connections ADD CONSTRAINT git_connections_access_state_check
    CHECK (access_state IN ('active', 'suspended', 'inaccessible', 'deleted'));

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
    IF OLD.access_state = 'deleted' AND NEW.access_state <> 'deleted' THEN
        RAISE EXCEPTION 'deleted GitHub installation tombstones cannot be reactivated' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION guard_github_webhook_v10_down() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF EXISTS(SELECT 1 FROM github_webhook_deliveries WHERE status IN('pending','leased') OR verified_payload IS NOT NULL) OR EXISTS(SELECT 1 FROM github_routing_intents WHERE status IN('pending','leased')) THEN RAISE EXCEPTION '000010 rollback blocked: drain webhook deliveries/intents and redact retained payloads before retrying' USING ERRCODE='check_violation'; END IF; RETURN NULL; END $$;
CREATE TRIGGER guard_github_webhook_v10_down BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT EXECUTE FUNCTION guard_github_webhook_v10_down();
