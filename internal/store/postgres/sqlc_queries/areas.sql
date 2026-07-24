-- name: CreateArea :one
INSERT INTO areas (
    name,
    warm_strategy,
    demand_idle_ttl_seconds,
    target_ttl_seconds,
    lease_duration_seconds,
    sweep_interval_seconds,
    routing_provider,
    geojson,
    enabled,
    created_at,
    updated_at
) VALUES (
    sqlc.arg(name),
    sqlc.arg(warm_strategy),
    sqlc.arg(demand_idle_ttl_seconds),
    sqlc.arg(target_ttl_seconds),
    sqlc.arg(lease_duration_seconds),
    sqlc.arg(sweep_interval_seconds),
    sqlc.arg(routing_provider),
    sqlc.narg(geojson),
    sqlc.arg(enabled),
    sqlc.arg(created_at),
    sqlc.arg(updated_at)
)
RETURNING id;

-- name: GetArea :one
SELECT
    id,
    name,
    warm_strategy,
    demand_idle_ttl_seconds,
    geojson,
    enabled,
    created_at,
    updated_at,
    routing_provider,
    target_ttl_seconds,
    lease_duration_seconds,
    sweep_interval_seconds
FROM areas
WHERE id = sqlc.arg(id);

-- name: ListAreas :many
SELECT
    id,
    name,
    warm_strategy,
    demand_idle_ttl_seconds,
    geojson,
    enabled,
    created_at,
    updated_at,
    routing_provider,
    target_ttl_seconds,
    lease_duration_seconds,
    sweep_interval_seconds
FROM areas
ORDER BY id;

-- name: UpdateArea :exec
UPDATE areas
SET
    name = sqlc.arg(name),
    warm_strategy = sqlc.arg(warm_strategy),
    demand_idle_ttl_seconds = sqlc.arg(demand_idle_ttl_seconds),
    target_ttl_seconds = sqlc.arg(target_ttl_seconds),
    lease_duration_seconds = sqlc.arg(lease_duration_seconds),
    sweep_interval_seconds = sqlc.arg(sweep_interval_seconds),
    routing_provider = sqlc.arg(routing_provider),
    geojson = sqlc.narg(geojson),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: SetAreaEnabled :exec
UPDATE areas
SET
    enabled = sqlc.arg(enabled),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: DeleteArea :exec
DELETE FROM areas
WHERE id = sqlc.arg(id);

-- name: ListAreaLayers :many
SELECT
    area_id,
    resolution,
    min_distance_meters,
    max_radius_meters,
    core_radius_meters
FROM area_layers
WHERE area_id = sqlc.arg(area_id)
ORDER BY resolution DESC;

-- name: InsertAreaLayer :exec
INSERT INTO area_layers (
    area_id,
    resolution,
    min_distance_meters,
    max_radius_meters,
    core_radius_meters
) VALUES (
    sqlc.arg(area_id),
    sqlc.arg(resolution),
    sqlc.arg(min_distance_meters),
    sqlc.arg(max_radius_meters),
    sqlc.arg(core_radius_meters)
);

-- name: DeleteAreaLayers :exec
DELETE FROM area_layers
WHERE area_id = sqlc.arg(area_id);
