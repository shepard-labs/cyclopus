CREATE OR REPLACE FUNCTION protect_github_installation_claim() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'GitHub installation claims are retained as tombstones' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.github_installation_id IS DISTINCT FROM OLD.github_installation_id
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

ALTER TABLE github_installations
    DROP CONSTRAINT github_installations_pkey,
    DROP CONSTRAINT github_installations_github_installation_id_key,
    ADD CONSTRAINT github_installations_pkey PRIMARY KEY (github_installation_id),
    DROP COLUMN id;
