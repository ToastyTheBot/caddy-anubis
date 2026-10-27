# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with codebase.

## Project Overview

This is a **Caddy HTTP server module** that integrates [Anubis](https://github.com/TecharoHQ/anubis) proof-of-work challenges to protect upstream resources from scraper bots and AI crawlers. It provides one Caddyfile directive, `anubis`, usable in two positions:

- **Top level of a site block** - Provisions the shared site Anubis server, serves static assets at `/.within.website/`, and records its `policy_file` as the site default policy. Place once per site, before the handle blocks. With `protect` it also challenges every non-static request.
- **Inside `handle`/`route` blocks** - Middleware gate. Each block provisions its **own** Anubis server (challenge store, signing key, per-gate cookie names), so gates are independent: clearing one neither auto-validates another nor overwrites its clearance. Accepts `policy_file` (default: the site policy) and `target`.

## Key Dependencies

- **Caddy v2** (`github.com/caddyserver/caddy/v2`) - The host server framework.
- **Anubis fork** (`github.com/ToastyTheBot/anubis`) - Fork of upstream Anubis with generated frontend assets checked in. Module path rewritten from `TecharoHQ` to `ToastyTheBot`. **Fork tags must always match upstream tags exactly** — the fork is the same version, just with build assets committed. Never suggest bumping to a new patch version (e.g., `v1.25.1`) to work around module proxy cache issues; resolve those through cache cleaning or `GOPROXY=direct` instead.

## Architecture

The entire plugin is a single Go file: `caddy_anubis.go`. One module, `AnubisMiddleware` (`http.handlers.anubis`), backs the unified directive; `RegisterDirective("anubis", parseAnubis)` serves both positions and `inHandleBlock` (Helper `State` vs `BlockState` map identity, requires caddy >= v2.11.7, pinned by adapt tests) picks the flavor:

- **Top-level flavor** - `authoritative:true` instances provision the site Anubis server (`Store` into `anubisServer atomic.Pointer[libanubis.Server]`) with `policy_file`/`target`/`protect`, record `policy_file` in Helper `State` (`statePolicyKey`) as the site default, and emit routes: the static route (`/.within.website/*`, `protect:false`) plus, with `protect`, a catch-all or matcher-scoped challenge route (inline `/x` or named `@m` matcher tokens supported). Site instances use the base Anubis cookie names (`cookieID ""`).
- **Handle-block flavor** - always provisions a dedicated `libanubis.Server` (never touches the global pointer). Policy = own `policy_file` or the site default from State; `target` is per-gate. The parser assigns each gate a 1-based per-site `Instance` counter (State key `stateGateCountKey`, stable across reloads since config order is fixed), and `cookieID = sha256(policyFile|target|instance)[:8]`, so gates with identical policy and target still get distinct cookie names. The instance registers in the `apiGates` registry.
- **Per-gate cookie renaming** - the lib uses fixed cookie names (`techaro.lol-anubis*`), so two gates would overwrite each other's clearances in one browser. Gate instances wrap the writer (`gateCookieWriter`: renames `Set-Cookie` to `<base>__<cookieID>` at WriteHeader) and the request (`renameRequestCookies`: maps `<base>__<cookieID>` back to `<base>` and drops other gates' cookies) around `server.ServeHTTP`.
- **API dispatch** - the pass-challenge/honeypot API lives under `anubis.APIPrefix` inside the static route. `serveAPI` picks candidate gates by renamed-cookie presence, serves each with a buffered writer, and confirms ownership with miss detection (500 "getChallenge failed" / missing test cookie after rename); it falls back to all gates then the global server. Dispatched `Set-Cookie` headers are renamed to the issuing gate's names before copy-out.
- **Shared pieces** - static assets are served from the global server, falling back to the instance's own. `releaseGlobalServer` re-points the global at a surviving gate when the authoritative instance cleans up. `ensureRealIP` sets `X-Real-IP` from the peer when absent (Anubis requires and trusts it); `ServeHTTP` restores `RequestURI`. Next handler is passed to Anubis via `context.WithValue(nextHandlerCtxKey)`.
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

The local Caddyfile serves on `:8080`: top-level `anubis` with `protect off` plus `policy_file tests/testdata/challenge.yaml` (static assets + site default policy), an independent gate on `/admin/*` with `tests/testdata/challenge2.yaml`, and a root gate inheriting the site policy in front of the `web/` file server.

## Git Identity

Never modify the configured local/global/repository git author. Always set identity via runtime flags only:

```bash
git -c user.name="Claude Mythos" -c user.email=noreply@anthropic.com commit -m "${MESSAGE}"
```

## CI/CD

- **build.yml** - On push/PR: runs `xcaddy build`, `./caddy version`, and `go vet ./...`. Requires `GOPRIVATE` env vars.

The Anubis fork has its own **upstream-sync.yml** workflow that fetches upstream release tags, builds frontend assets, and pushes tagged commits to the fork.

Tests live in `tests/` (`go test ./tests/`): `adapt_test.go` covers the Caddyfile adapter for both directive positions (gate options, site-policy inheritance, error cases); `serve_test.go` covers ServeHTTP behavior (independent gates, per-gate cookie isolation, API dispatch to the issuing gate, chained target short-circuit, challenge/pass/deny/static, global cleanup re-pointing) against `testdata/*.yaml` policies. `go vet ./...` is the static check.
