VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_DIR := build
GO := go
GOFLAGS := -trimpath
LDFLAGS := -ldflags "-s -w -X main.Version=$(VERSION)"

BINARIES := convocate convocate-host convocate-agent

.PHONY: all generate build build-convocate build-convocate-host build-convocate-agent install clean fmt lint lint-fmt lint-go lint-yaml lint-json lint-vuln test test-unit test-integration test-e2e release release/major release/minor

all: lint test build

generate:
	@echo "Generating embedded assets..."
	$(GO) generate ./internal/assets/
	@echo "Assets generated."

build: build-convocate build-convocate-host build-convocate-agent
	@echo "Build complete: $(BINARIES:%=$(BUILD_DIR)/%)"

build-convocate: generate
	@echo "Building convocate $(VERSION)..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/convocate ./cmd/convocate/

build-convocate-host:
	@echo "Building convocate-host $(VERSION)..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/convocate-host ./cmd/convocate-host/

build-convocate-agent:
	@echo "Building convocate-agent $(VERSION)..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/convocate-agent ./cmd/convocate-agent/

install: build
	@echo "Installing convocate, convocate-host, convocate-agent to /usr/local/bin..."
	sudo install -m 0755 $(BUILD_DIR)/convocate  /usr/local/bin/convocate
	sudo install -m 0755 $(BUILD_DIR)/convocate-host   /usr/local/bin/convocate-host
	sudo install -m 0755 $(BUILD_DIR)/convocate-agent  /usr/local/bin/convocate-agent
	@echo "Running 'convocate install' to finish setup..."
	sudo /usr/local/bin/convocate install

clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)
	@rm -f coverage.out coverage.html
	@$(GO) clean -testcache
	@echo "Clean complete."

lint: lint-fmt lint-go lint-yaml lint-vuln
	@echo "All linters passed."

# fmt rewrites sources in place; lint-fmt only reports. Formatting drifted
# unnoticed across 24 files during the project-wide rename — the longer
# identifiers broke struct field alignment and nothing in lint looked at
# gofmt. Keep lint-fmt in the lint chain so that can't recur.
fmt:
	@echo "Formatting Go sources..."
	@gofmt -w .

lint-fmt:
	@echo "Checking Go formatting..."
	@unformatted=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt-clean:"; \
		echo "$$unformatted" | sed 's/^/  /'; \
		echo "Run 'make fmt' to fix."; \
		exit 1; \
	fi

lint-go:
	@echo "Running Go linter..."
	go vet -v ./...

lint-yaml:
	@echo "Running YAML linter..."
	@find . -name '*.yml' -o -name '*.yaml' | grep -v vendor | grep -v node_modules | xargs yamllint -s

# lint-vuln scans the call graph against the Go vulnerability database
# (vuln.go.dev). Auto-installs govulncheck if missing — first run pulls
# the binary into $(go env GOBIN) (or ~/go/bin); subsequent runs hit the
# cached binary.
lint-vuln:
	@echo "Running govulncheck against vuln.go.dev..."
	@command -v govulncheck >/dev/null 2>&1 || $(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	@PATH="$$(go env GOBIN):$$(go env GOPATH)/bin:$$PATH" govulncheck ./...

test: test-unit test-integration
	@echo "All tests passed."

test-unit:
	@echo "Running unit tests..."
	$(GO) test -v -race -count=1 -coverprofile=coverage.out ./internal/...
	@$(GO) tool cover -func=coverage.out | tail -1
	@echo "Unit tests complete."

test-integration:
	@echo "Running integration tests..."
	$(GO) test -v -race -count=1 -tags=integration ./test/integration/...
	@echo "Integration tests complete."

test-e2e:
	@echo "Running end-to-end tests..."
	$(GO) test -v -count=1 -tags=e2e -timeout=300s ./test/e2e/...
	@echo "End-to-end tests complete."

test-coverage: test-unit
	@echo "Generating coverage report..."
	@$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

release:
	@LATEST=$$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0"); \
	MAJOR=$$(echo "$$LATEST" | sed 's/^v//' | cut -d. -f1); \
	MINOR=$$(echo "$$LATEST" | sed 's/^v//' | cut -d. -f2); \
	PATCH=$$(echo "$$LATEST" | sed 's/^v//' | cut -d. -f3); \
	NEXT="v$$MAJOR.$$MINOR.$$((PATCH + 1))"; \
	echo "Bumping $$LATEST -> $$NEXT"; \
	git tag "$$NEXT" && git push --tags && \
	echo "Released $$NEXT"

release/minor:
	@LATEST=$$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0"); \
	MAJOR=$$(echo "$$LATEST" | sed 's/^v//' | cut -d. -f1); \
	MINOR=$$(echo "$$LATEST" | sed 's/^v//' | cut -d. -f2); \
	NEXT="v$$MAJOR.$$((MINOR + 1)).0"; \
	echo "Bumping $$LATEST -> $$NEXT"; \
	git tag "$$NEXT" && git push --tags && \
	echo "Released $$NEXT"

release/major:
	@LATEST=$$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0"); \
	MAJOR=$$(echo "$$LATEST" | sed 's/^v//' | cut -d. -f1); \
	NEXT="v$$((MAJOR + 1)).0.0"; \
	echo "Bumping $$LATEST -> $$NEXT"; \
	git tag "$$NEXT" && git push --tags && \
	echo "Released $$NEXT"
