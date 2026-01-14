package caddyanubis

import (
	"github.com/caddyserver/caddy/v2"
)

func init() {
	caddy.RegisterModule(App{})
	caddy.RegisterModule(Endpoint{})
	caddy.RegisterModule(Middleware{})
}
