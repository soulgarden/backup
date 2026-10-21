#!/usr/bin/env bash
set -euo pipefail
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
umask 077
printf '%s' "${CONTRACT_ACCESS_KEY:?}" > "$work/access_key"
printf '%s' "${CONTRACT_SECRET_KEY:?}" > "$work/secret_key"
# Non-root image may read only these transient credential files.
chmod 755 "$work"
chmod 444 "$work/access_key" "$work/secret_key"
jq --arg bucket "${CONTRACT_BUCKET:?}" --arg prefix "${CONTRACT_BACKUP_PREFIX:?}" \
  '.s3.bucket=$bucket | .s3.prefix=$prefix' config.example.json > "$work/config.json"
chmod 444 "$work/config.json"
mkdir -p artifacts
chmod 777 artifacts
report="artifacts/provider-${CONTRACT_BUCKET}.json"
if test -f "$report"; then
  mv "$report" "${report%.json}.previous-$(date -u +%Y%m%dT%H%M%SZ)-$$.json"
fi
image_id=$(docker image inspect "${IMAGE:?}" --format '{{.Id}}')
image_revision=$(docker image inspect "$IMAGE" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
workspace_dirty=false
if test -n "$(git status --porcelain --untracked-files=no)"; then workspace_dirty=true; fi
docker run --rm --stop-timeout 330 --mount "type=bind,src=$work,dst=/credentials,readonly" \
  --mount "type=bind,src=$work/config.json,dst=/config/config.json,readonly" \
  --mount "type=bind,src=$PWD/artifacts,dst=/reports" "${IMAGE:?}" \
  contract --allow-production-bucket --report "/reports/provider-${CONTRACT_BUCKET}.json"
jq --arg image "$IMAGE" --arg image_id "$image_id" --arg revision "$image_revision" \
  --arg head "$(git rev-parse HEAD)" --argjson dirty "$workspace_dirty" \
  '. + {image_reference:$image,image_id:$image_id,image_revision:$revision,workspace_head:$head,workspace_dirty:$dirty}' \
  "$report" > "$work/report.json"
chmod 644 "$work/report.json"
mv "$work/report.json" "$report"
