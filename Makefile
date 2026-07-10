BINARY       := tplink-m7010
PREFIX       ?= $(HOME)/.local
BINDIR       := $(PREFIX)/bin
CONFDIR_M7010:= $(HOME)/.config/tplink-m7010
CONFDIR_MUDI := $(HOME)/.config/gl-e5800
WAYBAR_DIR   := $(HOME)/.config/waybar
GO           ?= go
GOFLAGS      ?= -trimpath -ldflags="-s -w"

.PHONY: all build run waybar raw clean install uninstall install-waybar fmt vet tidy test

all: build

build: $(BINARY)

SRC := $(wildcard *.go)

$(BINARY): $(SRC) go.mod go.sum
	$(GO) build $(GOFLAGS) -o $@ .

run: build
	./$(BINARY)

waybar: build
	./$(BINARY) --waybar

raw: build
	./$(BINARY) --raw --debug

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

tidy:
	$(GO) mod tidy

clean:
	rm -f $(BINARY)

install: build
	install -d $(BINDIR)
	install -m755 $(BINARY) $(BINDIR)/$(BINARY)
	@echo "Installed to $(BINDIR)/$(BINARY)"
	@if [ ! -f $(CONFDIR_M7010)/password ] && [ ! -f $(CONFDIR_MUDI)/password ]; then \
		install -d -m700 $(CONFDIR_M7010) $(CONFDIR_MUDI); \
		echo "No password file found. Create one (chmod 600) for whichever device you use:"; \
		echo "  $(CONFDIR_M7010)/password   (TP-Link M7010)"; \
		echo "  $(CONFDIR_MUDI)/password    (GL.iNet Mudi GL-E5800)"; \
	fi

uninstall:
	rm -f $(BINDIR)/$(BINARY)

# Drops the waybar wrapper scripts. The caller must still edit
# config.jsonc + style.css — see WAYBAR.md.
install-waybar:
	install -d $(WAYBAR_DIR)/scripts
	install -m755 contrib/mifi.sh $(WAYBAR_DIR)/scripts/mifi.sh
	install -m755 contrib/mifi-tui.sh $(WAYBAR_DIR)/scripts/mifi-tui.sh
	@echo "Installed wrappers to $(WAYBAR_DIR)/scripts/"
	@echo "Reload waybar with: pkill -SIGUSR2 waybar"
