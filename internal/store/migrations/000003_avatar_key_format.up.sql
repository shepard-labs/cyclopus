-- Avatar keys are persisted asset identifiers. Exact membership is validated by
-- the application so new bundled avatars do not require a schema migration.
ALTER TABLE users
    DROP CONSTRAINT users_avatar_key_check,
    ADD CONSTRAINT users_avatar_key_check CHECK (avatar_key ~ '^[a-f0-9]{10}$');
