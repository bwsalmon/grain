# Mirrors the steps .github/workflows/tests.yml's go-test job runs, so
# `make` reproduces CI locally instead of needing paraphrased README
# instructions kept in sync by hand.

BIN := bin
CMDS := grain

BUILDVCS ?= auto

# The sandbox container this build of grain goes with -- stamped into the
# binary, so a deployment needs to be told nothing and a rollback to an
# older tag asks for its *own* older sandbox rather than today's. See
# cmd/grain/sandboximage.go for what reads it and why it is a build-time
# value at all.
#
# Empty here on purpose: the source default in that file (the tag CI
# keeps pointed at main) is what an unstamped `make build` should get,
# and stamping an empty string over it would replace a real answer with
# no answer. .github/workflows/build-artifacts.yml passes the exact
# sha- tag of the sandbox image built from this same commit.
SANDBOX_IMAGE ?=
LDFLAGS := $(if $(SANDBOX_IMAGE),-X main.defaultSandboxImage=$(SANDBOX_IMAGE),)

# --- Containerised build ----------------------------------------------
#
# grain is pure Go (bwsalmon/agents#366 removed the one dependency that
# wasn't -- embedded Dolt's go-icu-regex, which needed cgo and, through
# it, a build host whose ICU archives and glibc agreed with the
# controller's), so `make build` alone is already what most checkouts
# want: no C toolchain, no ICU headers, and the binary it produces is
# statically linked with no runtime library coupling to carry to the
# controller at all.
#
# `make container-build` runs this same `make build`, out of this same
# Makefile, inside Dockerfile.build's pinned Debian 12 toolchain -- the
# release scripts/kontur/build-guest.sh and terraform/gcp/variables.tf
# both deploy to -- so neither depends on the machine that drove it. It
# is not the default: it wants a container engine, and a first run pays
# for an image pull and a cold module cache, which is a poor trade on a
# host that agrees with itself. `make build` stays what CI and a working
# laptop use.
CONTAINER_ENGINE ?= docker
# Substituted whole on a network that does not reach Docker Hub, e.g.
# GO_IMAGE=mirror.gcr.io/library/golang.
GO_IMAGE ?= golang
BUILDER_IMAGE ?= grain-builder:bookworm
# Read out of go.mod rather than written down a second time here: the Go
# the container carries and the one the module asks for cannot drift if
# only one of them is ever edited. GOTOOLCHAIN=local in Dockerfile.build
# turns a stale image into an error that names both versions.
GO_VERSION := $(shell sed -n 's/^go[[:space:]]\{1,\}//p' go.mod)
# Bind-mounted rather than left inside the container: a build cache that
# dies with the container makes every run a cold one. Under the working
# tree (and .gitignore'd) rather than in a named volume so that `rm -rf`
# is enough to clear it and nothing survives the checkout.
CONTAINER_CACHE ?= $(CURDIR)/.container-cache
# Runs as the invoking user so bin/grain and the caches come out owned by
# whoever ran make, not by root. Rootless podman already maps the caller
# to the container's root and needs this left empty -- CONTAINER_USER=
# does that, and the flag disappears with it.
CONTAINER_USER ?= $(shell id -u):$(shell id -g)

# --- the deployment image ---------------------------------------------
#
# `make image` builds Dockerfile (not Dockerfile.build: that one is the
# toolchain `container-build` above runs in) -- the runtime image a
# deployment actually runs, carrying bin/grain and every binary it shells
# out to. .github/workflows/build-artifacts.yml builds the same image
# the same way on every commit and pushes it to GHCR, so this target is
# for a local build against a change to the Dockerfile itself, not
# something a deployment ever needs to run.
#
# The context is the repository root, which is this directory: konturctl
# is built from third_party/kontur and `go build`'s commit stamp comes
# from .git. GO_VERSION is passed for the same reason `builder` passes
# it -- one version, read out of go.mod.
IMAGE ?= grain:dev
.PHONY: all build test test-e2e vet fmt clean builder container-build image frontend loadtest $(CMDS)

all: vet test build

build: frontend $(CMDS)

# ui/ (bwsalmon/agents#356) is React+Vite, built into pkg/ui/static --
# the directory server.go embeds -- rather than checked in itself, so
# this has to run before `go build`/`go vet`/`go test` can see real
# content there. `npm ci` rather than `npm install`: a reproducible
# install from package-lock.json, the same reason `go build` trusts
# go.sum over re-resolving go.mod.
#
# The find clears out a previous build first rather than leaving Vite's
# own emptyOutDir to do it: that would also take pkg/ui/static's own
# .gitignore, .gitkeep and placeholder.html with it, the three files
# that let go:embed compile against this generated directory on a fresh
# checkout that has never run `npm run build` at all.
#
# placeholder.html is exempt because it is *tracked*, unlike everything
# else this deletes. Removing it left the checkout permanently dirty,
# and scripts/setup.sh's own sync_repo refuses to update a dirty
# checkout rather than clobber what might be an operator's edit -- so on
# a host that builds in place from a git clone, which is exactly what
# terraform/gcp deploys, the first successful build broke every
# deploy after it.
#
# Keeping it cannot ship a placeholder in place of a real UI, which is
# what deleting it was guarding against: server.go serves this directory
# with http.FileServerFS, so "/" resolves to index.html once a build
# exists and placeholder.html is reachable only at its own path.
frontend:
	find pkg/ui/static -mindepth 1 -not -name '.gitignore' -not -name '.gitkeep' \
		-not -name 'placeholder.html' -delete
	cd ui && npm ci && npm run build

