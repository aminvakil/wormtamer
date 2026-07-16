GO_TAGS := sqlite_fts5

.PHONY: build test test-race

build:
	CGO_ENABLED=1 go build -tags '$(GO_TAGS)' -o bin/wormtamer ./cmd/wormtamer

test:
	CGO_ENABLED=1 go test -tags '$(GO_TAGS)' ./...

test-race:
	CGO_ENABLED=1 go test -race -tags '$(GO_TAGS)' ./...
