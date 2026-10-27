# caddy-anubis

A Caddy plugin that integrates [Anubis](https://github.com/TecharoHQ/anubis) proof-of-work challenges to protect upstream resources from scraper bots and AI crawlers.

## Installation

Build Caddy with this plugin using xcaddy:

```bash
xcaddy build --with github.com/ToastyTheBot/caddy-anubis
```

## Usage

One directive, `anubis`, used in two positions:

- **Top level of a site block** — provisions the shared Anubis server and serves the static challenge assets under `/.within.website/`. Place it once per site, outside any `handle` block. With its `protect` option it also challenges every non-static request and, on success, continues to the rest of the site, so one directive is enough to protect a whole site.
- **Inside `handle` (or `route`) blocks** — a middleware that challenges the block's requests. All instances in a site share one Anubis server: a client that solved the challenge in one `handle` block passes every other block without a second challenge.

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

### Path-Scoped Protection Across Multiple Handle Blocks

Protect only specific paths while leaving others public. The two protected blocks share one Anubis server, so a client verifies once for both:

```caddy
:8080 {
    anubis               # Provisions the shared server + static assets

    handle /admin/* {
        anubis
        respond "Admin area"
    }

    handle /api/* {
        anubis
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

    # Path to a custom Anubis policy file
    policy_file /etc/anubis/policy.yaml
}
```

Inside `handle` blocks `anubis` takes no options: policy and target belong to the top-level directive that provisions the shared server.

### Migrating from `init_anubis`

`init_anubis` is now the top-level behavior of `anubis` — rename the directive. Handle-block `anubis` instances no longer accept `policy_file`/`target`; move them to the top-level directive.

## Automated updates

Renovate runs daily from `.github/workflows/renovate.yml` and opens pull
requests to bump `github.com/ToastyTheBot/anubis`, Caddy, and GitHub
Actions. It needs a repository secret `RENOVATE_TOKEN`: a fine-grained PAT
with Contents, Pull requests, and Workflows write access. If you install the
Renovate GitHub App on the repository instead, delete the workflow; the
`renovate.json` config works for both.

## Credits

- [Anubis](https://github.com/TecharoHQ/anubis) - the proof-of-work challenge engine.
