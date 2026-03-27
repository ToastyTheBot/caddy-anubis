package caddyanubis

import (
	"context"
	"net/http"

	"github.com/ToastyTheBot/anubis"
	libanubis "github.com/ToastyTheBot/anubis/lib"
	"github.com/ToastyTheBot/anubis/lib/policy"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// nextHandlerCtxKey is used to pass the Caddy next handler through the
// request context, avoiding shared mutable state on the middleware struct.
type nextHandlerCtxKey struct{}

func init() {
	caddy.RegisterModule(AnubisMiddleware{})
	httpcaddyfile.RegisterHandlerDirective("anubis", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("anubis", httpcaddyfile.After, "templates")
}

type AnubisMiddleware struct {
	Target     *string `json:"target,omitempty"`
	PolicyFile string  `json:"policy_file,omitempty"`

	anubisPolicy *policy.ParsedConfig
	anubisServer *libanubis.Server
	logger       *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (AnubisMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.anubis",
		New: func() caddy.Module { return new(AnubisMiddleware) },
	}
}

// Provision implements caddy.Provisioner.
func (m *AnubisMiddleware) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger().Named("anubis")

	pol, err := libanubis.LoadPoliciesOrDefault(ctx, m.PolicyFile, anubis.DefaultDifficulty, "info")
	if err != nil {
		return err
	}
	m.anubisPolicy = pol

	m.anubisServer, err = libanubis.New(libanubis.Options{
		Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m.Target != nil {
				http.Redirect(w, r, *m.Target, http.StatusTemporaryRedirect)
				return
			}
			next, ok := r.Context().Value(nextHandlerCtxKey{}).(caddyhttp.Handler)
			if ok && next != nil {
				if err := next.ServeHTTP(w, r); err != nil {
					m.logger.Error("downstream handler error", zap.Error(err))
				}
			}
		}),
		Policy:           m.anubisPolicy,
		ServeRobotsTXT:   true,
		CookieExpiration: anubis.CookieDefaultExpirationTime,
	})
	if err != nil {
		return err
	}

	m.logger.Info("anubis middleware provisioned")
	return nil
}

// Validate implements caddy.Validator.
func (m *AnubisMiddleware) Validate() error {
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m *AnubisMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	ctx := context.WithValue(r.Context(), nextHandlerCtxKey{}, next)
	m.anubisServer.ServeHTTP(w, r.WithContext(ctx))
	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *AnubisMiddleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "target":
			if d.NextArg() {
				val := d.Val()
				m.Target = &val
			}
		case "policy_file":
			if d.NextArg() {
				m.PolicyFile = d.Val()
			}
		default:
			return d.Errf("unrecognized option: %s", d.Val())
		}
	}

	return nil
}

// parseCaddyfile unmarshals tokens from h into a new Middleware.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m AnubisMiddleware
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return &m, err
}

// Interface guards
var (
	_ caddy.Provisioner           = (*AnubisMiddleware)(nil)
	_ caddy.Validator             = (*AnubisMiddleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*AnubisMiddleware)(nil)
	_ caddyfile.Unmarshaler       = (*AnubisMiddleware)(nil)
)
