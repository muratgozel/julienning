# julienning — POSIX sh recipes; works with macOS make (GNU make 3.81).

SHELL := /bin/sh

MODULE  := github.com/muratgozel/julienning
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# `make release VERSION=X.Y.Z` passes the target version here (VERSION itself is the build stamp).
VERSION_ARG := $(VERSION)
# The toolchain in go.mod is authoritative; never download another one.
GO      := GOTOOLCHAIN=local go
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version

BIN := bin/julienning

.PHONY: all build install test test-go test-worker e2e lint clean release

all: build

build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)=$(VERSION)" -o $(BIN) ./cmd/julienning
	@echo "built $(BIN) ($(VERSION))"

# Same layout as scripts/install.sh and `julienning update`, with version
# `dev`: the binary goes to <versions dir>/dev and <bin dir>/julienning is
# atomically repointed at it. Honors JULIENNING_BIN_DIR,
# JULIENNING_VERSIONS_DIR and XDG_DATA_HOME like the installer.
install:
	@set -eu; \
	bin_dir="$${JULIENNING_BIN_DIR:-$$HOME/.local/bin}"; \
	versions_dir="$${JULIENNING_VERSIONS_DIR:-$${XDG_DATA_HOME:-$$HOME/.local/share}/julienning/versions}"; \
	mkdir -p "$$bin_dir" "$$versions_dir"; \
	versions_dir="$$(CDPATH='' cd -- "$$versions_dir" && pwd)"; \
	link="$$bin_dir/julienning"; \
	if [ -d "$$link" ]; then echo "make install: $$link is a directory; remove it and retry" >&2; exit 1; fi; \
	staged="$$versions_dir/.julienning-dev.$$$$.tmp"; \
	tmp_link="$$bin_dir/.julienning.$$$$.tmp"; \
	trap 'rm -f "$$staged" "$$tmp_link"' EXIT; \
	$(GO) build -trimpath -ldflags "$(LDFLAGS)=dev" -o "$$staged" ./cmd/julienning; \
	chmod 755 "$$staged"; \
	mv -f "$$staged" "$$versions_dir/dev"; \
	rm -f "$$tmp_link"; \
	ln -s "$$versions_dir/dev" "$$tmp_link"; \
	mv -f "$$tmp_link" "$$link"; \
	echo "installed $$link -> $$versions_dir/dev"; \
	case ":$$PATH:" in *":$$bin_dir:"*|*":$$bin_dir/:"*) ;; *) echo "note: $$bin_dir is not on your PATH";; esac; \
	echo "next: julienning setup"

test: test-go test-worker

test-go:
	$(GO) test -race ./...

test-worker:
	cd worker && npm test

# End-to-end check in a throwaway HOME against a local Worker and a fake
# releases host; nothing on this machine is touched (see scripts/e2e.sh).
e2e:
	bash scripts/e2e.sh

lint:
	@out=`gofmt -l .`; \
	if [ -n "$$out" ]; then \
		echo "gofmt: these files need formatting (run: gofmt -w .):" >&2; \
		echo "$$out" >&2; \
		exit 1; \
	fi
	$(GO) vet ./...
	cd worker && npm run typecheck

clean:
	rm -rf bin dist

# release VERSION=0.2.0: tag vVERSION and push it; GitHub Actions builds the
# archives. Refuses a malformed version, a dirty tree, a branch other than
# main, an existing tag, or failing tests.
release:
	@v='$(VERSION_ARG)'; \
	case "$$v" in \
	  [0-9]*.[0-9]*.[0-9]*) ;; \
	  *) echo "usage: make release VERSION=X.Y.Z (got '$$v')" >&2; exit 2 ;; \
	esac; \
	echo "$$v" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "VERSION must be X.Y.Z, got '$$v'" >&2; exit 2; }; \
	[ -z "$$(git status --porcelain)" ] || { echo "working tree is not clean; commit or stash first" >&2; exit 1; }; \
	[ "$$(git rev-parse --abbrev-ref HEAD)" = main ] || { echo "release from main (on $$(git rev-parse --abbrev-ref HEAD))" >&2; exit 1; }; \
	! git rev-parse -q --verify "refs/tags/v$$v" >/dev/null || { echo "tag v$$v already exists" >&2; exit 1; }; \
	$(MAKE) test && \
	git tag -a "v$$v" -m "julienning $$v" && git push origin "v$$v" && \
	echo "tagged and pushed v$$v; watch: gh run list --workflow release"
