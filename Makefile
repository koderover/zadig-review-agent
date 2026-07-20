APP := zadig-review-agent
CMD := .
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PACKAGE := github.com/koderover/zadig-review-agent/internal/version
LDFLAGS := -s -w -X $(VERSION_PACKAGE).Version=$(VERSION) -X $(VERSION_PACKAGE).Commit=$(COMMIT) -X $(VERSION_PACKAGE).Date=$(BUILD_DATE)

.PHONY: all help fmt fmt-check verify vet test test-race check build install clean

all: check

help:
	@echo "Available targets:"
	@echo "  fmt         Format Go source files"
	@echo "  fmt-check   Check Go source formatting"
	@echo "  verify      Verify downloaded modules"
	@echo "  vet         Run go vet"
	@echo "  test        Run unit tests"
	@echo "  test-race   Run unit tests with the race detector"
	@echo "  check       Run all CI checks and build"
	@echo "  build       Build bin/$(APP)"
	@echo "  install     Install $(APP)"
	@echo "  clean       Remove build output"

fmt:
	gofmt -w cmd internal

fmt-check:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal && exit 1)

verify:
	go mod verify

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

check: fmt-check verify vet test-race build

build:
	mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) $(CMD)

install:
	go install -trimpath -ldflags "$(LDFLAGS)" $(CMD)

clean:
	rm -rf bin dist
