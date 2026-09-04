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
# qdb <side> <db> <sql> runs a query on any side's database (the whole-
# database scenarios use the secondary srcdb2/dstdb2 pair).
qdb() {
  $COMPOSE -f e2e/docker-compose.yml exec -T -e MYSQL_PWD=rootpw "mysql-$1" \
    mysql -uroot -D "$2" -N -B -e "$3"
}

# showai reads the TRUE next AUTO_INCREMENT value: the AUTO_INCREMENT=
# clause of SHOW CREATE TABLE (empty when the counter was never set
# explicitly, i.e. it follows max(id)+1). InnoDB's information_schema
# estimate is not refreshed after a second counter change (a second
# ALTER or a TRUNCATE), so the assertions read the value the server
# actually persists.
showai() {
  qdb "$1" "$2" "SHOW CREATE TABLE $3\G" | grep -oE 'AUTO_INCREMENT=[0-9]+' | head -n1 | cut -d= -f2
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
# t_strkey seeds itself with NO_BACKSLASH_ESCAPES (literal backslash keys)
sql src srcdb seed_str.sql
sql dst dstdb seed_str.sql

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
# dst has MORE rows than src (1498 vs 999): the extra rows are addressed
# by their key and deleted one by one — the row counts never force a full
# resync (no TRUNCATE).
sql dst dstdb m_sync_more.sql
expect 1 "sync dry-run: dst has more rows -> deletes planned" sync --src "$SRC" --dst "$DST" --tables t_mut
if ! grep -q 'DELETE FROM `t_mut`' "$OUT"; then
  echo "FAIL: dry-run showed no DELETE sample"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows the DELETE sample"
if grep -q 'TRUNCATE' "$OUT"; then
  echo "FAIL: extra rows escalated to a full resync"; cat "$OUT"; exit 1
fi
echo "ok: dry-run is row-level (no TRUNCATE)"
expect 0 "sync: dst has more rows -> row-level deletes" sync --src "$SRC" --dst "$DST" --tables t_mut --apply --yes
expect 0 "diff identical after delete sync" --src "$SRC" --dst "$DST" --tables t_mut
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

say "whole-database sync (create / drop / state)"
SRC2=root:rootpw@127.0.0.1:13306/srcdb2
DST2=root:rootpw@127.0.0.1:13307/dstdb2
# A second, small database pair: dstdb2 starts EMPTY, so whole-database
# mode (no --tables) must discover the source's BASE TABLE set and create
# every table it finds there. The dst database is reset up front, so a
# re-run after a failed run (which may have left created tables behind)
# starts from the same state.
qdb src srcdb2 "SELECT 1" >/dev/null 2>&1 || \
  $COMPOSE -f e2e/docker-compose.yml exec -T -e MYSQL_PWD=rootpw mysql-src mysql -uroot -e "CREATE DATABASE IF NOT EXISTS srcdb2"
$COMPOSE -f e2e/docker-compose.yml exec -T -e MYSQL_PWD=rootpw mysql-dst \
  mysql -uroot -e "DROP DATABASE IF EXISTS dstdb2; CREATE DATABASE dstdb2"
sql src srcdb2 seed_src2.sql
# (a) empty dst database: the dry-run plans a CREATE for every source
# table (t_new included) and writes nothing.
expect 1 "empty dst: dry-run plans the creates" sync --src "$SRC2" --dst "$DST2"
if ! grep -q 'CREATE TABLE `t_ai`' "$OUT"; then
  echo "FAIL: no CREATE TABLE sample for t_ai"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows CREATE TABLE samples"
if ! grep -q 'AUTO_INCREMENT=1500' "$OUT"; then
  echo "FAIL: the create did not start on the source AUTO_INCREMENT value"; cat "$OUT"; exit 1
fi
echo "ok: the create carries the source AUTO_INCREMENT start value"
if ! grep -q 'UNIQUE KEY' "$OUT"; then
  echo "FAIL: the create did not reproduce t_new's unique key"; cat "$OUT"; exit 1
fi
echo "ok: the create reproduces the unique key"
n=$(qdb dst dstdb2 "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb2' AND TABLE_TYPE='BASE TABLE'")
[ "$n" = "0" ] || { echo "FAIL: the create dry-run wrote to the dst ($n tables exist)"; exit 1; }
echo "ok: the create dry-run is zero-write"
# apply: every source table is created and the data converges; the
# auto-increment table starts (and stays) on the source's counter.
expect 0 "empty dst: --apply creates all source tables" sync --src "$SRC2" --dst "$DST2" --apply --yes
expect 0 "re-run after create: nothing to do" sync --src "$SRC2" --dst "$DST2"
# the plain index that only the source has (t_idx) must not turn into a
# DDL: plain non-unique indexes are outside the synced structure scope.
if grep -q "ALTER TABLE" "$OUT"; then
  echo "FAIL: a plain-index difference must not produce DDL"; cat "$OUT"; exit 1
fi
echo "ok: plain-index difference causes no DDL"
n=$(qdb dst dstdb2 "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb2' AND TABLE_NAME='t_new' AND TABLE_TYPE='BASE TABLE'")
[ "$n" = "1" ] || { echo "FAIL: t_new was not created on the dst"; exit 1; }
echo "ok: t_new created on the dst"
u=$(qdb dst dstdb2 "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='dstdb2' AND TABLE_NAME='t_new' AND INDEX_NAME='u_code'")
[ "$u" = "1" ] || { echo "FAIL: t_new's unique key was not created"; exit 1; }
echo "ok: t_new's unique key created"
ai=$(showai dst dstdb2 t_ai)
[ "$ai" = "1500" ] || { echo "FAIL: t_ai AUTO_INCREMENT after create ($ai, want 1500)"; exit 1; }
echo "ok: t_ai AUTO_INCREMENT converged (1500)"
# (b) table state only: the data is identical, only the counter drifted
# (1200 vs 1500) -> a STATE plan, no data work, no TRUNCATE.
sql dst dstdb2 m_dst2_ai_state.sql
expect 1 "state drift: AUTO_INCREMENT planned (dry-run)" sync --src "$SRC2" --dst "$DST2"
if ! grep -q 'ALTER TABLE `t_ai` AUTO_INCREMENT = 1500' "$OUT"; then
  echo "FAIL: no AUTO_INCREMENT alignment planned"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows the AUTO_INCREMENT alignment"
if grep -q 'TRUNCATE' "$OUT"; then
  echo "FAIL: a state-only drift must not truncate"; cat "$OUT"; exit 1
fi
echo "ok: state-only drift plans no TRUNCATE"
ai=$(showai dst dstdb2 t_ai)
[ "$ai" = "1200" ] || { echo "FAIL: the state dry-run wrote to the dst ($ai)"; exit 1; }
echo "ok: the state dry-run is zero-write"
expect 0 "state drift: --apply realigns the counter" sync --src "$SRC2" --dst "$DST2" --apply --yes
ai=$(showai dst dstdb2 t_ai)
[ "$ai" = "1500" ] || { echo "FAIL: t_ai AUTO_INCREMENT after apply ($ai, want 1500)"; exit 1; }
echo "ok: t_ai AUTO_INCREMENT realigned (1500)"
# (c) structure drift on an auto-increment table: the repair truncates and
# reloads (which resets the counter) -> the state is re-aligned afterwards.
sql dst dstdb2 m_dst2_ai_struct.sql
expect 1 "ai structure drift: DDL planned (dry-run)" sync --src "$SRC2" --dst "$DST2" --tables t_ai
if ! grep -q 'ALTER TABLE `t_ai`' "$OUT"; then
  echo "FAIL: no structure DDL for the drifted table"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows the structure DDL"
expect 0 "ai structure drift: --apply (repaired + reloaded + state)" sync --src "$SRC2" --dst "$DST2" --tables t_ai --apply --yes
valtype=$(qdb dst dstdb2 "SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='dstdb2' AND TABLE_NAME='t_ai' AND COLUMN_NAME='val'")
[ "$valtype" = "varchar" ] || { echo "FAIL: t_ai.val type after repair ($valtype)"; exit 1; }
echo "ok: t_ai.val type repaired"
ai=$(showai dst dstdb2 t_ai)
[ "$ai" = "1500" ] || { echo "FAIL: t_ai AUTO_INCREMENT after reload ($ai, want 1500)"; exit 1; }
echo "ok: t_ai AUTO_INCREMENT re-aligned after the reload"
# (d) the destination loses t_plain's primary key: the structure repair
# restores the key and the repaired table goes back to row-level sync
# instead of an unconditional full resync.
sql dst dstdb2 m_dst2_plain_nopk.sql
expect 1 "pk lost: key DDL planned (dry-run)" sync --src "$SRC2" --dst "$DST2" --tables t_plain
if ! grep -q 'ADD PRIMARY KEY' "$OUT"; then
  echo "FAIL: the key repair DDL is missing"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows the ADD PRIMARY KEY DDL"
if ! grep -q 'row-level' "$OUT"; then
  echo "FAIL: the repaired table must go back to row-level sync"; cat "$OUT"; exit 1
fi
echo "ok: the repaired table is planned row-level"
expect 0 "pk lost: --apply restores the key and the data" sync --src "$SRC2" --dst "$DST2" --tables t_plain --apply --yes
pk=$(qdb dst dstdb2 "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='dstdb2' AND TABLE_NAME='t_plain' AND INDEX_NAME='PRIMARY'")
[ "$pk" = "1" ] || { echo "FAIL: t_plain PRIMARY key after repair (count=$pk, want 1)"; exit 1; }
echo "ok: t_plain PRIMARY key restored"
expect 0 "re-run after pk restore: identical" sync --src "$SRC2" --dst "$DST2" --tables t_plain
# (e) a destination-only table: whole-database mode plans a DROP TABLE for
# it, listed under the destructive changes; the dry-run writes nothing.
sql dst dstdb2 m_dst2_extra.sql
expect 1 "extra dst table: DROP planned (dry-run)" sync --src "$SRC2" --dst "$DST2"
if ! grep -q 'DROP TABLE IF EXISTS `t_extra`' "$OUT"; then
  echo "FAIL: no DROP TABLE planned for the extra table"; cat "$OUT"; exit 1
fi
echo "ok: dry-run plans the DROP TABLE"
if ! grep -q 'DESTRUCTIVE' "$OUT"; then
  echo "FAIL: the destructive change is not listed separately"; cat "$OUT"; exit 1
fi
echo "ok: the destructive change is listed separately"
n=$(qdb dst dstdb2 "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb2' AND TABLE_NAME='t_extra'")
[ "$n" = "1" ] || { echo "FAIL: the drop dry-run wrote to the dst (t_extra count=$n)"; exit 1; }
echo "ok: the drop dry-run is zero-write"
expect 0 "extra dst table: --apply drops it" sync --src "$SRC2" --dst "$DST2" --apply --yes
n=$(qdb dst dstdb2 "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb2' AND TABLE_NAME='t_extra'")
[ "$n" = "0" ] || { echo "FAIL: t_extra still exists after the drop"; exit 1; }
echo "ok: t_extra dropped"
expect 0 "re-run after drop: nothing to do" sync --src "$SRC2" --dst "$DST2"
# (f) --tables scopes the run strictly: an out-of-scope destination table
# is never dropped, even with --apply.
sql dst dstdb2 m_dst2_extra.sql
expect 0 "--tables: in-scope table identical, nothing to do" sync --src "$SRC2" --dst "$DST2" --tables t_plain
expect 0 "--tables --apply: out-of-scope table is not dropped" sync --src "$SRC2" --dst "$DST2" --tables t_plain --apply --yes
n=$(qdb dst dstdb2 "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb2' AND TABLE_NAME='t_extra'")
[ "$n" = "1" ] || { echo "FAIL: --tables dropped an out-of-scope table (count=$n)"; exit 1; }
echo "ok: --tables never drops out-of-scope tables"
# (g) --exclude-tables removes a table from both the sync set and the drop
# set: a whole-database run leaves it alone.
sql dst dstdb2 m_dst2_extra.sql
expect 0 "excluded table: whole-database run is clean" sync --src "$SRC2" --dst "$DST2" --exclude-tables t_extra --apply --yes
n=$(qdb dst dstdb2 "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='dstdb2' AND TABLE_NAME='t_extra'")
[ "$n" = "1" ] || { echo "FAIL: --exclude-tables dropped the excluded table (count=$n)"; exit 1; }
echo "ok: --exclude-tables spares the extra table"
# (h) a stray row on a keyed table (3 vs 4): one row-level DELETE, never a
# full resync.
sql dst dstdb2 m_dst2_plain_stray.sql
expect 1 "stray row: DELETE planned (dry-run)" sync --src "$SRC2" --dst "$DST2" --tables t_plain
if ! grep -q 'DELETE FROM `t_plain`' "$OUT"; then
  echo "FAIL: no DELETE sample for the stray row"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows the DELETE sample"
if grep -q 'TRUNCATE' "$OUT"; then
  echo "FAIL: a stray row escalated to a full resync"; cat "$OUT"; exit 1
fi
echo "ok: stray row stays row-level (no TRUNCATE)"
expect 0 "stray row: --apply deletes it" sync --src "$SRC2" --dst "$DST2" --tables t_plain --apply --yes
n=$(qdb dst dstdb2 "SELECT COUNT(*) FROM t_plain")
[ "$n" = "3" ] || { echo "FAIL: stray row not deleted (count=$n, want 3)"; exit 1; }
echo "ok: stray row deleted, count back to 3"
# (i) a missing table under --where (a row filter) or with the structure
# sync off is not created: a clear failure, not a silent skip.
sql dst dstdb2 m_dst2_drop_new.sql
expect 2 "missing table with --where: not created" sync --src "$SRC2" --dst "$DST2" --tables t_new --where "id >= 1"
if ! grep -q "does not create tables" "$OUT"; then
  echo "FAIL: missing the --where explanation"; cat "$OUT"; exit 1
fi
echo "ok: --where explains why the table is not created"
expect 2 "missing table with --no-sync-schema: not created" sync --src "$SRC2" --dst "$DST2" --tables t_new --no-sync-schema
if ! grep -q "no-sync-schema" "$OUT"; then
  echo "FAIL: missing the --no-sync-schema explanation"; cat "$OUT"; exit 1
fi
echo "ok: --no-sync-schema explains why the table is not created"
# ...and the plain default (structure sync on, no --where) creates it.
expect 0 "missing table: created by the default sync" sync --src "$SRC2" --dst "$DST2" --tables t_new --apply --yes
expect 0 "diff identical after re-create" --src "$SRC2" --dst "$DST2" --tables t_new

say "explicit --key uniqueness (P0-1 / P1-1)"
# t_wk.k has only a plain (non-unique) index. With --where, a row-level
# sync would delete whole key groups on the rows the filter excluded:
# an argument error, in the dry run and under --apply alike, before any
# write connection exists.
sql dst dstdb m_wk_change.sql
expect 3 "non-unique --key + --where: arg error (dry-run)" sync --src "$SRC" --dst "$DST" --tables t_wk --key k --where "id < 5"
if ! grep -q 'not PRIMARY KEY or NOT NULL UNIQUE' "$OUT"; then
  echo "FAIL: missing the uniqueness rejection"; cat "$OUT"; exit 1
fi
echo "ok: the rejection names the requirement"
expect 3 "non-unique --key + --where: arg error (--apply)" sync --src "$SRC" --dst "$DST" --tables t_wk --key k --where "id < 5" --apply --yes
n=$(qdst "SELECT COUNT(*) FROM t_wk WHERE k = 99")
[ "$n" = "1" ] || { echo "FAIL: the rejected sync wrote to the dst (k=99 count=$n, want 1)"; exit 1; }
echo "ok: the rejected sync wrote nothing"
# t_swap.v is NOT NULL UNIQUE: an explicit --key on it is recognized as
# unique. With the key value stable and the other column changed, a
# recognized-unique key converges with a plain row-level UPDATE — group
# replace (non-unique semantics) would delete + re-insert instead.
sql dst dstdb m_swap_change.sql
expect 1 "unique --key: row-level UPDATE planned" sync --src "$SRC" --dst "$DST" --tables t_swap --key v
if ! grep -q 'UPDATE `t_swap`' "$OUT"; then
  echo "FAIL: the unique --key must yield a plain update (group replace would delete+insert)"; cat "$OUT"; exit 1
fi
if grep -q 'TRUNCATE' "$OUT"; then
  echo "FAIL: the unique --key escalated to a full resync"; cat "$OUT"; exit 1
fi
echo "ok: the unique --key yields row-level updates"
expect 0 "unique --key --apply: converged" sync --src "$SRC" --dst "$DST" --tables t_swap --key v --apply --yes
# Address change: when the unique key's OWN value changes, the row address
# moved — delete + insert is the only correct shape (never an UPDATE).
# The 'Z' row is OUTSIDE the source's key span (src has 'A','B'), so it
# converges via the out-of-range scan: that delete must commit BEFORE the
# insert, or the insert duplicates the PK the out-of-range row still holds
# (regression: duplicate entry for PRIMARY at apply time).
sql dst dstdb m_swap_addr.sql
expect 1 "unique --key, changed key value: delete+insert" sync --src "$SRC" --dst "$DST" --tables t_swap --key v
if ! grep -q 'INSERT INTO `t_swap`' "$OUT" || ! grep -q 'DELETE FROM `t_swap`' "$OUT"; then
  echo "FAIL: a changed unique key value must convert to delete+insert"; cat "$OUT"; exit 1
fi
dl=$(grep -n 'DELETE FROM `t_swap`' "$OUT" | head -1 | cut -d: -f1)
il=$(grep -n 'INSERT INTO `t_swap`' "$OUT" | head -1 | cut -d: -f1)
if [ -z "$dl" ] || [ -z "$il" ] || [ "$dl" -gt "$il" ]; then
  echo "FAIL: the out-of-range delete must precede the insert (dl=$dl il=$il)"; cat "$OUT"; exit 1
fi
echo "ok: the changed key value is delete+insert, delete first (no TRUNCATE)"
expect 0 "unique --key, changed key value --apply" sync --src "$SRC" --dst "$DST" --tables t_swap --key v --apply --yes
expect 0 "unique --key + --where: allowed (identical)" --src "$SRC" --dst "$DST" --tables t_swap --key v --where "id < 5"

say "unique value swap: default refusal, opt-in rewrite (P0-2)"
sql dst dstdb m_swap.sql
# Default: the destructive DELETE+INSERT rewrite is DISABLED (it fires
# FK ON DELETE CASCADE, triggers and audit logs for rows the user never
# asked to change): the table is refused, in the dry run and under
# --apply alike, with zero writes.
expect 2 "swap dry-run: REFUSED (rewrite disabled by default)" sync --src "$SRC" --dst "$DST" --tables t_swap
if ! grep -q -- '--allow-row-rewrite' "$OUT"; then
  echo "FAIL: the refusal must name the opt-in flag"; cat "$OUT"; exit 1
fi
echo "ok: the refusal names the opt-in flag"
expect 2 "swap --apply: REFUSED before any write" sync --src "$SRC" --dst "$DST" --tables t_swap --apply --yes
v=$(qdst "SELECT v FROM t_swap WHERE id = 1")
[ "$v" = "A" ] || { echo "FAIL: the refused swap wrote to the dst (id=1 v=$v, want the drifted A)"; exit 1; }
echo "ok: the refused swap left the dst untouched"
# With the flag: the destructive rewrite is permitted and converges.
expect 0 "swap --allow-row-rewrite --apply: rewritten" sync --src "$SRC" --dst "$DST" --tables t_swap --allow-row-rewrite --apply --yes
expect 0 "swap identical after the rewrite" --src "$SRC" --dst "$DST" --tables t_swap
v=$(qdst "SELECT v FROM t_swap WHERE id = 1")
[ "$v" = "B" ] || { echo "FAIL: swapped value not restored (id=1 v=$v, want B)"; exit 1; }
echo "ok: the destructive rewrite converged the swap"

say "unique value cycle: default refusal (P0-2)"
sql dst dstdb m_u3_cycle.sql
expect 2 "cycle dry-run: REFUSED (no row order applies a cycle)" sync --src "$SRC" --dst "$DST" --tables t_u3
if ! grep -q -- '--allow-row-rewrite' "$OUT"; then
  echo "FAIL: the cycle refusal must name the opt-in flag"; cat "$OUT"; exit 1
fi
v=$(qdst "SELECT u FROM t_u3 WHERE id = 1")
[ "$v" = "B" ] || { echo "FAIL: the refused cycle wrote to the dst (id=1 u=$v, want the drifted B)"; exit 1; }
echo "ok: the refused cycle left the dst untouched"
expect 0 "cycle --allow-row-rewrite --apply: rewritten" sync --src "$SRC" --dst "$DST" --tables t_u3 --allow-row-rewrite --apply --yes
expect 0 "cycle identical after the rewrite" --src "$SRC" --dst "$DST" --tables t_u3
v=$(qdst "SELECT u FROM t_u3 WHERE id = 1")
[ "$v" = "A" ] || { echo "FAIL: cycled value not restored (id=1 u=$v, want A)"; exit 1; }
echo "ok: the destructive rewrite converged the cycle"

say "FK ON DELETE CASCADE: the default refusal never cascades (P0-2)"
sql dst dstdb m_fk_swap.sql
expect 2 "fk swap dry-run: REFUSED" sync --src "$SRC" --dst "$DST" --tables t_fk
expect 2 "fk swap --apply: REFUSED" sync --src "$SRC" --dst "$DST" --tables t_fk --apply --yes
n=$(qdst "SELECT COUNT(*) FROM t_fkc")
[ "$n" = "2" ] || { echo "FAIL: the child rows are gone after the refusal (count=$n, want 2) — the default must never rewrite"; exit 1; }
echo "ok: the child rows survived the refusal (no cascade)"
# With the flag the rewrite is permitted — and the cascade follows
# (the documented risk the default exists to prevent).
expect 0 "fk swap --allow-row-rewrite --apply" sync --src "$SRC" --dst "$DST" --tables t_fk --allow-row-rewrite --apply --yes
v=$(qdst "SELECT code FROM t_fk WHERE id = 1")
[ "$v" = "A" ] || { echo "FAIL: the parent did not converge (id=1 code=$v, want A)"; exit 1; }
n=$(qdst "SELECT COUNT(*) FROM t_fkc")
[ "$n" = "0" ] || { echo "FAIL: the rewrite's cascades are not visible (child count=$n, want 0)"; exit 1; }
echo "ok: the opt-in rewrite cascaded the child deletes (the documented risk)"

say "generated columns: compared, never written (P0-2)"
sql dst dstdb m_gen_change.sql
expect 1 "gen: val drift detected (doubled re-derives)" --src "$SRC" --dst "$DST" --tables t_gen
expect 1 "gen dry-run: UPDATE planned" sync --src "$SRC" --dst "$DST" --tables t_gen
if ! grep -q 'UPDATE `t_gen`' "$OUT"; then
  echo "FAIL: no UPDATE sample for the generated-column table"; cat "$OUT"; exit 1
fi
echo "ok: dry-run shows the UPDATE sample"
if grep '^UPDATE' "$OUT" | grep -q 'doubled'; then
  echo "FAIL: the generated column must never be written"; cat "$OUT"; exit 1
fi
echo "ok: the generated column is out of the write path"
expect 0 "gen --apply --yes: val converged, doubled re-derived" sync --src "$SRC" --dst "$DST" --tables t_gen --apply --yes
expect 0 "gen identical after sync" --src "$SRC" --dst "$DST" --tables t_gen
d=$(qdst "SELECT doubled FROM t_gen WHERE id = 1")
# Sync restores the SOURCE row (val=10): the generated column must have
# re-derived from the restored base value (doubled=20), i.e. the whole
# row now matches src exactly — and it got there without mtdiff ever
# writing `doubled` (the sample above is asserted free of it).
[ "$d" = "20" ] || { echo "FAIL: the generated column did not re-derive (doubled=$d, want 20)"; exit 1; }
v=$(qdst "SELECT val FROM t_gen WHERE id = 1")
[ "$v" = "10" ] || { echo "FAIL: val not restored to the source value (val=$v, want 10)"; exit 1; }
echo "ok: the row matches src exactly (val=10, doubled re-derived to 20)"
# structure drift: the dst lost the generated column. The structure sync
# must REFUSE the table (the expression cannot be reproduced) and write
# nothing — in the dry run and under --apply alike.
sql dst dstdb m_gen_drift.sql
expect 2 "gen drift (col dropped): sync refuses (dry-run)" sync --src "$SRC" --dst "$DST" --tables t_gen
if ! grep -q 'generated column' "$OUT"; then
  echo "FAIL: missing the generated-column refusal"; cat "$OUT"; exit 1
fi
if ! grep -q -- '--no-sync-schema' "$OUT"; then
  echo "FAIL: missing the --no-sync-schema hint"; cat "$OUT"; exit 1
fi
echo "ok: the refusal names the column and the escape hatch"
expect 2 "gen drift: --apply refuses before any write" sync --src "$SRC" --dst "$DST" --tables t_gen --apply --yes
# After the convergence apply above, the dst row matches the source
# (val=10); the drift script only dropped the column. A refusing sync
# must leave even that alone — val still at the source value.
v=$(qdst "SELECT val FROM t_gen WHERE id = 1")
[ "$v" = "10" ] || { echo "FAIL: the refused sync wrote to the dst (val=$v, want 10)"; exit 1; }
echo "ok: the refused sync left the data untouched"

say "generated column expressions are compared (P1-1)"
# Same expression on both sides: no structure drift, no refusal.
sql dst dstdb m_genx_same.sql
expect 0 "genx identical expression: no drift" --src "$SRC" --dst "$DST" --tables t_genx
# A different expression: detected drift, refused (an auto rebuild of a
# generated column would change what it computes).
sql dst dstdb m_genx_diff.sql
expect 2 "genx different expression: drift, refused" sync --src "$SRC" --dst "$DST" --tables t_genx --apply --yes
if ! grep -q 'generated column' "$OUT"; then
  echo "FAIL: missing the generated-column refusal"; cat "$OUT"; exit 1
fi
echo "ok: the expression drift is detected and refused"
# A storage-type drift (the same expression, VIRTUAL vs STORED):
# likewise a drift, refused.
sql dst dstdb m_genv_diff.sql
expect 2 "genv VIRTUAL vs STORED: drift, refused" sync --src "$SRC" --dst "$DST" --tables t_genv --apply --yes
if ! grep -q 'generated' "$OUT"; then
  echo "FAIL: missing the generated-column refusal"; cat "$OUT"; exit 1
fi
echo "ok: the storage drift is detected and refused"

say "structure ALTER failure keeps the data (P1-3)"
sql dst dstdb m_structfail_drift.sql
expect 1 "structfail dry-run: in-place ALTER planned" sync --src "$SRC" --dst "$DST" --tables t_structfail
if ! grep -q 'ALTER TABLE `t_structfail`' "$OUT"; then
  echo "FAIL: no structure DDL for the drifted table"; cat "$OUT"; exit 1
fi
if grep -q 'TRUNCATE' "$OUT"; then
  echo "FAIL: the default structure sync must not truncate"; cat "$OUT"; exit 1
fi
echo "ok: the dry run plans the in-place ALTER (no TRUNCATE)"
expect 2 "structfail --apply: the ALTER fails, data preserved" sync --src "$SRC" --dst "$DST" --tables t_structfail --apply --yes
if ! grep -q -- '--allow-structure-truncate' "$OUT"; then
  echo "FAIL: the failure must point at the opt-in flag"; cat "$OUT"; exit 1
fi
n=$(qdst "SELECT COUNT(*) FROM t_structfail WHERE amt = 12345.67")
[ "$n" = "1" ] || { echo "FAIL: the failed in-place ALTER lost the data (count=$n, want 1)"; exit 1; }
echo "ok: the wide value survived the failed in-place ALTER"
expect 0 "structfail --allow-structure-truncate: reloaded" sync --src "$SRC" --dst "$DST" --tables t_structfail --allow-structure-truncate --apply --yes
expect 0 "structfail identical after reload" --src "$SRC" --dst "$DST" --tables t_structfail
n=$(qdst "SELECT COUNT(*) FROM t_structfail WHERE amt > 99.99")
[ "$n" = "0" ] || { echo "FAIL: the wide value survived the reload (count=$n, want 0)"; exit 1; }
echo "ok: the table was reloaded from the src (opt-in truncate)"

say "multi-statement DDL: partial failure, re-plan, no stale replay (P1-2)"
sql dst dstdb m_mddl_drift.sql
expect 1 "mddl dry-run: structure drift, two DDLs planned" sync --src "$SRC" --dst "$DST" --tables t_mddl
n=$(grep -c 'ALTER TABLE `t_mddl`' "$OUT")
[ "$n" = "2" ] || { echo "FAIL: want exactly 2 DDL statements (add column + deferred unique index), got $n"; cat "$OUT"; exit 1; }
if grep -q 'TRUNCATE' "$OUT"; then
  echo "FAIL: the default structure sync must not truncate"; cat "$OUT"; exit 1
fi
echo "ok: the plan is two statements (the index follows the column it references)"
# Statement 2 (ADD UNIQUE on the duplicate codes) fails; statement 1
# (ADD COLUMN) already applied: the data is preserved, the error names
# the partial application, and nothing is re-applied on a re-run.
expect 2 "mddl --apply: statement 2 fails, data preserved" sync --src "$SRC" --dst "$DST" --tables t_mddl --apply --yes
if ! grep -q 'prior DDL statements may already have been applied' "$OUT"; then
  echo "FAIL: the failure must name the possible partial application"; cat "$OUT"; exit 1
fi
n=$(qdst "SELECT COUNT(*) FROM t_mddl")
[ "$n" = "4" ] || { echo "FAIL: the failed DDL lost the data (count=$n, want 4)"; exit 1; }
c=$(qdst "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = 'dstdb' AND TABLE_NAME = 't_mddl' AND COLUMN_NAME = 'x'")
[ "$c" = "1" ] || { echo "FAIL: statement 1 (ADD COLUMN x) must have applied (x count=$c, want 1)"; exit 1; }
echo "ok: the partial application is named and the data is preserved"
# Re-run (default): it re-plans from the CURRENT schema — the column now
# exists, so only the still-missing index is planned (a stale replay
# would re-add the column and fail with a duplicate-column error).
expect 2 "mddl re-run: re-planned, the remaining DDL still fails" sync --src "$SRC" --dst "$DST" --tables t_mddl --apply --yes
n=$(qdst "SELECT COUNT(*) FROM t_mddl")
[ "$n" = "4" ] || { echo "FAIL: the re-run lost the data (count=$n, want 4)"; exit 1; }
c=$(qdst "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = 'dstdb' AND TABLE_NAME = 't_mddl' AND COLUMN_NAME = 'x'")
[ "$c" = "1" ] || { echo "FAIL: the re-run must not replay the applied DDL (x count=$c, want 1)"; exit 1; }
echo "ok: the re-run re-plans (no stale replay) and still fails on the duplicates"
# With the flag: truncate, re-plan from the fresh introspection (only the
# missing unique index remains), apply it, full reload.
expect 0 "mddl --allow-structure-truncate: truncated, re-planned, reloaded" sync --src "$SRC" --dst "$DST" --tables t_mddl --allow-structure-truncate --apply --yes
expect 0 "mddl identical after the reload" --src "$SRC" --dst "$DST" --tables t_mddl
n=$(qdst "SELECT COUNT(*) FROM t_mddl")
[ "$n" = "3" ] || { echo "FAIL: the reload did not restore the source rows (count=$n, want 3)"; exit 1; }
n=$(qdst "SELECT COUNT(DISTINCT code) FROM t_mddl")
[ "$n" = "3" ] || { echo "FAIL: the duplicates survived the reload (distinct codes=$n, want 3)"; exit 1; }
echo "ok: the re-planned DDL applied and the table reloaded from the src"

say "write-path escaping under NO_BACKSLASH_ESCAPES (P0-3)"
# Flip the dst server into NO_BACKSLASH_ESCAPES: every session mtdiff
# opens (including its write connection) inherits the mode, so a
# client-side interpolated literal would store the backslash values
# mangled. The parameterized write path must round-trip them byte-exact.
qdb dst dstdb "SET GLOBAL sql_mode = CONCAT(@@GLOBAL.sql_mode, ',NO_BACKSLASH_ESCAPES')"
sql dst dstdb m_esc_change.sql
expect 1 "esc dry-run: updates planned" sync --src "$SRC" --dst "$DST" --tables t_esc
expect 1 "diff still differs after esc dry-run (zero writes)" --src "$SRC" --dst "$DST" --tables t_esc
expect 0 "esc --apply --yes: backslash rows converge" sync --src "$SRC" --dst "$DST" --tables t_esc --apply --yes
# byte-exact: the full HEX of both sides must match row for row.
sh=$(qdb src srcdb "SELECT id, HEX(val) FROM t_esc ORDER BY id")
dh=$(qdb dst dstdb "SELECT id, HEX(val) FROM t_esc ORDER BY id")
if [ "$sh" != "$dh" ]; then
  echo "FAIL: t_esc bytes differ after the sync (src: $sh; dst: $dh)"; exit 1
fi
echo "ok: the backslash/quote values round-tripped byte-exact under NO_BACKSLASH_ESCAPES"
qdb dst dstdb "SET GLOBAL sql_mode = REPLACE(@@GLOBAL.sql_mode, ',NO_BACKSLASH_ESCAPES', '')"

say "string primary keys: parameterized read predicates (P0-1)"
# The dst server runs NO_BACKSLASH_ESCAPES globally: the read-side key
# bounds (chunk plan, chunk scans, out-of-range deletes) must address
# backslash/quote/CJK keys byte-exact — bound values travel as
# parameters, never as interpolated literals. 12 rows, chunk size 10:
# the string-keyed sampler must span the table in multiple chunks.
qdb dst dstdb "SET GLOBAL sql_mode = CONCAT(@@GLOBAL.sql_mode, ',NO_BACKSLASH_ESCAPES')"
expect 0 "strkey identical (multi-chunk string keys)" --src "$SRC" --dst "$DST" --tables t_strkey --chunk-size 10
sql dst dstdb m_strkey_change.sql
expect 1 "strkey: the drifted values differ" --src "$SRC" --dst "$DST" --tables t_strkey --chunk-size 10
expect 0 "strkey --apply: converged" sync --src "$SRC" --dst "$DST" --tables t_strkey --chunk-size 10 --apply --yes
sh=$(qdb src srcdb "SELECT k, HEX(k), HEX(v) FROM t_strkey ORDER BY k")
dh=$(qdb dst dstdb "SELECT k, HEX(k), HEX(v) FROM t_strkey ORDER BY k")
if [ "$sh" != "$dh" ]; then
  echo "FAIL: t_strkey bytes differ after the sync (src: $sh; dst: $dh)"; exit 1
fi
echo "ok: the string keys and values round-tripped byte-exact"
# An out-of-range row (its key sorts above the source's maximum): the
# string-keyed out-of-range delete must remove it.
sql dst dstdb m_strkey_oor.sql
expect 1 "strkey: the out-of-range row differs" --src "$SRC" --dst "$DST" --tables t_strkey --chunk-size 10
expect 0 "strkey --apply: the out-of-range row is deleted" sync --src "$SRC" --dst "$DST" --tables t_strkey --chunk-size 10 --apply --yes
n=$(qdst "SELECT COUNT(*) FROM t_strkey WHERE k = '末\末'")
[ "$n" = "0" ] || { echo "FAIL: the out-of-range row survived (count=$n, want 0)"; exit 1; }
sh=$(qdb src srcdb "SELECT k, HEX(k), HEX(v) FROM t_strkey ORDER BY k")
dh=$(qdb dst dstdb "SELECT k, HEX(k), HEX(v) FROM t_strkey ORDER BY k")
if [ "$sh" != "$dh" ]; then
  echo "FAIL: t_strkey bytes differ after the OOR delete (src: $sh; dst: $dh)"; exit 1
fi
echo "ok: the out-of-range string key was deleted, the table is byte-exact"
qdb dst dstdb "SET GLOBAL sql_mode = REPLACE(@@GLOBAL.sql_mode, ',NO_BACKSLASH_ESCAPES', '')"

say "unique constraints are tuples, not members (P1-5)"
# Composite UNIQUE(a,b): a repeated a (different b) is NOT a conflict —
# plain updates, no destructive rewrite.
sql dst dstdb m_comp_change.sql
expect 1 "comp: the plain drift is detected" sync --src "$SRC" --dst "$DST" --tables t_comp
if ! grep -q 'UPDATE `t_comp`' "$OUT"; then
  echo "FAIL: a non-conflicting composite drift must be a plain update"; cat "$OUT"; exit 1
fi
if grep -q 'DELETE FROM `t_comp`' "$OUT"; then
  echo "FAIL: a repeated composite MEMBER must not trigger the rewrite"; cat "$OUT"; exit 1
fi
echo "ok: the repeated composite member stays a plain update"
expect 0 "comp --apply: converged without a rewrite" sync --src "$SRC" --dst "$DST" --tables t_comp --apply --yes
expect 0 "comp identical after sync" --src "$SRC" --dst "$DST" --tables t_comp
# A whole-tuple swap: a true unique-tuple conflict — refused by default,
# converged with the opt-in flag.
sql dst dstdb m_comp_swap.sql
expect 2 "comp tuple swap: REFUSED by default" sync --src "$SRC" --dst "$DST" --tables t_comp
expect 0 "comp tuple swap --allow-row-rewrite --apply" sync --src "$SRC" --dst "$DST" --tables t_comp --allow-row-rewrite --apply --yes
expect 0 "comp identical after the rewrite" --src "$SRC" --dst "$DST" --tables t_comp
echo "ok: the tuple swap is refused by default, converged with the flag"
# Two separate constraints: an email equal to another row's phone must
# not cross-collide.
sql dst dstdb m_two_change.sql
expect 1 "two: the cross-constraint drift is detected" sync --src "$SRC" --dst "$DST" --tables t_two
if grep -q 'DELETE FROM `t_two`' "$OUT"; then
  echo "FAIL: different constraints must not cross-collide (a rewrite was planned)"; cat "$OUT"; exit 1
fi
expect 0 "two --apply: converged without a rewrite" sync --src "$SRC" --dst "$DST" --tables t_two --apply --yes
expect 0 "two identical after sync" --src "$SRC" --dst "$DST" --tables t_two
echo "ok: the cross-constraint value is no false conflict"
# A NULLABLE unique column: NULL tuples never occupy a slot.
sql dst dstdb m_nu_change.sql
expect 1 "nu: the NULL move is detected" sync --src "$SRC" --dst "$DST" --tables t_nu
if grep -q 'DELETE FROM `t_nu`' "$OUT"; then
  echo "FAIL: repeated NULLs in a nullable unique column must not conflict"; cat "$OUT"; exit 1
fi
expect 0 "nu --apply: converged without a rewrite" sync --src "$SRC" --dst "$DST" --tables t_nu --apply --yes
expect 0 "nu identical after sync" --src "$SRC" --dst "$DST" --tables t_nu
echo "ok: the NULL move stays a plain update"

say "cross-chunk unique swap: refusal, then full resync (P1-6)"
sql dst dstdb m_xchunk_swap.sql
# Default: a swap that crosses chunk commits cannot be ordered — refused.
expect 2 "xchunk cross-chunk swap: REFUSED by default" sync --src "$SRC" --dst "$DST" --tables t_xchunk --chunk-size 10
if ! grep -q -- '--allow-row-rewrite' "$OUT"; then
  echo "FAIL: the cross-chunk refusal must name the opt-in flag"; cat "$OUT"; exit 1
fi
v=$(qdst "SELECT u FROM t_xchunk WHERE id = 1")
[ "$v" = "v12" ] || { echo "FAIL: the refused swap wrote to the dst (id=1 u=$v, want the drifted v12)"; exit 1; }
echo "ok: the cross-chunk swap is refused, the dst untouched"
# With the flag: row-level writes cannot order it — the plan escalates
# to the order-independent FULL resync (TRUNCATE + reload).
expect 1 "xchunk --allow-row-rewrite: the full resync is planned" sync --src "$SRC" --dst "$DST" --tables t_xchunk --chunk-size 10 --allow-row-rewrite
if ! grep -q 'TRUNCATE' "$OUT"; then
  echo "FAIL: the escalated plan must be a full resync"; cat "$OUT"; exit 1
fi
echo "ok: the escalation is a full resync"
expect 0 "xchunk --allow-row-rewrite --apply: reloaded" sync --src "$SRC" --dst "$DST" --tables t_xchunk --chunk-size 10 --allow-row-rewrite --apply --yes
expect 0 "xchunk identical after the resync" --src "$SRC" --dst "$DST" --tables t_xchunk --chunk-size 10
v=$(qdst "SELECT u FROM t_xchunk WHERE id = 1")
[ "$v" = "v1" ] || { echo "FAIL: the resync did not restore the value (id=1 u=$v, want v1)"; exit 1; }
echo "ok: the full resync converged the cross-chunk swap"

say "wide table: the INSERT batch shrinks below the bind budget (P2-3)"
sql dst dstdb m_wide_change.sql
expect 1 "wide: the drift is detected" sync --src "$SRC" --dst "$DST" --tables t_wide
expect 0 "wide --apply: converged" sync --src "$SRC" --dst "$DST" --tables t_wide --apply --yes
expect 0 "wide identical after sync" --src "$SRC" --dst "$DST" --tables t_wide
echo "ok: the 120-column table converged (batch capped below 60000 params)"

say "sparse --where: split points from the filtered rows (P2-2)"
sql dst dstdb m_where_change.sql
expect 1 "wheresparse: the filtered row differs" sync --src "$SRC" --dst "$DST" --tables t_wheresparse --where "g < 1" --chunk-size 1000
expect 0 "wheresparse --apply: converged" sync --src "$SRC" --dst "$DST" --tables t_wheresparse --where "g < 1" --chunk-size 1000 --apply --yes
expect 0 "wheresparse identical after sync" --src "$SRC" --dst "$DST" --tables t_wheresparse --where "g < 1" --chunk-size 1000
v=$(qdst "SELECT v FROM t_wheresparse WHERE k = 'k00100'")
[ "$v" = "w100" ] || { echo "FAIL: the filtered row did not converge (id=100 v=$v, want w100)"; exit 1; }
echo "ok: the sparse filter converged (split points from the filtered rows)"

say "--snapshot under a READ-COMMITTED global: strict, not downgraded (P1-4)"
qdb dst dstdb "SET GLOBAL transaction_isolation = 'READ-COMMITTED'"
sql dst dstdb m_snap_drift.sql
set +e
timeout 300 "$MTDIFF" --src "$SRC" --dst "$DST" --tables t_snap --snapshot > "$OUT" 2>&1
rc=$?
set -e
if [ "$rc" -ne 1 ]; then
  echo "FAIL: --snapshot under READ-COMMITTED exited $rc (want a clean 1, not an error)"; cat "$OUT"; exit 1
fi
echo "ok: --snapshot ran strictly under a READ-COMMITTED global (clean diff)"
sql dst dstdb m_snap_reset.sql
expect 0 "snapshot under READ-COMMITTED: identical after reset" --src "$SRC" --dst "$DST" --tables t_snap --snapshot
qdb dst dstdb "SET GLOBAL transaction_isolation = 'REPEATABLE-READ'"

say "snapshot mode under concurrent writes (P1-5)"
sql dst dstdb m_snap_drift.sql
# churn the dst from a background client while the snapshot diff runs:
# the run must finish cleanly (0/1), never with a runtime error.
(
  i=0
  while [ "$i" -lt 30 ]; do
    qdb dst dstdb "UPDATE t_snap SET v = 'x1' WHERE id = 1" >/dev/null 2>&1 || true
    i=$((i+1))
    sleep 0.2
  done
) &
churn=$!
set +e
timeout 300 "$MTDIFF" --src "$SRC" --dst "$DST" --tables t_snap --snapshot > "$OUT" 2>&1
rc=$?
set -e
wait "$churn"
if [ "$rc" -ne 0 ] && [ "$rc" -ne 1 ]; then
  echo "FAIL: --snapshot diff under churn exit $rc (want 0 or 1)"; cat "$OUT"; exit 1
fi
echo "ok: --snapshot diff survived the concurrent writes (exit $rc)"
sql dst dstdb m_snap_reset.sql
expect 0 "snapshot: identical after reset" --src "$SRC" --dst "$DST" --tables t_snap --snapshot

say "read connections stay read-only under load (P1-2)"
# MySQL cannot read another session's variables, so the policy is verified
# from the src server's general log (TABLE output): every connection that
# read t_large must show its read-only session setup (applySession's
# SET SESSION TRANSACTION READ ONLY, the tier that lands on MySQL 8) in
# the log. Two visibility rules shape the "read" pattern:
#  - chunk scans are PARAMETERIZED (P0-1): they run as COM_STMT_PREPARE/
#    EXECUTE, which the general log records as command types Prepare/
#    Execute but WITHOUT the statement text — so a scan worker is
#    identified by its prepared-statement VOLUME (each of the 4 workers
#    runs ~250 chunk pairs; the control connection runs <20), not by text.
#  - the non-parameterized reads (COUNT / key extremes / drill-downs)
#    still land as classic Query rows with matchable text.
# The log is snapshotted into a temp table and the general log switched
# off BEFORE the analysis queries run, so the analysis cannot match
# itself (its own text contains the patterns).
qdb src srcdb "DROP TABLE IF EXISTS mysql.mtdiff_probe_gl; SET GLOBAL general_log = OFF; SET GLOBAL log_output = 'TABLE'; TRUNCATE TABLE mysql.general_log; SET GLOBAL general_log = ON"
set +e
timeout 600 "$MTDIFF" --src "$SRC" --dst "$DST" --tables t_large --parallel 4 --chunk-size 100 > "$OUT" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || { echo "FAIL: the probe diff itself exited $rc"; cat "$OUT"; exit 1; }
grep -q "comparing 1000 chunks" "$OUT" || { echo "FAIL: the probe diff did not report its 1000 chunk scans"; cat "$OUT"; exit 1; }
qdb src srcdb "CREATE TABLE mysql.mtdiff_probe_gl AS SELECT * FROM mysql.general_log; SET GLOBAL general_log = OFF; SET GLOBAL log_output = 'FILE'"
scan_threads=$(qdb src srcdb "SELECT COUNT(*) FROM (SELECT thread_id FROM mysql.mtdiff_probe_gl WHERE command_type = 'Query' AND argument LIKE '%FROM \`t_large\`%' UNION SELECT thread_id FROM mysql.mtdiff_probe_gl WHERE command_type IN ('Prepare','Execute') GROUP BY thread_id HAVING COUNT(*) >= 50) sc")
unenforced=$(qdb src srcdb "SELECT COUNT(*) FROM (SELECT DISTINCT thread_id AS tid FROM (SELECT thread_id FROM mysql.mtdiff_probe_gl WHERE command_type = 'Query' AND argument LIKE '%FROM \`t_large\`%' UNION SELECT thread_id FROM mysql.mtdiff_probe_gl WHERE command_type IN ('Prepare','Execute') GROUP BY thread_id HAVING COUNT(*) >= 50) x) sc LEFT JOIN (SELECT DISTINCT thread_id AS tid FROM mysql.mtdiff_probe_gl WHERE argument LIKE '%TRANSACTION READ ONLY%' OR argument LIKE 'SET SESSION read_only%') po ON po.tid = sc.tid WHERE po.tid IS NULL")
qdb src srcdb "DROP TABLE mysql.mtdiff_probe_gl"
case "$scan_threads" in ''|*[!0-9]*) scan_threads=0;; esac
case "$unenforced" in ''|*[!0-9]*) unenforced=1;; esac
if [ "$scan_threads" -lt 2 ]; then
  echo "FAIL: only $scan_threads connection(s) read t_large (want >= 2)"; exit 1
