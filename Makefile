GO ?= go
GOLANGCI_LINT ?= golangci-lint
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/cb-Sigma/scale-backtest/internal/cli.Version=$(VERSION)

.PHONY: all build test test-short race vet fmt fmt-check lint demo golden validate clean

all: fmt-check vet lint test build

build: ## build ./bin/scale-backtest
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/scale-backtest ./cmd/scale-backtest

test: ## run all tests with the race detector
	$(GO) test ./... -race -count=1

test-short: ## quick run: smaller queueing-theory samples
	$(GO) test ./... -short -count=1

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)

lint:
	$(GOLANGCI_LINT) run ./...

demo: build ## run the bundled demo
	./bin/scale-backtest run --demo
	./bin/scale-backtest compare --demo

golden: ## regenerate golden files and the output blocks in README.md and docs/
	$(GO) test ./internal/cli -update
	$(GO) test . -update

validate: ## print the queueing-theory validation numbers
	$(GO) test ./internal/sim -run 'MM|Processor|Loss' -v -count=1 | grep -E 'queueing_test|^(ok|FAIL|---)'

clean:
	rm -rf bin dist coverage.*
