BOOTSTRAP_BINARY := bootstrap
BOOTSTRAP_CMD := ./cmd/bootstrap
HOST_PROVIDER_BINARY := pulumi-resource-magnumhost
HOST_PROVIDER_CMD := ./cmd/pulumi-resource-magnumhost
DIST := ./dist
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REPOSITORY ?= ventus-ag/magnum-bootstrap
LDFLAGS := -s -w -X github.com/ventus-ag/magnum-bootstrap/internal/buildinfo.Version=$(VERSION) -X github.com/ventus-ag/magnum-bootstrap/internal/buildinfo.Repository=$(REPOSITORY)

.PHONY: build fmt clean

build:
	mkdir -p $(DIST)
	CGO_ENABLED=0 go build \
		-ldflags="$(LDFLAGS)" \
		-trimpath -o $(DIST)/$(BOOTSTRAP_BINARY) $(BOOTSTRAP_CMD)
	CGO_ENABLED=0 go build \
		-ldflags="$(LDFLAGS)" \
		-trimpath -o $(DIST)/$(HOST_PROVIDER_BINARY) $(HOST_PROVIDER_CMD)

fmt:
	gofmt -w ./cmd ./internal ./provider

clean:
	rm -rf $(DIST)
