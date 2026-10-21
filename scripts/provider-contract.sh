#!/usr/bin/env bash
set -euo pipefail
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
umask 077
printf '%s' "${FIXTURE_ACCESS_KEY:?}" > "$work/access_key"
printf '%s' "${FIXTURE_SECRET_KEY:?}" > "$work/secret_key"
printf '%s' "${SCOPE_EVIDENCE:?}" > "$work/scope.json"
# Non-root image may read only these transient test credentials.
chmod 755 "$work"
chmod 444 "$work/access_key" "$work/secret_key" "$work/scope.json"
# Chart/JSON fixture profile contains no credentials or production bucket names.
jq --arg bucket "${FIXTURE_BUCKET:?}" '.s3.bucket=$bucket' config.example.json > "$work/config.json"
chmod 444 "$work/config.json"
mkdir -p artifacts
chmod 777 artifacts
docker run --rm --mount "type=bind,src=$work,dst=/credentials,readonly" \
  --mount "type=bind,src=$work/config.json,dst=/config/config.json,readonly" \
  --mount "type=bind,src=$PWD/artifacts,dst=/reports" "${IMAGE:?}" \
  contract --canary-bucket "${FIXTURE_CANARY_BUCKET:?}" \
  --scope-evidence /credentials/scope.json --report /reports/provider.json
