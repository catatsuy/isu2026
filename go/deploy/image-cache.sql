-- Apply once to the shared database before deploying the new Go application.
BEGIN;
CREATE TABLE IF NOT EXISTS image_cache_generation (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    generation bigint NOT NULL DEFAULT 1
);
INSERT INTO image_cache_generation (singleton) VALUES (true)
ON CONFLICT (singleton) DO NOTHING;
COMMIT;
