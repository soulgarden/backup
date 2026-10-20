import json
import os
from pathlib import Path
from urllib.parse import urlparse
from jsonschema import Draft202012Validator

SCHEMA = Path(__file__).with_name("config.schema.json")


def validate(config):
    errors = list(Draft202012Validator(json.loads(SCHEMA.read_text())).iter_errors(config))
    if errors:
        raise ValueError("INVALID_CONFIG")
    endpoint = urlparse(config["s3"]["endpoint"])
    if endpoint.username or endpoint.password or endpoint.query or endpoint.fragment:
        raise ValueError("INVALID_ENDPOINT")
    prefix = config["s3"]["prefix"]
    if any(part in ("", ".", "..") for part in prefix.rstrip("/").split("/")):
        raise ValueError("INVALID_PREFIX")
    if config["postgresql"]["majorVersion"] != config["postgresql"]["targetServerMajor"]:
        raise ValueError("TARGET_MAJOR_MISMATCH")
    legacy = config["retention"]["legacy"]
    if legacy["enabled"] and (not legacy["prefix"] or not legacy["filenamePattern"].startswith("^")
                              or not legacy["filenamePattern"].endswith("$")):
        raise ValueError("INVALID_LEGACY_SCOPE")
    if not config["retention"]["dryRun"] and not config["retention"]["deletionApproved"]:
        raise ValueError("DELETION_NOT_APPROVED")
    anomaly = config["observability"]["sizeAnomaly"]
    if anomaly["minSamples"] > anomaly["historySamples"] or anomaly["minRatio"] >= anomaly["maxRatio"]:
        raise ValueError("INVALID_SIZE_POLICY")
    return config


def load(path):
    return validate(json.loads(Path(path).read_text()))


def credentials(directory):
    directory = Path(directory)
    access = (directory / "access_key").read_text().strip()
    secret = (directory / "secret_key").read_text().strip()
    token = directory / "session_token"
    if not access or not secret:
        raise ValueError("MISSING_CREDENTIALS")
    return dict(aws_access_key_id=access, aws_secret_access_key=secret,
                aws_session_token=(token.read_text().strip() or None) if token.exists() else None)


def pg_environment(config):
    for name in ("PGHOST", "PGUSER", "PGDATABASE", "PGPASSWORD"):
        if not os.environ.get(name):
            raise ValueError("MISSING_PG_ENV")
    env = os.environ.copy()
    env["PGCONNECT_TIMEOUT"] = str(config["postgresql"]["connectTimeoutSeconds"])
    env["PGOPTIONS"] = "-c default_transaction_read_only=on -c statement_timeout=5000"
    return env
