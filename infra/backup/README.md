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
| `install_walg.sh` | Checksum-verified WAL-G `v3.0.8` install; wires Postgres `archive_command`/`restore_command` and `archive_timeout=60` for the 60s RPO. |
| `backup.sh` | On-demand WAL-G base backup (also run daily by cron). |
| `restore.sh` | Restore latest base backup + WAL replay (optional PITR via `PITR_TARGET_TIME`). |
| `dr_runbook.sh` | Full DR: reattach floating IP → helm-install → restore → org-scoped reconcile loop (§9.1). |
| `upgrade.sh` | `helm upgrade` with the expand/contract contract, single-pod brief downtime, downgrade-refusal, Kyverno sequenced separately (§9.3). |

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

## Upgrades (§9.3)

`helm upgrade` replaces the single API pod; migrations are embedded and run at
boot under an advisory lock. Expand/contract only — the previous binary must
still read the migrated schema so `helm rollback` works. There is no automatic
schema downgrade; `upgrade.sh` refuses an appVersion downgrade. Kyverno upgrades
are sequenced separately because its fail-closed webhooks pause pod admission
across app namespaces while it restarts.
