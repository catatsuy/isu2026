#!/bin/bash
# Run on isu04. Does not restart or reinitialize the Primary.
set -euo pipefail
cd -- "$(dirname -- "$0")"
[[ "$(hostname)" = ip-192-168-4-10 ]]
source /home/isucon/env.sh
[[ "$(sudo -n -u postgres psql -XAtc 'show wal_level')" = replica ]]
[[ "$(sudo -n -u postgres psql -XAtc 'show max_wal_senders')" -ge 6 ]]
[[ "$(sudo -n -u postgres psql -XAtc 'show max_replication_slots')" -ge 2 ]]
sudo -n cp -an /etc/postgresql/16/main/pg_hba.conf /etc/postgresql/16/main/pg_hba.conf.before-replicas
export PGPASSWORD="$ISUREAL_DB_PASSWORD"
sudo -n --preserve-env=PGPASSWORD -u postgres psql -X -v ON_ERROR_STOP=1 -f replica-primary.sql
for ip in 192.168.2.10 192.168.3.10; do
 line="host replication isureal_replication $ip/32 scram-sha-256"
 sudo -n grep -qFx "$line" /etc/postgresql/16/main/pg_hba.conf || printf '%s\n' "$line" | sudo -n tee -a /etc/postgresql/16/main/pg_hba.conf >/dev/null
done
sudo -n -u postgres psql -X -v ON_ERROR_STOP=1 -f replica-enable.sql
