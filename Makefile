BIN := proton-drive-fs
PKG := ./cmd/proton-drive-fs
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
PREFIX ?= $(HOME)/.local
MP ?= $(HOME)/ProtonDrive
GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2

.PHONY: all build test race lint check generate install uninstall clean restart status packages aur-check help

all: build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

test:
	go test ./...

race:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
	@go vet ./...
	@$(GOLANGCI) run ./...

check: test lint

generate:
	go generate ./internal/tray ./internal/about

install: build
	install -Dm755 $(BIN) $(PREFIX)/bin/$(BIN)
	install -Dm644 contrib/proton-drive-fs.desktop $(PREFIX)/share/applications/proton-drive-fs.desktop
	install -Dm644 contrib/icons/proton-drive-fs.png $(PREFIX)/share/icons/hicolor/64x64/apps/proton-drive-fs.png
	install -Dm644 contrib/systemd/proton-drive-fs.service $(HOME)/.config/systemd/user/proton-drive-fs.service
	install -Dm644 contrib/systemd/proton-drive-fs-tray.service $(HOME)/.config/systemd/user/proton-drive-fs-tray.service
	@systemctl --user daemon-reload 2>/dev/null || true
	@echo "Installed. To enable:"
	@echo "  systemctl --user enable --now proton-drive-fs-tray"
	@echo "  systemctl --user enable --now proton-drive-fs"
	@echo "Note: systemd units reference ~/.local/bin/$(BIN). If PREFIX is changed, edit the units."

uninstall:
	@rm -f $(PREFIX)/bin/$(BIN)
	@rm -f $(PREFIX)/share/applications/proton-drive-fs.desktop
	@rm -f $(PREFIX)/share/icons/hicolor/64x64/apps/proton-drive-fs.png
	@rm -f $(HOME)/.config/systemd/user/proton-drive-fs.service
	@rm -f $(HOME)/.config/systemd/user/proton-drive-fs-tray.service
	@systemctl --user daemon-reload 2>/dev/null || true

clean:
	rm -f $(BIN)
	rm -rf bin/

restart:
	-./$(BIN) unmount $(MP)
	$(MAKE) build
	./$(BIN) mount $(MP)

status:
	./$(BIN) status $(MP)

packages:
	go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish,docker

aur-check:
	go run github.com/goreleaser/goreleaser/v2@latest check

help:
	@printf "Usage: make [target]\n\nTargets:\n"
	@printf "  all\t\tBuild the project (default)\n"
	@printf "  build\t\tBuild the binary\n"
	@printf "  test\t\tRun tests\n"
	@printf "  race\t\tRun tests with race detector\n"
	@printf "  lint\t\tRun linters (gofmt, go vet, golangci-lint)\n"
	@printf "  check\t\tRun tests and linters\n"
	@printf "  generate\tRun go generate for internal/tray icons and internal/about licenses\n"
	@printf "  install\tBuild and install to \$$PREFIX (default: \$$HOME/.local)\n"
	@printf "  uninstall\tRemove installed files\n"
	@printf "  clean\t\tRemove built binary and bin/ directory\n"
	@printf "  restart\tUnmount, rebuild, and remount MP (default: \$$HOME/ProtonDrive)\n"
	@printf "  status\tShow proton-drive-fs status for MP (default: \$$HOME/ProtonDrive)\n"
	@printf "  packages\tBuild local deb/rpm/apk/pkg.tar.zst packages into dist/\n"
	@printf "  aur-check\tValidate .goreleaser.yaml, including the AUR publishers\n"
	@printf "  help\t\tShow this message\n"
