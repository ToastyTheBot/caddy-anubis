# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with codebase.

## Project Overview

This is a **Caddy HTTP server module** that integrates [Anubis](https://github.com/TecharoHQ/anubis) proof-of-work challenges to protect upstream resources from scraper bots and AI crawlers. It provides one Caddyfile directive, `anubis`, usable in two positions:

- **Top level of a site block** - Provisions the shared Anubis server and serves static assets at `/.within.website/`. Place once per site. With `protect` it also challenges every non-static request.
- **Inside `handle`/`route` blocks** - Middleware that challenges the block's requests. All instances share one Anubis server, so a solved challenge verifies in every block.

## Key Dependencies

- **Caddy v2** (`github.com/caddyserver/caddy/v2`) - The host server framework.
- **Anubis fork** (`github.com/ToastyTheBot/anubis`) - Fork of upstream Anubis with generated frontend assets checked in. Module path rewritten from `TecharoHQ` to `ToastyTheBot`. **Fork tags must always match upstream tags exactly** — the fork is the same version, just with build assets committed. Never suggest bumping to a new patch version (e.g., `v1.25.1`) to work around module proxy cache issues; resolve those through cache cleaning or `GOPROXY=direct` instead.

## Architecture

The entire plugin is a single Go file: `caddy_anubis.go`. One module, `AnubisMiddleware` (`http.handlers.anubis`), backs the unified directive; `RegisterDirective("anubis", parseAnubis)` serves both positions and `inHandleBlock` (Helper `State` vs `BlockState` map identity, requires caddy >= v2.11.7, pinned by adapt tests) picks the flavor:

- **Top-level flavor** - `authoritative:true` instances provision the shared Anubis server (`Store`) with `policy_file`/`target`/`protect`, and emit routes: the static route (`/.within.website/*`, `protect:false`) plus, with `protect`, a catch-all or matcher-scoped challenge route (inline `/x` or named `@m` matcher tokens supported).
- **Handle-block flavor** - `protect:true`, non-authoritative, no Caddyfile options (`policy_file`/`target` error and point at the top-level directive). Challenges the enclosing block; next handler is passed to Anubis via `context.WithValue(nextHandlerCtxKey)`.
- **Shared server** - every instance serves through `anubisServer atomic.Pointer[libanubis.Server]`; handle-block instances `CompareAndSwap` in a default server when none exists, the top-level instance `Store`s the authoritative one. One challenge store and session key site-wide. `liveServers` lets cleanup re-point the global at a survivor. `ensureRealIP` sets `X-Real-IP` from the peer when absent (Anubis requires and trusts it).
- **`anubisStaticMiddleware`** - Backward compatibility handler that delegates `/.within.website/` to the global Anubis server.

### Caddyfile Example

```caddy
:8080 {
    anubis               # Global: provisions server + serves static assets

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

### Build Caddy with Plugin

No Go environment overrides are needed. The fork publishes complete module
versions (compiled frontend assets committed by the `upstream-sync` workflow
in the anubis fork), so the module proxy serves them like any other module:

```bash
xcaddy build --with github.com/ToastyTheBot/caddy-anubis
```

If a build ever fails with `module declares its path as github.com/TecharoHQ/anubis`
or a `go:embed` "no matching files" error, the proxy cached an incomplete copy
of that anubis version. Fix the fork tag (assets must be committed:
`web/static/**`, `lib/challenge/**/static/**`, `xess/static/**`), then require
a version the proxy has not cached yet. Do not paper over it with GOPRIVATE
or GONOPROXY in consumer docs or CI.

For local fork development, a manual build is still available:

1. Clone the fork: `git clone https://github.com/ToastyTheBot/anubis /tmp/anubis-fork`
2. Create a temp build dir with `main.go` and `go.mod`:
   ```go
   // main.go
   package main
   import (
       _ "github.com/ToastyTheBot/caddy-anubis"
       _ "github.com/caddyserver/caddy/v2/modules/standard"
       caddycmd "github.com/caddyserver/caddy/v2/cmd"
   )
   func main() { caddycmd.Main() }
   ```
   `go.mod` must include both replace directives:
   ```
   replace github.com/ToastyTheBot/caddy-anubis => /path/to/caddy-anubis
   replace github.com/ToastyTheBot/anubis => /tmp/anubis-fork
   ```
3. Run `go mod tidy` then `go build -o caddy -tags nobadger,nomysql,nopgx -trimpath .`

**Note:** Importing `modules/standard` is required for standard Caddy directives like `request_header` to work.

### Run with the Local Caddyfile

```bash
./caddy run --config Caddyfile
```

The local Caddyfile serves on `:8080` with top-level `anubis` plus `anubis` in a `handle` block, falling back to `web/` file server.

## Git Identity

Never modify the configured local/global/repository git author. Always set identity via runtime flags only:

```bash
git -c user.name="Claude Mythos" -c user.email=noreply@anthropic.com commit -m "${MESSAGE}"
```

## CI/CD

- **build.yml** - On push/PR: runs `xcaddy build`, `./caddy version`, and `go vet ./...`. Requires `GOPRIVATE` env vars.

The Anubis fork has its own **upstream-sync.yml** workflow that fetches upstream release tags, builds frontend assets, and pushes tagged commits to the fork.

Tests live in `tests/` (`go test ./tests/`): `adapt_test.go` covers the Caddyfile adapter for both directive positions; `serve_test.go` covers ServeHTTP behavior (challenge/pass/deny/target/static/shared server) against `testdata/*.yaml` policies. `go vet ./...` is the static check.
