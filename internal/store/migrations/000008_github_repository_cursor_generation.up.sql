ALTER TABLE github_installations
    ADD COLUMN repository_reconciliation_generation bigint NOT NULL DEFAULT 1
        CHECK (repository_reconciliation_generation > 0);
