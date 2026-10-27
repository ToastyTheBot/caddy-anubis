package caddyanubis

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
	"runtime/debug"
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

// staticPrefix is where Anubis serves its challenge assets and API, under
// the /.within.website/ namespace.
const staticPrefix = "/.within.website/"

// anubisServer is the single shared Anubis server for the process. Every
// anubis instance (top-level or inside a handle block) serves through it,
// so one solved challenge verifies everywhere.
var anubisServer atomic.Pointer[libanubis.Server]

// liveServers lists the servers of currently provisioned instances so a
// cleanup can re-point the global pointer at a surviving server instead of
// stranding requests on an unprovisioned one.
var liveServers atomic.Pointer[[]*libanubis.Server]

// nextHandlerCtxKey passes the Caddy next handler through the request
// context, avoiding shared mutable state on the middleware struct.
type nextHandlerCtxKey struct{}

func registerLiveServer(s *libanubis.Server) {
	for {
		old := liveServers.Load()
		next := []*libanubis.Server{s}
		if old != nil {
			next = append(next, *old...)
		}
		if liveServers.CompareAndSwap(old, &next) {
			return
		}
	}
}

func unregisterLiveServer(s *libanubis.Server) {
	for {
		old := liveServers.Load()
		if old == nil {
			return
		}
		next := make([]*libanubis.Server, 0, len(*old))
		for _, x := range *old {
			if x != s {
				next = append(next, x)
			}
		}
		if liveServers.CompareAndSwap(old, &next) {
			return
		}
	}
}

// releaseGlobalServer clears the global pointer when it still points at s
// and re-points it at any surviving provisioned server.
func releaseGlobalServer(s *libanubis.Server) {
	if !anubisServer.CompareAndSwap(s, nil) {
		return
	}
	if p := liveServers.Load(); p != nil {
		for _, other := range *p {
			if other != nil && other != s && anubisServer.CompareAndSwap(nil, other) {
				return
			}
		}
	}
}

// stampVersion replaces Anubis' "devel" version string with the module
// versions this binary was compiled with, e.g. "v1.28.2-v1.0.5", so the
// challenge page footer reflects the pinned releases. An xcaddy build has
// no ldflags, and Anubis only reads its own main module's version, which is
// Caddy's here.
func stampVersion() {
	if anubis.Version != "devel" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var anubisV, pluginV string
	for _, d := range bi.Deps {
		switch d.Path {
		case "github.com/ToastyTheBot/anubis":
			anubisV = d.Version
		case "github.com/ToastyTheBot/caddy-anubis":
			pluginV = d.Version
		}
	}
	if anubisV == "" || anubisV == "(devel)" {
		return
	}
	if pluginV == "" || pluginV == "(devel)" {
		anubis.Version = anubisV
		return
	}
	anubis.Version = anubisV + "-" + pluginV
}

func init() {
	stampVersion()
	caddy.RegisterModule(AnubisMiddleware{})
	caddy.RegisterModule(anubisStaticMiddleware{})
	httpcaddyfile.RegisterHandlerDirective("anubisStatic", parseAnubisStatic)
	httpcaddyfile.RegisterDirective("anubis", parseAnubis)
	httpcaddyfile.RegisterDirectiveOrder("anubis", httpcaddyfile.After, "invoke")
}

// AnubisMiddleware is the single module behind the anubis directive.
//
// At the top level of a site block it provisions the shared Anubis server
// and emits the static-asset route (and, with protect, the challenge
// route). Inside a handle or route block it is a middleware that challenges
// the block's requests. All instances share one Anubis server, so a client
// verified in one handle block passes every other block without a second
// challenge.
type AnubisMiddleware struct {
	// Target redirects verified requests to a fixed URL instead of
	// continuing to the next handler. Top-level only.
	Target *string `json:"target,omitempty"`
	// PolicyFile is the path to a custom Anubis policy file. Top-level only.
	PolicyFile string `json:"policy_file,omitempty"`
	// Protect challenges non-static requests. Set by the parser: true for
	// handle-block usage and the top-level protect route, false for the
	// static route. Unset means challenge (hand-written JSON default).
	Protect *bool `json:"protect,omitempty"`
	// Authoritative marks the top-level instance whose provisioning creates
	// the shared server with PolicyFile/Target and claims the global slot.
	Authoritative bool `json:"authoritative,omitempty"`

	anubisPolicy *policy.ParsedConfig
	server       *libanubis.Server
	logger       *zap.Logger
}

