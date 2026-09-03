GO  ?= go
BIN := bin/mtdiff

.PHONY: build test e2e compat-57 compat-tidb lint clean

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
