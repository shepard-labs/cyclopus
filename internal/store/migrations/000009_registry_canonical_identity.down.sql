DROP TRIGGER guard_registry_v9_down ON schema_migrations;
DROP FUNCTION guard_registry_v9_down();

-- Validate the complete pre-000009 representation before changing anything.
-- A failing DO block aborts this migration transaction, preserving schema and
-- data at version 9.
DO $$
DECLARE
    incompatible_app record;
    registry_count bigint;
    incompatible_registry record;
    incompatible_repository record;
BEGIN
    SELECT id, org_id INTO incompatible_app
    FROM apps
    WHERE registry_id IS NOT NULL
      AND (github_workflow_path IS NULL OR production_branch IS NULL)
    ORDER BY id LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION '000009 rollback blocked by registry-backed app % in org %: pre-000009 requires non-null github_workflow_path and production_branch; migrate or remove the app before retrying',
            incompatible_app.id, incompatible_app.org_id
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT count(*) INTO registry_count FROM registries;
    IF registry_count > 1 THEN
        RAISE EXCEPTION '000009 rollback blocked by % registry rows: pre-000009 supports one global GHCR registry; consolidate registry rows before retrying', registry_count
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT id, org_id, provider, registry_host INTO incompatible_registry
    FROM registries
    WHERE provider <> 'ghcr' OR registry_host <> 'ghcr.io'
    ORDER BY id LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION '000009 rollback blocked by registry % in org % with provider=% host=%: pre-000009 supports only ghcr/ghcr.io',
            incompatible_registry.id, incompatible_registry.org_id,
            incompatible_registry.provider, incompatible_registry.registry_host
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT id, org_id, image_repository INTO incompatible_repository
    FROM apps
    WHERE NOT (
        image_repository ~ '^ghcr\.io/[a-z0-9._-]+(/[a-z0-9._-]+)+$'
        AND pg_catalog.octet_length(image_repository) <= 255
    )
    ORDER BY id LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION '000009 rollback blocked by app % in org % with image_repository %: pre-000009 requires a full ghcr.io repository',
            incompatible_repository.id, incompatible_repository.org_id, incompatible_repository.image_repository
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

DROP TRIGGER enforce_registry_app_repository ON apps;
DROP FUNCTION enforce_registry_app_repository();
ALTER TABLE apps DROP CONSTRAINT apps_registry_source_consistency;
ALTER TABLE apps DROP CONSTRAINT apps_image_repository_registry;
ALTER TABLE apps
    ALTER COLUMN github_workflow_path SET NOT NULL,
    ALTER COLUMN production_branch SET NOT NULL,
    ADD CONSTRAINT apps_image_repository_ghcr CHECK (
        image_repository ~ '^ghcr\.io/[a-z0-9._-]+(/[a-z0-9._-]+)+$'
        AND octet_length(image_repository) <= 255
    );
DROP FUNCTION valid_repository_path(text, integer);

ALTER TABLE registries DROP CONSTRAINT registries_canonical_identity_check;
DROP INDEX registries_canonical_identity_key;
ALTER TABLE registries
    ADD COLUMN singleton boolean NOT NULL DEFAULT true,
    ADD CONSTRAINT registries_singleton_check CHECK (singleton),
    ADD CONSTRAINT registries_singleton_key UNIQUE (singleton),
    ADD CONSTRAINT registries_provider_check CHECK (provider = 'ghcr'),
    ADD CONSTRAINT registries_registry_host_check CHECK (registry_host = 'ghcr.io');