func (AnubisMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.anubis",
		New: func() caddy.Module { return new(AnubisMiddleware) },
	}
}

// challenges reports whether this instance challenges non-static requests.
func (m *AnubisMiddleware) challenges() bool {
	return m.Protect == nil || *m.Protect
}

func (m *AnubisMiddleware) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger().Named("anubis")

	// subrequestMode matches upstream: true unless a target redirects.
	pol, err := libanubis.LoadPoliciesOrDefault(ctx, m.PolicyFile, anubis.DefaultDifficulty, "info", m.Target == nil)
	if err != nil {
		return err
	}
	m.anubisPolicy = pol

	m.server, err = libanubis.New(libanubis.Options{
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
				return
			}
			http.NotFound(w, r)
		}),
		Policy:           m.anubisPolicy,
		ServeRobotsTXT:   true,
		CookieExpiration: anubis.CookieDefaultExpirationTime,
	})
	if err != nil {
		return err
	}

	if m.Authoritative {
		// The top-level instance owns the shared server configuration
		// (policy_file, target) and claims the global slot.
		anubisServer.Store(m.server)
	} else {
		// Handle-block instances join an existing shared server or seed a
		// default one until the top-level instance provisions its own.
		anubisServer.CompareAndSwap(nil, m.server)
	}
	registerLiveServer(m.server)
	m.logger.Info("anubis provisioned", zap.Bool("authoritative", m.Authoritative))
	return nil
}

func (m *AnubisMiddleware) Validate() error { return nil }

// Cleanup releases the global pointer when it still points at this
// instance's server and re-points it at a surviving server, so reloads
// neither strand static assets nor leave a dangling unprovisioned server.
func (m *AnubisMiddleware) Cleanup() error {
	if m.server == nil {
		return nil
	}
	unregisterLiveServer(m.server)
	releaseGlobalServer(m.server)
	return nil
}

// ensureRealIP sets X-Real-IP from the peer address when it is absent.
// Anubis requires the header (lib/anubis.go check()) and trusts it blindly,
// which is the contract for its reverse proxy; this plugin is that proxy.
// A value set upstream (request_header directive or a trusted proxy) wins.
func ensureRealIP(r *http.Request) {
	if r.Header.Get("X-Real-IP") != "" {
		return
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// Strip the IPv6 zone so anubis can parse the address.
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if host != "" {
		r.Header.Set("X-Real-IP", host)
	}
}

func (m *AnubisMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	srv := anubisServer.Load()
	if srv == nil {
		srv = m.server
	}
	if srv == nil {
		return next.ServeHTTP(w, r)
	}
	if strings.HasPrefix(r.URL.Path, staticPrefix) {
		// Challenge assets, the pass-challenge API, and the honeypot live
		// under the shared server; the instance that receives them does not
		// matter because every instance shares one challenge store.
		ensureRealIP(r)
		r.RequestURI = r.URL.RequestURI()
		srv.ServeHTTP(w, r)
		return nil
	}
	if !m.challenges() {
		return next.ServeHTTP(w, r)
	}
	ensureRealIP(r)
	ctx := context.WithValue(r.Context(), nextHandlerCtxKey{}, next)
	srv.ServeHTTP(w, r.WithContext(ctx))
	return nil
}

// UnmarshalCaddyfile parses the handle-block flavor: anubis takes no
// options there; site-wide configuration belongs to the top-level directive
// that provisions the shared server.
func (m *AnubisMiddleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name

	if d.NextArg() {
		return d.Errf("unexpected argument %q; configure policy_file and target on the top-level anubis directive", d.Val())
	}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "target", "policy_file":
			return d.Errf("%s is only supported on the top-level anubis directive", d.Val())
		default:
			return d.Errf("unrecognized option: %s", d.Val())
		}
	}
	return nil
}

