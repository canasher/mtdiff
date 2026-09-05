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

# Push the working tree to the remote: stage everything, commit it (only
# if there is anything), and push the CURRENT branch to origin (creating
# the upstream if it does not exist yet):
#   make push m="round-5: fix the thing"
#   make push m="..." CHECK=1     # run lint + unit tests before pushing
push:
	@if [ -z "$(m)" ]; then echo 'usage: make push m="commit message" [CHECK=1]'; exit 1; fi
	@if [ "$(CHECK)" = "1" ]; then $(MAKE) lint test; fi
	git add -A
	if git diff --cached --quiet; then \
		echo "nothing to commit; nothing pushed"; \
	else \
		git commit -m "$(m)" && git push -u origin HEAD; \
	fi
