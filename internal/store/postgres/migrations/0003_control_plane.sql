-- +goose Up

-- The distributed control plane: operator config shared by every head. These
-- are the Postgres ports of the SQLite areas/area_layers/providers tables, so
-- that Enable on one head is visible to all (via config_version below). Only
-- definitions live here — cells are derived by polyfill at seed time, and the
-- computed matrix lives in estimates/pair_freshness.

CREATE TABLE areas (
    id                      BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                    TEXT        NOT NULL,
    warm_strategy           TEXT        NOT NULL DEFAULT 'eager',
    demand_idle_ttl_seconds BIGINT      NOT NULL DEFAULT 0,
    target_ttl_seconds      BIGINT      NOT NULL DEFAULT 60,
    lease_duration_seconds  BIGINT      NOT NULL DEFAULT 15,
    sweep_interval_seconds  BIGINT      NOT NULL DEFAULT 15,
    routing_provider        TEXT        NOT NULL DEFAULT 'haversine',
    geojson                 TEXT,
    enabled                 BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at              TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL
);

CREATE TABLE area_layers (
    area_id             BIGINT           NOT NULL REFERENCES areas (id) ON DELETE CASCADE,
    resolution          BIGINT           NOT NULL,
    min_distance_meters DOUBLE PRECISION NOT NULL DEFAULT 0,
    max_radius_meters   DOUBLE PRECISION NOT NULL,
    core_radius_meters  DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (area_id, resolution)
);

CREATE TABLE providers (
    name           TEXT        NOT NULL PRIMARY KEY,
    type           TEXT        NOT NULL,
    base_url       TEXT        NOT NULL DEFAULT '',
    profiles_json  TEXT        NOT NULL DEFAULT '{}',
    max_table_size BIGINT      NOT NULL DEFAULT 0,
    timeout_ms     BIGINT      NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);

-- config_version is the cross-head convergence signal: every control-plane
-- mutation bumps its kind's generation in the same transaction, and each head
-- polls the generations, re-deriving its in-process projections (polyfilled
-- cells, engines, catalog hash) on change.
CREATE TABLE config_version (
    kind       TEXT        PRIMARY KEY,
    generation BIGINT      NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO config_version (kind) VALUES ('areas'), ('providers');

-- +goose Down

DROP TABLE config_version;
DROP TABLE providers;
DROP TABLE area_layers;
DROP TABLE areas;
