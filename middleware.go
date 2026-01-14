package caddyanubis

import (
	"errors"
	"net/http"

	"github.com/TecharoHQ/anubis/lib"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

type Middleware struct {
	server *lib.Server
}

func (m *Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	m.server.ServeHTTP(w, r)
	return next.ServeHTTP(w, r)
}

func (m *Middleware) Provision(ctx caddy.Context) error {
	appRaw, err := ctx.App("anubis")
	if err != nil {
		return err
	}
	app := appRaw.(*App)

	server := app.GetServer()
	if server == nil {
		return errors.New("no global anubis app found")
	}
	m.server = server

	return nil
}

func (Middleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.anubis",
		New: func() caddy.Module { return new(Middleware) },
	}
}

var (
	_ caddy.Provisioner           = (*Middleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*Middleware)(nil)
)
