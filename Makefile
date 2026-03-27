ANUBIS_VERSION := $(shell grep 'github.com/TecharoHQ/anubis' go.mod | grep -v replace | awk '{print $$2}')
GOMODCACHE := $(shell go env GOMODCACHE)
# Go module cache uses lowercased-with-exclamation encoding for uppercase chars
ANUBIS_MOD_DIR := $(GOMODCACHE)/github.com/!techaro!h!q/anubis@$(ANUBIS_VERSION)

.PHONY: prep build clean

# Construct _anubis/ from the Go module cache source + committed assets.
# This is needed because Anubis's preact challenge embeds JS files that
# are not included in the Go module distribution.
prep:
	@go mod download github.com/TecharoHQ/anubis@$(ANUBIS_VERSION)
	@rm -rf _anubis
	@cp -r $(ANUBIS_MOD_DIR) _anubis
	@chmod -R u+w _anubis
	@cp -r _anubis_assets/* _anubis/
	@echo "Prepared _anubis/ with Anubis $(ANUBIS_VERSION) + generated assets"

build: prep
	xcaddy build \
		--with github.com/daegalus/caddy-anubis=. \
		--replace github.com/TecharoHQ/anubis=./_anubis

clean:
	rm -rf _anubis caddy
