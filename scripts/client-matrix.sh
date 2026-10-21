#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
for major in 17 18; do
  image="backup-client-check:$$-$major"
  trap 'docker image rm "$image" >/dev/null 2>&1 || true' EXIT
  docker build --platform linux/amd64 --target integration --build-arg "POSTGRES_MAJOR=$major" -t "$image" .
  docker run --rm -e BACKUP_CLIENT_MATRIX=1 --entrypoint backup-test "$image" -test.run '^TestClientMatrix$' -test.v
  docker image rm "$image" >/dev/null
  trap - EXIT
done
