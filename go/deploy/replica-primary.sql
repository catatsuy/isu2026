-- Run as postgres, with PGPASSWORD containing the replication password.
\getenv repl_password PGPASSWORD
SELECT format('CREATE ROLE isureal_replication LOGIN REPLICATION PASSWORD %L', :'repl_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='isureal_replication') \gexec
SELECT pg_create_physical_replication_slot(name,true) FROM (VALUES ('isu02'),('isu03')) s(name)
WHERE NOT EXISTS (SELECT FROM pg_replication_slots WHERE slot_name=name);
ALTER SYSTEM SET max_slot_wal_keep_size = '4GB';
SELECT pg_reload_conf();
