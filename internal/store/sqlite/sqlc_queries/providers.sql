-- name: ListProviders :many
SELECT
    name,
    type,
    base_url,
    profiles_json,
    max_table_size,
    timeout_ms,
    created_at,
    updated_at
FROM providers
ORDER BY name;

-- name: UpsertProvider :exec
INSERT INTO providers (
    name,
    type,
    base_url,
    profiles_json,
    max_table_size,
    timeout_ms,
    created_at,
    updated_at
) VALUES (
    sqlc.arg(name),
    sqlc.arg(type),
    sqlc.arg(base_url),
    sqlc.arg(profiles_json),
    sqlc.arg(max_table_size),
    sqlc.arg(timeout_ms),
    sqlc.arg(created_at),
    sqlc.arg(updated_at)
)
ON CONFLICT (name) DO UPDATE SET
    type = excluded.type,
    base_url = excluded.base_url,
    profiles_json = excluded.profiles_json,
    max_table_size = excluded.max_table_size,
    timeout_ms = excluded.timeout_ms,
    updated_at = excluded.updated_at;

-- name: DeleteProvider :exec
DELETE FROM providers
WHERE name = sqlc.arg(name);
