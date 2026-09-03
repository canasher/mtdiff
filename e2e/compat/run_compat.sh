#!/usr/bin/env bash
# Compatibility suite for non-8.0 backends. Usage: run_compat.sh 57|tidb
#
#   57    two MySQL 5.7 containers (docker-compose-57.yml)
#   tidb  MySQL 8.0 source + single-node TiDB destination
#         (docker-compose-tidb.yml)
#
# The seed avoids recursive CTEs (absent on 5.7) and generates rows from
# a one-digit helper table joined against itself, so the same SQL runs on
# both backends. The scenario set covers the high-risk paths on foreign
# backends: core diff/sync, out-of-range deletion (int / composite / NULL
# key), --where and zero-match deletion, keyless full resync, structure
# sync (DDL rendered from the backend's information_schema), and the
# key-drift keyless fallback.
set -euo pipefail
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required"; exit 1
fi
COMPOSE="docker compose"
docker compose version >/dev/null 2>&1 || COMPOSE="docker-compose"

MTDIFF=bin/mtdiff
[ -x "$MTDIFF" ] || { echo "build first: make build"; exit 1; }
CLIENT=mysql:8.0
BACKEND=${1:?usage: run_compat.sh 57|tidb}
# per-backend report file: the 5.7 and TiDB suites can run in parallel
# (distinct compose projects), and a shared file would race — one suite's
# expect() truncating the other's report mid-scenario
OUT=/tmp/mtdiff-compat-$BACKEND.out
EXTRA=  # per-backend extra mtdiff flags (unset backends: none)

if [ "$BACKEND" = "57" ]; then
  COMP=e2e/compat/docker-compose-57.yml
  PROJ=compat57
  SRC=root:rootpw@127.0.0.1:15306/srcdb
  DST=root:rootpw@127.0.0.1:15307/dstdb
  SRC_P=15306
  DST_P=15307
  SRC_PWD=rootpw
  DST_PWD=rootpw
else
  COMP=e2e/compat/docker-compose-tidb.yml
  PROJ=compat-tidb
  SRC=root:rootpw@127.0.0.1:15308/srcdb
  # root:@ = explicit empty password (TiDB's root has none); without the
  # colon a non-interactive run would try to prompt and fail.
  DST=root:@127.0.0.1:15309/dstdb
  SRC_P=15308
  DST_P=15309
  SRC_PWD=rootpw
  DST_PWD=
  # TiDB cannot enforce a session read-only (read_only is GLOBAL-only;
  # SET SESSION TRANSACTION READ ONLY is a disabled no-op), so mtdiff
  # refuses the connection by default. Relax explicitly for the suite:
  # read connections still issue SELECTs only.
  EXTRA="--allow-unenforced-readonly"
fi
# Distinct compose project names so the 5.7 and TiDB suites can run in
# parallel (same directory would otherwise share one project and each
# "up" would orphan the other's services).
c() { COMPOSE_PROJECT_NAME="$PROJ" $COMPOSE -f "$COMP" "$@"; }

say()  { printf '\n== %s\n' "$*"; }

# sql <port> <pwd> <db> <file> runs a seed/mutate script against a backend
# via a throwaway client container (the 5.7 and TiDB images do not both
# ship a mysql client). A failed statement aborts the suite with context;
# the first run right after container start can hit a transient socket-down
# window, so retry (seeds/mutations are idempotent).
sql() {
  local port="$1" pwd="$2" db="$3" file="$4" out attempt
  for attempt in 1 2 3; do
    if out=$(timeout 600 docker run --rm -i --network host -e MYSQL_PWD="$pwd" "$CLIENT" \
        mysql -h127.0.0.1 -P"$port" -uroot -D "$db" < "e2e/compat/$file" 2>&1); then
      break
    fi
    if [ "$attempt" = 3 ]; then
      echo "FAIL: script $file (port $port, db $db) failed:"
      echo "$out"
      exit 1
    fi
    sleep 5
  done
  if [ -n "$out" ]; then
    echo "$out"
  fi
  return 0
}

