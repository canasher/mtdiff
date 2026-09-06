GO  ?= go
BIN := bin/mtdiff

.PHONY: build test e2e compat-57 compat-tidb lint clean push

build:
	$(GO) build -o $(BIN) .

test:
	$(GO) test ./...

e2e: build
	bash e2e/run_e2e.sh

# Compatibility suites on foreign backends (see e2e/compat/).
compat-57: build
	bash e2e/compat/run_compat.sh 57

compat-tidb: build
	bash e2e/compat/run_compat.sh tidb

lint:
	@out=$$(gofmt -l .); [ -z "$$out" ] || { echo "gofmt:"; echo "$$out"; exit 1; }
	$(GO) vet ./...

clean:
	rm -rf bin

# Push the current branch to origin. Two independent steps, in order:
#
#   1. COMMIT (conditional): commits ONLY what is already staged — the
#      user stages deliberately with `git add`. The working tree is
#      never auto-staged (no `git add -A`): an untracked file (a secret,
#      a DSN, an editor backup) must not ride along by default. Nothing
#      staged -> no commit.
#   2. PUSH (final): always runs — local commits made outside this
#      target (or a clean tree that is ahead) still reach the remote.
#
#   git add <files>
#   m="message" make push       # commit the staged files, then push
#   make push                   # no commit; push existing local commits
#   m="..." make push CHECK=1   # run lint + unit tests before pushing
#
# The message is an environment variable and the recipe references it as
# $$m: make leaves a literal $m in the script text (it does NOT expand
# the value — make's own expansion is textual and would hand the value's
# quotes to the shell's parser), and the SHELL expands the environment
# value inside the double quotes, where an expansion result is never
# re-parsed. So quotes, semicolons, backticks, newlines and $(...) in
# the message are literal message text, never commands. (A make command-
# line m=... must be avoided: make strips double quotes from it.)
export m
push:
	@if [ "$(CHECK)" = "1" ]; then $(MAKE) lint test; fi
	@rc=$$(git diff --cached --quiet 2>/dev/null; echo $$?); \
	if [ "$$rc" = 1 ]; then \
		if [ -z "$$m" ]; then \
			echo 'usage: m="commit message" make push (stage nothing to push without committing) [CHECK=1]'; \
			exit 1; \
		fi; \
		git commit -m "$$m"; \
	elif [ "$$rc" = 0 ]; then \
		echo "nothing staged: no commit (the push still runs below)"; \
	else \
		echo "git diff --cached failed (rc=$$rc): not a git repository, or an unhealthy index — refusing to commit or push"; \
		exit 1; \
	fi; \
	git push -u origin HEAD
