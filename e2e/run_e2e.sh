#!/usr/bin/env bash
# E2E suite: two MySQL 8.0 containers with different system time zones,
# seeded with type-trap tables; asserts the exit code of every scenario.
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required"; exit 1
fi
COMPOSE="docker compose"
docker compose version >/dev/null 2>&1 || COMPOSE="docker-compose"

MTDIFF=bin/mtdiff
[ -x "$MTDIFF" ] || { echo "build first: make build"; exit 1; }
SRC=root:rootpw@127.0.0.1:13306/srcdb
DST=root:rootpw@127.0.0.1:13307/dstdb
OUT=/tmp/mtdiff-e2e.out

say()  { printf '\n== %s\n' "$*"; }
sql() {
  # Run a seed/mutate script; a failed statement aborts the suite with context.
  # MYSQL_PWD keeps the password off the command line (no client warning) and
  # keeps us out of a grep pipeline (grep -v exits 1 on empty output, which
  # pipefail would treat as failure for silent DDL seeds).
  # The first exec right after container start can hit a transient
  # socket-down window even after the readiness probe passed, so retry
  # before failing (seeds are idempotent: DROP TABLE IF EXISTS + reseed).
  local out attempt
  for attempt in 1 2 3; do
    if out=$($COMPOSE -f e2e/docker-compose.yml exec -T -e MYSQL_PWD=rootpw "mysql-$1" mysql -uroot -D "$2" < "e2e/$3" 2>&1); then
      break
    fi
    if [ "$attempt" = 3 ]; then
      echo "FAIL: mysql script $3 on $1 ($2) failed:"
      echo "$out"
      exit 1
    fi
    sleep 5
  done
  if [ -n "$out" ]; then
    echo "$out"
  fi
  # A silent (DDL-only) seed is success; end on an explicit 0 so the
  # empty-output check above can't make the function return 1 under set -e.
  return 0
}
# qdst <sql> runs a query on the dst database and returns the result
# machine-readable (-N no headers, -B tab/line separated).
qdst() {
  $COMPOSE -f e2e/docker-compose.yml exec -T -e MYSQL_PWD=rootpw mysql-dst \
    mysql -uroot -D dstdb -N -B -e "$1"
}

wait_ready() {
  local svc="mysql-$1"
  # Probe with a real authenticated query (mysqladmin ping exits 0 even on
  # access-denied, so it would not prove the root password works).
  for _ in $(seq 1 150); do
    if $COMPOSE -f e2e/docker-compose.yml exec -T "$svc" mysql -uroot -prootpw -e 'SELECT 1' >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "timeout waiting for $1 (auth probe failed)"; exit 1
}

