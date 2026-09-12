#!/bin/sh
# Restores a dump produced by backup.sh into PGDATABASE.
#
# Restores into the target database as-is — it does not create or drop a
# database itself, deliberately: choosing whether to restore into a fresh
# database, a same-named replacement, or a differently-named drill target is
# an operator decision with real consequences (a same-named restore
# overwrites live data), and this script does not make it for you. Point
# PGDATABASE at whichever database should receive the restored data; create
# it first if it does not already exist.
#
# Usage: restore.sh /backups/sannad-sannad-20260101T000000Z.dump
#
# Required env: PGHOST, PGUSER, PGPASSWORD, PGDATABASE.
# Optional: PGPORT (default 5432).
set -eu

: "${PGHOST:?PGHOST is required}"
: "${PGUSER:?PGUSER is required}"
: "${PGPASSWORD:?PGPASSWORD is required}"
: "${PGDATABASE:?PGDATABASE is required}"
PGPORT="${PGPORT:-5432}"

export PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE

dump="${1:?usage: restore.sh <dump-file>}"
if [ ! -f "$dump" ]; then
    echo "restore: $dump does not exist" >&2
    exit 1
fi

# --clean drops each object before recreating it, so restoring into a
# database that already has these tables (a drill against a copy of
# production, say) replaces them rather than failing on "already exists".
# --if-exists silences the drop's complaint when the table is not there yet
# (a restore into a freshly created, empty database).
pg_restore --clean --if-exists --no-owner --no-privileges \
    -d "$PGDATABASE" "$dump"

echo "restore complete: $dump -> $PGDATABASE"
