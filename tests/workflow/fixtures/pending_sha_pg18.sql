BEGIN;

CREATE TEMP TABLE environments (
    id bigint PRIMARY KEY,
    pending_commit_sha text
) ON COMMIT DROP;

INSERT INTO environments (id, pending_commit_sha)
VALUES (1, repeat('a', 40));

WITH taken AS (
    UPDATE environments
       SET pending_commit_sha = NULL
     WHERE id = 1
    RETURNING WITH (OLD AS old_row, NEW AS new_row)
              old_row.pending_commit_sha AS taken_sha,
              new_row.pending_commit_sha AS remaining_sha
)
SELECT 1 / CASE
    WHEN taken_sha = repeat('a', 40) AND remaining_sha IS NULL THEN 1
    ELSE 0
END AS old_row_contract_holds
FROM taken;

SELECT 1 / CASE WHEN pending_commit_sha IS NULL THEN 1 ELSE 0 END AS value_was_cleared
FROM environments
WHERE id = 1;

ROLLBACK;
