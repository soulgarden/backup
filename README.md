# PostgreSQL archive backup

Creates a custom-format PostgreSQL 16 archive, reads the entire archive with
`pg_restore --file=/dev/null`, uploads it to private S3 storage and downloads it
as a bounded stream to verify byte count and SHA-256. Publishes and reads back
`manifest.json` only after those checks. No SQL restore is performed or claimed.

Configuration is versioned JSON (`config.example.json`, strict schema in
`internal/backup/config.schema.json`). Helm renders arrays without shell interpolation.
PGHOST/PGPORT/PGUSER/PGDATABASE/PGPASSWORD provide the source connection.
S3 credentials are read only from `/credentials/access_key`, `secret_key` and
optional `session_token`; SDK credential discovery is not used. Exceptions never
print credential-bearing SDK/server diagnostics. JSON events identify the failed
stage. `BACKUP_CONFIG`, `BACKUP_WORKSPACE`, `S3_CREDENTIALS_DIR` override file paths.

`includeSchemas: []` covers the database's non-system scope; inaccessible objects
fail the dump. PostgreSQL pattern semantics apply to schema/table filters.
Excluding a partition root does not implicitly exclude its children in PG16;
list the required children explicitly. ACL and large objects are retained by
default. Required partition tables require data entries for every ordinary leaf;
conflicting exclude-data policies fail validation. No row-count scans are used.

## Local checks

The runner is Go 1.27.2 with the AWS SDK for Go v2. The final image contains the
compiled runner and PostgreSQL client tools; Python is not installed.

Run `make lint test test-race vet build`. `make image` builds without publishing.
`make integration` creates disposable PostgreSQL16/Moto fixtures and runs native
Go integration tests; SQL is never restored. `scripts/logalert.sh` uses an isolated
Elasticsearch and mock sender. `scripts/client-matrix.sh` checks actual PG17/18
clients are rejected. `BACKUP_CI_CHART=/path/to/ci/helm/backup go test
./internal/backup -run TestCharts -count=1` checks both charts together.

Go coverage must be at least 90% of statements in the backup package, together
with explicit failure/cancellation assertions.

## Completion and recovery runbook

List the configured `<prefix>/<UTC timestamp>-<UUID>/manifest.json` objects. Only
supported format_version=2, status=completed and
verification_scope=archive_read_and_remote_sha256 with an existing correctly
sized archive count as completed. Unknown versions or unreadable markers require
investigation. Download manifest and archive, calculate `sha256sum backup.dump`,
compare `archive_sha256` and `archive_bytes`, then use the recorded PostgreSQL16
pg_restore. The manifest records exact client/server versions, filters,
configuration fingerprint and `database_restore_tested=false`.

For a future separately approved SQL recovery: provision a PostgreSQL16 target,
roles, locales and required extensions first. Custom archives retain owners/ACL;
choose whether the future pg_restore uses `--no-owner --no-acl` when roles differ.
A public-only dump may require extensions to be installed separately. Check the
application after any actual recovery. This project does not execute that step.

Legacy `.sql.gz` objects have no verified manifest: download, run `gzip -t` and
check gzip's exit status separately. A separately approved legacy SQL recovery
must use `psql -X -v ON_ERROR_STOP=1` and check both decompression and psql status;
`set -e` on a plain POSIX pipeline does not do this.

## Retention and rollout gates

Completed sets expire only after **365 UTC days**, while the latest seven
completed sets and the current run are always retained. Incomplete sets have a
separate seven-day threshold. Rotation runs after manifest confirmation. Unknown,
corrupt or inaccessible markers, contradictory listings, unknown files and any
versioning status stop all cleanup. Listing is fully paginated before deletion.
Dry-run prevents every delete. Legacy cleanup is separate and disabled by default;
its own dryRun must also be false and at least seven verified sets must exist.
Errors in retention or size anomalies emit fixed ERROR events that match logalert;
the completed backup remains valid. S3 availability can change after verification.

Both charts initially suspend the new CronJob. Before activation:

1. Run all checks against the exact image. Run `backup -mode contract` in
   that image against a dedicated `backup-fixture-*` DigitalOcean AMS3 bucket.
   The contract command rejects production bucket names and requires sanitized provider
   evidence `{bucket, limited_access:true, access:"Read/Write/Delete"}`. It tests
   single/multipart upload, full SHA, >1000-key pagination, GetBucketVersioning,
   denied LIST on a separate provisioned `backup-fixture-*` canary bucket,
   real abort and fixture cleanup. It does not replace the provider-side scope review.
2. An operator configures and reads back prefix-scoped
   `AbortIncompleteMultipartUpload: {DaysAfterInitiation: 1}` while preserving all
   existing lifecycle rules. Runner credentials must not administer the bucket.
3. Set the **published tested image digest**, `providerContractVerified=true`,
   `multipartLifecycleVerified=true`, then `suspend=false` in the deployment chart.
   Use the original cadence; initial limits are estimates, measure RAM, disk and
   duration in one controlled run. Verify source role RLS/extension permissions.
4. Keep retention dryRun=true, review candidates. Before setting dryRun=false,
   prove the production key is limited to the intended bucket, keep sanitized
   evidence and set deletionApproved=true. No bucket-wide expiry or automatic
   versioning is applied. Legacy deletion is a later independent opt-in.

Release version comes from VERSION. One `.github/workflows/main.yml` runs checks
for PRs and pushes to the single primary branch, `main`. Publication runs only
on pushes to `main`, after checks and integration tests. The tested image is
transferred as an artifact and published without rebuilding.

The protected `backup-release` environment needs
DOCKER_HUB_USERNAME/DOCKER_HUB_ACCESS_TOKEN and separate Spaces fixture
credentials/evidence. The native `-mode contract` command runs from the tested
image via `scripts/provider-contract.sh`; workflow files contain no inline Python.
Existing version tags and ambiguous registry responses block publication. No
latest tag is overwritten. Production charts receive the published digest in a
separate rollout change. Do not deploy a draft suspended chart over a running
production schedule. `backup -mode verify-lifecycle -report /work/lifecycle.json`
reads back the required prefix-scoped lifecycle rule without changing it.

The CI chart migrates encrypted SOPS values to s3.accessKey/secretKey/sessionToken.
It generates SDK and PostgreSQL Secrets and retains `backup-config` (.s3cfg and
pg-password) from the same canonical values for the rollback window. Keep the old
immutable image and release configuration. Roll back using the old image's old
mount paths; do not merely run an old image with new runner mounts. Remove the
legacy Secret only in a later explicit change. With external credentials, prepare
that rollback Secret separately before switching (rendering refuses an ambiguous
migration). No secret plaintext belongs in version control or rendered artifacts.

SWUP production is DigitalOcean AMS3 (`swup-backups`), not Scaleway. Live release,
lifecycle and provider key scope still require preflight confirmation. Alert rules
are in CI's kube-prometheus-stack chart and deploy separately. Use isolated
microk8s kubeconfig and only `swup-db-swup-backup-*` and
`observability-kube-prometheus-stack-*` targets, not aggregate apply targets.
PITR/WAL archiving and recoverability testing remain separate projects.
