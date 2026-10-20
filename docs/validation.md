# Validation record — 2026-10-20

Scope: local implementation in separate service and CI worktrees. No production
changes, image publication, SQL restore, real backup deletion or notifications.

- 120 local unit/storage tests passed; own runner branch coverage 98.04%.
- Real PG17/PG18 image tools were each rejected by the PG16 preflight;
  both pg_dump and pg_restore were exercised independently.
- Both Helm backup charts passed lint, configuration schema, escaping, security,
  digest override and version conformance checks.
- Observability chart lint and promtool validation passed for all eight rules.
  Rule tests cover fresh/stale/critical, missing/no-success, suspended, active
  long-running and failed owned Jobs.
- Real image PG16.15 tools created/read custom archives. Source fixtures include
  schema/table names with spaces, quotes/Unicode/newlines, partitions, ACL, large
  objects, pgcrypto, identity sequence, index and foreign key. Empty DB passes
  general policy and fails required-object policy. No SQL is restored.
- A valid TOC with truncated archive data fails full pg_restore reading.
- S3 emulator tests passed multipart, HEAD/full GET SHA, 1001-key pagination,
  actual abort after injected failure and SIGTERM during blocked upload workers.
- Non-root UID10001, read-only container, dropped capabilities, 384MiB memory
  limit and bounded PG process-group shutdown passed. Synthetic archive:
  47,779,261 bytes; streaming test object: 100,663,296 bytes. Test workspace peak:
  290,907,526 bytes (several intentionally retained fixtures); Python RSS peak:
  157,160 KiB. Production sizing still requires measurement.
- Exact existing logalert query matched both ERROR events in isolated
  Elasticsearch8.15.5 and a mock sender. No external notification was sent.
- SOPS encrypted credential migration round-tripped and both SDK/rollback Secret
  renders matched canonical credentials in memory. ConfigMap contains no keys.
- DockerHub confirmed 0.2.0 was unpublished; release repeats the check and refuses
  existing tags or ambiguous/network responses.

Independent agent review found and verified fixes for swallowed cancellation,
MPU worker shutdown delaying abort, missing tag overwrite protection, missing
negative key-scope canary and version wiring. Final review reported no open P1/P2
runtime defects; the requested sequence/index/FK/empty-DB fixtures were then added.

Release gates remain incomplete: DigitalOcean dedicated fixture contract report,
provider-side production key scope evidence, prefix lifecycle read-back, published
immutable digest, deployment preflight, controlled first production run and
retention dry-run review. `suspend: true` and retention dryRun preserve that boundary.

Repository-wide `make swup-lint observability-lint` cannot complete in the clean CI
worktree: it stops at unrelated `helm/redis/secrets.values.yaml.dec`, absent by
design. All changed charts were linted directly. Existing repository-wide secret
checks report unrelated older potential plaintext files; the changed backup
Secret remains encrypted and was verified separately. No unrelated files were
modified or removed to bypass these checks.
