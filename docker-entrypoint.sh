#!/bin/sh
set -eu
exec python -m backup.runner "$@"
