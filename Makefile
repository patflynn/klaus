.PHONY: build test lint vet clean install

BINARY := klaus
BUILD_DIR := bin

build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/klaus/

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
