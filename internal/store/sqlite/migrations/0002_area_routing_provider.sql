-- +goose Up
-- routing_provider names the engine an area is served by, referencing an entry in the
-- globally configured provider registry. 'haversine' is the built-in default, so every
-- existing row keeps routing through the in-process engine.
ALTER TABLE areas ADD COLUMN routing_provider TEXT NOT NULL DEFAULT 'haversine';

-- +goose Down
ALTER TABLE areas DROP COLUMN routing_provider;
