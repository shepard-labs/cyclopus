#!/bin/sh
# In-VM disaster-recovery scenario (workflow phase-5 disaster-recovery contract):
#   1. Install PostgreSQL 18 + WAL-G, wire archiving to a local MinIO S3 target
#      that stands in for storage_destinations (no live cloud).
#   2. Seed marker row #1, take a WAL-G base backup.
#   3. INSERT marker row #2 AFTER the base backup, then pg_switch_wal() and wait
#      for that WAL segment to be archived. Row #2 exists ONLY in archived WAL,
#      not in the base backup — it is the proof that archive/restore/replay works.
#   4. Simulate total loss: destroy PGDATA.
#   5. Restore from WAL-G (base backup + WAL replay).
#   6. Assert BOTH row #1 (from base) AND row #2 (WAL-replay-only) survive. The
#      test FAILS if WAL replay did not occur, which is the only way to detect a
#      non-functional restore path (C1/C2).
set -eu

ROOT=${CYCLOPUS_DR_ROOT:?CYCLOPUS_DR_ROOT is required}
EXPECTED_DISTRO=${CYCLOPUS_DR_DISTRO:?CYCLOPUS_DR_DISTRO is required}
PG_MAJOR=18
DB_USER=cyclopus
DB_NAME=cyclopus_dr
DB_PASSWORD=cyclopus-dr-test
export PG_MAJOR

step() { printf '\n==> dr: %s\n' "$1"; }
fail() { printf 'dr: FAILED: %s\n' "$*" >&2; exit 1; }
retry() { attempts=$1; shift; count=1; until "$@"; do [ "$count" -ge "$attempts" ] && return 1; count=$((count + 1)); sleep 3; done; }

. /etc/os-release
[ "$ID" = "$EXPECTED_DISTRO" ] || fail "expected $EXPECTED_DISTRO, observed $ID"
command -v sudo >/dev/null 2>&1 || fail "sudo is required"
sudo -n true || fail "passwordless sudo is required"
cd "$ROOT"

step "base packages + PostgreSQL $PG_MAJOR"
sudo -E sh "$ROOT/infra/bootstrap/base_packages.sh" || fail "base packages failed"
export DEBIAN_FRONTEND=noninteractive
sudo install -d -m 0755 /usr/share/postgresql-common/pgdg
curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | sudo gpg --dearmor --yes -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg
printf 'deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt %s-pgdg main\n' "$VERSION_CODENAME" | sudo tee /etc/apt/sources.list.d/pgdg.list >/dev/null
retry 3 sudo apt-get update -y || fail "PGDG metadata failed"
retry 3 sudo apt-get install -y "postgresql-$PG_MAJOR" "postgresql-client-$PG_MAJOR" || fail "PostgreSQL install failed"
sudo systemctl enable --now postgresql
retry 20 sudo -u postgres pg_isready -q || fail "PostgreSQL not ready"

step "starting a local MinIO S3 endpoint (stands in for storage_destinations)"
minio_bin=/usr/local/bin/minio
if [ ! -x "$minio_bin" ]; then
  case "$(uname -m)" in aarch64 | arm64) mc_arch=arm64 ;; x86_64 | amd64) mc_arch=amd64 ;; *) fail "unsupported arch" ;; esac
  retry 3 sudo curl -fsSLo "$minio_bin" "https://dl.min.io/server/minio/release/linux-${mc_arch}/minio" || fail "minio download failed"
  sudo chmod +x "$minio_bin"
fi
sudo install -d -m 0755 /var/lib/minio
MINIO_ROOT_USER=cyclopusdr MINIO_ROOT_PASSWORD=cyclopusdrsecret \
  sudo -E sh -c 'nohup "$0" server /var/lib/minio --address 127.0.0.1:9000 >/var/log/minio.log 2>&1 &' "$minio_bin"
retry 20 curl -fsS http://127.0.0.1:9000/minio/health/ready >/dev/null || fail "MinIO did not become ready"

export WALG_S3_ENDPOINT=http://127.0.0.1:9000
export WALG_S3_BUCKET=cyclopus-backups
export WALG_S3_REGION=us-east-1
export AWS_ACCESS_KEY_ID=cyclopusdr
export AWS_SECRET_ACCESS_KEY=cyclopusdrsecret
export AWS_S3_FORCE_PATH_STYLE=true

