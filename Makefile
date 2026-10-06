VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Release public keys (minisign base64 lines, comma-separated) compiled in
# so updates can be verified. Without them a build refuses all updates.
RELEASE_KEYS ?= $(shell cat release-keys.txt 2>/dev/null | grep -v '^untrusted' | paste -sd, -)
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)

# Reproducible, static builds (DESIGN.md §9.1).
GOFLAGS_BUILD = -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version=$(VERSION) -X pi-fleet/internal/release.trustedKeys=$(RELEASE_KEYS)"

.PHONY: all build build-arm64 release test vet fmt clean

all: vet test build

build:
	CGO_ENABLED=0 go build $(GOFLAGS_BUILD) -o bin/pi-fleet ./cmd/pi-fleet

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) \
		go build $(GOFLAGS_BUILD) -o dist/pi-fleet_$(VERSION)_linux_arm64/pi-fleet ./cmd/pi-fleet

# make release VERSION=v1.0.0, then on an offline machine:
#   pi-fleet release-sign -key release.key -dir dist/v1.0.0 -version v1.0.0
release:
	@test -n "$(RELEASE_KEYS)" || (echo "RELEASE_KEYS is empty: add release-keys.txt" && exit 1)
	mkdir -p dist/$(VERSION)
	for arch in arm64 amd64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) \
			go build $(GOFLAGS_BUILD) -o dist/$(VERSION)/pi-fleet_$(VERSION)_linux_$$arch ./cmd/pi-fleet || exit 1; \
	done

test:
	go test ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt needed" && exit 1)

fmt:
	gofmt -w .

clean:
	rm -rf bin dist
