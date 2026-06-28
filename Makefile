.PHONY: build test test-e2e lint vet clean install

BINARY := klaus
BUILD_DIR := bin
VERSION := $(shell cat VERSION)
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dirty")
LDFLAGS := -ldflags "-X github.com/patflynn/klaus/internal/cmd.version=$(VERSION)-$(GIT_SHA)"

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/klaus/

test:
	go test ./...

# End-to-end tests drive the real binary against an isolated tmux server.
# Kept separate from `test` so the default suite needs no tmux.
test-e2e:
	go test -tags e2e ./e2e/...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

install: build
	mkdir -p $(HOME)/.local/bin
	cp $(BUILD_DIR)/$(BINARY) $(HOME)/.local/bin/$(BINARY)

clean:
	rm -rf $(BUILD_DIR)

all: vet test build
