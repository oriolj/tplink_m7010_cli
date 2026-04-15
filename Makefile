BINARY       := tplink-m7010
PREFIX       ?= $(HOME)/.local
BINDIR       := $(PREFIX)/bin
CONFDIR      := $(HOME)/.config/tplink-m7010
WAYBAR_DIR   := $(HOME)/.config/waybar
GO           ?= go
GOFLAGS      ?= -trimpath -ldflags="-s -w"

.PHONY: all build run tui waybar raw clean install uninstall install-waybar fmt vet tidy

all: build

build: $(BINARY)

$(BINARY): main.go client.go go.mod go.sum
	$(GO) build $(GOFLAGS) -o $@ .

run: tui

tui: build
	./$(BINARY)

waybar: build
	./$(BINARY) --waybar

raw: build
	./$(BINARY) --raw --debug

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -f $(BINARY)

install: build
	install -d $(BINDIR)
	install -m755 $(BINARY) $(BINDIR)/$(BINARY)
	@echo "Installed to $(BINDIR)/$(BINARY)"
	@if [ ! -f $(CONFDIR)/password ]; then \
		install -d -m700 $(CONFDIR); \
		echo "Create $(CONFDIR)/password (chmod 600) with your admin password"; \
	fi

uninstall:
	rm -f $(BINDIR)/$(BINARY)

# Drops the waybar wrapper script only. The caller must still edit
# config.jsonc + style.css — see WAYBAR.md.
install-waybar:
	install -d $(WAYBAR_DIR)/scripts
	install -m755 contrib/mifi.sh $(WAYBAR_DIR)/scripts/mifi.sh
	@echo "Installed wrapper to $(WAYBAR_DIR)/scripts/mifi.sh"
	@echo "Reload waybar with: pkill -SIGUSR2 waybar"
