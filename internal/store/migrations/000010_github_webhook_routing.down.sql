DROP TRIGGER guard_github_webhook_v10_down ON schema_migrations;
DROP FUNCTION guard_github_webhook_v10_down();
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM github_webhook_deliveries WHERE status IN ('pending','leased') OR verified_payload IS NOT NULL)
       OR EXISTS (SELECT 1 FROM github_routing_intents WHERE status IN ('pending','leased')) THEN
        RAISE EXCEPTION '000010 rollback blocked: drain webhook deliveries/intents and redact retained payloads before retrying'
            USING ERRCODE = 'check_violation';
    END IF;
END $$;
DROP TABLE github_routing_intents;
DROP TABLE github_ref_routing_state;
DROP TABLE github_repository_routing_state;
DROP TABLE github_webhook_deliveries;
ALTER TABLE github_repositories DROP CONSTRAINT github_repositories_id_node_unique;
ALTER TABLE github_repositories DROP CONSTRAINT github_repositories_org_installation_id_node_unique;
ALTER TABLE github_installations DROP CONSTRAINT github_installations_org_external_unique;
