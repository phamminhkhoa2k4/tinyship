# TinyShip build entrypoints. CI runs `make ci`; local runs should match.

GO      ?= go
VERSION ?= 0.1.0-dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DIST    ?= bin/release
RELEASE_VERSION := $(patsubst v%,%,$(VERSION))
LDFLAGS  = -X github.com/tinyship/tinyship/internal/version.Version=$(VERSION) \
           -X github.com/tinyship/tinyship/internal/version.Commit=$(COMMIT)
RELEASE_LDFLAGS = -X github.com/tinyship/tinyship/internal/version.Version=$(RELEASE_VERSION) \
                  -X github.com/tinyship/tinyship/internal/version.Commit=$(COMMIT)

.PHONY: build test vet fmt-check ci clean release

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

# release builds the artifacts published for a version tag. It refuses the
# dev version, renders a static linux/amd64 binary, and writes the SBOM and
# checksums beside it. Verify the directory before uploading it.
release:
	@case "$(RELEASE_VERSION)" in \
		""|*-dev) echo "release: VERSION must be a release version, got '$(VERSION)'" >&2; exit 1 ;; \
	esac
	@printf '%s\n' "$(RELEASE_VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$$' \
		|| { echo "release: VERSION must be semver, got '$(VERSION)'" >&2; exit 1; }
	rm -rf "$(DIST)"
	mkdir -p "$(DIST)"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -buildvcs=false \
		-ldflags "$(RELEASE_LDFLAGS)" -o "$(DIST)/tinyship-linux-amd64" ./cmd/tinyship
	cp install.sh "$(DIST)/install.sh"
	chmod 0755 "$(DIST)/install.sh"
	bash -n "$(DIST)/install.sh"
	$(GO) run ./cmd/sbomgen -version "$(RELEASE_VERSION)" -commit "$(COMMIT)" \
		-out "$(DIST)/tinyship-linux-amd64.cdx.json"
	cd "$(DIST)" && sha256sum tinyship-linux-amd64 install.sh tinyship-linux-amd64.cdx.json > SHA256SUMS
	cd "$(DIST)" && sha256sum -c SHA256SUMS
	@"$(DIST)/tinyship-linux-amd64" version