// inHandleBlock reports whether the directive is being parsed inside a
// handle or route block rather than at the top level of a server block.
// Caddy evaluates server-block segments with BlockState aliasing the shared
// State map, and copies BlockState for each nested block (see
// httpcaddyfile parseSubroute), so map identity distinguishes the two
// positions. Pinned by TestAdaptHandleBlockUsesMiddlewareFlavor.
func inHandleBlock(h httpcaddyfile.Helper) bool {
	if h.State == nil || h.BlockState == nil {
		return h.Nesting() > 0
	}
	return reflect.ValueOf(h.State).Pointer() != reflect.ValueOf(h.BlockState).Pointer()
}

// parseAnubis implements the unified anubis directive for both positions.
func parseAnubis(h httpcaddyfile.Helper) ([]httpcaddyfile.ConfigValue, error) {
	if !h.Next() {
		return nil, h.ArgErr()
	}

	inBlock := inHandleBlock(h)

	matcherSet, err := h.ExtractMatcherSet()
	if err != nil {
		return nil, err
	}

	if inBlock {
		// Middleware flavor: challenge the enclosing block's requests.
		var m AnubisMiddleware
		if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
			return nil, err
		}
		m.Protect = new(true)
		return h.NewRoute(matcherSet, &m), nil
	}

	// Top-level flavor: provision the shared server and emit routes.
	d := h.Dispenser
	d.Next() // ExtractMatcherSet resets the dispenser; skip the directive name again
	var m AnubisMiddleware
	protected := false
	for d.NextBlock(0) {
		switch d.Val() {
		case "policy_file":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			m.PolicyFile = d.Val()
		case "target":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			m.Target = new(d.Val())
		case "protect":
			if d.NextArg() && d.Val() == "off" {
				m.Protect = new(false)
				protected = false
			} else {
				m.Protect = new(true)
				protected = true
			}
		default:
			return nil, d.Errf("unrecognized option: %s", d.Val())
		}
	}
	m.Authoritative = true

	staticVal := caddyconfig.JSONModuleObject(AnubisMiddleware{
		PolicyFile:    m.PolicyFile,
		Protect:       new(false),
		Authoritative: true,
	}, "handler", "anubis", nil)
	staticRoute := httpcaddyfile.ConfigValue{
		Class: "route",
		Value: caddyhttp.Route{
			MatcherSetsRaw: []caddy.ModuleMap{
				{"path": caddyconfig.JSON(caddyhttp.MatchPath{staticPrefix + "*"}, nil)},
			},
			HandlersRaw: []json.RawMessage{staticVal},
		},
	}

	if !protected {
		// Static assets and provisioning only; protection comes from
		// handle-block instances.
		return []httpcaddyfile.ConfigValue{staticRoute}, nil
	}

	protectVal := caddyconfig.JSONModuleObject(m, "handler", "anubis", nil)
	if matcherSet == nil {
		// Protected site-wide: one catch-all route serves static assets,
		// the API, and the challenge (ServeHTTP branches by path).
		return []httpcaddyfile.ConfigValue{
			{
				Class: "route",
				Value: caddyhttp.Route{
					HandlersRaw: []json.RawMessage{protectVal},
				},
			},
		}, nil
	}

	// Matcher-scoped protection: the static route stays global so challenge
	// assets and the API remain reachable.
	return []httpcaddyfile.ConfigValue{
		staticRoute,
		{
			Class: "route",
			Value: caddyhttp.Route{
				MatcherSetsRaw: []caddy.ModuleMap{matcherSet},
				HandlersRaw:    []json.RawMessage{protectVal},
			},
		},
	}, nil
}

// anubisStaticMiddleware serves Anubis static assets (/.within.website/) by
// delegating to the shared server. Kept for backward compatibility with
// configs that reference http.handlers.anubis_static directly.
type anubisStaticMiddleware struct{}

func (anubisStaticMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.anubis_static",
		New: func() caddy.Module { return new(anubisStaticMiddleware) },
	}
}

func (anubisStaticMiddleware) Provision(caddy.Context) error { return nil }
func (anubisStaticMiddleware) Validate() error               { return nil }

func (m anubisStaticMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	srv := anubisServer.Load()
	if srv == nil || !strings.HasPrefix(r.URL.Path, staticPrefix) {
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
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return m, nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*AnubisMiddleware)(nil)
	_ caddy.Validator             = (*AnubisMiddleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*AnubisMiddleware)(nil)
	_ caddyfile.Unmarshaler       = (*AnubisMiddleware)(nil)
	_ caddy.CleanerUpper          = (*AnubisMiddleware)(nil)
)
