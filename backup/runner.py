import hashlib
import json
import os
import signal
import sys
import tempfile
import uuid
from datetime import datetime, timezone
from pathlib import Path

from . import archive, config, retention, storage
from .events import emit
from .process import Cancelled, Processes


def utcnow():
    return datetime.now(timezone.utc)


def run(settings, processes, store, workspace, run_id, clock=utcnow):
    stage = "preflight"
    try:
        env = config.pg_environment(settings)
        inv = archive.inventory(processes, env)
        versions = archive.versions(processes, settings, inv)
        # The 5s metadata timeout must not cap the actual backup.
        env["PGOPTIONS"] = "-c default_transaction_read_only=on"
        profile = {k: settings[k] for k in ("backup", "validation", "postgresql")}
        profile["source"] = {k: os.environ[k] for k in ("PGDATABASE", "PGHOST", "PGUSER")}
        fingerprint = hashlib.sha256(json.dumps(profile, sort_keys=True).encode()).hexdigest()
        stage = "dump"
        started = clock().isoformat()
        part = Path(workspace) / "backup.dump.part"
        processes.run(archive.dump_args(settings, part), env)
        finished = clock().isoformat()
        stage = "local_validation"
        toc_path = Path(workspace) / "toc.txt"
        with toc_path.open("wb") as toc:
            processes.run(["pg_restore", "--list", "--verbose", str(part)], output=toc)
        processes.run(["pg_restore", "--file=/dev/null", str(part)])
        metadata_env = dict(env, PGOPTIONS="-c default_transaction_read_only=on -c statement_timeout=5000")
        result = archive.validate_toc(toc_path.read_text(), inv,
                                      archive.inventory(processes, metadata_env), settings["validation"])
        path = Path(workspace) / "backup.dump"
        part.rename(path)
        size, sha = archive.hash_file(path)
        if not size:
            raise ValueError("EMPTY_ARCHIVE")
        key = settings["s3"]["prefix"] + run_id + "/backup.dump"
        stage = "upload"
        store.upload(path, key)
        stage = "remote_validation"
        store.verify(key, size, sha)
        manifest = dict(format_version=2, run_id=run_id, database=os.environ["PGDATABASE"],
                        archive_key=key, archive_bytes=size, archive_sha256=sha,
                        dump_started_at=started, dump_finished_at=finished,
                        verification_finished_at=clock().isoformat(),
                        image_version=Path(__file__).parent.parent.joinpath("VERSION").read_text().strip(),
                        dump_major=settings["postgresql"]["majorVersion"],
                        required_restore_major=settings["postgresql"]["majorVersion"],
                        target_server_major=settings["postgresql"]["targetServerMajor"],
                        configuration_fingerprint=fingerprint, filters=settings["backup"],
                        checks=dict(toc=result, full_archive_read=True, remote_sha256=True),
                        verification_scope=retention.SCOPE, database_restore_tested=False,
                        status="completed", **versions)
        stage = "manifest"
        store.publish(key.rsplit("/", 1)[0] + "/manifest.json", manifest)
        emit("backup_completed", stage, run_id)
    except Cancelled:
        emit("backup_failed", stage, run_id, "CANCELLED", "ERROR")
        return 143
    except Exception:
        emit("backup_failed", stage, run_id, "STAGE_FAILED", "ERROR")
        return 1
    try:
        complete, _ = retention.inventory(store, settings["s3"]["prefix"])
        if retention.size_anomaly(complete, manifest, settings["observability"]["sizeAnomaly"]):
            emit("backup_size_anomaly", "retention", run_id, "SIZE_CHANGED", "ERROR")
        candidates = retention.rotate(store, settings, run_id, clock())
        # Candidate keys are only printed in a separate INFO line; no credentials.
        print(json.dumps(dict(level="INFO", event="backup_retention_plan", candidates=candidates)), flush=True)
    except Cancelled:
        return 143
    except Exception:
        emit("backup_retention_failed", "retention", run_id, "CLEANUP_FAILED", "ERROR")
    return 0


def main():
    run_id = utcnow().strftime("%Y%m%dT%H%M%SZ-") + uuid.uuid4().hex
    try:
        settings = config.load(os.environ.get("BACKUP_CONFIG", "/config/config.json"))
        processes = Processes(settings["process"]["shutdownGraceSeconds"])
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, processes.handle_signal)
        store = storage.connect(settings["s3"], config.credentials(
            os.environ.get("S3_CREDENTIALS_DIR", "/credentials")))
        with tempfile.TemporaryDirectory(prefix="backup-", dir=os.environ.get("BACKUP_WORKSPACE", "/work")) as work:
            return run(settings, processes, store, work, run_id)
    except Cancelled:
        return 143
    except Exception:
        emit("backup_failed", "preflight", run_id, "INVALID_SETUP", "ERROR")
        return 1


if __name__ == "__main__":
    sys.exit(main())
