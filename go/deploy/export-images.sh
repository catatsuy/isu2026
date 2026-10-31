#!/bin/bash
set -euo pipefail
# Run on an application server after placing the Linux exporter in this directory.
# Usage: bash /home/isucon/webapp/go/deploy/export-images.sh [--since RFC3339]
set +x
. /home/isucon/env.sh
set +x
export ISUREAL_DB_HOST ISUREAL_DB_PORT ISUREAL_DB_USER ISUREAL_DB_PASSWORD ISUREAL_DB_NAME
script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
image_dir=${ISUREAL_IMAGE_FILE_DIR:-/home/isucon/image-files}
mkdir -p "$image_dir"
# Avoid duplicate full scans when a deployment resumes or exporters overlap.
exec 9>"$image_dir/.export.lock"
flock 9
if [ "$#" -eq 0 ] && [ -f "$image_dir/.export-complete" ]; then
  echo "Full image export already completed; use --since for a delta export"
  exit 0
fi
exec "$script_dir/export-images" "$@"
