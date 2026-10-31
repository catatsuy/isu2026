#!/bin/bash
set -euo pipefail

# Usage: bash deploy.sh isu01 isu02 isu03
# Deploy application hosts only; do not include the PostgreSQL host.
if [ "$#" -eq 0 ]; then
  echo "Usage: bash deploy.sh APP_HOST [APP_HOST ...]" >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
remote_dir=${DEPLOY_DIR:-/home/isucon/webapp/go}
service=${DEPLOY_SERVICE:-isureal-go.service}
arch=${DEPLOY_ARCH:-amd64}

# These values become arguments to the remote login shell. Keep them literal.
[[ "$remote_dir" =~ ^/[a-zA-Z0-9_./-]+$ && "$remote_dir" != / ]] || { echo "Invalid DEPLOY_DIR" >&2; exit 2; }
[[ "$service" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.@-]*\.service$ ]] || { echo "Invalid DEPLOY_SERVICE" >&2; exit 2; }
[[ "$arch" == amd64 || "$arch" == arm64 ]] || { echo "DEPLOY_ARCH must be amd64 or arm64" >&2; exit 2; }
for server in "$@"; do
  [[ "$server" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.@-]*$ ]] || { echo "Invalid host: $server" >&2; exit 2; }
done

ssh_options=(-o BatchMode=yes -o ConnectTimeout=10)
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/isureal-build.XXXXXX")
trap 'rm -rf -- "$build_dir"' EXIT

echo "Building linux/$arch application"
cd -- "$script_dir"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$build_dir/isureal" ./cmd/isureal

# Check all hosts before changing any application.
for server in "$@"; do
  echo "Checking $server"
  ssh "${ssh_options[@]}" "$server" "bash -s -- $remote_dir $service $arch" <<'REMOTE'
set -euo pipefail
dir=$1
service=$2
arch=$3
test -d "$dir" && test -w "$dir" && test -f "$dir/isureal"
case "$arch:$(uname -m)" in
  amd64:x86_64|arm64:aarch64|arm64:arm64) ;;
  *) echo "Target architecture mismatch" >&2; exit 1 ;;
esac
command -v curl >/dev/null
command -v install >/dev/null
sudo -n systemctl is-active --quiet "$service"
working_dir=$(systemctl show --property=WorkingDirectory --value "$service")
test "$working_dir" = "$dir" || { echo "Service WorkingDirectory differs from DEPLOY_DIR" >&2; exit 1; }
REMOTE
done

for server in "$@"; do
  echo "Deploying $server"
  stage=$(ssh "${ssh_options[@]}" "$server" "mktemp -d $remote_dir/.deploy.XXXXXX")
  # Restrict paths used in subsequent remote commands and cleanup.
  [[ "$stage" == "$remote_dir"/.deploy.* && "$stage" =~ ^/[a-zA-Z0-9_./-]+$ ]] || { echo "Unexpected staging path" >&2; exit 1; }
  if ! scp "${ssh_options[@]}" "$build_dir/isureal" "$server:$stage/upload"; then
    ssh "${ssh_options[@]}" "$server" "rm -rf -- $stage" || true
    exit 1
  fi
  ssh "${ssh_options[@]}" "$server" "bash -s -- $remote_dir $service $stage" <<'REMOTE'
set -euo pipefail
dir=$1
service=$2
stage=$3
rollback=0
finish() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$rollback" -eq 1 ]; then
    echo "Deployment failed; restoring previous application" >&2
    if mv -f -- "$stage/previous" "$dir/isureal" && sudo -n systemctl restart "$service"; then
      echo "Previous binary restored; check application health before retrying" >&2
    else
      echo "ROLLBACK FAILED: manual recovery required; staging directory: $stage" >&2
      exit 1
    fi
  fi
  rm -rf -- "$stage"
  exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM

cp -p -- "$dir/isureal" "$stage/previous"
install -m 0755 -- "$stage/upload" "$stage/next"
# Stage on the same filesystem so rename replaces the executable atomically.
rollback=1
mv -f -- "$stage/next" "$dir/isureal"
sudo -n systemctl restart "$service"

# An unsigned image request must return 403. This checks Go directly, not nginx.
for ((attempt=1; attempt<=20; attempt++)); do
  status=$(curl --silent --output /dev/null --write-out '%{http_code}' \
    --connect-timeout 1 --max-time 2 http://127.0.0.1:8080/api/images/deploy-check || true)
  if [ "$status" = 403 ] && sudo -n systemctl is-active --quiet "$service"; then
    rollback=0
    echo "Application started successfully"
    exit 0
  fi
  sleep 1
done
echo "Application health check failed" >&2
exit 1
REMOTE
done

echo "Deployment finished"
