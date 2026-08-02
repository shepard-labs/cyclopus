# Cyclopus backup, restore, disaster recovery, and upgrades (§9.1, §9.3)

WAL-G streams Cyclopus's own Postgres to an S3-compatible `storage_destinations`
target. WAL-G restore **is** the recovery path — Postgres sits on VPS1
local-path storage with no replica (§2.2), so this is the only line of defense
for control-plane state. Customer app deployments need no dedicated backup: every
Kubernetes object is regenerable from Postgres via `POST /admin/reconcile` (§9.2)
because the deploy model is imperative with Postgres as source of truth (§3.2).

| Script | Purpose |
|---|---|
| `common.sh` | Shared helpers + WAL-G / storage_destinations env (sourced). |
| `configure_from_db.sh` | Wire everything from the `storage_destinations` row: reads the `postgres_backup` destination + resolves its `credential_secret_ref`, writes `/etc/cyclopus/walg.env`, runs `install_walg.sh`, converges archiving, installs/enables the daily timer. Runs on the Postgres host. |
| `install_walg.sh` | Checksum-verified WAL-G `v3.0.8` install; wires Postgres `archive_command`/`restore_command` and `archive_timeout=60` for the 60s RPO. `WALG_LOCAL_DIR=<dir>` uses a pre-downloaded tarball from `<dir>` (offline install bundle) instead of downloading — the checksum is still verified. |
| `backup.sh` | On-demand WAL-G base backup (also run daily by `cyclopus-walg-backup.timer`). |
| `restore.sh` | Restore latest base backup + WAL replay (optional PITR via `PITR_TARGET_TIME`). |
| `dr_runbook.sh` | Full DR: reattach floating IP → helm-install → restore → org-scoped reconcile loop (§9.1). |
| `upgrade.sh` | `helm upgrade` with the expand/contract contract, single-pod brief downtime, downgrade-refusal, Kyverno sequenced separately (§9.3). |
| `cyclopus-walg-backup.{service,timer}` | systemd units for daily base backups. `@INSTALL_DIR@` placeholder templated by `configure_from_db.sh` / `scripts/install.sh` on install into `/etc/systemd/system/`. |

## RPO / recovery model (§9.1)

- **RPO 60s.** `archive_timeout = 60s` closes and ships a WAL segment at least
  once a minute. Worst-case loss on total node failure is up to 60 seconds of
  control-plane state.
- **Base backups daily**; the restore window is bounded by base-backup retention
  in the storage destination.
- **pgBackRest** is the documented heavier alternative if point-in-time recovery
  needs outgrow WAL-G. **Velero** is not a substitute for Postgres — it captures
  Kubernetes resources/volumes with no native Postgres consistency guarantee; it
  is only an optional, faster secondary path for cluster resources.

## Hostname survival across DR (§9.1)

The ingress address is a **floating IP**, so DR reattaches it to the new node and
every default IP-embedded hostname resolves unchanged, existing
certificates stay valid, and custom domains pointing at the floating IP need no
DNS edit. If the address genuinely cannot be recovered, reconcile regenerates
default hostnames against the new IP, reissues certificates, and emits the full
old→new mapping.

## Credentials (environment only — never hardcoded)

WAL-G reads storage_destinations from the environment, sourced from
`/etc/cyclopus/walg.env` (created by the operator/DR runbook from the object
storage bucket details):

```sh
export WALG_S3_ENDPOINT=https://s3.<region>.<provider>.example
export WALG_S3_BUCKET=cyclopus-backups
export WALG_S3_REGION=<region>
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
```

`/etc/cyclopus/walg.env` uses plain `VAR=value` lines (no `export ` prefix) so
the file is shell-sourceable for the `archive_command`/`restore_command`
(`set -a; . /etc/cyclopus/walg.env`) and for `backup.sh`/`restore.sh`, which
source it themselves — the systemd unit deliberately avoids `EnvironmentFile=`
because unit introspection (`systemctl show`) would expose the S3 secret.
Values must stay within `[A-Za-z0-9/+=:._-]`; `write_walg_env` rejects anything
else so the emitted file always remains sh-sourceable.

## Operator wiring from the DB (configure_from_db.sh)

`configure_from_db.sh` makes a `storage_destinations` row self-wiring on the
Postgres host (needs `sudo -u postgres psql` and `k3s kubectl`):

1. Reads the newest `purpose='postgres_backup'` row (`endpoint`, `bucket`,
   `credential_secret_ref`). No row → exits 64 with instructions (create the
   destination via the Cyclopus API/UI first).
2. Resolves `credential_secret_ref` (`k8s-secret://<namespace>/<name>#<key>`)
   against the k3s API. The referenced Secret **must carry both
   `access_key_id` and `secret_access_key` data keys** (an S3 credential is a
   pair); a missing key fails loudly.
3. Writes `/etc/cyclopus/walg.env` (0600, dir 0700), runs `install_walg.sh`,
   restarts Postgres only if archiving was not already active (otherwise
   reloads), and installs + enables `cyclopus-walg-backup.timer`
   (`OnCalendar=daily`, `Persistent=true`).

Idempotent — re-running converges. `WALG_S3_REGION` (default `us-east-1`) and
`WALG_S3_PREFIX_SUBPATH` (default `postgres_backup`) are env overrides.
The one-line installer stages this toolkit at `/opt/cyclopus/backup/` (plus
`versions.env`) even without S3 env, so this post-install path needs no
network; with `WALG_S3_ENDPOINT` + `WALG_S3_BUCKET` + `AWS_ACCESS_KEY_ID` +
`AWS_SECRET_ACCESS_KEY` all set, the installer wires it zero-touch.

## Upgrades (§9.3)

`helm upgrade` replaces the single API pod; migrations are embedded and run at
boot under an advisory lock. Expand/contract only — the previous binary must
still read the migrated schema so `helm rollback` works. There is no automatic
schema downgrade; `upgrade.sh` refuses an appVersion downgrade. Kyverno upgrades
are sequenced separately because its fail-closed webhooks pause pod admission
across app namespaces while it restarts.
