#!/usr/bin/env bash
# Regression for the `make push` target (run from anywhere):
#   - the commit is CONDITIONAL and stages nothing: it commits only what
#     is already staged, and an untracked file (a secret, a DSN) must
#     never ride along;
#   - the push is FINAL: a clean tree that is ahead of the origin still
#     pushes (without a commit message, no commit made);
#   - the commit message travels as an environment variable: quotes,
#     semicolons, backticks, $(...) and non-ASCII text all land in the
#     commit LITERALLY, and the pwnedN canaries prove nothing executed.
#
# The script builds a throwaway git repository with a bare origin and
# drives the real Makefile from the repository root.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

export GIT_AUTHOR_NAME=mt GIT_AUTHOR_EMAIL=mt@test GIT_COMMITTER_NAME=mt GIT_COMMITTER_EMAIL=mt@test
git init -q --bare origin.git
git init -q work
cd work
git config user.name mt
git config user.email mt@test
git remote add origin "$WORK/origin.git"

fail() { echo "FAIL: $*" >&2; exit 1; }

# the first commit + push establishes main on the origin
echo base > base.txt
git add base.txt
git commit -qm "base"
git branch -M main
git push -qu origin main

# the message is an environment variable (byte-exact; a make command-
# line m=... would have make strip its quotes)
make_push() { make -s -C "$WORK/work" -f "$ROOT/Makefile" push; }
remote_head() { git --git-dir="$WORK/origin.git" rev-parse refs/heads/main; }
local_head() { git rev-parse HEAD; }
top_msg() { git log -1 --format=%B; }
origin_files() { git --git-dir="$WORK/origin.git" ls-tree -r --name-only main; }

# --- (1) staged change: the commit happens and reaches the origin -------
echo a > a.txt
git add a.txt
m="round: staged case" make_push
[ "$(top_msg)" = "round: staged case" ] || fail "commit message mismatch: $(top_msg)"
[ "$(remote_head)" = "$(local_head)" ] || fail "the commit did not reach the origin"
origin_files | grep -qx a.txt || fail "a.txt must be on the origin"

# --- (2) clean tree, local ahead: the push runs, no commit, no m -------
git commit -q --allow-empty -m "made outside make push"
make_push
[ "$(remote_head)" = "$(local_head)" ] || fail "a clean tree ahead of the origin must still push"

# --- (3) an untracked file is never auto-staged ------------------------
echo "password=hunter2" > secret.env
echo b > b.txt
git add b.txt
m="round: untracked stays local" make_push
if origin_files | grep -q secret.env; then fail "the untracked secret.env was committed"; fi
origin_files | grep -qx b.txt || fail "b.txt must be committed"
[ -f secret.env ] || fail "secret.env must still exist locally, untracked"

# --- (4) staged changes but no message: usage error, nothing pushed ----
echo s > s.txt
git add s.txt
if make -s -C "$WORK/work" -f "$ROOT/Makefile" push 2>/dev/null; then
  fail "staged-but-messageless must refuse (usage error) without pushing"
fi
[ "$(remote_head)" = "$(local_head)" ] || fail "the usage-error path must not push"
git reset -q -- s.txt   # unstage again

# --- (5) metacharacter messages: literal, and nothing executes ---------
for msg in \
  'fix "quote"' \
  'fix; touch pwned1' \
  'fix `touch pwned2`' \
  'fix $(touch pwned3)' \
  '中文提交'
do
  echo c >> c.txt
  git add c.txt
  m="$msg" make_push
  [ "$(top_msg)" = "$msg" ] || fail "message not literal: want $(printf %q "$msg"), got $(printf %q "$(top_msg)")"
done
for f in pwned1 pwned2 pwned3; do
  [ -e "$f" ] && fail "the message EXECUTED as shell code: $f exists"
done

echo "ok: make push (staged-only commit, final push, literal messages, untracked untouched)"
