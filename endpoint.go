package caddyanubis

import (
	"errors"
	"net/http"

	"github.com/TecharoHQ/anubis/lib"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

type Endpoint struct {
	server *lib.Server
}

func (e *Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	e.server.ServeHTTP(w, r)
	return nil
}

func (e *Endpoint) Provision(ctx caddy.Context) error {
	appRaw, err := ctx.App("anubis")
	if err != nil {
		return err
	}
	app := appRaw.(*App)

	server := app.GetServer()
	if server == nil {
		return errors.New("no global anubis app found")
	}
	e.server = server

	return nil
}

func (Endpoint) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.anubis_endpoint",
		New: func() caddy.Module { return new(Endpoint) },
	}
}

var (
	_ caddy.Provisioner           = (*Endpoint)(nil)
	_ caddyhttp.MiddlewareHandler = (*Endpoint)(nil)
)
