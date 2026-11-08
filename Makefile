# Ether — developer makefile.
GO ?= go
# Pure-Go SIMD yEnc; set GOEXPERIMENT= to build the portable SWAR path.
export GOEXPERIMENT ?= simd
VERSION ?= dev
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w \
	-X github.com/braedonsaunders/ether/internal/updater.Version=$(VERSION) \
	-X github.com/braedonsaunders/ether/internal/updater.Commit=$(COMMIT) \
	-X github.com/braedonsaunders/ether/internal/updater.Date=$(DATE) \
	-X github.com/braedonsaunders/ether/internal/tmdb.BundledKey=$(ETHER_TMDB_BUNDLED_KEY) \
	-X github.com/braedonsaunders/ether/internal/updater.PublicKeyB64=$(ETHER_UPDATE_PUBLIC_KEY)

.PHONY: build test lint vet web spike run tray ffmpeg icons release-snapshot keys clean

## build: server + tray + release-sign tool (CGO-free server)
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ether ./cmd/ether
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ether-release-sign ./cmd/ether-release-sign
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ether-tray ./cmd/ether-tray

## test: full suite with the race detector
test:
	$(GO) test -race ./...

## lint: the CI lint job: gofmt, go mod tidy, vet, golangci-lint, shellcheck
## (golangci-lint reports only issues new since LINT_BASE, as CI does)
LINT_BASE ?= origin/main
lint: vet
	@test -z "$$($(GO)fmt -l .)" || { echo "gofmt needed:"; $(GO)fmt -l .; exit 1; }
	@$(GO) mod tidy -diff || { echo "go.mod/go.sum not tidy: run 'go mod tidy'"; exit 1; }
	@if command -v golangci-lint >/dev/null; then \
		CGO_ENABLED=0 golangci-lint run --new-from-merge-base=$(LINT_BASE) ./...; \
	else echo "golangci-lint not installed — skipping (https://golangci-lint.run/docs/welcome/install/)"; fi
	@if command -v shellcheck >/dev/null; then \
		shellcheck scripts/*.sh packaging/linux/*.sh packaging/macos/*.sh packaging/docker/*.sh && \
		git ls-files -z '*.sh' | xargs -0 shellcheck --severity=error; \
	else echo "shellcheck not installed — skipping"; fi

vet:
	CGO_ENABLED=0 $(GO) vet ./...

## web: build the admin/setup SPA (embedded via go:embed)
web:
	cd web && pnpm install --frozen-lockfile && pnpm build

## spike: run a local files library (make spike FILES="movie.mp4 ...")
spike:
	CGO_ENABLED=0 $(GO) run ./cmd/ether --spike $(FILES)

## run: run the server from source
run:
	CGO_ENABLED=0 $(GO) run ./cmd/ether

## tray: run the tray (headless under CGO_ENABLED=0)
tray:
	$(GO) run ./cmd/ether-tray

## ffmpeg: stage the pinned ffmpeg/ffprobe for the host platform
ffmpeg:
	scripts/fetch-ffmpeg.sh

## icons: regenerate the tray icons
icons:
	$(GO) run scripts/generate-tray-icons.go cmd/ether-tray/assets

## release-snapshot: goreleaser dry run (fetches pinned ffmpeg)
release-snapshot:
	goreleaser release --snapshot --clean --skip=docker,publish

## keys: generate an update-signing keypair (prints env lines; keep private!)
keys:
	$(GO) run ./cmd/ether-release-sign keygen

clean:
	rm -rf dist
