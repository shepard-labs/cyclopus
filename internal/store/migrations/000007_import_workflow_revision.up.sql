ALTER TABLE apps
    ADD COLUMN github_workflow_revision text NOT NULL DEFAULT '0000000000000000000000000000000000000000'
        CHECK (github_workflow_revision ~ '^[0-9a-f]{40}$');
