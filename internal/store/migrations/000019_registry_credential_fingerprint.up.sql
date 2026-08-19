DO $$ BEGIN
    IF EXISTS (
        SELECT 1 FROM registry_credential_operations
        WHERE status NOT IN ('succeeded','failed','cancelled')
    ) THEN
        RAISE EXCEPTION '000019 requires legacy nonterminal registry credential operations to be drained'
            USING ERRCODE='check_violation';
    END IF;
END $$;

ALTER TABLE registry_credential_operations
    ADD COLUMN credential_fingerprint bytea,
    ADD COLUMN actor_user_id uuid,
    ADD CONSTRAINT registry_credential_operations_actor_fk
        FOREIGN KEY (org_id,actor_user_id) REFERENCES users(org_id,id) ON DELETE SET NULL (actor_user_id),
    ADD CONSTRAINT registry_credential_operations_fingerprint_length
        CHECK (credential_fingerprint IS NULL OR octet_length(credential_fingerprint)=32);

CREATE FUNCTION enforce_registry_credential_operation_attribution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status NOT IN ('succeeded','failed','cancelled') AND
       (NEW.credential_fingerprint IS NULL OR octet_length(NEW.credential_fingerprint)<>32 OR NEW.actor_user_id IS NULL) THEN
        RAISE EXCEPTION 'active registry credential operations require a keyed fingerprint and actor'
            USING ERRCODE='check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER registry_credential_operation_attribution
    BEFORE INSERT OR UPDATE OF status,credential_fingerprint,actor_user_id ON registry_credential_operations
    FOR EACH ROW EXECUTE FUNCTION enforce_registry_credential_operation_attribution();
