ALTER TABLE github_setup_sessions
    ADD COLUMN state_generation text
        CHECK (state_generation ~ '^[0-9a-f]{64}$');

UPDATE github_setup_sessions
SET status = 'expired',
    checkpoint = 'completed',
    failure_code = 'github_setup_protocol_incompatible',
    failure_message = 'Setup session expired during state protocol migration; restart setup.',
    updated_at = now()
WHERE status = 'active';

ALTER TABLE github_setup_sessions
    ADD CONSTRAINT github_setup_sessions_generation_required_for_active
    CHECK (status <> 'active' OR state_generation IS NOT NULL);
