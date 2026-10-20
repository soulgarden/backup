import re
import json
from pathlib import Path
from jsonschema import Draft202012Validator
from datetime import timedelta
from statistics import median

SCOPE = "archive_read_and_remote_sha256"
MANIFEST = Draft202012Validator(json.loads(Path(__file__).with_name("manifest.schema.json").read_text()))
RUN = re.compile(r"[0-9]{8}T[0-9]{6}Z-[a-f0-9]{32}")


def completed_manifest(manifest, prefix, run_id):
    if not MANIFEST.is_valid(manifest):
        raise ValueError("AMBIGUOUS_MANIFEST")
    expected = dict(format_version=2, run_id=run_id, status="completed",
                    verification_scope=SCOPE, database_restore_tested=False,
                    archive_key=prefix + run_id + "/backup.dump")
    if any(manifest.get(k) != v for k, v in expected.items()):
        raise ValueError("AMBIGUOUS_MANIFEST")
    return manifest


def inventory(store, prefix):
    groups = {}
    for obj in store.listing(prefix):
        relative = obj["Key"][len(prefix):]
        fields = relative.split("/")
        if len(fields) != 2 or not RUN.fullmatch(fields[0]) or fields[1] not in (
                "backup.dump", "manifest.json"):
            raise ValueError("AMBIGUOUS_LAYOUT")
        groups.setdefault(fields[0], {})[fields[1]] = obj
    completed, incomplete = [], []
    for run_id, objects in groups.items():
        marker = prefix + run_id + "/manifest.json"
        if "manifest.json" not in objects:
            if not store.absent(marker):
                raise ValueError("CONCURRENT_MANIFEST")
            incomplete.append((run_id, objects))
            continue
        m = completed_manifest(store.read_manifest(marker), prefix, run_id)
        if "backup.dump" not in objects or store.head(m["archive_key"])["ContentLength"] != m["archive_bytes"]:
            raise ValueError("AMBIGUOUS_ARCHIVE")
        completed.append((run_id, objects, m))
    completed.sort(key=lambda item: (item[1]["manifest.json"]["LastModified"], item[0]), reverse=True)
    return completed, incomplete


def rotate(store, config, current_run, now):
    policy = config["retention"]
    if not policy["enabled"]:
        return []
    if store.client.get_bucket_versioning(Bucket=store.bucket).get("Status"):
        raise ValueError("VERSIONING_UNSUPPORTED")
    prefix = config["s3"]["prefix"]
    complete, incomplete = inventory(store, prefix)
    if current_run not in {item[0] for item in complete}:
        raise ValueError("CURRENT_MANIFEST_MISSING")
    protected = [item for i, item in enumerate(complete)
                 if i < policy["keepLast"] or item[0] == current_run]
    protected_ids = {item[0] for item in protected}
    candidates = []
    for run_id, objects, _ in complete:
        if run_id not in protected_ids and now - objects["manifest.json"]["LastModified"] > timedelta(days=policy["maxAgeDays"]):
            candidates.extend([objects["manifest.json"]["Key"], objects["backup.dump"]["Key"]])
    orphan_candidates = []
    for run_id, objects in incomplete:
        if now - max(o["LastModified"] for o in objects.values()) > timedelta(days=policy["incompleteRunMaxAgeDays"]):
            orphan_candidates.append((run_id, objects))
    # Get all listings before any deletion, including optional legacy.
    legacy = policy["legacy"]
    legacy_keys = []
    if legacy["enabled"] and len(complete) >= legacy["minVerifiedV2Sets"]:
        for obj in store.listing(legacy["prefix"]):
            name = obj["Key"][len(legacy["prefix"]):]
            if "/" not in name and re.fullmatch(legacy["filenamePattern"], name) and now - obj["LastModified"] > timedelta(days=policy["maxAgeDays"]):
                legacy_keys.append(obj["Key"])
    plan = ([dict(key=key, reason="completed_older_than_retention", dry_run=policy["dryRun"])
             for key in candidates] +
            [dict(key=o["Key"], reason="incomplete_run_expired", dry_run=policy["dryRun"])
             for _, objects in orphan_candidates for o in objects.values()] +
            [dict(key=key, reason="legacy_older_than_retention",
                  dry_run=policy["dryRun"] or legacy["dryRun"]) for key in legacy_keys])
    if policy["dryRun"]:
        return plan
    if not policy["deletionApproved"]:
        raise ValueError("DELETION_NOT_APPROVED")
    # Recheck the protected reserve immediately before the destructive pass.
    for _, objects, manifest in protected:
        if store.read_manifest(objects["manifest.json"]["Key"]) != manifest:
            raise ValueError("PROTECTED_MANIFEST_CHANGED")
        if store.head(manifest["archive_key"])["ContentLength"] != manifest["archive_bytes"]:
            raise ValueError("PROTECTED_ARCHIVE_CHANGED")
    for key in candidates:
        store.delete(key)
    for run_id, objects in orphan_candidates:
        if not store.absent(prefix + run_id + "/manifest.json"):
            raise ValueError("CONCURRENT_MANIFEST")
        for obj in objects.values():
            store.delete(obj["Key"])
    if not legacy["dryRun"]:
        for key in legacy_keys:
            store.delete(key)
    return plan


def size_anomaly(complete, manifest, policy):
    samples = [m["archive_bytes"] for _, _, m in complete
               if m["run_id"] != manifest["run_id"] and
               m["configuration_fingerprint"] == manifest["configuration_fingerprint"]][:policy["historySamples"]]
    if len(samples) < policy["minSamples"]:
        return False
    ratio = manifest["archive_bytes"] / median(samples)
    return ratio < policy["minRatio"] or ratio > policy["maxRatio"]
