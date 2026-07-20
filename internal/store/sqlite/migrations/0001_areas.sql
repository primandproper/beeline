-- +goose Up
-- areas holds operator-configured service areas. An area is disabled by default and
-- only enters the refresh working set when enabled. The canonical geometry is the
-- cell set in area_cells; geojson retains the uploaded polygon as provenance.
CREATE TABLE areas (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT    NOT NULL,
    resolution         INTEGER NOT NULL,
    -- radius_meters is the per-origin travel-radius bound in meters; 0 = full mesh.
    radius_meters      REAL    NOT NULL,
    -- warm_strategy is how much of the bound is pinned fresh: eager | lazy | hybrid.
    warm_strategy      TEXT    NOT NULL DEFAULT 'eager',
    -- core_radius_meters is the hybrid strategy's eagerly-pinned near field (meters).
    core_radius_meters REAL    NOT NULL DEFAULT 0,
    geojson            TEXT,
    enabled            INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT    NOT NULL,
    updated_at         TEXT    NOT NULL
);

-- area_cells is the explicit H3 cell set of an area, one row per cell (hex string).
-- Normalized so a single manual hex add/remove is one row of DML.
CREATE TABLE area_cells (
    area_id INTEGER NOT NULL REFERENCES areas (id) ON DELETE CASCADE,
    cell    TEXT    NOT NULL,
    PRIMARY KEY (area_id, cell)
);

-- +goose Down
DROP TABLE area_cells;
DROP TABLE areas;
