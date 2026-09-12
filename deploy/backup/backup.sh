#!/bin/sh
# Logical backup of the Sannad Postgres database, via pg_dump's custom format
# (-Fc): compressed, and restorable selectively or in parallel with
# pg_restore, unlike a plain SQL dump.
#
# This is the self-hosted fallback, not the primary recommendation. A managed
# Postgres (RDS, Cloud SQL, etc.) with automated snapshots and point-in-time
# recovery is what production should run on — see
# docs/operations/backup-and-dr.md for why a nightly logical dump alone is not
# enough on its own (it gives you a recovery point, not a recovery *time*: an
# hour of writes since the last dump is an hour lost).
#
# Required env: PGHOST, PGUSER, PGPASSWORD, PGDATABASE.
# Optional: PGPORT (default 5432), BACKUP_DIR (default /backups),
# RETENTION_DAYS (default 14 — set to 0 to disable pruning).
set -eu

: "${PGHOST:?PGHOST is required}"
: "${PGUSER:?PGUSER is required}"
: "${PGPASSWORD:?PGPASSWORD is required}"
: "${PGDATABASE:?PGDATABASE is required}"
PGPORT="${PGPORT:-5432}"
BACKUP_DIR="${BACKUP_DIR:-/backups}"
RETENTION_DAYS="${RETENTION_DAYS:-14}"

export PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE

mkdir -p "$BACKUP_DIR"

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
out="$BACKUP_DIR/sannad-${PGDATABASE}-${timestamp}.dump"
tmp="${out}.partial"

# Written to a .partial name first and renamed only on success, so a backup
# that dies partway through (disk full, connection drop) never leaves
# something that looks like a complete, restorable dump sitting in the
# directory a restore or retention sweep will read from.
pg_dump -Fc --no-owner --no-privileges -f "$tmp" "$PGDATABASE"
mv "$tmp" "$out"

echo "backup written: $out ($(du -h "$out" | cut -f1))"

if [ "$RETENTION_DAYS" -gt 0 ]; then
    find "$BACKUP_DIR" -maxdepth 1 -name "sannad-${PGDATABASE}-*.dump" \
        -mtime "+${RETENTION_DAYS}" -print -delete
fi
