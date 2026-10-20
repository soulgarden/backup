import hashlib
import json
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError
from .process import Cancelled


def canonical_json(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("DUPLICATE_JSON_KEY")
        result[key] = value
    return result


def client(config, credentials):
    return boto3.client("s3", endpoint_url=config["endpoint"], region_name=config["region"],
                        **credentials, config=Config(
                            signature_version=config["signatureVersion"],
                            s3={"addressing_style": config["addressingStyle"]},
                            connect_timeout=config["connectTimeoutSeconds"],
                            read_timeout=config["readTimeoutSeconds"],
                            retries={"mode": "standard", "total_max_attempts": config["totalAttempts"]},
                            request_checksum_calculation=config["requestChecksumCalculation"],
                            response_checksum_validation=config["responseChecksumValidation"]))


def connect(config, credentials):
    abort_config = dict(config, connectTimeoutSeconds=2, readTimeoutSeconds=3, totalAttempts=1)
    return Storage(client(config, credentials), config, client(abort_config, credentials))


class Storage:
    def __init__(self, client, config, abort_client=None):
        self.client = client
        self.abort_client = abort_client or client
        self.config = config
        self.bucket = config["bucket"]

    def head(self, key):
        return self.client.head_object(Bucket=self.bucket, Key=key)

    def absent(self, key):
        try:
            self.head(key)
        except ClientError as error:
            if error.response["Error"]["Code"] in ("404", "NoSuchKey", "NotFound"):
                return True
            raise
        return False

    def upload(self, path, key):
        c = self.config
        size = Path(path).stat().st_size
        if size < c["multipartThresholdMiB"] * 1024**2:
            with open(path, "rb") as source:
                self.client.put_object(Bucket=self.bucket, Key=key, Body=source, ACL="private")
            return
        upload_id = self.client.create_multipart_upload(
            Bucket=self.bucket, Key=key, ACL="private")["UploadId"]
        args = dict(Bucket=self.bucket, Key=key, UploadId=upload_id)
        completed = False
        try:
            # At most maxConcurrency part buffers, never the entire archive.
            parts = []
            pool = ThreadPoolExecutor(c["maxConcurrency"])
            try:
                with open(path, "rb") as source:
                    number = 1
                    while True:
                        batch = []
                        for _ in range(c["maxConcurrency"]):
                            block = source.read(c["multipartChunkSizeMiB"] * 1024**2)
                            if not block:
                                break
                            batch.append((number, pool.submit(self.client.upload_part,
                                                             **args, PartNumber=number, Body=block)))
                            number += 1
                        if not batch:
                            break
                        for part_number, future in batch:
                            parts.append(dict(PartNumber=part_number, ETag=future.result()["ETag"]))
            finally:
                # Never wait for network workers before aborting on SIGTERM.
                pool.shutdown(wait=False, cancel_futures=True)
            self.client.complete_multipart_upload(**args, MultipartUpload={"Parts": parts})
            completed = True
        finally:
            if not completed:
                self.abort_client.abort_multipart_upload(**args)

    def verify(self, key, size, sha):
        if self.head(key)["ContentLength"] != size:
            raise ValueError("HEAD_SIZE_MISMATCH")
        for attempt in range(self.config["totalAttempts"]):
            try:
                digest, count = hashlib.sha256(), 0
                response = self.client.get_object(Bucket=self.bucket, Key=key)
                body = response["Body"]
                try:
                    while block := body.read(self.config["readChunkMiB"] * 1024**2):
                        count += len(block)
                        if count > size:
                            raise ValueError("GET_SIZE_MISMATCH")
                        digest.update(block)
                finally:
                    body.close()
                if count != size or response["ContentLength"] != size or digest.hexdigest() != sha:
                    raise ValueError("GET_HASH_MISMATCH")
                return
            except Cancelled:
                raise
            except Exception:
                if attempt + 1 == self.config["totalAttempts"]:
                    raise

    def read_manifest(self, key):
        response = self.client.get_object(Bucket=self.bucket, Key=key)
        body = response["Body"]
        try:
            if response["ContentLength"] > 1024**2:
                raise ValueError("MANIFEST_TOO_LARGE")
            raw = body.read(1024**2 + 1)
            if len(raw) != response["ContentLength"]:
                raise ValueError("MANIFEST_SIZE_MISMATCH")
            return json.loads(raw, object_pairs_hook=unique_object)
        finally:
            body.close()

    def publish(self, key, manifest):
        raw = canonical_json(manifest)
        try:
            self.client.put_object(Bucket=self.bucket, Key=key, Body=raw,
                                   ACL="private", ContentType="application/json")
        except Cancelled:
            raise
        except Exception:
            # Timeout may follow a successful PUT. Only a matching GET can confirm it.
            if canonical_json(self.read_manifest(key)) != raw:
                raise ValueError("MANIFEST_UNCONFIRMED") from None
            return
        if canonical_json(self.read_manifest(key)) != raw:
            raise ValueError("MANIFEST_MISMATCH")

    def listing(self, prefix):
        objects = []
        for page in self.client.get_paginator("list_objects_v2").paginate(
                Bucket=self.bucket, Prefix=prefix):
            if page.get("IsTruncated") and not page.get("NextContinuationToken"):
                raise ValueError("INCOMPLETE_LISTING")
            objects.extend(page.get("Contents", []))
        return objects

    def delete(self, key):
        self.client.delete_object(Bucket=self.bucket, Key=key)
