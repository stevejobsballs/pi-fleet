VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)

# Reproducible, static builds (DESIGN.md §9.1).
GOFLAGS_BUILD = -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version=$(VERSION)"

.PHONY: all build build-arm64 test vet fmt clean

all: vet test build

build:
	CGO_ENABLED=0 go build $(GOFLAGS_BUILD) -o bin/pi-fleet ./cmd/pi-fleet

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) \
		go build $(GOFLAGS_BUILD) -o dist/pi-fleet_$(VERSION)_linux_arm64/pi-fleet ./cmd/pi-fleet

test:
	go test ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt needed" && exit 1)

fmt:
	gofmt -w .

clean:
	rm -rf bin dist
