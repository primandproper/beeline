-- name: CreateArea :one
INSERT INTO areas (
    name,
    resolution,
    radius_meters,
    warm_strategy,
    core_radius_meters,
    geojson,
    enabled,
    created_at,
    updated_at
) VALUES (
    sqlc.arg(name),
    sqlc.arg(resolution),
    sqlc.arg(radius_meters),
    sqlc.arg(warm_strategy),
    sqlc.arg(core_radius_meters),
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
    resolution,
    radius_meters,
    warm_strategy,
    core_radius_meters,
    geojson,
    enabled,
    created_at,
    updated_at
FROM areas
WHERE id = sqlc.arg(id);

-- name: ListAreas :many
SELECT
    id,
    name,
    resolution,
    radius_meters,
    warm_strategy,
    core_radius_meters,
    geojson,
    enabled,
    created_at,
    updated_at
FROM areas
ORDER BY id;

-- name: UpdateArea :exec
UPDATE areas
SET
    name = sqlc.arg(name),
    resolution = sqlc.arg(resolution),
    radius_meters = sqlc.arg(radius_meters),
    warm_strategy = sqlc.arg(warm_strategy),
    core_radius_meters = sqlc.arg(core_radius_meters),
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

-- name: ListAreaCells :many
SELECT cell
FROM area_cells
WHERE area_id = sqlc.arg(area_id)
ORDER BY cell;

-- name: AddAreaCell :exec
INSERT INTO area_cells (area_id, cell)
VALUES (sqlc.arg(area_id), sqlc.arg(cell))
ON CONFLICT (area_id, cell) DO NOTHING;

-- name: RemoveAreaCell :exec
DELETE FROM area_cells
WHERE area_id = sqlc.arg(area_id) AND cell = sqlc.arg(cell);

-- name: DeleteAreaCells :exec
DELETE FROM area_cells
WHERE area_id = sqlc.arg(area_id);
