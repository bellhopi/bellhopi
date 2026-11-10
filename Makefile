.PHONY: build test check

EXE := $(shell go env GOEXE)

build:
	go build -o bin/concierge-dialer$(EXE) ./cmd/concierge-dialer

test:
	go test ./...

check:
	go test -race ./...
	go vet ./...
