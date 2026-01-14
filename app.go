package caddyanubis

import (
	"github.com/TecharoHQ/anubis/lib"
	"github.com/caddyserver/caddy/v2"
)

type App struct {
	server *lib.Server
}

func (a *App) GetServer() *lib.Server {
	return a.server
}

func (a *App) Provision(ctx caddy.Context) error {
	policy, err := lib.LoadPoliciesOrDefault(ctx.Context, "", 0)
	if err != nil {
		return err
	}

	server, err := lib.New(lib.Options{
		Policy:         policy,
		ServeRobotsTXT: true,
	})
	if err != nil {
		return err
	}

	a.server = server
	ctx.Logger().Debug("anubis instance provisioned")
	return nil
}

func (a *App) Validate() error {
	return nil
}

func (a *App) Start() error {
	return nil
}

func (a *App) Stop() error {
	return nil
}

func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "anubis",
		New: func() caddy.Module { return new(App) },
	}
}

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
	_ caddy.Validator   = (*App)(nil)
)
