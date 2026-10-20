#!/usr/bin/env bash
set -euo pipefail
version=$(cat VERSION)
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
status=$(curl --silent --show-error --max-time 20 --output /dev/null --write-out '%{http_code}' "https://hub.docker.com/v2/repositories/soulgarden/backup/tags/$version")
if [[ "$status" != 404 ]]; then
  echo "Cannot confirm an unpublished tag; publication blocked (HTTP $status)" >&2
  exit 1
fi
