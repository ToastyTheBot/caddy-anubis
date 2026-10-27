# caddy-anubis

A Caddy plugin that integrates [Anubis](https://github.com/TecharoHQ/anubis) proof-of-work challenges to protect upstream resources from scraper bots and AI crawlers.

## Installation

Build Caddy with this plugin using xcaddy:

```bash
xcaddy build --with github.com/ToastyTheBot/caddy-anubis
```

## Usage

Two directives are available:

- **`init_anubis`** — server-level directive that provisions the Anubis server and serves static challenge assets at `/.within.website/`. Place once per server block, outside any `handle` blocks. With its `protect` option it also challenges every non-static request and, on success, continues to the rest of the site, so one directive is enough to protect a whole server block.
- **`anubis`** — handler-level middleware that protects specific paths. Use inside `handle` blocks to protect root or subdirectory paths.

The `anubis` middleware sets `X-Real-IP` from the peer address when the request has none; Anubis requires that header. When Caddy runs behind another proxy or TLS terminator, set `X-Real-IP` in that front proxy — Anubis trusts the header unconditionally.

### Recommended Setup

```caddy
:8080 {
    init_anubis          # Global: provisions server + serves static assets

    handle {
        anubis           # Protect all paths under this handler
        file_server
    }
}
```

### Single-Directive Protection

`init_anubis` alone can protect the entire site; the remaining directives run after the challenge passes:

```caddy
:8080 {
    init_anubis {
        protect
    }

    file_server
}
```

### Path-Scoped Protection

Protect only specific paths while leaving others public:

```caddy
:8080 {
    init_anubis

    handle /admin/* {
        anubis
        respond "Admin area"
    }

    handle {
        respond "Public page"
    }
}
```

### `anubis` Options

```caddy
anubis {
    # Redirect to a fixed URL instead of proxying to the next handler
    target https://example.com

    # Path to a custom Anubis policy file
    policy_file /etc/anubis/policy.yaml
}
```

### `init_anubis` Options

```caddy
init_anubis {
    # Challenge all non-static requests (default: off, static assets only)
    protect

    # Path to a custom Anubis policy file
    policy_file /etc/anubis/policy.yaml
}
```

## Automated updates

Renovate runs daily from `.github/workflows/renovate.yml` and opens pull
requests to bump `github.com/ToastyTheBot/anubis`, Caddy, and GitHub
Actions. It needs a repository secret `RENOVATE_TOKEN`: a fine-grained PAT
with Contents, Pull requests, and Workflows write access. If you install the
Renovate GitHub App on the repository instead, delete the workflow; the
`renovate.json` config works for both.

## Credits

- [Anubis](https://github.com/TecharoHQ/anubis) - the proof-of-work challenge engine.
