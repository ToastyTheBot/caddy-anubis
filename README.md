# caddy-anubis

A Caddy plugin that integrates [Anubis](https://github.com/TecharoHQ/anubis) proof-of-work challenges to protect upstream resources from scraper bots and AI crawlers.

## Installation

Build Caddy with this plugin using xcaddy:

```bash
xcaddy build --with github.com/ToastyTheBot/caddy-anubis
```

## Usage

One directive, `anubis`, used in two positions:

- **Top level of a site block** — provisions the shared Anubis server and serves the static challenge assets under `/.within.website/`. Place it once per site, outside any `handle` block, before the blocks. Its `policy_file` also becomes the default policy that handle-block gates inherit. With the `protect` option it additionally challenges every non-static request and, on success, continues to the rest of the site, so one directive is enough to protect a whole site.
- **Inside `handle` (or `route`) blocks** — a middleware gate. Each block gets its own Anubis server: its own challenge store, signing key and cookie names. Clearing one gate never auto-validates another and never overwrites another gate's clearance in the browser. A block without `policy_file` inherits the top-level directive's policy.

The pass-challenge API lives under `/.within.website/`; requests to it are dispatched to the gate that issued the challenge, so gates stay independent even though they share the static route.

The `anubis` middleware sets `X-Real-IP` from the peer address when the request has none; Anubis requires that header. When Caddy runs behind another proxy or TLS terminator, set `X-Real-IP` in that front proxy — Anubis trusts the header unconditionally.

### Recommended Setup

```caddy
:8080 {
    anubis               # Global: provisions server + serves static assets

    handle {
        anubis           # Protect all paths under this handler
        file_server
    }
}
```

### Site-Wide Protection

The top-level directive alone can protect the entire site; the remaining directives run after the challenge passes:

```caddy
:8080 {
    anubis {
        protect
    }

    file_server
}
```

### Independent Gates in Multiple Handle Blocks

Each `anubis` block is a separate gate. Clients verify per gate; the two gates below never see each other's clearances:

```caddy
:8080 {
    anubis {             # Static assets + default policy for the gates
        protect off
        policy_file /etc/anubis/site.yaml
    }

    handle /admin/* {
        anubis {         # Own store, key and cookies
            policy_file /etc/anubis/admin.yaml
        }
        respond "Admin area"
    }

    handle /api/* {
        anubis           # Inherits /etc/anubis/site.yaml
        respond "API"
    }

    handle {
        respond "Public page"
    }
}
```

### `anubis` Options (top level)

```caddy
anubis {
    # Challenge all non-static requests (default: off, static assets only)
    protect

    # Redirect to a fixed URL instead of proxying to the next handler
    target https://example.com

    # Path to a custom Anubis policy file (also the site default policy)
    policy_file /etc/anubis/policy.yaml
}
```

Inside `handle` blocks `anubis` accepts `policy_file` and `target`. Both are per-gate: a gate with `target` redirects verified clients to its own URL, and a gate's `policy_file` selects only that gate's rules.

### Migrating from `init_anubis`

`init_anubis` is now the top-level behavior of `anubis` — rename the directive. Handle-block `anubis` instances are independent gates: each provisions its own server, and `policy_file`/`target` are per-gate options again.

## Automated updates

Renovate runs daily from `.github/workflows/renovate.yml` and opens pull
requests to bump `github.com/ToastyTheBot/anubis`, Caddy, and GitHub
Actions. It needs a repository secret `RENOVATE_TOKEN`: a fine-grained PAT
with Contents, Pull requests, and Workflows write access. If you install the
Renovate GitHub App on the repository instead, delete the workflow; the
`renovate.json` config works for both.

## Credits

- [Anubis](https://github.com/TecharoHQ/anubis) - the proof-of-work challenge engine.
