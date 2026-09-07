# Hindsight developer gates. `make check` is the local CI equivalent
# (fast gates: fmt, codegen, hashes, vet, tests, lint; e2e runs in CI and
# via `make e2e` after `make e2e-setup`).
GO ?= go
GOLANGCI_VERSION ?= v2.13.2
GOLANGCI_LINT ?= golangci-lint
BIN ?= hindsight

.PHONY: build test vet fmt fmt-check lint generate generate-check hashes check clean e2e e2e-setup

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN) .

test:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt -l found unformatted files:"; gofmt -l .; exit 1)

lint:
	@if ! $(GOLANGCI_LINT) --version 2>/dev/null | grep -q "$$(echo $(GOLANGCI_VERSION) | sed 's/^v//')"; then \
		echo "want golangci-lint $(GOLANGCI_VERSION) (CI pins v2.13.2 for the v2 config schema); have: $$($(GOLANGCI_LINT) --version 2>/dev/null || echo missing)"; \
		echo "install: GOBIN=\$$$$(go env GOPATH)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)"; \
		exit 1; \
	fi
	$(GOLANGCI_LINT) run ./...

generate:
	$(GO) generate ./...

generate-check: generate
	git diff --exit-code -- '*_templ.go'

hashes:
	@if ls static/vendor/*.sha256 >/dev/null 2>&1; then \
		sha256sum -c static/vendor/*.sha256; \
	else \
		echo "no vendored hash files yet; skipping"; \
	fi

e2e-setup:
	cd e2e && npm ci && npx playwright install chromium

e2e:
	cd e2e && npx playwright test

# Full gate: formatting, codegen freshness, vendored hashes, vet, tests, lint.
check: fmt-check generate-check hashes vet test lint

clean:
	rm -f $(BIN) coverage.out
