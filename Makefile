BINARY := bootstrap
CMD := ./cmd/bootstrap
DIST := ./dist

.PHONY: build fmt

build:
	mkdir -p $(DIST)
	go build -ldflags="-s -w" -trimpath -o $(DIST)/$(BINARY) $(CMD)

fmt:
	gofmt -w ./cmd ./internal
