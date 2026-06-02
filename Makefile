VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test lint run release

build:
	go build -o bin/tend ./cmd/tend

test:
	go test ./...

# lint: runs go vet (swap in golangci-lint when adopted)
lint:
	go vet ./...

run:
	go run ./cmd/tend

release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags "-s -w -X github.com/marsadhq/tend/internal/cli.Version=$(VERSION)" \
		-o dist/tend-linux-amd64 ./cmd/tend
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags "-s -w -X github.com/marsadhq/tend/internal/cli.Version=$(VERSION)" \
		-o dist/tend-linux-arm64 ./cmd/tend
