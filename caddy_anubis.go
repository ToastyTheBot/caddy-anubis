package caddyanubis

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/ToastyTheBot/anubis"
	libanubis "github.com/ToastyTheBot/anubis/lib"
	"github.com/ToastyTheBot/anubis/lib/policy"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// anubisServer holds the most recently provisioned Anubis server.
// The static middleware reads this to serve /.within.website/ assets.
var anubisServer atomic.Pointer[libanubis.Server]

// nextHandlerCtxKey is used to pass the Caddy next handler through the
// request context, avoiding shared mutable state on the middleware struct.
type nextHandlerCtxKey struct{}

func init() {
	caddy.RegisterModule(AnubisMiddleware{})
	caddy.RegisterModule(anubisStaticMiddleware{})
	caddy.RegisterModule(initAnubisMiddleware{})
	httpcaddyfile.RegisterHandlerDirective("anubisStatic", parseAnubisStatic)
	httpcaddyfile.RegisterHandlerDirective("anubis", parseAnubis)
	httpcaddyfile.RegisterDirectiveOrder("anubis", httpcaddyfile.After, "invoke")
	httpcaddyfile.RegisterDirective("init_anubis", parseInitAnubis)
	httpcaddyfile.RegisterDirectiveOrder("init_anubis", httpcaddyfile.Before, "invoke")
}

// AnubisMiddleware is a Caddy middleware that integrates Anubis proof-of-work
// challenges to protect upstream resources from scraper bots and AI crawlers.
type AnubisMiddleware struct {
	Target     *string `json:"target,omitempty"`
	PolicyFile string  `json:"policy_file,omitempty"`

	anubisPolicy *policy.ParsedConfig
	anubisServer *libanubis.Server
	logger       *zap.Logger
}

func (AnubisMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.anubis",
		New: func() caddy.Module { return new(AnubisMiddleware) },
	}
}

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

	// If init_anubis has not provisioned a global server yet, store ours
	// so that static asset routes can delegate to it.
	anubisServer.CompareAndSwap(nil, m.anubisServer)
	m.logger.Info("anubis middleware provisioned")
	return nil
}

func (m *AnubisMiddleware) Validate() error { return nil }

func (m *AnubisMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	ctx := context.WithValue(r.Context(), nextHandlerCtxKey{}, next)
	m.anubisServer.ServeHTTP(w, r.WithContext(ctx))
	return nil
}

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

func parseAnubis(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m AnubisMiddleware
	m.UnmarshalCaddyfile(h.Dispenser)
	return &m, nil
}

// anubisStaticMiddleware serves Anubis static assets (/.within.website/) by
// delegating to the Anubis server provisioned by the anubis directive.
// Used for backward compatibility when init_anubis is not present.
type anubisStaticMiddleware struct{}

func (anubisStaticMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.anubis_static",
		New: func() caddy.Module { return new(anubisStaticMiddleware) },
	}
}

func (anubisStaticMiddleware) Provision(caddy.Context) error { return nil }
func (anubisStaticMiddleware) Validate() error               { return nil }

func (anubisStaticMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	srv := anubisServer.Load()
	if srv == nil || !strings.HasPrefix(r.URL.Path, "/.within.website/") {
		return next.ServeHTTP(w, r)
	}
	r.RequestURI = r.URL.RequestURI()
	srv.ServeHTTP(w, r)
	return nil
}

func (anubisStaticMiddleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name
	return nil
}

func parseAnubisStatic(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m anubisStaticMiddleware
	m.UnmarshalCaddyfile(h.Dispenser)
	return m, nil
}

// initAnubisMiddleware provisions the global Anubis server and serves static
// assets at /.within.website/. This directive should be placed at the server
// level (not inside a handle block) to ensure static assets are accessible
// regardless of which paths the anubis directive protects.
type initAnubisMiddleware struct {
	PolicyFile   string `json:"policy_file,omitempty"`
	anubisPolicy *policy.ParsedConfig
	anubisServer *libanubis.Server
	logger       *zap.Logger
}

func (initAnubisMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.init_anubis",
		New: func() caddy.Module { return new(initAnubisMiddleware) },
	}
}

func (m *initAnubisMiddleware) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger().Named("init_anubis")

	pol, err := libanubis.LoadPoliciesOrDefault(ctx, m.PolicyFile, anubis.DefaultDifficulty, "info")
	if err != nil {
		return err
	}
	m.anubisPolicy = pol

	m.anubisServer, err = libanubis.New(libanubis.Options{
		Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}),
		Policy:           pol,
		ServeRobotsTXT:   true,
		CookieExpiration: anubis.CookieDefaultExpirationTime,
	})
	if err != nil {
		return err
	}

	anubisServer.Store(m.anubisServer)
	m.logger.Info("init_anubis provisioned")
	return nil
}

func (initAnubisMiddleware) Validate() error { return nil }

func (m *initAnubisMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if strings.HasPrefix(r.URL.Path, "/.within.website/") {
		r.RequestURI = r.URL.RequestURI()
		m.anubisServer.ServeHTTP(w, r)
		return nil
	}
	return next.ServeHTTP(w, r)
}

func (m *initAnubisMiddleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
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

func parseInitAnubis(h httpcaddyfile.Helper) ([]httpcaddyfile.ConfigValue, error) {
	if !h.Next() {
		return nil, h.ArgErr()
	}

	d := h.Dispenser
	var m initAnubisMiddleware

	for d.NextBlock(0) {
		switch d.Val() {
		case "policy_file":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			m.PolicyFile = d.Val()
		default:
			return nil, d.Errf("unrecognized option: %s", d.Val())
		}
	}

	mod, _ := caddy.GetModule("http.handlers.init_anubis")
	var warnings []caddyconfig.Warning
	jsonObj := caddyconfig.JSONModuleObject(m, "handler", mod.ID.Name(), &warnings)

	matcherSet := caddy.ModuleMap{
		"path": caddyconfig.JSON(caddyhttp.MatchPath{"/.within.website/*"}, nil),
	}

	return []httpcaddyfile.ConfigValue{
		{
			Class: "route",
			Value: caddyhttp.Route{
				MatcherSetsRaw: []caddy.ModuleMap{matcherSet},
				HandlersRaw:    []json.RawMessage{jsonObj},
			},
		},
	}, nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*AnubisMiddleware)(nil)
	_ caddy.Validator             = (*AnubisMiddleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*AnubisMiddleware)(nil)
	_ caddyfile.Unmarshaler       = (*AnubisMiddleware)(nil)
	_ caddy.Provisioner           = (*initAnubisMiddleware)(nil)
	_ caddy.Validator             = (*initAnubisMiddleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*initAnubisMiddleware)(nil)
	_ caddyfile.Unmarshaler       = (*initAnubisMiddleware)(nil)
)
