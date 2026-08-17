ALTER TABLE github_setup_sessions
    DROP CONSTRAINT github_setup_sessions_generation_required_for_active,
    DROP COLUMN state_generation;
