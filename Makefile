.PHONY: build test check

build:
	go build -o bin/concierge-dialer ./cmd/concierge-dialer

test:
	go test ./...

check:
	go test -race ./...
	go vet ./...