# qdst <sql> / qsrc <sql> run a query machine-readable (-N -B).
qsrc() { timeout 60 docker run --rm -i --network host -e MYSQL_PWD="$SRC_PWD" "$CLIENT" \
  mysql -h127.0.0.1 -P"$SRC_P" -uroot -D srcdb -N -B -e "$1"; }
qdst() { timeout 60 docker run --rm -i --network host -e MYSQL_PWD="$DST_PWD" "$CLIENT" \
  mysql -h127.0.0.1 -P"$DST_P" -uroot -D dstdb -N -B -e "$1"; }

wait_ready() { # wait_ready <port> <pwd>
  # A hung docker run (daemon under load, e.g. a parallel suite) must not
  # wedge the wait loop: cap every probe.
  for _ in $(seq 1 150); do
    if timeout 30 docker run --rm -i --network host -e MYSQL_PWD="$2" "$CLIENT" \
        mysql -h127.0.0.1 -P"$1" -uroot -e 'SELECT 1' >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "timeout waiting for $BACKEND backend on port $1"; exit 1
}

# expect <want-exit> <description> <mtdiff args...>
expect() {
  local want="$1"; shift
  local desc="$1"; shift
  set +e
  timeout 600 "$MTDIFF" $EXTRA "$@" > "$OUT" 2>&1
  local rc=$?
  set -e
  if [ "$rc" -ne "$want" ]; then
    if [ "$rc" -eq 124 ]; then
      echo "FAIL [$desc]: TIMEOUT (mtdiff did not finish within 600s)"
    else
      echo "FAIL [$desc]: exit $rc, want $want"
    fi
    cat "$OUT"
    exit 1
  fi
  echo "ok: $desc (exit $rc)"
}

check() { # check <actual> <want> <description>
  if [ "$1" != "$2" ]; then
    echo "FAIL: $3 (got [$1], want [$2])"; exit 1
  fi
  echo "ok: $3"
}

c up -d
wait_ready "$SRC_P" "$SRC_PWD"
wait_ready "$DST_P" "$DST_PWD"
if [ "$BACKEND" = "tidb" ]; then
  # TiDB ships no databases; the 5.7/8.0 entrypoints create theirs.
  timeout 60 docker run --rm -i --network host -e MYSQL_PWD="$DST_PWD" "$CLIENT" \
    mysql -h127.0.0.1 -P"$DST_P" -uroot -e 'CREATE DATABASE IF NOT EXISTS dstdb'
fi

cleanup() {
  if [ "${E2E_OK:-0}" = "1" ]; then
    c down -v --remove-orphans >/dev/null 2>&1 || true
  else
    echo ""
    echo "compat suite failed — containers LEFT RUNNING for inspection."
    c ps
  fi
}
trap cleanup EXIT

say "$BACKEND versions"
echo "src: $(qsrc 'SELECT VERSION()')"
echo "dst: $(qdst 'SELECT VERSION()')"

sql "$SRC_P" "$SRC_PWD" srcdb seed_compat.sql
sql "$DST_P" "$DST_PWD" dstdb seed_compat.sql

say "compat baseline"
expect 0 "baseline: all tables identical" --src "$SRC" --dst "$DST"
expect 0 "tables subcommand" tables --src "$SRC" --dst "$DST"
# A structurally identical table must show no DDL: a false positive here
# means the backend renders column types differently from the 8.0 source.
expect 0 "identical table: no DDL planned" sync --src "$SRC" --dst "$DST" --tables t_compat
if grep -q "ALTER TABLE" "$OUT"; then
  echo "FAIL: DDL shown for an identical table (type rendering incompatible?)"; cat "$OUT"; exit 1
fi
echo "ok: no DDL for an identical table"

say "compat row-level sync"
sql "$DST_P" "$DST_PWD" dstdb m_rowlevel.sql
expect 1 "row-level dry-run" sync --src "$SRC" --dst "$DST" --tables t_compat
expect 0 "row-level --apply --yes" sync --src "$SRC" --dst "$DST" --tables t_compat --apply --yes
expect 0 "diff identical after row-level sync" --src "$SRC" --dst "$DST" --tables t_compat
check "$(qdst "SELECT COUNT(*) FROM t_compat WHERE id IN (7, 17, 27)")" 3 \
  "deleted rows restored"
