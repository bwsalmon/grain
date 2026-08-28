# Mirrors the steps ../.github/workflows/tests.yml's go-test job runs, so
# `make` reproduces CI locally instead of needing paraphrased README
# instructions kept in sync by hand.

BIN := bin
CMDS := grain graind mcpserver ui

.PHONY: all build test vet fmt clean $(CMDS)

all: vet test build

build: $(CMDS)

$(CMDS):
	go build -o $(BIN)/$@ ./cmd/$@

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
