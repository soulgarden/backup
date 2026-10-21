#!/usr/bin/env bash
set -euo pipefail
umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
config=$(realpath "${BACKUP_CONFIG:?}")
credentials=$(realpath "${S3_CREDENTIALS_DIR:?}")
bucket=$(jq -er '.s3.bucket' "$config")
mkdir -p artifacts
report="artifacts/provider-${bucket}.json"
if test -f "$report"; then
  mv "$report" "${report%.json}.previous-$(date -u +%Y%m%dT%H%M%SZ)-$$.json"
fi
image_id=$(docker image inspect "${IMAGE:?}" --format '{{.Id}}')
image_revision=$(docker image inspect "$IMAGE" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
workspace_dirty=false
if test -n "$(git status --porcelain --untracked-files=no)"; then workspace_dirty=true; fi
workspace_head=$(git rev-parse HEAD)
docker run --rm --stop-timeout 330 --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$credentials,dst=/credentials,readonly" \
  --mount "type=bind,src=$config,dst=/config/config.json,readonly" \
  --mount "type=bind,src=$PWD/artifacts,dst=/reports" "${IMAGE:?}" \
  contract "$@" --report "/reports/provider-${bucket}.json"
jq --arg image "$IMAGE" --arg image_id "$image_id" --arg revision "$image_revision" \
  --arg head "$workspace_head" --argjson dirty "$workspace_dirty" \
  '. + {image_reference:$image,image_id:$image_id,image_revision:$revision,workspace_head:$head,workspace_dirty:$dirty}' \
  "$report" > "$work/report.json"
mv "$work/report.json" "$report"
