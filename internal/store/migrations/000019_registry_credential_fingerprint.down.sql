DO $$ BEGIN
    IF EXISTS (
        SELECT 1 FROM registry_credential_operations
        WHERE credential_fingerprint IS NOT NULL OR actor_user_id IS NOT NULL
    ) THEN
        RAISE EXCEPTION '000019 downgrade would discard registry credential fingerprint or actor evidence; delete affected operations first'
            USING ERRCODE='check_violation';
    END IF;
END $$;

DROP TRIGGER registry_credential_operation_attribution ON registry_credential_operations;
DROP FUNCTION enforce_registry_credential_operation_attribution();
ALTER TABLE registry_credential_operations
    DROP CONSTRAINT registry_credential_operations_actor_fk,
    DROP CONSTRAINT registry_credential_operations_fingerprint_length,
    DROP COLUMN actor_user_id,
    DROP COLUMN credential_fingerprint;
