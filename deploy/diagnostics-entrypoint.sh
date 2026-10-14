#!/bin/sh
set -eu
: "${MINIO_ROOT_USER_FILE:?configure the protected diagnostics user file}"
: "${MINIO_ROOT_PASSWORD_FILE:?configure the protected diagnostics password file}"
MINIO_ROOT_USER=$(cat "$MINIO_ROOT_USER_FILE")
MINIO_ROOT_PASSWORD=$(cat "$MINIO_ROOT_PASSWORD_FILE")
[ -n "$MINIO_ROOT_USER" ] && [ -n "$MINIO_ROOT_PASSWORD" ] || { echo 'Diagnostics credentials are empty.' >&2; exit 1; }
export MINIO_ROOT_USER MINIO_ROOT_PASSWORD
exec /usr/local/bin/minio "$@"
