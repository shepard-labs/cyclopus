ALTER TABLE registries DROP CONSTRAINT IF EXISTS registries_singleton_key;
ALTER TABLE registries DROP CONSTRAINT IF EXISTS registries_provider_check;
ALTER TABLE registries DROP CONSTRAINT IF EXISTS registries_registry_host_check;
ALTER TABLE registries DROP CONSTRAINT IF EXISTS registries_singleton_check;
ALTER TABLE registries DROP COLUMN IF EXISTS singleton;
ALTER TABLE registries DROP CONSTRAINT IF EXISTS registries_canonical_identity_check;
DROP INDEX IF EXISTS registries_canonical_identity_key;

UPDATE registries
SET provider = lower(btrim(provider)),
    registry_host = lower(btrim(registry_host));

-- Only these exact legacy URL spellings are unambiguous registry identities.
UPDATE registries SET registry_host = 'ghcr.io'
WHERE registry_host IN ('https://ghcr.io', 'https://ghcr.io/');
UPDATE registries SET registry_host = 'gcr.io'
WHERE registry_host IN ('https://gcr.io', 'https://gcr.io/');

DO $$
DECLARE
    invalid record;
    duplicate record;
BEGIN
    SELECT id, org_id, provider, registry_host INTO invalid
    FROM registries
    WHERE NOT (
        (provider = 'ghcr' AND registry_host = 'ghcr.io') OR
        (provider = 'gcr' AND registry_host = 'gcr.io')
    )
    ORDER BY id LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION '000009 cannot migrate registry id % in org %: unsupported or noncanonical identity provider=% host=%; supported identities are ghcr/ghcr.io and gcr/gcr.io',
            invalid.id, invalid.org_id, invalid.provider, invalid.registry_host
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT org_id, provider, registry_host, count(*) AS row_count INTO duplicate
    FROM registries
    GROUP BY org_id, provider, registry_host
    HAVING count(*) > 1
    ORDER BY org_id, provider, registry_host LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION '000009 normalization produces % duplicate registry rows in org % for provider=% host=%; merge the duplicate rows before retrying',
            duplicate.row_count, duplicate.org_id, duplicate.provider, duplicate.registry_host
            USING ERRCODE = 'unique_violation';
    END IF;
END;
$$;

ALTER TABLE registries
    ADD CONSTRAINT registries_canonical_identity_check CHECK (
        (provider = 'ghcr' AND registry_host = 'ghcr.io') OR
        (provider = 'gcr' AND registry_host = 'gcr.io')
    );
CREATE UNIQUE INDEX registries_canonical_identity_key
    ON registries(org_id, provider, registry_host);

ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_image_repository_ghcr;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_image_repository_registry;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_registry_source_consistency;

CREATE FUNCTION valid_repository_path(value text, minimum_components integer)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
STRICT
PARALLEL SAFE
SET search_path = pg_catalog
AS $$
DECLARE
    components text[];
    component text;
    component_length integer;
    position integer;
    code integer;
BEGIN
    IF pg_catalog.octet_length(value) > 255 THEN
        RETURN false;
    END IF;

    components := pg_catalog.string_to_array(value, '/');
    IF pg_catalog.cardinality(components) < minimum_components THEN
        RETURN false;
    END IF;

    FOREACH component IN ARRAY components LOOP
        component_length := pg_catalog.char_length(component);
        IF component_length = 0 THEN
            RETURN false;
        END IF;

        position := 1;
        WHILE position <= component_length LOOP
            code := pg_catalog.ascii(pg_catalog.substring(component, position, 1));
            IF (code BETWEEN 48 AND 57) OR (code BETWEEN 97 AND 122) THEN
                position := position + 1;
                CONTINUE;
            END IF;

            IF position = 1 THEN
                RETURN false;
            END IF;
            IF code = 46 THEN
                position := position + 1;
            ELSIF code = 95 THEN
                position := position + 1;
                IF position <= component_length
                   AND pg_catalog.ascii(pg_catalog.substring(component, position, 1)) = 95 THEN
                    position := position + 1;
                END IF;
            ELSIF code = 45 THEN
                position := position + 1;
                WHILE position <= component_length
                  AND pg_catalog.ascii(pg_catalog.substring(component, position, 1)) = 45 LOOP
                    position := position + 1;
                END LOOP;
            ELSE
                RETURN false;
            END IF;

            IF position > component_length THEN
                RETURN false;
            END IF;
            code := pg_catalog.ascii(pg_catalog.substring(component, position, 1));
            IF NOT ((code BETWEEN 48 AND 57) OR (code BETWEEN 97 AND 122)) THEN
                RETURN false;
            END IF;
        END LOOP;
    END LOOP;
    RETURN true;