check "$(qdst "SELECT amt FROM t_compat WHERE id = 47")" "70.50" \
  "changed row restored to source value"
check "$(qdst "SELECT COUNT(*) FROM t_compat WHERE id IN (1001, 1002)")" 0 \
  "added out-of-range rows deleted (OOR cleanup)"

say "compat --where"
# A filter matching everything: the filtered sync and diff must behave
# like the unfiltered ones on a clean table.
expect 0 "filtered sync (all rows match)" sync --src "$SRC" --dst "$DST" --tables t_compat --where "id >= 1" --apply --yes
expect 0 "filtered diff (all rows match)" --src "$SRC" --dst "$DST" --tables t_compat --where "id >= 1"
# Zero-match filter with matching dst rows: the destination-delete path
# removes them (a filtered table can never be truncated).
sql "$DST_P" "$DST_PWD" dstdb m_compat_zero.sql
expect 1 "zero-match dry-run plans dst deletes" sync --src "$SRC" --dst "$DST" --tables t_compat --where "id >= 1000000"
expect 0 "zero-match --apply --yes" sync --src "$SRC" --dst "$DST" --tables t_compat --where "id >= 1000000" --apply --yes
expect 0 "filtered diff after zero-match sync" --src "$SRC" --dst "$DST" --tables t_compat --where "id >= 1000000"
expect 0 "unfiltered diff after zero-match sync" --src "$SRC" --dst "$DST" --tables t_compat
check "$(qdst "SELECT COUNT(*) FROM t_compat WHERE id >= 1000000")" 0 \
  "zero-match dst rows deleted"

say "compat out-of-range sync"
sql "$DST_P" "$DST_PWD" dstdb m_oor.sql
expect 1 "oor dry-run (int key)" sync --src "$SRC" --dst "$DST" --tables t_oor
if ! grep -q 'DELETE FROM `t_oor`' "$OUT"; then
  echo "FAIL: oor dry-run showed no DELETE sample"; cat "$OUT"; exit 1
fi
echo "ok: oor dry-run shows a DELETE sample"
expect 0 "oor --apply --yes (int key)" sync --src "$SRC" --dst "$DST" --tables t_oor --apply --yes
expect 0 "diff identical after oor sync" --src "$SRC" --dst "$DST" --tables t_oor
check "$(qdst "SELECT COUNT(*) FROM t_oor WHERE id = 0 OR id > 100")" 0 \
  "out-of-range rows deleted"
check "$(qdst "SELECT COUNT(*) FROM t_oor WHERE id IN (5, 15, 25, 35)")" 4 \
  "deleted rows restored"
check "$(qdst "SELECT val FROM t_oor WHERE id = 65")" "v65" \
  "changed row restored"
check "$(qdst "SELECT COUNT(*) FROM t_oor")" 100 "row count back to 100"

sql "$DST_P" "$DST_PWD" dstdb m_oorc.sql
expect 1 "oor dry-run (composite key)" sync --src "$SRC" --dst "$DST" --tables t_oorc
expect 0 "oor --apply --yes (composite key)" sync --src "$SRC" --dst "$DST" --tables t_oorc --apply --yes
expect 0 "diff identical after composite oor sync" --src "$SRC" --dst "$DST" --tables t_oorc
check "$(qdst "SELECT COUNT(*) FROM t_oorc WHERE NOT (a BETWEEN 1 AND 10 AND b BETWEEN 1 AND 3)")" 0 \
  "composite out-of-range rows (incl. boundary probes) deleted"
check "$(qdst "SELECT COUNT(*) FROM t_oorc WHERE (a, b) IN ((1, 1), (10, 3))")" 2 \
  "boundary rows (1,1) and (10,3) kept"
check "$(qdst "SELECT val FROM t_oorc WHERE a = 7 AND b = 2")" "c7-2" \
  "changed row restored"
