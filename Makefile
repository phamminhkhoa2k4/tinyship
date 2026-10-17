# TinyShip build entrypoints. CI runs `make ci`; local runs should match.

GO      ?= go
VERSION ?= 0.1.0-dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  = -X github.com/tinyship/tinyship/internal/version.Version=$(VERSION) \
           -X github.com/tinyship/tinyship/internal/version.Commit=$(COMMIT)

.PHONY: build test vet fmt-check ci clean

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/tinyship ./cmd/tinyship

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt-check:
	@fmt_out=$$(gofmt -l .); if [ -n "$$fmt_out" ]; then \
		echo "gofmt needed on:"; echo "$$fmt_out"; exit 1; fi

ci: fmt-check vet test build

clean:
	rm -rf bin
