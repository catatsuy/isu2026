-- Final policy: asynchronous physical replication, durable local COMMIT.
ALTER SYSTEM SET synchronous_commit = 'on';
ALTER SYSTEM SET synchronous_standby_names = '';
SELECT pg_reload_conf();
