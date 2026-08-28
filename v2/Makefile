# Mirrors the steps ../.github/workflows/tests.yml's go-test job runs, so
# `make` reproduces CI locally instead of needing paraphrased README
# instructions kept in sync by hand.

BIN := bin
CMDS := grain

# --- ICU linkage ------------------------------------------------------
#
# Embedded Dolt pulls in go-icu-regex, whose cgo preamble links
# -licui18n -licuuc -licudata (../v2/README.md, "Two things the port
# corrected"). Linked dynamically, the binary records versioned SONAMEs
# -- libicui18n.so.74 -- and refuses to start against a host whose ICU
# major differs: Bookworm ships 72, Trixie and Ubuntu 24.04 ship 74. That
# silently couples the machine that builds a binary to the machine that
# runs it, and the failure lands at exec time on the target, not at build
# time where someone would see it. Linking ICU's static archives instead
# removes the coupling for about +31 MB (grain: 148 MB -> 179 MB, almost
# all of it libicudata.a -- unchanged since bwsalmon/agents#313 folded
# graind, ui and mcpserver into this one binary too). libstdc++, libgcc
# and libc stay dynamic, which a Debian host provides anyway.
#
# The archives are named by absolute path rather than with -Wl,-Bstatic
# because ld64 (macOS) does not understand -Bstatic, while both linkers
# understand an archive path. go-icu-regex's own -licui18n and friends
# still follow on the link line; by then they resolve nothing and are
# dropped, which `ldd` confirms -- no libicu*.so in the result.
#
# icu_static is go-icu-regex's own build tag. On ELF it changes nothing
# (it only adds -DU_STATIC_IMPLEMENTATION, which matters for Windows
# dllimport); it is passed because it is what the package documents for
# this linkage, not because the link fails without it.
#
# Build and test targets deliberately differ here: `test` and `vet` stay
# dynamic so they keep mirroring tests.yml, and only the binaries -- the
# things you would actually ship to a controller -- are linked static.
# ICU_STATIC=0 opts out. If the archives are missing (libicu-dev on
# Debian/Ubuntu, libicu-devel on Fedora, `brew install icu4c` on macOS)
# the build says so and falls back to dynamic rather than failing.
ICU_STATIC ?= 1
ICU_LIBDIR ?= $(shell pkg-config --variable=libdir icu-uc 2>/dev/null \
	|| { brew --prefix icu4c 2>/dev/null && echo /lib; } | tr -d '\n')
ICU_ARCHIVES := $(wildcard $(ICU_LIBDIR)/libicui18n.a $(ICU_LIBDIR)/libicuuc.a \
	$(ICU_LIBDIR)/libicudata.a)

ifeq ($(ICU_STATIC),1)
ifeq ($(words $(ICU_ARCHIVES)),3)
ICU_LDFLAGS := $(ICU_ARCHIVES)
ICU_TAGS := -tags icu_static
else
$(warning ICU static archives not found under "$(ICU_LIBDIR)" -- linking ICU \
dynamically. Install libicu-dev (Debian/Ubuntu), libicu-devel (Fedora) or \
icu4c (brew), or set ICU_STATIC=0 to silence this.)
endif
endif

# --- Containerised build ----------------------------------------------
#
# `make build` links against whatever ICU, GCC and ld the machine running
# it happens to have, which is right on a host whose toolchain agrees with
# its own prebuilt ICU archives and wrong on one where it does not. Two
# ways it goes wrong: where GCC writes .sframe unwind sections that the
# installed ld is too old to read, every member of libicuuc.a links with a
# "section ignored" warning; and where glibc is newer than the target's,
# the binary links clean and then fails to exec on the controller, which
# is the coupling static ICU was adopted to remove, reappearing one layer
# down in libc.
#
# `make container-build` runs this same `make build`, out of this same
# Makefile, inside Dockerfile.build's pinned Debian 12 toolchain -- the
# release packer/kontur/image.pkr.hcl and terraform/gcp/variables.tf
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

.PHONY: all build test vet fmt clean builder container-build $(CMDS)

all: vet test build

build: $(CMDS)

$(CMDS):
	CGO_LDFLAGS="$(ICU_LDFLAGS)" go build $(ICU_TAGS) -o $(BIN)/$@ ./cmd/$@

builder:
	$(CONTAINER_ENGINE) build \
		--build-arg GO_IMAGE=$(GO_IMAGE) \
		--build-arg GO_VERSION=$(GO_VERSION) \
		-t $(BUILDER_IMAGE) -f Dockerfile.build .

# The whole checkout is mounted, not just this directory: `go build`
# stamps the binary with the commit it came from, which it can only read
# from the .git at the repository root.
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
		-v "$(CURDIR)/..":/src \
		-v "$(CONTAINER_CACHE)/build":/gocache \
		-v "$(CONTAINER_CACHE)/mod":/gomodcache \
		-e GOCACHE=/gocache -e GOMODCACHE=/gomodcache -e HOME=/tmp \
		-e GIT_CONFIG_COUNT=1 \
		-e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/src \
		-w /src/v2 $(BUILDER_IMAGE) make build

test:
	go test ./...

vet:
	go vet ./...

# CI has no equivalent fmt check; this just fails the way `go vet` does
# when a file needs gofmt, instead of only listing it.
fmt:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

clean:
	rm -rf $(BIN) $(CONTAINER_CACHE)
