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

.PHONY: all build test vet fmt clean $(CMDS)

all: vet test build

build: $(CMDS)

$(CMDS):
	CGO_LDFLAGS="$(ICU_LDFLAGS)" go build $(ICU_TAGS) -o $(BIN)/$@ ./cmd/$@

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
	rm -rf $(BIN)
