-- +goose Up

-- The freshness index (design §5.3/§8): one row per working-set pair, the
-- Postgres implementation of beeline.FreshnessIndex + control.AreaIndex. The
-- database clock (now()) is the single authority for every timestamp here —
-- lease expiry, staleness, access recency — so any number of heads and
-- followers coordinate without comparing their own clocks.
--
-- stale_at denormalizes computed_at + the area's target TTL at mark time, so
-- the hot Claim scan never joins the per-area override table.
CREATE TABLE pair_freshness (
    area_id     BIGINT      NOT NULL,
    profile     TEXT        NOT NULL,
    res         SMALLINT    NOT NULL,
    origin      BIGINT      NOT NULL,
    dest        BIGINT      NOT NULL,
    computed_at TIMESTAMPTZ,                        -- NULL = never computed (maximally stale)
    stale_at    TIMESTAMPTZ,                        -- computed_at + area TTL; NULL while never computed
    lease_until TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    last_access TIMESTAMPTZ NOT NULL DEFAULT now(),
    bumped      BOOLEAN     NOT NULL DEFAULT FALSE,
    pinned      BOOLEAN     NOT NULL DEFAULT FALSE,
    PRIMARY KEY (area_id, profile, res, origin, dest)
);

-- Claim order = the memory index's claimBefore(): demand-bumped first, then
-- never-computed (NULLS FIRST), then oldest, ties by origin/dest so a batch
-- clusters into dense origin-centric 1×K table calls (§6).
CREATE INDEX pair_freshness_claim_idx
    ON pair_freshness (bumped DESC, computed_at ASC NULLS FIRST, origin, dest);

-- Every MarkComputed rewrites rows, so the claim index's front accumulates
-- dead tuples fast; sweep them aggressively.
ALTER TABLE pair_freshness SET (
    autovacuum_vacuum_scale_factor = 0.01,
    autovacuum_analyze_scale_factor = 0.02
);

-- Per-area freshness contract overrides (control.AreaIndex.SetAreaFreshness).
-- Zero seconds = fall back to the index-wide default.
CREATE TABLE area_freshness (
    area_id            BIGINT PRIMARY KEY,
    target_ttl_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,
    lease_seconds      DOUBLE PRECISION NOT NULL DEFAULT 0
);

-- Per-area achieved-throughput baseline: reset when the area is (re)seeded so
-- a freshly enabled area's load progress reads from zero (the memory index's
-- baseline map, made shared).
CREATE TABLE area_baselines (
    area_id        BIGINT      PRIMARY KEY,
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    computed_total BIGINT      NOT NULL DEFAULT 0
);

-- The global achieved-throughput counter behind the aggregate Debt signal
-- (single row, created here so MarkComputed can always UPDATE it).
CREATE TABLE freshness_counters (
    singleton      BOOLEAN     PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    computed_total BIGINT      NOT NULL DEFAULT 0
);

INSERT INTO freshness_counters DEFAULT VALUES;

-- +goose Down

DROP TABLE freshness_counters;
DROP TABLE area_baselines;
DROP TABLE area_freshness;
DROP TABLE pair_freshness;