# expect <want-exit> <description> <mtdiff args...>
# timeout guards against a hung mtdiff (exit 124 = timeout, reported as FAIL).
expect() {
  local want="$1"; shift
  local desc="$1"; shift
  set +e
  timeout 600 "$MTDIFF" "$@" > "$OUT" 2>&1
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

cleanup() {
  if [ "${E2E_OK:-0}" = "1" ]; then
    $COMPOSE -f e2e/docker-compose.yml down -v --remove-orphans >/dev/null 2>&1 || true
  else
    echo ""
    echo "E2E failed — containers LEFT RUNNING for inspection."
    echo "  inspect: docker logs e2e-mysql-src-1 --tail 50"
    echo "  clean:   docker compose -f e2e/docker-compose.yml down -v --remove-orphans"
  fi
}
trap cleanup EXIT

say "starting containers"
$COMPOSE -f e2e/docker-compose.yml up -d
wait_ready src
wait_ready dst

say "seeding"
sql src srcdb seed_common.sql
sql src srcdb seed_src.sql
sql dst dstdb seed_common.sql
sql dst dstdb seed_dst.sql

say "consistency"
expect 0 "identical clean tables" \
  --src "$SRC" --dst "$DST" \
  --tables t_large,t_nopk,t_mut,t_ignore,tz_ts,t_dec,t_char,t_bit,t_null,t_enum
expect 1 "tz: DATETIME wall clock differs" --src "$SRC" --dst "$DST" --tables tz_dt
expect 2 "DATETIME/TIMESTAMP swap rejected by default" --src "$SRC" --dst "$DST" --tables dt_ts_swap
expect 0 "swap accepted with --allow-tz-swap" --src "$SRC" --dst "$DST" --tables dt_ts_swap --allow-tz-swap
expect 1 "float bit-diff (exact mode)" --src "$SRC" --dst "$DST" --tables t_float_small
expect 0 "float within --tolerance" --src "$SRC" --dst "$DST" --tables t_float_small --tolerance 1e-9
expect 1 "float 0.01 apart (exact)" --src "$SRC" --dst "$DST" --tables t_float_big
expect 1 "float 0.01 apart (tolerance 1e-9)" --src "$SRC" --dst "$DST" --tables t_float_big --tolerance 1e-9
expect 1 "JSON text differs raw" --src "$SRC" --dst "$DST" --tables t_json
expect 0 "JSON text equal after --normalize-json" --src "$SRC" --dst "$DST" --tables t_json --normalize-json
expect 1 "NULL vs empty string" --src "$SRC" --dst "$DST" --tables t_nulltrap
expect 1 "enum value differs" --src "$SRC" --dst "$DST" --tables t_enumtrap

say "P1 review regressions"
# t_chunk spans id 1..90001; at --chunk-size 10000 the span is divisible by
# the chunk count, the shape where the old intBoundaries off-by-one skipped
# the max-id row on both sides (silent false "identical").
expect 0 "identical divisible-span table" --src "$SRC" --dst "$DST" --tables t_chunk --chunk-size 10000
sql dst dstdb m_chunk_max.sql
expect 1 "divisible span: max-id row changed" --src "$SRC" --dst "$DST" --tables t_chunk --chunk-size 10000
# t_chunkc: composite PK (a, b), 30001 rows; the span is divisible by the
# chunk count at --chunk-size 10000 — regression shape for arithmetic
# split on the integer lead column (P3-#15).
expect 0 "identical composite lead-int key table" --src "$SRC" --dst "$DST" --tables t_chunkc --chunk-size 10000
sql dst dstdb m_chunkc_change.sql
expect 1 "composite lead-int: max lead row changed" --src "$SRC" --dst "$DST" --tables t_chunkc --chunk-size 10000
# t_nullkey: no PK, UNIQUE-but-NULLABLE key column. Auto key selection must
# reject it (keyless multiset); the old code chunked on it and the NULL-key
# row fell out of every predicate, so a change to it was missed.
expect 0 "identical nullable-unique-key table (keyless)" --src "$SRC" --dst "$DST" --tables t_nullkey
sql dst dstdb m_nullkey_change.sql
expect 1 "nullable-unique-key: NULL row changed" --src "$SRC" --dst "$DST" --tables t_nullkey
# t_fracsec: 100ms vs 10ms; the old trailing-zero strip rendered both ".1".
expect 0 "identical fractional-second table" --src "$SRC" --dst "$DST" --tables t_fracsec
sql dst dstdb m_fracsec_change.sql
expect 1 "fractional seconds: 0.1 vs 0.01 differ" --src "$SRC" --dst "$DST" --tables t_fracsec

say "where / mutations on t_mut"
sql dst dstdb m_where.sql
expect 1 "changed row detected" --src "$SRC" --dst "$DST" --tables t_mut
expect 0 "--where excludes the change" --src "$SRC" --dst "$DST" --tables t_mut --where "id < 100"

sql dst dstdb m_mut_reseed.sql
sql dst dstdb m_update.sql
expect 1 "3 rows updated" --src "$SRC" --dst "$DST" --tables t_mut
expect 1 "--drill lists changed rows" --src "$SRC" --dst "$DST" --tables t_mut --drill
if ! grep -q "CHANGED" "$OUT"; then
  echo "FAIL: --drill produced no CHANGED rows"; cat "$OUT"; exit 1
fi
echo "ok: --drill rows shown"

sql dst dstdb m_mut_reseed.sql
sql dst dstdb m_delete.sql
expect 1 "row deleted (count differs)" --src "$SRC" --dst "$DST" --tables t_mut

sql dst dstdb m_mut_reseed.sql
sql dst dstdb m_insert.sql
expect 1 "row inserted (count differs)" --src "$SRC" --dst "$DST" --tables t_mut

say "ignore-columns"
sql dst dstdb m_ignore_shift.sql
expect 1 "updated_at shifted" --src "$SRC" --dst "$DST" --tables t_ignore
expect 0 "updated_at hidden by --ignore-columns" --src "$SRC" --dst "$DST" --tables t_ignore --ignore-columns updated_at

say "keyless table semantics"
sql dst dstdb m_nopk_reorder.sql
expect 0 "keyless: reordered multiset equal" --src "$SRC" --dst "$DST" --tables t_nopk
sql dst dstdb m_nopk_change.sql
expect 1 "keyless: one value changed" --src "$SRC" --dst "$DST" --tables t_nopk
expect 1 "keyless: --key w upgrades to chunked" --src "$SRC" --dst "$DST" --tables t_nopk --key w

say "whole-database"
expect 0 "tables subcommand" tables --src "$SRC" --dst "$DST"
expect 0 "version subcommand" version
# dt_ts_swap is excluded: its DATETIME/TIMESTAMP swap is a hard error by
# default (covered by the dedicated scenarios above), and one ERROR would
# make the whole run exit 2 instead of 1.
expect 1 "all common tables (traps present)" --src "$SRC" --dst "$DST" --exclude-tables dt_ts_swap
expect 2 "unknown table" --src "$SRC" --dst "$DST" --tables no_such_table
expect 2 "wrong password" --src root:wrongpw@127.0.0.1:13306/srcdb --dst "$DST" --tables t_mut
if grep -q "wrongpw" "$OUT"; then echo "FAIL: password leaked into output"; exit 1; fi
echo "ok: password not leaked"
expect 2 "nonexistent database" --src root:rootpw@127.0.0.1:13306/nodb --dst "$DST" --tables t_mut
expect 3 "parallel zero" --src "$SRC" --dst "$DST" --tables t_mut --parallel 0

say "json report"
if command -v jq >/dev/null 2>&1; then
  # mtdiff exits 1 when differences exist — the expected result below, so
  # guard against set -e like expect() does (an unguarded call under set -e
  # kills the suite the moment a difference is correctly reported).
  set +e
  timeout 600 "$MTDIFF" --src "$SRC" --dst "$DST" --tables t_mut,t_ignore --json > /tmp/mtdiff-e2e.json
  set -e
  if [ "$(jq -r '.ok' /tmp/mtdiff-e2e.json)" = "false" ]; then
    echo "ok: jq .ok is false with differences"
  else
    echo "FAIL: expected ok=false"; cat /tmp/mtdiff-e2e.json; exit 1
  fi
  sql dst dstdb m_mut_reseed.sql
  set +e
  timeout 600 "$MTDIFF" --src "$SRC" --dst "$DST" --tables t_mut --json > /tmp/mtdiff-e2e.json
  set -e
  if [ "$(jq -r '.ok' /tmp/mtdiff-e2e.json)" = "true" ]; then
    echo "ok: jq .ok is true when identical"
  else
    echo "FAIL: expected ok=true"; cat /tmp/mtdiff-e2e.json; exit 1
  fi
else
  echo "skip: jq not available"
fi

say "snapshot mode"
sql dst dstdb m_mut_reseed.sql
expect 0 "--snapshot identical tables" --src "$SRC" --dst "$DST" --tables t_mut,t_large --snapshot

say "parallel determinism"
if command -v jq >/dev/null 2>&1; then
  p1=$(timeout 600 "$MTDIFF" --src "$SRC" --dst "$DST" --tables t_large --parallel 1 --json | jq -r '.tables[0].src_fp + .tables[0].dst_fp')
  p8=$(timeout 600 "$MTDIFF" --src "$SRC" --dst "$DST" --tables t_large --parallel 8 --json | jq -r '.tables[0].src_fp + .tables[0].dst_fp')
  if [ -z "$p1" ] || [ "$p1" != "$p8" ]; then
    echo "FAIL: fingerprints differ between parallel 1 and 8 (p1=$p1 p8=$p8)"; exit 1
  fi
  echo "ok: fingerprints bit-identical (parallel 1 vs 8)"
else
  echo "skip: jq not available"
fi

say "sync"
# dry-run: a value change is planned (UPDATE sample shown) but nothing is
# written — a plain diff right after must still report the difference.
sql dst dstdb m_mut_reseed.sql
sql dst dstdb m_update.sql
expect 1 "sync dry-run: 3 updated rows planned" sync --src "$SRC" --dst "$DST" --tables t_mut
if ! grep -q 'UPDATE `t_mut`' "$OUT"; then
  echo "FAIL: sync dry-run showed no UPDATE sample"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows UPDATE sample"
expect 1 "diff still differs after dry-run (zero writes)" --src "$SRC" --dst "$DST" --tables t_mut
# apply: row-level (same row counts) -> verified -> plain diff is clean.
expect 0 "sync --apply --yes: row-level updates" sync --src "$SRC" --dst "$DST" --tables t_mut --apply --yes
expect 0 "diff identical after row-level sync" --src "$SRC" --dst "$DST" --tables t_mut
# dst has MORE rows than src (999 vs 1499) -> TRUNCATE + full resync.
sql dst dstdb m_sync_more.sql
expect 0 "sync: dst has more rows -> truncate + resync" sync --src "$SRC" --dst "$DST" --tables t_mut --apply --yes
expect 0 "diff identical after full resync" --src "$SRC" --dst "$DST" --tables t_mut
# dst is MISSING rows (899 vs 999) -> row-level inserts, no TRUNCATE.
sql dst dstdb m_mut_reseed.sql
sql dst dstdb m_sync_missing.sql
expect 0 "sync: dst missing rows -> inserts" sync --src "$SRC" --dst "$DST" --tables t_mut --apply --yes
expect 0 "diff identical after insert sync" --src "$SRC" --dst "$DST" --tables t_mut
# keyless table: any difference means a full resync (rows cannot be targeted).
sql dst dstdb m_nopk_change.sql
expect 1 "keyless dry-run: full resync planned" sync --src "$SRC" --dst "$DST" --tables t_nopk
if ! grep -q 'TRUNCATE TABLE `t_nopk`' "$OUT"; then
  echo "FAIL: keyless dry-run showed no TRUNCATE sample"; cat "$OUT"; exit 1
fi
echo "ok: keyless dry-run shows TRUNCATE sample"
expect 0 "keyless --apply --yes: full resync" sync --src "$SRC" --dst "$DST" --tables t_nopk --apply --yes
expect 0 "keyless diff identical after sync" --src "$SRC" --dst "$DST" --tables t_nopk
# keyless + --where: a filtered keyless table cannot be synced (arg error).
# The table must actually differ: identical tables are skipped before the
# plan (and its error) is even computed.
sql dst dstdb m_nopk_change.sql
expect 3 "keyless + --where: cannot sync" sync --src "$SRC" --dst "$DST" --tables t_nopk --where "w < 'zzz'"
# --where with zero source matches: the source re-plan yields no chunks, so
# the destination's matching rows are planned from the destination side and
# deleted outright (a filtered table cannot be truncated — this is the only
# path to convergence). m_sync_more left 500 rows with id 5001..5499 on the
# dst, none of which exist on the src.
sql dst dstdb m_sync_more.sql
expect 1 "where dry-run: empty source match set planned" sync --src "$SRC" --dst "$DST" --tables t_mut --where "id >= 5001"
if ! grep -q 'DELETE FROM `t_mut`' "$OUT"; then
  echo "FAIL: where dry-run showed no DELETE sample"; cat "$OUT"; exit 1
fi
echo "ok: where dry-run shows DELETE sample"
expect 0 "sync --where: empty source match set -> deletes" sync --src "$SRC" --dst "$DST" --tables t_mut --where "id >= 5001" --apply --yes
expect 0 "diff identical after where-delete sync" --src "$SRC" --dst "$DST" --tables t_mut --where "id >= 5001"
# non-TTY --apply without --yes: no terminal to confirm in -> arg error.
# stdin is a pipe (not a character device), so stdinIsTTY is false.
sql dst dstdb m_mut_reseed.sql
sql dst dstdb m_update.sql
set +e
echo "" | timeout 600 "$MTDIFF" sync --src "$SRC" --dst "$DST" --tables t_mut --apply > "$OUT" 2>&1
rc=$?
set -e
if [ "$rc" -ne 3 ]; then
  echo "FAIL [non-TTY --apply without --yes]: exit $rc, want 3"; cat "$OUT"; exit 1
fi
echo "ok: non-TTY --apply without --yes (exit 3)"
if ! grep -q "requires a terminal" "$OUT"; then
  echo "FAIL: missing the non-TTY confirmation message"; cat "$OUT"; exit 1
fi
echo "ok: non-TTY message shown"
# nothing to do: identical table, no writes, clean exit.
sql dst dstdb m_mut_reseed.sql
expect 0 "sync: identical table, nothing to do" sync --src "$SRC" --dst "$DST" --tables t_mut

say "out-of-range sync"
# (a) t_oor: dst loses 4 rows, 2 are changed, and 4 keys outside the
# source's range (id 0 below, 101..103 above) are added — equal counts, so
# row-level. The out-of-range rows converge only via the out-of-range scan.
sql dst dstdb m_oor_oor.sql
expect 1 "oor dry-run: deletes planned" sync --src "$SRC" --dst "$DST" --tables t_oor
if ! grep -q 'DELETE FROM `t_oor`' "$OUT"; then
  echo "FAIL: oor dry-run showed no DELETE sample"; cat "$OUT"; exit 1
fi
echo "ok: oor dry-run shows DELETE sample"
if grep -q 'TRUNCATE' "$OUT"; then
  echo "FAIL: equal-count oor table was planned as a full resync"; cat "$OUT"; exit 1
fi
echo "ok: oor dry-run is row-level (no TRUNCATE)"
expect 0 "oor --apply --yes: out-of-range rows deleted" sync --src "$SRC" --dst "$DST" --tables t_oor --apply --yes
expect 0 "diff identical after oor sync" --src "$SRC" --dst "$DST" --tables t_oor
oor=$(qdst "SELECT COUNT(*) FROM t_oor WHERE id < 1 OR id > 100")
[ "$oor" = "0" ] || { echo "FAIL: out-of-range rows remain on dst: $oor"; exit 1; }
echo "ok: out-of-range rows (id 0, 101..103) deleted"
oor=$(qdst "SELECT COUNT(*) FROM t_oor WHERE id IN (10, 20, 30, 40, 50, 60) AND val = CONCAT('o', id)")
[ "$oor" = "6" ] || { echo "FAIL: deleted/changed rows not restored: $oor/6"; exit 1; }
echo "ok: deleted and changed rows restored"
oor=$(qdst "SELECT COUNT(*) FROM t_oor")
[ "$oor" = "100" ] || { echo "FAIL: row count after oor sync: $oor (want 100)"; exit 1; }
echo "ok: row count back to 100"
# (b) t_oorc (composite PK): out-of-range rows on both tails, plus the
# strict boundary probes (1,0) and (10,4); the boundary rows (1,1)/(10,3)
# must survive (a strict predicate excludes them, an inclusive one would not).
sql dst dstdb m_oorc_oor.sql
expect 0 "oorc --apply --yes: composite out-of-range rows deleted" sync --src "$SRC" --dst "$DST" --tables t_oorc --apply --yes
expect 0 "diff identical after composite oor sync" --src "$SRC" --dst "$DST" --tables t_oorc
oor=$(qdst "SELECT COUNT(*) FROM t_oorc WHERE a < 1 OR a > 10 OR (a = 1 AND b < 1) OR (a = 10 AND b > 3)")
[ "$oor" = "0" ] || { echo "FAIL: composite out-of-range rows remain: $oor"; exit 1; }
echo "ok: composite out-of-range rows (incl. boundary probes) deleted"
oor=$(qdst "SELECT COUNT(*) FROM t_oorc WHERE (a, b) IN ((1, 1), (10, 3))")
[ "$oor" = "2" ] || { echo "FAIL: boundary rows deleted (strict predicate must keep them): $oor/2"; exit 1; }
echo "ok: boundary rows (1,1) and (10,3) kept"
oor=$(qdst "SELECT COUNT(*) FROM t_oorc")
[ "$oor" = "30" ] || { echo "FAIL: row count after composite oor sync: $oor (want 30)"; exit 1; }
echo "ok: row count back to 30 (deleted rows re-inserted)"
# (c) t_oors (VARCHAR PK): character keys rendered as quoted strings.
sql dst dstdb m_oors_oor.sql
expect 0 "oors --apply --yes: varchar out-of-range keys deleted" sync --src "$SRC" --dst "$DST" --tables t_oors --apply --yes
expect 0 "diff identical after varchar oor sync" --src "$SRC" --dst "$DST" --tables t_oors
oor=$(qdst "SELECT COUNT(*) FROM t_oors WHERE k < 'k001' OR k > 'k050'")
[ "$oor" = "0" ] || { echo "FAIL: varchar out-of-range keys remain: $oor"; exit 1; }
echo "ok: varchar out-of-range keys deleted"
oor=$(qdst "SELECT COUNT(*) FROM t_oors WHERE k IN ('k010', 'k020', 'k030') AND val = CONCAT('s', CAST(SUBSTRING(k, 2) AS UNSIGNED))")
[ "$oor" = "3" ] || { echo "FAIL: deleted varchar rows not restored: $oor/3"; exit 1; }
echo "ok: deleted varchar rows restored"
# (d) t_oorn (explicit --key a, nullable): the source minimum is NULL, so
# only the upper tail can be out of range; the duplicate NULL-key rows and
# the missing in-range row must be handled correctly.
sql dst dstdb m_oorn_oor.sql
expect 0 "oorn --apply --yes --key a: NULL-minimum table converges" sync --src "$SRC" --dst "$DST" --tables t_oorn --key a --apply --yes
expect 0 "diff identical after NULL-key oor sync" --src "$SRC" --dst "$DST" --tables t_oorn --key a
oor=$(qdst "SELECT COUNT(*) FROM t_oorn WHERE a IS NULL")
[ "$oor" = "2" ] || { echo "FAIL: NULL-key rows must survive: $oor/2"; exit 1; }
echo "ok: duplicate NULL-key rows kept"
oor=$(qdst "SELECT COUNT(*) FROM t_oorn WHERE a = 5")
[ "$oor" = "0" ] || { echo "FAIL: out-of-range row (a=5) not deleted: $oor"; exit 1; }
echo "ok: out-of-range row (a=5) deleted"
oor=$(qdst "SELECT COUNT(*) FROM t_oorn WHERE a = 2 AND v = 'y'")
[ "$oor" = "1" ] || { echo "FAIL: missing row (2, 'y') not restored: $oor/1"; exit 1; }
echo "ok: missing row (2, 'y') restored"
# (e) t_mut + --where: out-of-range rows matching the filter are deleted,
# the non-matching one is the documented residual (a filtered table cannot
# be truncated): the filtered comparison converges, the plain one does not.
sql dst dstdb m_oor_where.sql
expect 0 "oor --where --apply: matching out-of-range rows deleted" sync --src "$SRC" --dst "$DST" --tables t_mut --where "updated_at < '2025-01-01'" --apply --yes
expect 0 "diff identical after where oor sync (filtered)" --src "$SRC" --dst "$DST" --tables t_mut --where "updated_at < '2025-01-01'"
expect 1 "unfiltered diff still differs: non-matching residual" --src "$SRC" --dst "$DST" --tables t_mut
oor=$(qdst "SELECT COUNT(*) FROM t_mut WHERE id IN (1001, 1003)")
[ "$oor" = "0" ] || { echo "FAIL: filter-matching out-of-range rows remain: $oor"; exit 1; }
echo "ok: filter-matching out-of-range rows (1001, 1003) deleted"
oor=$(qdst "SELECT COUNT(*) FROM t_mut WHERE id = 1002")
[ "$oor" = "1" ] || { echo "FAIL: non-matching residual must stay (documented): $oor/1"; exit 1; }
echo "ok: non-matching residual (1002) left in place"
# (f) t_mut (no --where, equal counts): first-round convergence without a
# full resync — the pre-fix behavior escalated to TRUNCATE + resync here.
sql dst dstdb m_oor_converge.sql
expect 1 "converge dry-run: row-level deletes planned" sync --src "$SRC" --dst "$DST" --tables t_mut
if ! grep -q 'DELETE FROM `t_mut`' "$OUT"; then
  echo "FAIL: converge dry-run showed no DELETE sample"; cat "$OUT"; exit 1
fi
echo "ok: converge dry-run shows DELETE sample"
if grep -q 'TRUNCATE' "$OUT"; then
  echo "FAIL: equal-count table escalated to a full resync"; cat "$OUT"; exit 1
fi
echo "ok: converge dry-run is row-level (no TRUNCATE)"
expect 0 "converge --apply --yes: first-round convergence" sync --src "$SRC" --dst "$DST" --tables t_mut --apply --yes
expect 0 "diff identical after converge sync" --src "$SRC" --dst "$DST" --tables t_mut
oor=$(qdst "SELECT COUNT(*) FROM t_mut WHERE id > 1000")
[ "$oor" = "0" ] || { echo "FAIL: out-of-range rows remain: $oor"; exit 1; }
echo "ok: out-of-range rows (1001..1003) deleted"
oor=$(qdst "SELECT COUNT(*) FROM t_mut")
[ "$oor" = "1000" ] || { echo "FAIL: row count after converge sync: $oor (want 1000)"; exit 1; }
echo "ok: row count back to 1000"

say "structure sync"
# t_struct on the dst drifts: a column dropped, a type changed, an extra
# column added, the PK removed. Structure sync (default on) plans the DDL.
sql dst dstdb m_struct_drift.sql
expect 1 "structure drift: DDL planned (dry-run)" sync --src "$SRC" --dst "$DST" --tables t_struct
if ! grep -q 'ALTER TABLE `t_struct`' "$OUT"; then
  echo "FAIL: structure dry-run showed no ALTER TABLE"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows the structure DDL"
# the dry-run wrote nothing: the dst still errors the plain diff (structure
# drift is not comparable), so this also proves the DDL was not executed.
expect 2 "diff still errors after structure dry-run (zero writes)" --src "$SRC" --dst "$DST" --tables t_struct
expect 0 "structure drift --apply: aligned + resynced" sync --src "$SRC" --dst "$DST" --tables t_struct --apply --yes
expect 0 "diff identical after structure sync" --src "$SRC" --dst "$DST" --tables t_struct
# information_schema content: the dst must now mirror the src exactly.
cols=$(qdst "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='dstdb' AND TABLE_NAME='t_struct' ORDER BY ORDINAL_POSITION")
[ "$(printf '%s\n' "$cols" | tr '\n' ' ')" = "id name amt ts " ] || {
  echo "FAIL: dst columns after structure sync: [$cols] (want: id name amt ts)"; exit 1; }
echo "ok: dst column set and order aligned"
idtype=$(qdst "SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='dstdb' AND TABLE_NAME='t_struct' AND COLUMN_NAME='id'")
[ "$idtype" = "int" ] || {
  echo "FAIL: dst id type after structure sync: $idtype (want int)"; exit 1; }
echo "ok: dst id type restored to int"
pk=$(qdst "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='dstdb' AND TABLE_NAME='t_struct' AND INDEX_NAME='PRIMARY'")
[ "$pk" = "1" ] || {
  echo "FAIL: dst PRIMARY index after structure sync (count=$pk, want 1)"; exit 1; }
echo "ok: dst PRIMARY key restored"
# Structure-only drift (data identical): structure sync alone must converge.
sql dst dstdb m_struct_drift.sql
expect 1 "structure-only drift: DDL planned" sync --src "$SRC" --dst "$DST" --tables t_struct
expect 0 "structure-only drift --apply: aligned" sync --src "$SRC" --dst "$DST" --tables t_struct --apply --yes
expect 0 "diff identical after structure-only sync" --src "$SRC" --dst "$DST" --tables t_struct
# A structurally identical table must show no DDL.
sql dst dstdb m_mut_reseed.sql
expect 0 "identical table: no DDL planned" sync --src "$SRC" --dst "$DST" --tables t_mut
if grep -q "ALTER TABLE" "$OUT"; then
  echo "FAIL: DDL shown for a structurally identical table"; cat "$OUT"; exit 1
fi
echo "ok: no DDL for an identical table"
# --no-sync-schema restores the old behavior: structure drift is a hard
# failure instead of a planned DDL.
sql dst dstdb m_struct_drift.sql
expect 2 "structure drift with --no-sync-schema: not syncable" sync --src "$SRC" --dst "$DST" --tables t_struct --no-sync-schema

say "key drift (keyed source vs keyless destination)"
# t_keyless on the dst loses only its PRIMARY KEY: columns and data stay
# identical, so the tables are still comparable, but no shared key exists
# to plan chunks by. Before the keyless fallback this pair crashed with an
# index-out-of-range panic in the chunk predicate.
sql dst dstdb m_keyless_drift.sql
expect 0 "keyless dst: diff falls back to keyless comparison (identical)" --src "$SRC" --dst "$DST" --tables t_keyless
if ! grep -q "warn t_keyless" "$OUT"; then
  echo "FAIL: the keyless fallback must be announced in the report"; cat "$OUT"; exit 1
fi
echo "ok: keyless fallback announced in the report"
# Now the data drifts too: the multiset comparison still sees it, and the
# sync (which cannot target rows without a shared key) must take the full
# resync; --where on such a table is a misconfiguration.
sql dst dstdb m_keyless_data.sql
expect 1 "keyless dst: diff reports the data drift" --src "$SRC" --dst "$DST" --tables t_keyless
expect 1 "keyless dst: dry-run plans the full resync" sync --src "$SRC" --dst "$DST" --tables t_keyless --no-sync-schema
if ! grep -q 'TRUNCATE TABLE `t_keyless`' "$OUT"; then
  echo "FAIL: keyless dst dry-run showed no TRUNCATE sample"; cat "$OUT"; exit 1
fi
echo "ok: keyless dst dry-run shows the TRUNCATE sample"
expect 3 "keyless dst with --where: cannot sync" sync --src "$SRC" --dst "$DST" --tables t_keyless --no-sync-schema --where "id >= 1"
expect 0 "keyless dst: full resync applied" sync --src "$SRC" --dst "$DST" --tables t_keyless --no-sync-schema --apply --yes
expect 0 "diff identical after keyless-dst resync" --src "$SRC" --dst "$DST" --tables t_keyless
n=$(qdst "SELECT COUNT(*) FROM t_keyless WHERE id IN (7, 17, 27)")
if [ "$n" != "3" ]; then
  echo "FAIL: resync restored the deleted rows (count=$n, want 3)"; exit 1
fi
echo "ok: resync restored the deleted rows"
v=$(qdst "SELECT val FROM t_keyless WHERE id = 47")
if [ "$v" != "v47" ]; then
  echo "FAIL: resync restored the changed row (val=$v, want v47)"; exit 1
fi
echo "ok: resync restored the changed row"
n=$(qdst "SELECT COUNT(*) FROM t_keyless WHERE id IN (51, 52, 53)")
if [ "$n" != "0" ]; then
  echo "FAIL: resync dropped the extra rows (count=$n, want 0)"; exit 1
fi
echo "ok: resync dropped the extra rows"
# With structure sync (the default) the same drift is repaired instead:
# the primary key is re-added and the table resynced.
sql dst dstdb m_keyless_drift.sql
sql dst dstdb m_keyless_data.sql
expect 1 "key drift: default sync plans the key DDL" sync --src "$SRC" --dst "$DST" --tables t_keyless
if ! grep -q "ADD PRIMARY KEY" "$OUT"; then
  echo "FAIL: default sync must show the ADD PRIMARY KEY DDL"; cat "$OUT"; exit 1
fi
echo "ok: default sync shows the key DDL"
expect 0 "key drift: default sync --apply" sync --src "$SRC" --dst "$DST" --tables t_keyless --apply --yes
expect 0 "diff identical after default sync" --src "$SRC" --dst "$DST" --tables t_keyless
pk=$(qdst "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='dstdb' AND TABLE_NAME='t_keyless' AND INDEX_NAME='PRIMARY'")
if [ "$pk" != "1" ]; then
  echo "FAIL: dst PRIMARY key after default sync (count=$pk, want 1)"; exit 1
fi
echo "ok: dst PRIMARY key restored by default sync"

E2E_OK=1
say "ALL E2E SCENARIOS PASSED"
