-- Existing blobs are retained for verification/recovery; new images store NULL.
-- Apply before deploying file-only readers/writers.
SET lock_timeout = '2s';
SET statement_timeout = '10s';
ALTER TABLE image_objects ALTER COLUMN data DROP NOT NULL;