check "$(qdst "SELECT COUNT(*) FROM t_oorc")" 30 "row count back to 30"

sql "$DST_P" "$DST_PWD" dstdb m_oorn.sql
expect 1 "oor dry-run (explicit nullable key)" sync --src "$SRC" --dst "$DST" --tables t_oorn --key a
expect 0 "oor --apply --yes --key a" sync --src "$SRC" --dst "$DST" --tables t_oorn --key a --apply --yes
expect 0 "diff identical after NULL-key oor sync" --src "$SRC" --dst "$DST" --tables t_oorn --key a
check "$(qdst "SELECT COUNT(*) FROM t_oorn WHERE a IS NULL")" 2 \
  "duplicate NULL-key rows kept"
check "$(qdst "SELECT COUNT(*) FROM t_oorn WHERE a = 5")" 0 \
  "out-of-range row (a=5) deleted"
check "$(qdst "SELECT COUNT(*) FROM t_oorn WHERE a = 2 AND v = 'y'")" 1 \
  "missing row (2, 'y') restored"

say "compat keyless"
sql "$DST_P" "$DST_PWD" dstdb m_nopk.sql
expect 1 "keyless pair: diff differs" --src "$SRC" --dst "$DST" --tables t_nopk
expect 1 "keyless dry-run: full resync" sync --src "$SRC" --dst "$DST" --tables t_nopk
if ! grep -q 'TRUNCATE TABLE `t_nopk`' "$OUT"; then
  echo "FAIL: keyless dry-run showed no TRUNCATE sample"; cat "$OUT"; exit 1
fi
echo "ok: keyless dry-run shows the TRUNCATE sample"
expect 0 "keyless --apply --yes" sync --src "$SRC" --dst "$DST" --tables t_nopk --apply --yes
expect 0 "diff identical after keyless resync" --src "$SRC" --dst "$DST" --tables t_nopk
check "$(qdst "SELECT COUNT(*) FROM t_nopk WHERE id IN (3, 11)")" 2 \
  "deleted rows restored"
check "$(qdst "SELECT COUNT(*) FROM t_nopk WHERE id IN (21, 22)")" 0 \
  "extra rows dropped"
check "$(qdst "SELECT COUNT(*) FROM t_nopk")" 20 "row count back to 20"

say "compat structure sync"
sql "$DST_P" "$DST_PWD" dstdb m_struct_drift.sql
expect 1 "structure drift: DDL planned (dry-run)" sync --src "$SRC" --dst "$DST" --tables t_struct
if ! grep -q 'ALTER TABLE `t_struct`' "$OUT"; then
  echo "FAIL: dry-run showed no structure DDL"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows the structure DDL"
expect 0 "structure drift --apply: aligned + resynced" sync --src "$SRC" --dst "$DST" --tables t_struct --apply --yes
expect 0 "diff identical after structure sync" --src "$SRC" --dst "$DST" --tables t_struct
cols=$(qdst "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 't_struct' ORDER BY ORDINAL_POSITION")
check "$(echo "$cols" | tr '\n' ' ' | sed 's/ $//')" "id name amt ts" \
  "dst column set and order aligned"
check "$(qdst "SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 't_struct' AND COLUMN_NAME = 'id'")" "int" \
  "dst id type restored to int"
check "$(qdst "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 't_struct' AND INDEX_NAME = 'PRIMARY'")" 1 \
  "dst PRIMARY key restored"

say "compat key drift"
sql "$DST_P" "$DST_PWD" dstdb m_keyless_drift.sql
expect 0 "keyless dst: diff falls back to keyless comparison (identical)" \
  --src "$SRC" --dst "$DST" --tables t_keyless
if ! grep -q "warn t_keyless" "$OUT"; then
  echo "FAIL: the keyless fallback must be announced in the report"; cat "$OUT"; exit 1
