-- name: BumpConfigGeneration :exec
UPDATE config_version
SET generation = generation + 1, updated_at = now()
WHERE kind = sqlc.arg(kind);

-- name: GetConfigGenerations :many
SELECT kind, generation
FROM config_version
ORDER BY kind;
