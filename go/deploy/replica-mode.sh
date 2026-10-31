#!/bin/bash
# Run from the operator's PC. Deliberate manual degradation, never automatic.
set -euo pipefail
mode=${1:?primary-only or async-profile}
[[ "$mode" = primary-only || "$mode" = async-profile ]]
ssh_options=(-o BatchMode=yes -o ConnectTimeout=10)
if [[ "$mode" = async-profile ]]; then
  # Asynchronous replication; only tolerant own-profile metadata is eligible.
  ssh "${ssh_options[@]}" isu04 "sudo -n -u postgres psql -X -v ON_ERROR_STOP=1" <<'SQL'
ALTER SYSTEM SET synchronous_commit = 'on';
ALTER SYSTEM SET synchronous_standby_names = '';
SELECT pg_reload_conf();
SQL
fi
for host in isu01 isu02 isu03; do
 ssh "${ssh_options[@]}" "$host" "bash -s -- $mode $host" <<'REMOTE'
set -euo pipefail
mode=$1
host=$2
sudo -n mkdir -p /etc/systemd/system/isureal-go.service.d
if [[ "$mode" = primary-only ]]; then
 printf '[Service]\nEnvironment="ISUREAL_REPLICA_HOST="\nEnvironment="ISUREAL_REPLICA_NAME="\nEnvironment="ISUREAL_WAIT_REPLICAS=true"\n' | sudo -n tee /etc/systemd/system/isureal-go.service.d/zz-replica-mode.conf >/dev/null
else
 sudo -n rm -f /etc/systemd/system/isureal-go.service.d/zz-replica-mode.conf
fi
sudo -n systemctl daemon-reload
sudo -n systemctl restart isureal-go
sudo -n systemctl is-active --quiet isureal-go
# Restart kills all old replica readers before Primary quorum can be reduced.
REMOTE
done
if [[ "$mode" = primary-only ]]; then
 echo 'All applications restarted in Primary-only mode. Enabling asynchronous Primary-only operation.' >&2
 ssh "${ssh_options[@]}" isu04 "sudo -n -u postgres psql -X -v ON_ERROR_STOP=1" <<'SQL'
ALTER SYSTEM SET synchronous_standby_names = '';
ALTER SYSTEM SET synchronous_commit = 'on';
SELECT pg_reload_conf();
SQL
fi
