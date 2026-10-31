#!/bin/bash
# Deploy asia receiver first, validate signed API, then enable overseas callers.
set -euo pipefail
binary=${1:?linux amd64 binary}
dir=$(cd -- "$(dirname -- "$0")" && pwd)
stamp=$(date -u +%Y%m%dT%H%M%SZ)
for host in isu01 isu02 isu03; do
 scp "$binary" "$host:/home/isucon/webapp/go/isureal.asia-next"
 ssh -o BatchMode=yes "$host" "bash -s -- $host $stamp" <<'REMOTE'
set -euo pipefail
name=$1
stamp=$2
cd /home/isucon/webapp/go
cp -p isureal "isureal.before-asia-api-$stamp"
sudo -n mkdir -p /etc/systemd/system/isureal-go.service.d
config=/etc/systemd/system/isureal-go.service.d/primary-api.conf
if [[ -f "$config" ]];then sudo -n cp -p "$config" "$config.before-$stamp";fi
if [[ "$name" != isu01 ]];then
 printf '[Service]\nEnvironment="ISUREAL_PRIMARY_API_URL=http://192.168.1.10"\n' | sudo -n tee "$config" >/dev/null
fi
chmod 755 isureal.asia-next
mv isureal.asia-next isureal
sudo -n systemctl daemon-reload
sudo -n systemctl restart isureal-go
for ((attempt=0;attempt<30;attempt++)); do
 code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:8080/api/images/deploy-check || true)
 if [[ "$code" = 403 ]];then echo "$name deployed backup=$stamp";exit 0;fi
 sleep 1
done
# Roll back the binary and only this rollout's own environment change.
mv "isureal.before-asia-api-$stamp" isureal
if [[ -f "$config.before-$stamp" ]];then sudo -n cp -p "$config.before-$stamp" "$config";else sudo -n rm -f "$config";fi
sudo -n systemctl daemon-reload
sudo -n systemctl restart isureal-go
exit 1
REMOTE
 if [[ "$host" = isu01 ]];then
  scp "$dir/primary-api-probe.py" isu02:/tmp/primary-api-probe.py
  ssh -o BatchMode=yes isu02 'python3 /tmp/primary-api-probe.py europe ready && python3 /tmp/primary-api-probe.py europe upload_ready'
 fi
done
