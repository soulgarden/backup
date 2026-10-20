#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
image=${IMAGE:-backup:test}
test_image="backup-checks:$(date +%s)-$$"
run="backup-go-$$"
work=$(mktemp -d)
cleanup() {
  docker rm -f "$run-signal" "$run-pg" "$run-moto" >/dev/null 2>&1 || true
  docker network rm "$run" >/dev/null 2>&1 || true
  docker image rm "$test_image" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p artifacts "$work/data"
chmod 755 "$work"
chmod 777 "$work/data"
docker build --platform linux/amd64 --target integration -t "$test_image" .
# Assert that integration tests use the exact same runner binary and PG tools.
actual=$(docker run --rm --entrypoint sh "$image" -c 'sha256sum /usr/local/bin/backup; pg_dump --version; pg_restore --version')
fixture=$(docker run --rm --entrypoint sh "$test_image" -c 'sha256sum /usr/local/bin/backup; pg_dump --version; pg_restore --version')
test "$actual" = "$fixture"
docker network create "$run" >/dev/null
docker run -d --name "$run-pg" --network "$run" --network-alias pg -e POSTGRES_PASSWORD=fixture -e POSTGRES_DB=fixture postgres:16-alpine >/dev/null
docker run -d --name "$run-moto" --network "$run" --network-alias moto motoserver/moto:5.1.15 >/dev/null
for _ in {1..90}; do
  if docker exec "$run-pg" pg_isready -h 127.0.0.1 -U postgres -d fixture >/dev/null 2>&1; then break; fi
  sleep 1
done
for _ in {1..90}; do
  if docker exec "$run-moto" python -c "import urllib.request;urllib.request.urlopen('http://localhost:5000')" >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec -i "$run-pg" psql -U postgres -d fixture -v ON_ERROR_STOP=1 < tests/fixture.sql
pg=(-e PGHOST=pg -e PGPORT=5432 -e PGUSER=postgres -e PGDATABASE=fixture -e PGPASSWORD=fixture)
docker run --rm --network "$run" "${pg[@]}" --read-only --cap-drop=ALL --security-opt=no-new-privileges --memory=256m --cpus=2 \
  --mount "type=bind,src=$work/data,dst=/work" -e BACKUP_INTEGRATION=1 -e INTEGRATION_REPORT=/work/integration.json \
  --entrypoint backup-test "$test_image" -test.run '^TestFixture$' -test.v
cp "$work/data/integration.json" artifacts/integration.json
# PID1 cancellation of the real pg_dump process group, with no upload/manifest.
docker exec -d "$run-pg" psql -U postgres -d fixture -c 'BEGIN; LOCK TABLE public."leaf one" IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(120);'
cp config.example.json "$work/config.json"
printf '%s' fixture > "$work/access_key"
printf '%s' fixture > "$work/secret_key"
chmod 444 "$work/config.json" "$work/access_key" "$work/secret_key"
docker run -d --name "$run-signal" --network "$run" "${pg[@]}" --mount "type=bind,src=$work,dst=/config,readonly" \
  --mount "type=bind,src=$work,dst=/credentials,readonly" --tmpfs /work:uid=10001,gid=10001,size=134217728 "$image" >/dev/null
found=false
for _ in {1..30}; do
  if docker exec "$run-signal" sh -c "ps -o args | grep -q '[p]g_dump --no-password'"; then found=true; break; fi
  sleep 1
done
$found
docker stop --time 50 "$run-signal" >/dev/null
test "$(docker inspect --format '{{.State.ExitCode}}' "$run-signal")" = 143
docker logs "$run-signal" > artifacts/signal.jsonl
grep -q CANCELLED artifacts/signal.jsonl
! grep -q backup_completed artifacts/signal.jsonl