# BUILDVCS is passed explicitly rather than left to `go build`'s default
# (which is this same `auto`) so that turning it off is a documented word
# on the command line -- `make build BUILDVCS=false`, and likewise
# `make container-build BUILDVCS=false`, which forwards it -- rather than
# an edit to this file.
#
# It is worth reaching for exactly once: when `go build` stops with
# "error obtaining VCS status: exit status 128". Go reports every failure
# of the git it shells out to under that one message, and the underlying
# reason is usually ownership (see container-build's safe.directory) but
# can be a .git the build cannot read at all -- an unreadable index, or a
# worktree or submodule whose real gitdir is outside the mount. What is
# lost is the commit stamp: `go version -m bin/grain` stops reporting
# vcs.revision, and nothing else changes.
#
# CGO_ENABLED=0 here, and only here: with no cgo anywhere left in the
# dependency graph, the default (CGO_ENABLED=1) build still links
# dynamically against libc -- os/user and net's own cgo-based lookups
# pull it in even though nothing in this module writes any C -- which is
# exactly the "binary needs a newer glibc than the controller has"
# coupling embedded Dolt's ICU dependency used to cause one layer up.
# Forcing it off produces a genuinely static binary instead, with nothing
# left to carry to the controller. `test` and `vet` below leave
# CGO_ENABLED alone: `go test -race` requires it (the race detector is a
# cgo library), and nothing about testing this module ships anywhere.
$(CMDS):
	CGO_ENABLED=0 go build -buildvcs=$(BUILDVCS) -ldflags "$(LDFLAGS)" -o $(BIN)/$@ ./cmd/$@

builder:
	$(CONTAINER_ENGINE) build \
		--build-arg GO_IMAGE=$(GO_IMAGE) \
		--build-arg GO_VERSION=$(GO_VERSION) \
		-t $(BUILDER_IMAGE) -f Dockerfile.build .

# The whole checkout is mounted, `.git` included: `go build` stamps the
# binary with the commit it came from, which it can only read from the
# .git at the repository root.
#
# Which is also why safe.directory is set. The uid inside the container
# need not own the mounted tree -- it does not under rootless podman,
# which maps the invoking user to the container's root, nor under Docker
# with userns-remap, nor on a checkout owned by another account -- and
# git refuses to read a repository it sees as someone else's ("detected
# dubious ownership"). `go build` does not treat that as "no VCS here"
# and carry on; it stops with "error obtaining VCS status: exit status
# 128", and the suggestion it prints, -buildvcs=false, buys the build at
# the cost of the commit stamp. Passed as GIT_CONFIG_* rather than
# `git config --global` so the exception lasts exactly one container and
# covers exactly the mount, instead of being written into a gitconfig
# somewhere that outlives the build.
container-build: builder
	@mkdir -p $(CONTAINER_CACHE)/build $(CONTAINER_CACHE)/mod
	$(CONTAINER_ENGINE) run --rm \
		$(if $(CONTAINER_USER),--user $(CONTAINER_USER),) \
		-v "$(CURDIR)":/src \
		-v "$(CONTAINER_CACHE)/build":/gocache \
		-v "$(CONTAINER_CACHE)/mod":/gomodcache \
		-e GOCACHE=/gocache -e GOMODCACHE=/gomodcache -e HOME=/tmp \
		-e GIT_CONFIG_COUNT=1 \
		-e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/src \
		-w /src $(BUILDER_IMAGE) make build BUILDVCS=$(BUILDVCS)

# Both need pkg/ui/static populated first, same as build: go:embed
# fails to compile -- not just to run -- against a static/ holding
# nothing but .gitkeep.
test: frontend
	go test -race ./...
	cd ui && npm test

# ui/e2e (bwsalmon/agents#415) drives the real built frontend through a
# real Chromium, over a real pkg/ui.Server (playwright.config.js's
# webServer runs `go run ../cmd/grain demo`) -- unlike `test` above,
# that needs a real browser on the machine, which `npx playwright
# install --with-deps` fetches and apt-installs itself.
# Left out of `test`/`all` and its own target instead: forcing a ~300MB
# browser download and a handful of apt packages (X11, fonts, codecs --
# see playwright.config.js) on every `make test` would be a poor trade
# for a checkout that just wants the unit suite; CI runs this as its own
# job (.github/workflows/tests.yml) so the coverage still exists.
test-e2e: frontend
	cd ui && npx playwright install --with-deps chromium && npm run test:e2e

vet: frontend
	go vet ./...

# The sustained concurrent-load harness (e2e/loadtest_test.go,
# bwsalmon/agents#416) -- skipped by `test` above (GRAIN_LOAD_TEST unset)
# since it takes minutes rather than seconds and is meant to be run by
# hand against a host actually sized for it, not on every commit. See
# that file's own doc comment for how to size it up further; every
# GRAIN_LOAD_TEST_* env var below is optional.
loadtest: frontend
	GRAIN_LOAD_TEST=1 go test ./e2e/... -run TestLoadSustainedConcurrency -v -timeout 30m

# CI has no equivalent fmt check; this just fails the way `go vet` does
# when a file needs gofmt, instead of only listing it.
fmt:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

clean:
	rm -rf $(BIN) $(CONTAINER_CACHE)

image:
	$(CONTAINER_ENGINE) build \
		--build-arg GO_IMAGE=$(GO_IMAGE) \
		--build-arg GO_VERSION=$(GO_VERSION) \
		--build-arg BUILDVCS=$(BUILDVCS) \
		$(if $(SANDBOX_IMAGE),--build-arg SANDBOX_IMAGE=$(SANDBOX_IMAGE),) \
		-t $(IMAGE) -f Dockerfile .
