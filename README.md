# caddy-anubis

A Caddy plugin that integrates [Anubis](https://github.com/TecharoHQ/anubis) proof-of-work challenges to protect upstream resources from scraper bots and AI crawlers.

## Installation

Build Caddy with this plugin using xcaddy:

```bash
GOPRIVATE=github.com/ToastyTheBot/* xcaddy build --with github.com/ToastyTheBot/caddy-anubis
```

## Usage

Add `anubis` to your Caddyfile. It works both at the top level and inside `route`/`handle` blocks:

```caddy
:80 {
    handle {
        anubis
        reverse_proxy localhost:8080
    }
}
```

### Options

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
