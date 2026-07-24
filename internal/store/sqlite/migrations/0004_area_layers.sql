-- +goose Up
-- area_layers is the ordered set of precision layers an area is precomputed at,
-- one row per (area, resolution). The layer list replaces the areas table's single
-- resolution/radius_meters/core_radius_meters scalars: each layer carries its own
-- travel bound (max_radius_meters, 0 = full mesh) and hybrid near field
-- (core_radius_meters), plus min_distance_meters — the trip distance from which
-- this layer is meant to serve, recorded for future distance-based selection.
CREATE TABLE area_layers (
    area_id             INTEGER NOT NULL REFERENCES areas (id) ON DELETE CASCADE,
    resolution          INTEGER NOT NULL,
    min_distance_meters REAL    NOT NULL DEFAULT 0,
    max_radius_meters   REAL    NOT NULL,
    core_radius_meters  REAL    NOT NULL DEFAULT 0,
    PRIMARY KEY (area_id, resolution)
);

-- Backfill: each existing area becomes a one-layer area (min distance 0).
INSERT INTO area_layers (area_id, resolution, min_distance_meters, max_radius_meters, core_radius_meters)
SELECT id, resolution, 0, radius_meters, core_radius_meters FROM areas;

-- GeoJSON is now the canonical, required geometry — every layer's cell set is
-- re-polyfilled from it at seed time. A hand-built area has nothing to derive
-- cells from, so disable it rather than let boot-time ResumeEnabled fail on it.
UPDATE areas SET enabled = 0 WHERE geojson IS NULL;

-- Cells are derived data now; the persisted set (and manual refinement) is gone.
DROP TABLE area_cells;

ALTER TABLE areas DROP COLUMN resolution;
ALTER TABLE areas DROP COLUMN radius_meters;
ALTER TABLE areas DROP COLUMN core_radius_meters;

-- +goose Down
ALTER TABLE areas ADD COLUMN resolution INTEGER NOT NULL DEFAULT 9;
ALTER TABLE areas ADD COLUMN radius_meters REAL NOT NULL DEFAULT 0;
ALTER TABLE areas ADD COLUMN core_radius_meters REAL NOT NULL DEFAULT 0;

-- Restore each area's scalars from its finest (highest-resolution) layer.
UPDATE areas SET
    resolution = (
        SELECT l.resolution FROM area_layers AS l
        WHERE l.area_id = areas.id ORDER BY l.resolution DESC LIMIT 1
    ),
    radius_meters = (
        SELECT l.max_radius_meters FROM area_layers AS l
        WHERE l.area_id = areas.id ORDER BY l.resolution DESC LIMIT 1
    ),
    core_radius_meters = (
        SELECT l.core_radius_meters FROM area_layers AS l
        WHERE l.area_id = areas.id ORDER BY l.resolution DESC LIMIT 1
    )
WHERE EXISTS (SELECT 1 FROM area_layers AS l WHERE l.area_id = areas.id);

CREATE TABLE area_cells (
    area_id INTEGER NOT NULL REFERENCES areas (id) ON DELETE CASCADE,
    cell    TEXT    NOT NULL,
    PRIMARY KEY (area_id, cell)
);

DROP TABLE area_layers;
