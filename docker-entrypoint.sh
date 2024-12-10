#!/bin/sh
set -e

datestamp=$(date +'%Y-%m-%d')
timestamp=$(date +'%H%M')

filename="$(date +%d-%m-%y_%H-%M-%S)".sql.gz
filepath=/tmp/"${PGDATABASE}"-"$filename"

pg_dump -v --no-acl -d "${PGDATABASE}" -n public | gzip -9 > "$filepath"

ls -s "$filepath"

s3cmd put --acl-private "$filepath" s3://"${BUCKET}"/"${PGDATABASE}"/"$filename"

s3cmd du s3://"${BUCKET}"/"${PGDATABASE}"/"$filename"

exec "$@"
