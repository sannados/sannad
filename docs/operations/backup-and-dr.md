# Backup and disaster recovery

This document names what actually exists, what it protects against, and what it does not —
the same honesty standard the rest of this repository's docs hold to. There is no Kubernetes
or Helm manifest here, and that omission is deliberate: writing one without a real cluster to
validate it against would be aspirational YAML nobody has run, which is worse than not having
it — it reads as tested when it is not. Everything below has actually been exercised.

## The primary recommendation: managed Postgres

For any deployment with a real recovery-time requirement, run Postgres as a managed service
(RDS, Cloud SQL, Azure Database for PostgreSQL, or equivalent) with:

- **Automated snapshots**, daily at minimum.
- **Point-in-time recovery (PITR)** via WAL archiving, so a restore target is "5 minutes before
  the bad migration ran," not "whenever last night's snapshot happened to run."
- **Cross-region replication** if the deployment's uptime commitment requires surviving a
  regional outage, not only a disk failure.

This is infrastructure configuration on whichever provider is chosen, not something this
repository can ship — every provider's PITR setup is different, and a generic Terraform module
guessing at one provider's API would be exactly the kind of untested artifact this document
is refusing to write. Configure it through the provider directly, and note the choice — engine
version, snapshot retention window, PITR retention window — somewhere your own operational
runbook lives.

## The self-hosted fallback: logical backups

`deploy/docker-compose/docker-compose.yml` runs a real Postgres container, not a managed one,
so it needs its own answer. `deploy/backup/backup.sh` and `deploy/backup/restore.sh` are that
answer: `pg_dump`/`pg_restore` wrappers, added as a `postgres-backup` sidecar service that runs
nightly and prunes anything older than `BACKUP_RETENTION_DAYS` (default 14).

**What a logical backup gives you: a recovery point, not a recovery time.** The gap between
"data as of last night's dump" and "data as of the moment before the incident" is real lost
work — everything written since the last successful dump. A logical dump is what this compose
file can offer without operating WAL archiving itself; it is not equivalent to PITR, and a
deployment that has outgrown "acceptable to lose up to a day of writes" has outgrown this
fallback and belongs on a managed Postgres instead.

### What was actually tested

Both scripts were run end to end against a real, disposable PostgreSQL 18 instance (not the
project's usual SQLite test path — a backup/restore drill has to prove something about the
actual production database engine): a table was created and seeded, `backup.sh` produced a
dump, the table was dropped entirely to simulate real data loss, `restore.sh` restored from
that dump into the same database, and the original rows were confirmed present and correct
afterward. Retention pruning was verified separately: a dump backdated 20 days old was removed
by a run with `RETENTION_DAYS=14` while a fresh dump from the same run was kept.

### Running the drill yourself

Do this against a real deployment before relying on it — a backup nobody has restored from is
a belief, not a plan.

```sh
# Take a backup.
docker compose exec postgres-backup sh /scripts/backup.sh

# List what's in the volume.
docker compose exec postgres-backup ls -la /backups

# Restore into a scratch database, never directly over the live one.
docker compose exec postgres createdb -U "$POSTGRES_USER" sannad_restore_drill
docker compose exec -e PGDATABASE=sannad_restore_drill postgres-backup \
  sh /scripts/restore.sh /backups/<the-dump-file>

# Verify, then drop the scratch database.
docker compose exec postgres psql -U "$POSTGRES_USER" -d sannad_restore_drill -c '\dt'
docker compose exec postgres dropdb -U "$POSTGRES_USER" sannad_restore_drill
```

Restoring into a same-named database is what an actual incident calls for, and `restore.sh`
supports it (point `PGDATABASE` at it) — but a drill should never risk live data to prove the
tooling works, which is why the scratch-database path above is the one to practice.

## Migration rollback is not a backup strategy

Every migration in `internal/app/migrations/` carries a goose `-- +goose Down` block, but
treat those as a convenience for local development, not a production safety net. A down
migration reverses schema, not data: dropping a column a forward migration added does not
restore whatever was in it if the forward migration also transformed or discarded data on the
way in (the rebuild-and-copy migrations — `00003_account_tenancy.sql`, `00011`'s `audit_events`
rebuild — are exactly this shape). If a migration has already run against production and
something is wrong, the safe response is a database restore from a backup taken before it ran,
not a down migration — the down block is there so a developer can iterate locally, not so an
operator can undo a bad production deploy.

## Secrets are not in this backup

`deploy/backup/backup.sh` backs up the database; it does not back up `SANNAD_SESSION_SECRET`,
TLS certificates, or anything else supplied only as an environment variable at deploy time. A
lost `SANNAD_SESSION_SECRET` with no backup of your own is not a data-recovery problem the
database backup solves — see the session-secret-rotation entry in
`docs/architecture/roadmap.md` for what changing it does and does not force. Keep deployment
secrets in whatever secret store or password manager your own operations already use; this
document is about the database only.
