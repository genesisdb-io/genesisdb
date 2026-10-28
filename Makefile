.PHONY: build release-binaries test compatibility lint vuln fuzz bench licenses install-tools clean fmt

STATICCHECK_VERSION := v0.8.2
GOVULNCHECK_VERSION := v1.9.0
GITLEAKS_VERSION := v8.24.3
GOLICENSES_VERSION := v2.0.1

# VERSION and CHANNEL are injected into the binary. The release workflow
# passes the computed tag, a local build reports 0.0.0-dev/development.
VERSION ?= 0.0.0-dev
CHANNEL ?= development
MODULE := github.com/genesisdb-io/genesisdb/v2
LDFLAGS := -s -w -X $(MODULE)/packages/core.Version=$(VERSION) -X $(MODULE)/packages/core.ReleaseChannel=$(CHANNEL)

install-tools:
	go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	go install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)
	go install github.com/google/go-licenses/v2@$(GOLICENSES_VERSION)

# Every build regenerates THIRD_PARTY_LICENSES from the linked modules, so
# the file next to a binary always matches that binary.
build: licenses
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/genesisdb ./cmd/genesisdb
	cp LICENSE THIRD_PARTY_LICENSES bin/

# Cross builds for a release, one static binary per platform.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
release-binaries: licenses
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=dist/genesisdb-$(VERSION)-$$os-$$arch; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/genesisdb || exit 1; \
	done
	cp LICENSE THIRD_PARTY_LICENSES dist/
	cd dist && shasum -a 256 genesisdb-$(VERSION)-* > SHA256SUMS

test:
	go test -race ./...

# Replays the recorded 1.1.6 exchanges and fixtures.
compatibility:
	go test -count=1 -run Compatibility -v ./tests

lint:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt issues"; exit 1)
	go vet ./...
	staticcheck ./...

vuln:
	govulncheck ./...

# Writes THIRD_PARTY_LICENSES from the modules linked into the binary and
# fails on a license outside MIT, BSD, Apache 2.0 and ISC. go-licenses is
# installed on demand, so a plain "make build" works on a fresh machine.
GOBIN_DIR := $(shell go env GOPATH)/bin
licenses:
	@test -x $(GOBIN_DIR)/go-licenses || go install github.com/google/go-licenses/v2@$(GOLICENSES_VERSION)
	PATH="$(GOBIN_DIR):$$PATH" scripts/third-party-licenses.sh

fuzz:
	go test ./packages/query -run='^$$' -fuzz=FuzzParseAndExecute -fuzztime=15s -parallel=2
	go test ./packages/core -run='^$$' -fuzz=FuzzDecodeBackup -fuzztime=15s -parallel=2
	go test ./packages/index -run='^$$' -fuzz=FuzzCompactCodec -fuzztime=15s -parallel=2
	go test ./packages/store -run='^$$' -fuzz=FuzzCommitRoundTrip -fuzztime=30s -parallel=2

bench:
	go test ./packages/query ./packages/store -run='^$$' -bench=. -benchmem -benchtime=10x

fmt:
	gofmt -w .

clean:
	rm -rf bin dist THIRD_PARTY_LICENSES
