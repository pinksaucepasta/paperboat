#!/usr/bin/env python3
"""Create/check a private bucket and verify a bounded, disposable upload round trip."""
import argparse
from pathlib import Path
import secrets
from urllib.parse import urlsplit, quote
from urllib.request import urlopen
from urllib.error import HTTPError

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError

parser = argparse.ArgumentParser()
parser.add_argument("--endpoint", required=True)
parser.add_argument("--region", required=True)
parser.add_argument("--bucket", required=True)
parser.add_argument("--secrets-directory", type=Path, required=True)
args = parser.parse_args()
endpoint = urlsplit(args.endpoint)
if endpoint.scheme != "https" or endpoint.username or endpoint.password:
    parser.error("diagnostics verification requires an HTTPS endpoint without credentials")
client = boto3.client(
    "s3", endpoint_url=args.endpoint, region_name=args.region,
    aws_access_key_id=(args.secrets_directory / "diagnostics_access_key").read_text().strip(),
    aws_secret_access_key=(args.secrets_directory / "diagnostics_secret_key").read_text().strip(),
    config=Config(connect_timeout=5, read_timeout=10, retries={"max_attempts": 1},
                  s3={"addressing_style": "path"}),
)
try:
    client.head_bucket(Bucket=args.bucket)
except ClientError as error:
    if error.response["ResponseMetadata"]["HTTPStatusCode"] != 404:
        raise SystemExit("Diagnostics bucket check failed; inspect access and endpoint configuration.") from None
    client.create_bucket(Bucket=args.bucket)
key = "deployment-check/" + secrets.token_hex(16)
payload = secrets.token_bytes(32)
try:
    client.put_object(Bucket=args.bucket, Key=key, Body=payload)
    try:
        with urlopen(args.endpoint.rstrip("/") + "/" + quote(args.bucket) + "/" + key, timeout=10):
            raise SystemExit("Diagnostics object is publicly readable; remove the bucket policy.")
    except HTTPError as error:
        if error.code != 403:
            raise SystemExit("Diagnostics anonymous-access check failed.") from None
    response = client.get_object(Bucket=args.bucket, Key=key)
    with response["Body"] as body:
        if body.read(33) != payload:
            raise SystemExit("Diagnostics upload verification failed.")
finally:
    client.delete_object(Bucket=args.bucket, Key=key)
print("Diagnostics private bucket upload/download/cleanup PASS")
