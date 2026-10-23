VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= /usr/local
LDFLAGS := -s -w -X github.com/pedrospdc/apple-container-compose/internal/compose.Version=$(VERSION)
PLUGIN_DIR ?= $(PREFIX)/libexec/container-plugins/compose

.PHONY: build test install install-plugin uninstall clean release-snapshot

build:
	go build -ldflags "$(LDFLAGS)" -o bin/container-compose ./cmd/container-compose

test:
	go vet ./...
	go test ./...

# Standalone binary: `container-compose up`
install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/container-compose $(PREFIX)/bin/container-compose
	@# Offers a docker-compose alias when none is installed.
	-@PATH="$(PREFIX)/bin:$$PATH" $(PREFIX)/bin/container-compose alias

# Apple container CLI plugin: `container compose up`
install-plugin: build
	install -d $(PLUGIN_DIR)/bin
	install -m 0755 bin/container-compose $(PLUGIN_DIR)/bin/compose
	printf 'abstract = "Define and run multi-container applications (Docker Compose)"\nauthor = "apple-container-compose"\nversion = 0.1\n' > $(PLUGIN_DIR)/config.toml

uninstall:
	-@$(PREFIX)/bin/container-compose alias --remove
	rm -f $(PREFIX)/bin/container-compose
	rm -rf $(PLUGIN_DIR)

# Local dry run of the release pipeline (no publishing).
release-snapshot:
	HOMEBREW_TAP_DEPLOY_KEY=unused go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish

clean:
	rm -rf bin dist
