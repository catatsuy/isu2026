#!/bin/bash
# Execute on isu02/isu03 only. Existing data must have been explicitly removed.
set -euo pipefail
name=${1:?isu02 or isu03}
[[ "$name" = isu02 || "$name" = isu03 ]]
source /home/isucon/env.sh
[[ "$ISUREAL_DB_HOST" = 192.168.4.10 ]]
data=/var/lib/postgresql/16/main
! sudo -n pg_ctlcluster 16 main status
# Refuse to overwrite ANY existing cluster, including a partial previous seed.
[[ ! -e "$data/PG_VERSION" ]]
sudo -n install -d -o postgres -g postgres -m 700 "$data"
[[ -z "$(sudo -n ls -A "$data")" ]]
printf '%s:5432:replication:isureal_replication:%s\n' "$ISUREAL_DB_HOST" "$ISUREAL_DB_PASSWORD" | sudo -n -u postgres tee /var/lib/postgresql/.pgpass >/dev/null
sudo -n chmod 600 /var/lib/postgresql/.pgpass
sudo -n -u postgres /usr/lib/postgresql/16/bin/pg_basebackup \
  --dbname="host=$ISUREAL_DB_HOST port=5432 user=isureal_replication application_name=$name passfile=/var/lib/postgresql/.pgpass connect_timeout=5" \
  --pgdata="$data" --format=plain --wal-method=stream --checkpoint=fast \
  --compress=server-gzip:1 --slot="$name" --write-recovery-conf --progress --verbose
# Local-only app connections; the WAL receiver uses the existing routed path.
sudo -n tee /etc/postgresql/16/main/conf.d/99-replica.conf >/dev/null <<CONF
hot_standby = on
listen_addresses = '127.0.0.1,192.168.$([[ "$name" = isu02 ]] && echo 2 || echo 3).10'
shared_buffers = '512MB'
max_standby_streaming_delay = '5s'
hot_standby_feedback = off
CONF
printf 'auto\n' | sudo -n tee /etc/postgresql/16/main/start.conf >/dev/null
sudo -n systemctl enable postgresql
sudo -n systemctl start postgresql@16-main
sudo -n -u postgres psql -X -c 'select pg_is_in_recovery(), pg_last_wal_replay_lsn();'
