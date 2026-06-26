BOOTSTRAP_BINARY := bootstrap
BOOTSTRAP_CMD := ./cmd/bootstrap
HOST_PROVIDER_BINARY := pulumi-resource-magnumhost
HOST_PROVIDER_CMD := ./cmd/pulumi-resource-magnumhost
DIST := ./dist
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REPOSITORY ?= ventus-ag/magnum-bootstrap
RELEASE_TAG ?= v1.0.0
LDFLAGS := -s -w -X github.com/ventus-ag/magnum-bootstrap/internal/buildinfo.Version=$(VERSION) -X github.com/ventus-ag/magnum-bootstrap/internal/buildinfo.Repository=$(REPOSITORY)

.PHONY: build fmt clean retag

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

# Force-move RELEASE_TAG (default v1.0.0) onto the current commit and push it,
# re-firing the release build. The launcher resolves releases/latest and pulls
# the new binary on every node, so a retag rolls the fix out fleet-wide.
# Requires a clean, pushed working tree. Override the tag with: make retag RELEASE_TAG=v1.0.1
retag:
	@test -z "$$(git status --porcelain)" || { echo "working tree dirty — commit first"; exit 1; }
	git tag -f -a $(RELEASE_TAG) -m "$(RELEASE_TAG)"
	git push -f origin refs/tags/$(RELEASE_TAG)
	@echo "retagged $(RELEASE_TAG) -> $$(git rev-parse --short HEAD); release build re-firing"
