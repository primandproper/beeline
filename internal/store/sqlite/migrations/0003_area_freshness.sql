-- +goose Up
-- Per-area freshness knobs. These were previously global matrix config; each area now
-- carries its own freshness contract so mapping concerns are configured per area (the
-- global config values survive only as the defaults applied when a new area is created).
--   target_ttl_seconds     — a pair is stale once older than this; drives refresh cadence.
--   lease_duration_seconds — visibility timeout on a refresh claim.
--   sweep_interval_seconds — demand-decay janitor cadence for this area.
-- Non-zero defaults keep any pre-existing area on a sane contract after migration.
ALTER TABLE areas ADD COLUMN target_ttl_seconds     INTEGER NOT NULL DEFAULT 60;
ALTER TABLE areas ADD COLUMN lease_duration_seconds INTEGER NOT NULL DEFAULT 15;
ALTER TABLE areas ADD COLUMN sweep_interval_seconds INTEGER NOT NULL DEFAULT 15;

-- +goose Down
ALTER TABLE areas DROP COLUMN sweep_interval_seconds;
ALTER TABLE areas DROP COLUMN lease_duration_seconds;
ALTER TABLE areas DROP COLUMN target_ttl_seconds;
