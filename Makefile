GO  ?= go
BIN := bin/mtdiff

.PHONY: build test e2e lint clean

build:
	$(GO) build -o $(BIN) .

test:
	$(GO) test ./...

e2e: build
	bash e2e/run_e2e.sh

lint:
	@out=$$(gofmt -l .); [ -z "$$out" ] || { echo "gofmt:"; echo "$$out"; exit 1; }
	$(GO) vet ./...

clean:
	rm -rf bin
