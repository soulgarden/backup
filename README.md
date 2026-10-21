# PostgreSQL archive backup

Creates a custom-format PostgreSQL 16 archive, reads the entire archive with
`pg_restore --file=/dev/null`, uploads it to private S3 storage and downloads it
as a bounded stream to verify byte count and SHA-256. Publishes and reads back
`manifest.json` only after those checks. No SQL restore is performed or claimed.

Configuration is versioned JSON (`config.example.json`, strict schema in
`internal/backup/config.schema.json`). Helm renders arrays without shell interpolation.
PGHOST/PGPORT/PGUSER/PGDATABASE/PGPASSWORD provide the source connection.
Use a direct PostgreSQL service. The catalog query checks effective read-only
settings and its five-second timeout in the same connection before reading
metadata; discarded startup options stop the run before pg_dump.
S3 credentials are read only from `/credentials/access_key`, `secret_key` and
optional `session_token`; SDK credential discovery is not used. Exceptions never
print credential-bearing SDK/server diagnostics. JSON events identify the failed
stage. `BACKUP_CONFIG`, `BACKUP_WORKSPACE`, `S3_CREDENTIALS_DIR` override file paths.
The corresponding `--config`, `--workspace`, `--credentials` flags take precedence
over environment variables. Run `backup --help` for commands and flags.

`includeSchemas: []` covers the database's non-system scope; inaccessible objects
fail the dump. PostgreSQL pattern semantics apply to schema/table filters.
Excluding a partition root does not implicitly exclude its children in PG16;
list the required children explicitly. ACL and large objects are retained by
default. Required partition tables require data entries for every ordinary leaf;
conflicting exclude-data policies fail validation. No row-count scans are used.

## Local checks

The runner uses Go 1.27.2, AWS SDK for Go v2, Cobra and Viper. The final image
contains the compiled runner and PostgreSQL client tools.

Run `make lint helm-validate test test-race vet build`. `make helm-validate` runs
strict Helm lint and render tests, validates generated JSON and checks activation
gates. `make image` builds without publishing.
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

The chart initially suspends the CronJob. Before enabling it:

1. Run `backup contract` against the bucket in the configuration. It checks
   single/multipart uploads, full SHA-256 readback, pagination with more than
   1000 objects and multipart abort. Temporary objects use a unique
   `_backup-contract/<timestamp-UUID>/` prefix outside the backup prefix.
   Cleanup deletes only keys generated by that invocation; foreign objects are
   preserved and make the check fail. `backup contract --dry-run` reads bucket
   versioning settings and writes a report describing the planned checks,
   without uploading or deleting objects. A dry run does not verify uploads.
   A recovery journal with the exact generated keys is written before uploads
   and updated on failure. Contract shutdown allows up to five minutes of
   cleanup; retain a failed journal before retrying or manually removing
   incomplete test uploads.
2. Configure a prefix-scoped
   `AbortIncompleteMultipartUpload: {DaysAfterInitiation: 1}` lifecycle rule,
   preserving existing rules. `backup verify-lifecycle` reads it back without
   changing it. Runner credentials do not need permission to change bucket rules.
3. Set the **published tested image digest**, `providerContractVerified=true`,
   `multipartLifecycleVerified=true`, then activate with `suspend=false`.
   Measure RAM, disk and duration in the first run. Check that the PostgreSQL
   role can read the required tables, extensions and any rows protected by RLS.
4. Keep retention dryRun=true, review candidates. Before setting dryRun=false,
   check that the key is limited to the intended bucket and set
   deletionApproved=true. Legacy cleanup remains a separate opt-in.

Release version comes from VERSION. One `.github/workflows/main.yml` runs checks
for PRs and pushes to the single primary branch, `main`. Publication runs only
on pushes to `main` for a version absent from the registry, after checks and
integration tests. An existing version skips publication; unknown registry
responses fail closed. The tested image is
transferred as an artifact and published without rebuilding.

The `backup-release` environment needs only the dedicated
DOCKER_HUB_USERNAME/DOCKER_HUB_ACCESS_TOKEN for publication. Restrict environment
deployment branches to main. CI runs disposable PostgreSQL/S3 fixtures.
Before deployment, run `scripts/provider-contract.sh` locally with the published
image and the same configuration and credentials as the runner. Set `IMAGE`,
`BACKUP_CONFIG` (configuration file) and `S3_CREDENTIALS_DIR` (credentials directory).
The script also accepts `--dry-run` and records the image ID and revision in its
report. S3 credentials stay outside CI.
Existing version tags and ambiguous registry responses block publication. No
latest tag is overwritten. Production charts receive the published digest in a
separate rollout change. Do not deploy a draft suspended chart over a running
production schedule. `backup verify-lifecycle --report /work/lifecycle.json`
reads back the required prefix-scoped lifecycle rule without changing it.

When upgrading from an older image, retain its configuration and credential
mounts for rollback. An older image cannot use the new runner's configuration.
PITR/WAL archiving and recoverability testing remain separate projects.