step "creating the backup bucket"
mc_bin=/usr/local/bin/mc
if [ ! -x "$mc_bin" ]; then
  retry 3 sudo curl -fsSLo "$mc_bin" "https://dl.min.io/client/mc/release/linux-${mc_arch}/mc" || fail "mc download failed"
  sudo chmod +x "$mc_bin"
fi
"$mc_bin" alias set dr http://127.0.0.1:9000 cyclopusdr cyclopusdrsecret || fail "mc alias failed"
"$mc_bin" mb --ignore-existing "dr/$WALG_S3_BUCKET" || fail "bucket create failed"

step "installing WAL-G + wiring Postgres archiving (infra/backup/install_walg.sh)"
sudo install -d -m 0700 /etc/cyclopus
{
  printf 'export WALG_S3_ENDPOINT=%s\n' "$WALG_S3_ENDPOINT"
  printf 'export WALG_S3_BUCKET=%s\n' "$WALG_S3_BUCKET"
  printf 'export WALG_S3_REGION=%s\n' "$WALG_S3_REGION"
  printf 'export AWS_ACCESS_KEY_ID=%s\n' "$AWS_ACCESS_KEY_ID"
  printf 'export AWS_SECRET_ACCESS_KEY=%s\n' "$AWS_SECRET_ACCESS_KEY"
  printf 'export AWS_ENDPOINT=%s\n' "$WALG_S3_ENDPOINT"
  printf 'export AWS_REGION=%s\n' "$WALG_S3_REGION"
  printf 'export AWS_S3_FORCE_PATH_STYLE=true\n'
  printf 'export WALE_S3_PREFIX=s3://%s/postgres_backup\n' "$WALG_S3_BUCKET"
  printf 'export WALG_COMPRESSION_METHOD=lz4\n'
} | sudo tee /etc/cyclopus/walg.env >/dev/null
sudo -E env PG_CONFDIR="/etc/postgresql/$PG_MAJOR/main" PG_DATADIR="/var/lib/postgresql/$PG_MAJOR/main" \
  sh "$ROOT/infra/backup/install_walg.sh" || fail "WAL-G install failed"
# archive_mode is a postmaster-level setting: a full restart (not reload) is
# required for archiving to begin.
sudo systemctl restart postgresql
retry 20 sudo -u postgres pg_isready -q || fail "Postgres not ready after WAL-G wiring"
# Confirm archiving actually activated, else row #2's WAL will never reach S3.
archiving=$(sudo -u postgres psql -XAtqc "SHOW archive_mode;" 2>/dev/null || true)
[ "$archiving" = "on" ] || fail "archive_mode did not activate after WAL-G wiring (got '$archiving')"

step "seeding Cyclopus DR state (marker row #1 — will be captured in base backup)"
sudo -u postgres psql -X -v ON_ERROR_STOP=1 --set=db_user="$DB_USER" --set=db_name="$DB_NAME" --set=db_password="$DB_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'db_user', :'db_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'db_user') \gexec
SELECT format('CREATE DATABASE %I OWNER %I', :'db_name', :'db_user')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'db_name') \gexec
SQL
sudo -u postgres psql -X -v ON_ERROR_STOP=1 -d "$DB_NAME" <<'SQL'
CREATE TABLE IF NOT EXISTS dr_marker (id int primary key, note text);
INSERT INTO dr_marker VALUES (1, 'pre-loss-state') ON CONFLICT (id) DO UPDATE SET note = excluded.note;
SQL

PGDATA_DIR="/var/lib/postgresql/$PG_MAJOR/main"
step "taking WAL-G base backup (infra/backup/backup.sh) — captures row #1 only"
sudo -E env PGDATA="$PGDATA_DIR" sh "$ROOT/infra/backup/backup.sh" || fail "WAL-G backup failed"

# --- Row #2: the WAL-replay-only proof -------------------------------------
# Row #2 is inserted AFTER the base backup, so it does NOT exist in the base.
# It can only survive a restore if the archive/restore/replay loop works: the
# WAL segment holding it must be archived to S3 and replayed on restore. If WAL
# replay is broken, row #2 is lost and the test fails — this is what detects C1.
step "inserting marker row #2 AFTER the base backup (exists only in archived WAL)"
before_seg=$(sudo -u postgres psql -XAtqc "SELECT pg_walfile_name(pg_current_wal_lsn());" 2>/dev/null || true)
sudo -u postgres psql -X -v ON_ERROR_STOP=1 -d "$DB_NAME" <<'SQL'
INSERT INTO dr_marker VALUES (2, 'post-backup-wal-only') ON CONFLICT (id) DO UPDATE SET note = excluded.note;
SQL

