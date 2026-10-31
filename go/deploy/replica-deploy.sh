#!/bin/bash
# Deploy the already-built replica-capable binary without sending notifications.
set -euo pipefail
binary=${1:?path to linux amd64 binary}
script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
stamp=$(date -u +%Y%m%dT%H%M%SZ)
for host in isu01 isu02 isu03; do
 scp "$binary" "$host:/home/isucon/webapp/go/isureal.replica-next"
 scp "$script_dir/../../sql/init.sh" "$host:/home/isucon/webapp/sql/init.replica-next.sh"
 ssh -o BatchMode=yes "$host" "bash -s -- $host $stamp" <<'REMOTE'
set -euo pipefail
name=$1
stamp=$2
cd /home/isucon/webapp/go
cp -p isureal "isureal.before-replicas-$stamp"
cp -p ../sql/init.sh "../sql/init.before-replicas-$stamp.sh"
sudo -n mkdir -p /etc/systemd/system/isureal-go.service.d
{
 printf '[Service]\nEnvironment="ISUREAL_WAIT_REPLICAS=true"\nEnvironment="ISUREAL_REPLICA_INIT_TIMEOUT=50s"\n'
 if [[ "$name" != isu01 ]]; then
  printf 'Environment="ISUREAL_REPLICA_HOST=127.0.0.1"\nEnvironment="ISUREAL_REPLICA_NAME=%s"\n' "$name"
 fi
} | sudo -n tee /etc/systemd/system/isureal-go.service.d/replica.conf >/dev/null
chmod 755 isureal.replica-next ../sql/init.replica-next.sh
mv ../sql/init.replica-next.sh ../sql/init.sh
mv isureal.replica-next isureal
sudo -n systemctl daemon-reload
sudo -n systemctl restart isureal-go
for ((attempt=0;attempt<30;attempt++)); do
 code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 http://127.0.0.1:8080/api/images/deploy-check || true)
 if [[ "$code" = 403 ]]; then echo "$name deployed backup=$stamp"; exit 0;fi
 sleep 1
done
mv "isureal.before-replicas-$stamp" isureal
sudo -n systemctl restart isureal-go
exit 1
REMOTE
done
