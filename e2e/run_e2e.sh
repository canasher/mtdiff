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

E2E_OK=1
say "ALL E2E SCENARIOS PASSED"