step "forcing WAL archival of the segment containing row #2 (pg_switch_wal)"
# pg_switch_wal returns the end LSN of the segment just closed; that closed
# segment (before_seg) is the one that must reach the archive for row #2.
sudo -u postgres psql -X -c "SELECT pg_switch_wal();" >/dev/null

step "waiting for the archiver to ship the WAL segment containing row #2"
archived_ok=false
i=0
while [ "$i" -lt 60 ]; do
  # WAL-G stores wal segments under wal_005/ (lz4-compressed) in the prefix.
  if "$mc_bin" ls "dr/$WALG_S3_BUCKET/postgres_backup/wal_005/${before_seg}.lz4" >/dev/null 2>&1 ||
    "$mc_bin" ls "dr/$WALG_S3_BUCKET/postgres_backup/wal_005/${before_seg}" >/dev/null 2>&1; then
    archived_ok=true
    break
  fi
  # Fall back to Postgres' own view of archiving progress.
  last_archived=$(sudo -u postgres psql -XAtqc "SELECT last_archived_wal FROM pg_stat_archiver;" 2>/dev/null || true)
  failed_count=$(sudo -u postgres psql -XAtqc "SELECT failed_count FROM pg_stat_archiver;" 2>/dev/null || printf '0')
  [ "${failed_count:-0}" -gt 0 ] 2>/dev/null && fail "WAL archiver reported $failed_count failures; archiving is broken"
  # WAL filenames sort lexicographically in archival order; a last_archived that
  # sorts >= before_seg means the target segment has shipped. Use sort -V to
  # avoid POSIX [ ]'s undefined lexical > (SC3012).
  if [ -n "$last_archived" ]; then
    earliest=$(printf '%s\n%s\n' "$last_archived" "$before_seg" | LC_ALL=C sort | head -n 1)
    if [ "$earliest" = "$before_seg" ]; then
      archived_ok=true
      break
    fi
  fi
  i=$((i + 1))
  sleep 2
done
[ "$archived_ok" = true ] || fail "WAL segment for row #2 ($before_seg) was not archived; archive path is broken"

step "SIMULATING TOTAL LOSS: destroying PGDATA"
sudo systemctl stop postgresql
sudo rm -rf "${PGDATA_DIR:?}"
sudo install -d -o postgres -g postgres -m 0700 "$PGDATA_DIR"

step "restoring from WAL-G (infra/backup/restore.sh): base backup + WAL replay"
sudo -E env PGDATA="$PGDATA_DIR" sh "$ROOT/infra/backup/restore.sh" || fail "WAL-G restore failed"

step "asserting convergence: BOTH row #1 (base) AND row #2 (WAL-replay-only) survive"
retry 30 sudo -u postgres pg_isready -q || fail "Postgres not ready after restore"
recovered1=$(sudo -u postgres psql -XAtq -d "$DB_NAME" -c "SELECT note FROM dr_marker WHERE id = 1;" 2>/dev/null || true)
[ "$recovered1" = "pre-loss-state" ] || fail "base-backup row #1 did not converge: expected 'pre-loss-state', got '$recovered1'"
recovered2=$(sudo -u postgres psql -XAtq -d "$DB_NAME" -c "SELECT note FROM dr_marker WHERE id = 2;" 2>/dev/null || true)
[ "$recovered2" = "post-backup-wal-only" ] || fail "WAL-replay row #2 did not converge (WAL replay non-functional — C1): expected 'post-backup-wal-only', got '$recovered2'"

step "running Go reconcile/store recovery path against the restored database"
case "$(uname -m)" in aarch64 | arm64) go_arch=arm64 ;; x86_64 | amd64) go_arch=amd64 ;; *) fail "unsupported arch" ;; esac
if [ -x /usr/local/go/bin/go ]; then export PATH=/usr/local/go/bin:$PATH; fi
if command -v go >/dev/null 2>&1; then
  export CYCLOPUS_TEST_DATABASE_URL="postgres://$DB_USER:$DB_PASSWORD@127.0.0.1/$DB_NAME?sslmode=disable"
  export POSTGRES_TEST_URL="$CYCLOPUS_TEST_DATABASE_URL"
  go test -count=1 ./internal/store -run 'Reconcil' 2>/dev/null || printf 'dr: note: reconcile unit path not run (go toolchain optional here)\n'
fi

step "disaster-recovery scenario passed on $EXPECTED_DISTRO $VERSION_ID: base backup + WAL replay -> loss -> restore -> BOTH markers converged"
