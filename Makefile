# Every recipe line is one plain command with no shell syntax, because GNU make
# on Windows runs recipes through cmd.exe when no sh.exe is on PATH.
# Environment is set with target-specific exports instead.

GO      ?= go
TOOL    := $(GO) tool -modfile tools/go.mod
RELEASE_TOOL := $(GO) tool -modfile tools/release/go.mod
PNPM = corepack pnpm@$(shell node -p "require('./apps/spillway/web/package.json').packageManager.split('@')[1].split('+')[0]")
EXE     := $(shell $(GO) env GOEXE)
BIN     := bin/spillway$(EXE)
VERSION ?= dev
LDFLAGS := -s -w -X github.com/Polqt/spillway/internal/cli.version=$(VERSION)
# The race detector needs cgo; it is skipped where no C compiler is available.
RACE    ?= $(if $(filter 1,$(shell $(GO) env CGO_ENABLED)),-race,)

.PHONY: web-deps web-playwright-deps web-build web-test build test fuzz lint check run mock vuln cover release-check snapshot

web-deps:
	$(PNPM) --dir apps/spillway/web install --frozen-lockfile

web-playwright-deps:
	$(PNPM) --dir apps/spillway/web exec playwright install $(PLAYWRIGHT_INSTALL_ARGS) chromium

web-build: web-deps
	$(PNPM) --dir apps/spillway/web run build

web-test: web-build
	$(PNPM) --dir apps/spillway/web exec playwright install chromium
	$(PNPM) --dir apps/spillway/web run test:e2e

build: web-build
build: export CGO_ENABLED=0
build:
	$(GO) build -tags dashboardassets -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./apps/spillway

test: web-test
	$(GO) test $(RACE) -tags dashboardassets -count=1 ./...

fuzz:
	$(GO) test ./tests/config -run=^$$ -fuzz=FuzzParseWithEnv -fuzztime=60s
	$(GO) test ./tests/router -run=^$$ -fuzz=FuzzDetect -fuzztime=60s
	$(GO) test ./tests/providers/openaicompat -run=^$$ -parallel=1 -fuzz=FuzzStreamRecv -fuzztime=60s

# CGO_ENABLED=0 makes the build step prove the no-cgo hard rule on every check.
lint: web-build
lint: export CGO_ENABLED=0
lint:
	$(GO) mod tidy -diff
	$(GO) -C tools mod tidy -diff
	$(GO) build -tags dashboardassets ./...
	$(TOOL) golangci-lint config verify
	$(TOOL) golangci-lint run --build-tags dashboardassets ./...

check: lint test

run: build
	$(BIN) serve -config examples/config.yaml

mock: build
	$(BIN) serve -config examples/config.mock.yaml

vuln:
	$(TOOL) govulncheck ./...

# Tests live in tests/, outside the packages they cover, so coverage must be
# attributed across packages with -coverpkg.
cover: web-build
	$(GO) test -tags dashboardassets -count=1 -coverpkg=./... -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

release-check:
	$(GO) -C tools/release mod tidy -diff
	$(RELEASE_TOOL) goreleaser check --config .goreleaser.yaml

snapshot: export CGO_ENABLED=0
snapshot:
	$(GO) -C tools/release mod tidy -diff
	$(RELEASE_TOOL) goreleaser release --snapshot --clean --config .goreleaser.yaml
