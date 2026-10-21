#!/usr/bin/env bash
set -euo pipefail

if (( $# > 1 )) || [[ "${1:-}" != '' && "${1:-}" != '--if-needed' ]]; then
  echo 'Usage: check-unpublished.sh [--if-needed]' >&2
  exit 2
fi

version=$(cat VERSION)
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo 'Invalid VERSION; publication blocked' >&2
  exit 1
fi
if ! status=$(curl --silent --show-error --max-time 20 --output /dev/null --write-out '%{http_code}' "https://hub.docker.com/v2/repositories/soulgarden/backup/tags/$version"); then
  echo 'Registry request failed; publication blocked' >&2
  exit 1
fi

case "$status" in
  200)
    if [[ "${1:-}" == '--if-needed' ]]; then
      echo 'publish=false'
      exit 0
    fi
    echo 'Version already exists; publication blocked' >&2
    exit 1
    ;;
  404)
    if [[ "${1:-}" == '--if-needed' ]]; then
      echo 'publish=true'
    fi
    ;;
  *)
    echo "Cannot confirm an unpublished tag; publication blocked (HTTP $status)" >&2
    exit 1
    ;;
esac