fi
echo "ok: keyless fallback announced in the report"
sql "$DST_P" "$DST_PWD" dstdb m_keyless_data.sql
expect 1 "keyless dst: diff reports the data drift" --src "$SRC" --dst "$DST" --tables t_keyless
expect 1 "keyless dst: dry-run plans the full resync" sync --src "$SRC" --dst "$DST" --tables t_keyless --no-sync-schema
if ! grep -q 'TRUNCATE TABLE `t_keyless`' "$OUT"; then
  echo "FAIL: keyless dst dry-run showed no TRUNCATE sample"; cat "$OUT"; exit 1
fi
echo "ok: keyless dst dry-run shows the TRUNCATE sample"
expect 3 "keyless dst with --where: cannot sync" sync --src "$SRC" --dst "$DST" --tables t_keyless --no-sync-schema --where "id >= 1"
expect 0 "keyless dst: full resync applied" sync --src "$SRC" --dst "$DST" --tables t_keyless --no-sync-schema --apply --yes
expect 0 "diff identical after keyless-dst resync" --src "$SRC" --dst "$DST" --tables t_keyless
check "$(qdst "SELECT COUNT(*) FROM t_keyless WHERE id IN (7, 17, 27)")" 3 \
  "resync restored the deleted rows"
check "$(qdst "SELECT val FROM t_keyless WHERE id = 47")" "v47" \
  "resync restored the changed row"
check "$(qdst "SELECT COUNT(*) FROM t_keyless WHERE id IN (51, 52, 53)")" 0 \
  "resync dropped the extra rows"
sql "$DST_P" "$DST_PWD" dstdb m_keyless_drift.sql
sql "$DST_P" "$DST_PWD" dstdb m_keyless_data.sql
expect 1 "key drift: default sync plans the key DDL" sync --src "$SRC" --dst "$DST" --tables t_keyless
if ! grep -q "ADD PRIMARY KEY" "$OUT"; then
  echo "FAIL: default sync must show the ADD PRIMARY KEY DDL"; cat "$OUT"; exit 1
fi
echo "ok: default sync shows the key DDL"
expect 0 "key drift: default sync --apply" sync --src "$SRC" --dst "$DST" --tables t_keyless --apply --yes
expect 0 "diff identical after default sync" --src "$SRC" --dst "$DST" --tables t_keyless
check "$(qdst "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 't_keyless' AND INDEX_NAME = 'PRIMARY'")" 1 \
  "dst PRIMARY key restored by default sync"

say "compat whole-database sync (create / drop / state)"
# Whole-database mode (no --tables): the expected set is the source's BASE
# TABLE set; a table the destination lacks is created, a table the source
# lacks is dropped.
expect 0 "whole-DB sync: nothing to do (identical table set)" sync --src "$SRC" --dst "$DST"
# (a) a source-only table is created on the destination.
sql "$SRC_P" "$SRC_PWD" srcdb m_c2_src.sql
expect 1 "source-only table: create planned (dry-run)" sync --src "$SRC" --dst "$DST"
if ! grep -q 'CREATE TABLE `t_c2`' "$OUT"; then
  echo "FAIL: no CREATE TABLE for the missing table"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows the CREATE TABLE"
check "$(qdst "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb' AND TABLE_NAME='t_c2'")" 0 \
  "the create dry-run is zero-write"
expect 0 "source-only table: --apply creates it" sync --src "$SRC" --dst "$DST" --apply --yes
check "$(qdst "SELECT COUNT(*) FROM t_c2")" 10 "created table carries the source data"
expect 0 "re-run after create: nothing to do" sync --src "$SRC" --dst "$DST"
# (b) a destination-only table is dropped (destructive, listed separately).
sql "$DST_P" "$DST_PWD" dstdb m_c_extra.sql
expect 1 "destination-only table: DROP planned (dry-run)" sync --src "$SRC" --dst "$DST"
if ! grep -q 'DROP TABLE IF EXISTS `t_c_extra`' "$OUT"; then
  echo "FAIL: no DROP TABLE for the extra table"; cat "$OUT"; exit 1
fi
echo "ok: dry-run plans the DROP TABLE"
if ! grep -q 'DESTRUCTIVE' "$OUT"; then
  echo "FAIL: the destructive change is not listed separately"; cat "$OUT"; exit 1
