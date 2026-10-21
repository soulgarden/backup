#!/usr/bin/env bash
set -euo pipefail
name="backup-es-$$"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
docker run -d --name "$name" -p 127.0.0.1::9200 -e discovery.type=single-node -e xpack.security.enabled=false \
  -e ES_JAVA_OPTS='-Xms256m -Xmx256m' docker.elastic.co/elasticsearch/elasticsearch:8.15.5 >/dev/null
port=$(docker port "$name" 9200/tcp | head -1)
export BACKUP_LOGALERT_URL="http://$port"
for _ in {1..90}; do
  if curl --silent --fail "$BACKUP_LOGALERT_URL" >/dev/null; then break; fi
  sleep 1
done
go test ./internal/backup -run '^TestLogalertContract$' -count=1 -v
