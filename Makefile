.PHONY: build test test-race

build:
	CGO_ENABLED=1 go build -o bin/wormtamer ./cmd/wormtamer

test:
	CGO_ENABLED=1 go test ./...

test-race:
	CGO_ENABLED=1 go test -race ./...
