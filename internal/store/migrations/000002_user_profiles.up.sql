ALTER TABLE users
    ADD COLUMN first_name text NOT NULL DEFAULT '' CHECK (octet_length(first_name) <= 100),
    ADD COLUMN last_name text NOT NULL DEFAULT '' CHECK (octet_length(last_name) <= 100),
    ADD COLUMN display_name text NOT NULL DEFAULT '' CHECK (octet_length(display_name) <= 200),
    ADD COLUMN avatar_key text NOT NULL DEFAULT '1ba2e39a64' CHECK (avatar_key ~ '^[a-f0-9]{10}$');

DROP FUNCTION claim_first_user(text, text, bigint);

CREATE FUNCTION claim_first_user(
    claimed_email text,
    claimed_password_hash text DEFAULT NULL,
    claimed_github_user_id bigint DEFAULT NULL,
    claimed_avatar_key text DEFAULT '1ba2e39a64'
) RETURNS users
LANGUAGE plpgsql
AS $$
DECLARE
    claimed_org orgs;
    claimed_user users;
BEGIN
    PERFORM pg_advisory_xact_lock(1129599568, 1);
    IF EXISTS (SELECT 1 FROM users) THEN
        RAISE EXCEPTION 'cyclopus installation is already claimed' USING ERRCODE = 'unique_violation';
    END IF;

    INSERT INTO orgs (name, slug)
    VALUES ('My Organization', 'my-organization')
    RETURNING * INTO claimed_org;

    INSERT INTO users (org_id, email, password_hash, github_user_id, operator, avatar_key)
    VALUES (claimed_org.id, lower(claimed_email), claimed_password_hash, claimed_github_user_id, true, claimed_avatar_key)
    RETURNING * INTO claimed_user;

    RETURN claimed_user;
END;
$$;
