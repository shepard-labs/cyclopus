-- This alpha epoch is intentionally destructive and cannot reconstruct the
-- discarded schema or data. Refuse an unsafe logical rollback; operators must
-- restore the pre-upgrade snapshot before running the previous binary.
DO $$
BEGIN
    RAISE EXCEPTION '000004 is irreversible: restore the pre-upgrade database snapshot for rollback';
END;
$$;
