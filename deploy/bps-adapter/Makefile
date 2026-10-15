GO ?= go
# Preserve CPA's x/net HTTP/2 implementation on Go 1.27; its new net/http
# wrapper changes reservation/retirement semantics relied on by CPA transport.
TAGS := http2legacy
.PHONY: test race build
test:
	$(GO) test -tags=$(TAGS) -timeout 120s ./...
race:
	$(GO) test -race -tags=$(TAGS) -timeout 180s ./internal ./internal/proxyutil
build:
	CGO_ENABLED=0 $(GO) build -tags=$(TAGS) -trimpath -o bps-adapter ./cmd/server
