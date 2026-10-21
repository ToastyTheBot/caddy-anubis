# caddy-anubis

A Caddy plugin that integrates [Anubis](https://github.com/TecharoHQ/anubis) proof-of-work challenges to protect upstream resources from scraper bots and AI crawlers.

## Installation

Build Caddy with this plugin using xcaddy:

```bash
xcaddy build --with github.com/ToastyTheBot/caddy-anubis
```

## Usage

Two directives are available:

- **`init_anubis`** — server-level directive that provisions the Anubis server and serves static challenge assets at `/.within.website/`. Place once per server block, outside any `handle` blocks.
- **`anubis`** — handler-level middleware that protects specific paths. Use inside `handle` blocks to protect root or subdirectory paths.

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

## Credits

- [Anubis](https://github.com/TecharoHQ/anubis) - the proof-of-work challenge engine.
