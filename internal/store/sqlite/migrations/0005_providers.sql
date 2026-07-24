-- +goose Up
-- providers is the control-plane routing-provider registry (/_config_/providers):
-- one row per operator-defined provider, fully describing how to construct its
-- engine (the beeline.ProviderSpec shape). The built-in providers (haversine, and
-- latent-haversine when engine latency is enabled) are synthesized at boot and never
-- stored. profiles_json maps beeline profile names to OSRM profile path segments.
-- An empty table on a leader's first boot is seeded from the legacy matrix.providers
-- file config, after which the database is authoritative.
CREATE TABLE providers (
    name           TEXT    NOT NULL PRIMARY KEY,
    type           TEXT    NOT NULL,
    base_url       TEXT    NOT NULL DEFAULT '',
    profiles_json  TEXT    NOT NULL DEFAULT '{}',
    max_table_size INTEGER NOT NULL DEFAULT 0,
    timeout_ms     INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);

-- +goose Down
DROP TABLE providers;
