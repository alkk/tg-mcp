#!/bin/sh
# runs as root before /init drops privileges — makes the data dir usable whether it is a
# named volume, a bind mount, or nothing at all
set -e

mkdir -p "${DATA_DIR:-/srv/data}"
chown -R "${APP_UID:-101}:${APP_GID:-990}" "${DATA_DIR:-/srv/data}"
