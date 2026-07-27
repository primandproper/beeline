-- +goose Up

-- The hot estimate store: one row per directed origin→dest pair, per area,
-- profile and precision layer — the Postgres implementation of beeline.Store.
-- H3 cells are uint64 with the high bit always zero, so they bit-cast
-- losslessly to BIGINT. computed_at is stamped server-side at Put so the
-- database is the single clock authority (design §8 addendum).
--
-- Churn profile: every refresh rewrites rows in place, so autovacuum is tuned
-- aggressively to keep dead tuples from bloating the primary key's hot range.
CREATE TABLE estimates (
    area_id         BIGINT           NOT NULL,
    profile         TEXT             NOT NULL,
    res             SMALLINT         NOT NULL,
    origin          BIGINT           NOT NULL,
    dest            BIGINT           NOT NULL,
    duration_sec    DOUBLE PRECISION NOT NULL,
    distance_meters DOUBLE PRECISION NOT NULL,
    computed_at     TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (area_id, profile, res, origin, dest)
);

ALTER TABLE estimates SET (
    autovacuum_vacuum_scale_factor = 0.01,
    autovacuum_analyze_scale_factor = 0.02
);

-- +goose Down

DROP TABLE estimates;
