BINARY  := cronkite
# Derived from the tag, so a build never lies about its version. The leading "v"
# is stripped: the tag says v0.1.0, the binary reports 0.1.0.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
VERSION := $(if $(VERSION),$(VERSION),dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/jhuancz/cronkite/internal/version.Version=$(VERSION) \
	-X github.com/jhuancz/cronkite/internal/version.Commit=$(COMMIT) \
	-X github.com/jhuancz/cronkite/internal/version.Date=$(DATE)

.PHONY: build test vet fmt check clean install

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/cronkite

install:
	go install -ldflags '$(LDFLAGS)' ./cmd/cronkite

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Everything CI enforces. Run this before every commit.
check:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt: files need formatting" && exit 1)
	go vet ./...
	go test ./...

clean:
	rm -f $(BINARY) $(BINARY).exe
	rm -rf dist