END;
$$;

CREATE FUNCTION enforce_registry_app_repository()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
DECLARE
    canonical_host text;
BEGIN
    IF NEW.registry_id IS NULL THEN
        RETURN NEW;
    END IF;

    EXECUTE pg_catalog.format('SELECT registry_host FROM %I.registries WHERE id = $1', TG_TABLE_SCHEMA)
        INTO canonical_host
        USING NEW.registry_id;

    IF canonical_host IS NOT NULL AND pg_catalog.left(NEW.image_repository, pg_catalog.length(canonical_host) + 1) = canonical_host || '/' THEN
        RAISE EXCEPTION 'app image_repository % must be a hostless path for registry %', NEW.image_repository, canonical_host
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DO $$
DECLARE
    invalid_app record;
BEGIN
    SELECT a.id, a.org_id, a.image_repository INTO invalid_app
    FROM apps a
    WHERE a.registry_id IS NULL
      AND NOT (
          pg_catalog.octet_length(a.image_repository) <= 255
          AND pg_catalog.left(a.image_repository, 8) = 'ghcr.io/'
          AND valid_repository_path(pg_catalog.substring(a.image_repository, 9), 2)
      )
    ORDER BY a.id LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION '000009 cannot migrate public import-owned app % in org %: image_repository % is not a canonical public GHCR repository',
            invalid_app.id, invalid_app.org_id, invalid_app.image_repository
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT a.id, a.org_id, a.image_repository, r.registry_host INTO invalid_app
    FROM apps a
    JOIN registries r ON r.id = a.registry_id
    WHERE NOT valid_repository_path(a.image_repository, 1)
       OR pg_catalog.left(a.image_repository, pg_catalog.length(r.registry_host) + 1) = r.registry_host || '/'
    ORDER BY a.id LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION '000009 cannot migrate registry-backed app % in org %: image_repository % must be a canonical hostless repository path for registry %',
            invalid_app.id, invalid_app.org_id, invalid_app.image_repository, invalid_app.registry_host
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

ALTER TABLE apps
    ALTER COLUMN github_workflow_path DROP NOT NULL,
    ALTER COLUMN production_branch DROP NOT NULL,
    ADD CONSTRAINT apps_registry_source_consistency CHECK (
        (registry_id IS NULL AND github_workflow_path IS NOT NULL AND production_branch IS NOT NULL) OR
        (registry_id IS NOT NULL AND github_workflow_path IS NULL AND production_branch IS NULL)
    ),
    ADD CONSTRAINT apps_image_repository_registry CHECK (
        (registry_id IS NULL
            AND pg_catalog.octet_length(image_repository) <= 255
            AND pg_catalog.left(image_repository, 8) = 'ghcr.io/'
            AND valid_repository_path(pg_catalog.substring(image_repository, 9), 2)) OR
        (registry_id IS NOT NULL AND valid_repository_path(image_repository, 1))
    );
CREATE TRIGGER enforce_registry_app_repository
    BEFORE INSERT OR UPDATE OF registry_id, image_repository ON apps
    FOR EACH ROW EXECUTE FUNCTION enforce_registry_app_repository();

-- golang-migrate marks a migration dirty before executing its down SQL. Guard
-- that metadata transition so an incompatible rollback fails before the
-- migration row, schema, or application data can be mutated.
CREATE FUNCTION guard_registry_v9_down() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    migration_version bigint;
    migration_dirty boolean;
    incompatible_app record;
    registry_count bigint;
    incompatible_registry record;
BEGIN
    SELECT version, dirty INTO migration_version, migration_dirty FROM schema_migrations LIMIT 1;
    IF migration_version = 9 AND migration_dirty = false THEN
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
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER guard_registry_v9_down
    BEFORE TRUNCATE ON schema_migrations
    FOR EACH STATEMENT EXECUTE FUNCTION guard_registry_v9_down();
