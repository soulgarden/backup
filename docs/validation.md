# Go implementation validation

The runner and contracts now use Go 1.27.2 and AWS SDK for Go v2. The final image
contains a compiled executable, CA certificates and PostgreSQL client tools.
Source workflows contain no inline Python. Native Go tests and small shell
wrappers replace the earlier application and test scripts.

Local checks:

- `make lint test test-race vet build`: formatting, statement coverage gate for `internal/backup`
  (at least 90%; current local result90.3%), race detector, vet, native build, Helm/schema/version checks.
- `scripts/integration.sh`: PG16 custom archives and complete read-to-/dev/null,
  schema filters, partition leaves, ACL/large objects, quoted/Unicode/newline
  names, sequence/index/FK, valid empty DB and rejected required-object profile.
  Corruption after a valid TOC and failed/partial pg_dump are rejected.
- S3 Go SDK HTTP fixtures: failed and interrupted GET with clean hash reset,
  strict/duplicate-key manifest rejection, uncertain PUT confirmation,
  multipart failure and cancelled workers with actual abort requests, complete
  pagination and malformed metadata refusal before any DELETE.
- Retention fixtures cover 365 UTC days (strict boundary), latest seven/current
  protection, unknown/future/corrupt markers, dry-run, incomplete runs, versioning,
  36500-day overflow bound and legacy isolation from managed v2 objects.
- `scripts/logalert.sh`: exact existing query in isolated Elasticsearch8.15.5
  and mock delivery; no external notifications.
- `scripts/client-matrix.sh`: actual PG17/18 tools must each fail PG16 preflight.
- `go test -run TestCharts`: primary branch/version wiring and both Helm chart
  contracts when BACKUP_CI_CHART is set. Observability promtool rules remain in
  the separate CI worktree.

The integration harness compares the runner binary hash and exact pg_dump /
pg_restore versions in the runtime and fixture images. The image tested for PID1
shutdown is the same image later transferred as an artifact for publication.
The percentage excludes cmd/backup and does not measure branches; entrypoint
behavior is checked separately by CLI/PID1 integration. Native reports include archive/object size, peak RSS and workspace
usage. Current run results are retained in ignored `artifacts/` and test logs;
Python-era measurements are superseded, not reused as Go evidence.

Final synthetic run under a 256MiB container limit: archive 47,779,526 bytes; streaming object 100,663,296 bytes; peak RSS 71,968 KiB; peak workspace 290,908,593 bytes. A real uploaded MPU part was aborted after cancellation and Moto reported no remaining uploads. PID1 shutdown returned143 without a completed manifest. PG17/18 matrix and the exact ES query both passed.

Independent agent review found and corrected duration overflow, malformed S3
listing metadata and legacy prefixes bypassing the protected reserve. The Go
provider contract uploads a real 5MiB MPU part before abort and records SDK
versions and checksum settings.

A single main.yml checks PRs and pushes to main; publication is main-only after
checks and a protected provider contract gate. DockerHub secrets match the local
upload/logalert conventions. Existing version tags cannot be overwritten.

External release gates remain: DigitalOcean dedicated fixture contract report,
provider-side production key scope evidence, prefix lifecycle read-back, published
immutable digest, deployment preflight, controlled first run and retention dry-run
review. CronJob remains suspended and retention remains dry-run. No production
changes, publication or SQL restoration were performed during this implementation.

Repository-wide aggregate lint in the isolated CI worktree requires unrelated
decrypted Redis values. Changed charts are linted directly. Existing repository
secret scans report unrelated older potential plaintext files; the backup Secret
remains encrypted. No unrelated files were changed to bypass those checks.