fi
echo "ok: the destructive change is listed separately"
check "$(qdst "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb' AND TABLE_NAME='t_c_extra'")" 1 \
  "the drop dry-run is zero-write"
expect 0 "destination-only table: --apply drops it" sync --src "$SRC" --dst "$DST" --apply --yes
check "$(qdst "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb' AND TABLE_NAME='t_c_extra'")" 0 \
  "extra table dropped"
expect 0 "re-run after drop: nothing to do" sync --src "$SRC" --dst "$DST"
# (c) --tables never drops out-of-scope tables.
sql "$DST_P" "$DST_PWD" dstdb m_c_extra.sql
expect 0 "--tables --apply: out-of-scope table survives" sync --src "$SRC" --dst "$DST" --tables t_compat --apply --yes
check "$(qdst "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb' AND TABLE_NAME='t_c_extra'")" 1 \
  "--tables spares out-of-scope tables"
# (d) table state (AUTO_INCREMENT) convergence, capability-gated: the
# seed pins both sides' counters to a common explicit base, the source
# counter is then pushed to 5001, and the destination's must be raised
# to match (a plain raise on every backend). Two capability gates mirror
# the binary's own probes: a backend whose information_schema lacks a
# readable AUTO_INCREMENT column degrades to a skipped reconciliation,
# and a backend that pre-allocates ID ranges (TiDB's batch allocator:
# the reported next value sits tens of thousands above max(id), and an
# explicit value below the allocated range's end is silently ignored)
# degrades the same way — both are one-shot warnings, never a failure.
# The scenario is skipped, not faked.
sql "$SRC_P" "$SRC_PWD" srcdb m_cai_src.sql
set +e
ai_is=$(qdst "SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb' AND TABLE_NAME='t_cai'" 2>/dev/null)
ai_sh=$(qdst "SHOW CREATE TABLE t_cai\G" 2>/dev/null | grep -oE 'AUTO_INCREMENT=[0-9]+' | head -n1 | cut -d= -f2)
mx=$(qdst "SELECT COALESCE(MAX(id), 0) + 1 FROM t_cai" 2>/dev/null)
set -e
ai_val=$ai_sh
[ -n "$ai_val" ] || ai_val=$ai_is
if [ -z "$ai_is" ]; then
  echo "skip: backend information_schema has no readable AUTO_INCREMENT (state reconciliation degrades to skipped)"
  expect 0 "state unreadable: the sync still runs clean" sync --src "$SRC" --dst "$DST" --apply --yes
elif [ -n "$ai_val" ] && [ -n "$mx" ] && [ $((ai_val - mx)) -gt 10000 ]; then
  echo "skip: backend pre-allocates auto-increment ID ranges (reported next value $ai_val vs data max+1 $mx); state reconciliation degrades to skipped"
  expect 0 "state inexact: the sync still runs clean" sync --src "$SRC" --dst "$DST" --apply --yes
else
  expect 1 "state drift: AUTO_INCREMENT planned (dry-run)" sync --src "$SRC" --dst "$DST"
  if ! grep -q 'ALTER TABLE `t_cai` AUTO_INCREMENT = ' "$OUT"; then
    echo "FAIL: no AUTO_INCREMENT alignment planned"; cat "$OUT"; exit 1
  fi
  echo "ok: dry-run shows the AUTO_INCREMENT alignment"
  expect 0 "state drift: --apply realigns the counter" sync --src "$SRC" --dst "$DST" --apply --yes
  # read the true value (the SHOW CREATE clause): InnoDB's information_schema
  # estimate is not refreshed after a second counter change, so assert on
  # the value the server actually persists.
  check "$(qdst "SHOW CREATE TABLE t_cai\G" | grep -oE 'AUTO_INCREMENT=[0-9]+' | head -n1 | cut -d= -f2)" 5001 \
    "t_cai AUTO_INCREMENT realigned to the source's 5001"
fi

E2E_OK=1
say "ALL $BACKEND COMPAT SCENARIOS PASSED"
