.PHONY: build test lint vet clean install

BINARY := klaus
BUILD_DIR := bin
VERSION := $(shell cat VERSION)
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dirty")
LDFLAGS := -ldflags "-X github.com/patflynn/klaus/internal/cmd.version=$(VERSION)-$(GIT_SHA)"

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/klaus/

test:
	go test ./...

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
