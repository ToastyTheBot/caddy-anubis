package caddyanubis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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

// anubisServer holds the most recently provisioned Anubis server.
// The static middleware reads this to serve /.within.website/ assets.
var anubisServer atomic.Pointer[libanubis.Server]

// nextHandlerCtxKey is used to pass the Caddy next handler through the
// request context, avoiding shared mutable state on the middleware struct.
type nextHandlerCtxKey struct{}

// anubisAPIServers lists the Anubis servers provisioned by anubis
// middleware instances. Each instance keeps its own in-memory challenge
// store, so a challenge issued by one server can only be verified by that
// same server. The init_anubis route owns /.within.website/* (including the
// pass-challenge API), so API requests are delegated to the middleware
// servers instead of being answered by the global server.
var anubisAPIServers atomic.Pointer[[]*libanubis.Server]

func registerAPIServer(s *libanubis.Server) {
	for {
		old := anubisAPIServers.Load()
		next := []*libanubis.Server{s}
		if old != nil {
			next = append(next, *old...)
		}
		if anubisAPIServers.CompareAndSwap(old, &next) {
			return
		}
	}
}

func unregisterAPIServer(s *libanubis.Server) {
	for {
		old := anubisAPIServers.Load()
		if old == nil {
			return
		}
		next := make([]*libanubis.Server, 0, len(*old))
		for _, x := range *old {
			if x != s {
				next = append(next, x)
			}
		}
		if anubisAPIServers.CompareAndSwap(old, &next) {
			return
		}
	}
}

// releaseGlobalServer clears the global server pointer when it still points
// at s and re-points it at any remaining registered middleware server, so a
// reload that drops init_anubis does not strand the static handler on an
// unprovisioned server.
func releaseGlobalServer(s *libanubis.Server) {
	if !anubisServer.CompareAndSwap(s, nil) {
		return
	}
	if p := anubisAPIServers.Load(); p != nil {
		for _, other := range *p {
			if other != nil && anubisServer.CompareAndSwap(nil, other) {
				return
			}
		}
	}
}

// bufferedResponseWriter captures a response so a challenge-store miss can
// be detected before anything is sent to the client.
type bufferedResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponseWriter) Header() http.Header {
	if b.header == nil {
		b.header = http.Header{}
	}
	return b.header
}

func (b *bufferedResponseWriter) Write(p []byte) (int, error) { return b.body.Write(p) }

func (b *bufferedResponseWriter) WriteHeader(code int) { b.status = code }

// miss reports whether this server failed to answer because it did not hold
// the challenge: either the explicit "getChallenge failed" store error, or
// any 500 while the client lacks the verification cookie (the server that
// holds the challenge retries with a fresh challenge instead of erroring,
// so a 500 from a server without the challenge must not be copied).
func (b *bufferedResponseWriter) miss(noTestCookie bool) bool {
	if b.status != http.StatusInternalServerError {
		return false
	}
	return noTestCookie || bytes.Contains(b.body.Bytes(), []byte("getChallenge failed"))
}

