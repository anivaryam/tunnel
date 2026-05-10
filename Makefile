.PHONY: build-server build-client build-client-daemon install install-daemon test test-daemon test-all clean

VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

build-server:
	go build $(LDFLAGS) -o bin/tunnel-server ./cmd/server

build-client:
	go build $(LDFLAGS) -o bin/tunnel ./cmd/tunnel

build-client-daemon:
	go build $(LDFLAGS) -tags daemon -o bin/tunnel ./cmd/tunnel

install: build-client-daemon
	cp bin/tunnel ~/.local/bin/tunnel

install-no-daemon: build-client
	cp bin/tunnel ~/.local/bin/tunnel

install-daemon: install

test:
	go test ./... -race -count=1

test-daemon:
	go test -tags daemon ./... -race -count=1

test-all: test test-daemon

clean:
	rm -rf bin/
