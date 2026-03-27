# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with codebase.

## Project Overview

This is a **Caddy HTTP server module** that integrates [Anubis](https://github.com/TecharoHQ/anubis) proof-of-work challenges to protect upstream resources from scraper bots and AI crawlers. It provides two Caddyfile directives:

- **`init_anubis`** - Server-level directive that provisions the global Anubis server and serves static assets at `/.within.website/`. Place once per server block.
- **`anubis`** - Handler-level middleware that can be used inside `handle` blocks to protect specific paths (root or subdirectory).

## Key Dependencies

- **Caddy v2** (`github.com/caddyserver/caddy/v2`) - The host server framework.
- **Anubis fork** (`github.com/ToastyTheBot/anubis`) - Fork of upstream Anubis with generated frontend assets checked in. Module path rewritten from `TecharoHQ` to `ToastyTheBot`. Uses matching upstream tags (e.g., `v1.25.0`).

## Architecture

The entire plugin is a single Go file: `caddy_anubis.go`. Three middleware types:

- **`initAnubisMiddleware`** - Provisions the global Anubis server (with a stub `http.NotFound` next handler) and serves `/.within.website/*` routes. Registered via `RegisterDirective` so it creates its own route with path matching, independent of `handle` blocks.
- **`AnubisMiddleware`** - Creates its own Anubis server per-instance with the correct Caddy passthrough (via `context.WithValue`). Optionally stores itself as the global server if `init_anubis` hasn't run. Supports `target <url>` and `policy_file <path>`.
- **`anubisStaticMiddleware`** - Backward compatibility handler that delegates `/.within.website/` to the global Anubis server.

The global Anubis server is shared via `sync/atomic.Pointer[libanubis.Server]`.

### Caddyfile Example

```caddy
:8080 {
    init_anubis          # Global: provisions server + serves static assets

    handle /admin/* {
        anubis           # Path-scoped: protects /admin/ only
        respond "Admin area"
    }

    handle {
        anubis           # Root-scoped: protects all paths
        file_server { root web/ }
    }
}
```

## Build and Run

### Prerequisites

```bash
export GOPRIVATE=github.com/ToastyTheBot/*
export GONOSUMCHECK=github.com/ToastyTheBot/*,all
export GONOSUMDB=github.com/ToastyTheBot/*
```

### Build Caddy with Plugin

The standard `xcaddy build` may fail due to Go module cache holding stale `TecharoHQ` module paths for the forked Anubis tag. Working build process:

1. Clone the fork: `git clone https://github.com/ToastyTheBot/anubis /tmp/anubis-fork`
2. Create a temp build dir with `main.go` (imports `github.com/ToastyTheBot/caddy-anubis`) and `go.mod` containing both replace directives:
   ```
   replace github.com/ToastyTheBot/caddy-anubis => /path/to/caddy-anubis
   replace github.com/ToastyTheBot/anubis => /tmp/anubis-fork
   ```
3. Run `go mod tidy` then `go build -o caddy -tags nobadger,nomysql,nopgx -trimpath .`

### Run with the Local Caddyfile

```bash
./caddy run --config Caddyfile
```

The local Caddyfile serves on `:8080` with `init_anubis` + `anubis` in a `handle` block, falling back to `web/` file server.

## Git Identity

Never modify the configured local/global/repository git author. Always set identity via runtime flags only:

```bash
git -c user.name="Claude Mythos" -c user.email=noreply@anthropic.com commit -m "${MESSAGE}"
```

## CI/CD

- **build.yml** - On push/PR: runs `xcaddy build`, `./caddy version`, and `go vet ./...`. Requires `GOPRIVATE` env vars.

The Anubis fork has its own **upstream-sync.yml** workflow that fetches upstream release tags, builds frontend assets, and pushes tagged commits to the fork.

There are no tests in this repository. `go vet ./...` is the only static check.