func (b *bufferedResponseWriter) copyTo(w http.ResponseWriter) {
	dst := w.Header()
	for k, vs := range b.header {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	if b.status != 0 {
		w.WriteHeader(b.status)
	}
	_, _ = w.Write(b.body.Bytes())
}

// serveAnubisAPI answers an Anubis API request with the first server whose
// challenge store holds the challenge. preferred is tried first; the
// remaining registered middleware servers are tried as fallbacks.
func serveAnubisAPI(w http.ResponseWriter, r *http.Request, preferred *libanubis.Server) bool {
	servers := make([]*libanubis.Server, 0, 2)
	if preferred != nil {
		servers = append(servers, preferred)
	}
	if p := anubisAPIServers.Load(); p != nil {
		for _, s := range *p {
			if s != nil && s != preferred {
				servers = append(servers, s)
			}
		}
	}
	if len(servers) == 0 {
		return false
	}

	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		body, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}

	// Without the verification cookie the lib consults its own store before
	// deciding between the cookies-disabled error and a retry redirect, so a
	// 500 from a server that lacks the challenge is indistinguishable from
	// the real error; treat it as a miss and let the holder answer.
	_, cookieErr := r.Cookie(anubis.TestCookieName)
	noTestCookie := errors.Is(cookieErr, http.ErrNoCookie)

	var first *bufferedResponseWriter
	for _, s := range servers {
		rr := r.Clone(r.Context())
		if body != nil {
			rr.Body = io.NopCloser(bytes.NewReader(body))
			rr.ContentLength = int64(len(body))
		} else {
			rr.Body = http.NoBody
			rr.ContentLength = 0
		}
		buf := &bufferedResponseWriter{}
		s.ServeHTTP(buf, rr)
		if !buf.miss(noTestCookie) {
			buf.copyTo(w)
			return true
		}
		if first == nil {
			first = buf
		}
	}
	if first != nil {
		first.copyTo(w)
	}
	return true
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

	// subrequestMode matches upstream: true when no target redirects.
	pol, err := libanubis.LoadPoliciesOrDefault(ctx, m.PolicyFile, anubis.DefaultDifficulty, "info", m.Target == nil)
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
	registerAPIServer(m.anubisServer)
	m.logger.Info("anubis middleware provisioned")
	return nil
}

func (m *AnubisMiddleware) Validate() error { return nil }

// Cleanup removes this instance's server from the API delegation registry
// and releases the global pointer when it still points at this server, so
// reloaded configs neither accumulate stale servers nor strand static assets.
func (m *AnubisMiddleware) Cleanup() error {
	if m.anubisServer == nil {
		return nil
	}
	unregisterAPIServer(m.anubisServer)
	releaseGlobalServer(m.anubisServer)
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
	ensureRealIP(r)
	if strings.HasPrefix(r.URL.Path, anubis.APIPrefix) {
		r.RequestURI = r.URL.RequestURI()
		if serveAnubisAPI(w, r, m.anubisServer) {
			return nil
		}
	}
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
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
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

func (m anubisStaticMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	srv := anubisServer.Load()
	if srv == nil || !strings.HasPrefix(r.URL.Path, "/.within.website/") {
		return next.ServeHTTP(w, r)
	}
	r.RequestURI = r.URL.RequestURI()
	if strings.HasPrefix(r.URL.Path, anubis.APIPrefix) && serveAnubisAPI(w, r, srv) {
		return nil
	}
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

	// init_anubis has no target, so subrequestMode is always true.
	pol, err := libanubis.LoadPoliciesOrDefault(ctx, m.PolicyFile, anubis.DefaultDifficulty, "info", true)
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

// Cleanup releases the global server pointer when it still points at this
// instance's server, so a reload that drops init_anubis leaves no dangling
// global for the static handler to delegate to.
func (m *initAnubisMiddleware) Cleanup() error {
	if m.anubisServer != nil {
		releaseGlobalServer(m.anubisServer)
	}
	return nil
}

func (m *initAnubisMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if strings.HasPrefix(r.URL.Path, "/.within.website/") {
		ensureRealIP(r)
		r.RequestURI = r.URL.RequestURI()
		if strings.HasPrefix(r.URL.Path, anubis.APIPrefix) && serveAnubisAPI(w, r, m.anubisServer) {
			return nil
		}
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

	mod, err := caddy.GetModule("http.handlers.init_anubis")
	if err != nil {
		return nil, err
	}
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
	_ caddy.CleanerUpper          = (*AnubisMiddleware)(nil)
	_ caddy.Provisioner           = (*initAnubisMiddleware)(nil)
	_ caddy.Validator             = (*initAnubisMiddleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*initAnubisMiddleware)(nil)
	_ caddyfile.Unmarshaler       = (*initAnubisMiddleware)(nil)
	_ caddy.CleanerUpper          = (*initAnubisMiddleware)(nil)
)
