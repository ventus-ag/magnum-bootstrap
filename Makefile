BINARY := bootstrap
CMD := ./cmd/bootstrap
DIST := ./dist
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build fmt clean

build:
	mkdir -p $(DIST)
	CGO_ENABLED=0 go build \
		-ldflags="-s -w -X main.version=$(VERSION)" \
		-trimpath -o $(DIST)/$(BINARY) $(CMD)

fmt:
	gofmt -w ./cmd ./internal

clean:
	rm -rf $(DIST)