fi
if [ "$unenforced" -ne 0 ]; then
  echo "FAIL: $unenforced reading connection(s) never set the read-only session policy"; exit 1
fi
echo "ok: all $scan_threads reading connections set the read-only session policy"

say "BIGINT extremes: overflow-safe chunking"
# The key span (MinInt64..MaxInt64) is wider than MaxInt64 values: the
# arithmetic split must refuse and the sampler must partition the range
# instead; the extreme rows must still be diffed and converged.
expect 0 "bigint extremes identical" --src "$SRC" --dst "$DST" --tables t_bigint
sql dst dstdb m_bigint_max.sql
expect 1 "bigint extremes: the max row changed" --src "$SRC" --dst "$DST" --tables t_bigint
expect 0 "bigint extremes: sync restores the max row" sync --src "$SRC" --dst "$DST" --tables t_bigint --apply --yes
expect 0 "bigint extremes identical after sync" --src "$SRC" --dst "$DST" --tables t_bigint

say "--sample-limit 0 shows no sample SQL (P2-1)"
sql dst dstdb m_mut_reseed.sql
sql dst dstdb m_update.sql
set +e
timeout 600 "$MTDIFF" sync --src "$SRC" --dst "$DST" --tables t_mut --sample-limit 0 > "$OUT" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || { echo "FAIL: the sample-limit 0 dry-run exited $rc (want 1)"; cat "$OUT"; exit 1; }
if grep -q 'UPDATE `t_mut`' "$OUT"; then
  echo "FAIL: --sample-limit 0 still showed sample SQL"; cat "$OUT"; exit 1
fi
echo "ok: --sample-limit 0 keeps the plan, drops the samples"

E2E_OK=1
say "ALL E2E SCENARIOS PASSED"
