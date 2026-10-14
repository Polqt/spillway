# Every recipe line is one plain command with no shell syntax, because GNU make
# on Windows runs recipes through cmd.exe when no sh.exe is on PATH.
# Environment is set with target-specific exports instead.

GO      ?= go
TOOL    := $(GO) tool -modfile=tools/go.mod
EXE     := $(shell $(GO) env GOEXE)
BIN     := bin/spillway$(EXE)
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
# The race detector needs cgo; it is skipped where no C compiler is available.
RACE    ?= $(if $(filter 1,$(shell $(GO) env CGO_ENABLED)),-race,)

.PHONY: build test lint check run mock vuln cover

build: export CGO_ENABLED=0
build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/spillway

test:
	$(GO) test $(RACE) -count=1 ./...

# CGO_ENABLED=0 makes the build step prove the no-cgo hard rule on every check.
lint: export CGO_ENABLED=0
lint:
	$(GO) mod tidy -diff
	$(GO) -C tools mod tidy -diff
	$(GO) build ./...
	$(TOOL) golangci-lint config verify
	$(TOOL) golangci-lint run ./...

check: lint test

run: build
	$(BIN) serve -config examples/config.yaml

mock: build
	$(BIN) serve -config examples/config.mock.yaml

vuln:
	$(TOOL) govulncheck ./...

cover:
	$(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
