package caddyanubis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"runtime/debug"
	"strconv"
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

// statePolicyKey is the server-block State key under which the top-level
// directive records its policy_file so handle-block instances inherit the
// site policy when they set none of their own.
const (
	statePolicyKey = "caddyanubis.policy_file"
	// stateGateCountKey counts handle-block gates per site so two gates
	// with an identical policy_file and target still get distinct cookie
	// names. Config order is fixed, so the counter is stable across
	// reloads and clearances survive them.
	stateGateCountKey = "caddyanubis.gate_count"
)

// anubisServer is the site server provisioned by the top-level directive.
// Static assets are served from it; handle-block instances keep dedicated
// servers of their own.
var anubisServer atomic.Pointer[libanubis.Server]

// apiGate lists the live instances whose servers hold challenges. The
// pass-challenge API lives under staticPrefix, so requests reach whichever
// instance owns that route; serveAPI dispatches to the gate that issued the
// challenge, selected by its renamed cookies and confirmed by store miss.
type apiGate struct {
	server   *libanubis.Server
	cookieID string // "" means the server uses the base Anubis cookie names
}

var apiGates atomic.Pointer[[]*apiGate]

func registerGate(g *apiGate) {
	for {
		old := apiGates.Load()
		next := []*apiGate{g}
		if old != nil {
			next = append(next, *old...)
		}
		if apiGates.CompareAndSwap(old, &next) {
			return
		}
	}
}

func unregisterGate(g *apiGate) {
	for {
		old := apiGates.Load()
		if old == nil {
			return
		}
		next := make([]*apiGate, 0, len(*old))
		for _, x := range *old {
			if x != g {
				next = append(next, x)
			}
		}
		if apiGates.CompareAndSwap(old, &next) {
			return
		}
	}
}

// releaseGlobalServer clears the global pointer when it still points at s
// and re-points it at a surviving gate's server.
func releaseGlobalServer(s *libanubis.Server) {
	if !anubisServer.CompareAndSwap(s, nil) {
		return
	}
	if p := apiGates.Load(); p != nil {
		for _, g := range *p {
			if g.server != nil && g.server != s && anubisServer.CompareAndSwap(nil, g.server) {
				return
			}
		}
	}
}

// --- per-gate cookie renaming ---------------------------------------------
//
// Every Anubis server signs its clearance cookie with its own key but uses
// the same fixed cookie names. Without renaming, clearing gate B overwrites
// gate A's clearance in the browser. Each handle-block gate therefore runs
// with a per-instance cookie-name suffix: requests carry
// "<base>__<id>" cookies which are renamed to the base names before the
// lib sees them, and Set-Cookie responses are renamed back.

// gateCookieID derives a stable per-gate suffix from its configuration.
func gateCookieID(policyFile string, target *string, instance int) string {
	t := ""
	if target != nil {
		t = *target
	}
	sum := sha256.Sum256([]byte(policyFile + "|" + t + "|" + strconv.Itoa(instance)))
	return hex.EncodeToString(sum[:4])
}

func gateCookieName(base, id string) string {
	if id == "" {
		return base
	}
	return base + "__" + id
}

var cookieBases = []string{
	anubis.CookieName,
	anubis.TestCookieName,
	anubis.OriginalRefererCookieName,
}

// renameRequestCookies renames this gate's cookies to the base names the
// lib expects, dropping the rename marker. Other cookies pass through.
func renameRequestCookies(r *http.Request, id string) {
	if id == "" {
		return
	}
	suffix := "__" + id
	var sb strings.Builder
	for _, c := range r.Cookies() {
		name := c.Name
		if strings.HasSuffix(name, suffix) {
			base := strings.TrimSuffix(name, suffix)
			if !strings.HasPrefix(base, anubis.CookieName) {
				continue // not ours
			}
			name = base
		}
		if sb.Len() > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(name)
		sb.WriteString("=")
		sb.WriteString(c.Value)
	}
	if sb.Len() == 0 {
		r.Header.Del("Cookie")
		return
	}
	r.Header.Set("Cookie", sb.String())
}

// gateCookieWriter renames Set-Cookie headers from base names to this
// gate's names before the response header is committed.
type gateCookieWriter struct {
	http.ResponseWriter
	id          string
	wroteHeader bool
}

func (w *gateCookieWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		if w.id != "" {
			h := w.Header()
			if sc := h["Set-Cookie"]; len(sc) > 0 {
				out := make([]string, 0, len(sc))
				for _, c := range sc {
					out = append(out, renameSetCookie(c, w.id))
				}
				h["Set-Cookie"] = out
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gateCookieWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func renameSetCookie(setCookie, id string) string {
	for _, base := range cookieBases {
		if strings.HasPrefix(setCookie, base+"=") {
			return gateCookieName(base, id) + setCookie[len(base):]
		}
	}
	return setCookie
}

// --- API dispatch ----------------------------------------------------------

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
// any 500 while the client lacks the verification cookie (the holder
// retries with a fresh challenge instead of erroring, so a 500 from a
// non-holder must not be copied).
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

// serveAPI answers an Anubis API request with the gate whose store holds
// the challenge. Gates whose renamed cookies the client carries are tried
// first; the remaining gates and the site server are fallbacks.
func serveAPI(w http.ResponseWriter, r *http.Request) bool {
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		body, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}

	var preferred, others []*apiGate
	if p := apiGates.Load(); p != nil {
		for _, g := range *p {
			if g == nil || g.server == nil {
				continue
			}
			if g.cookieID != "" && hasGateCookies(r, g.cookieID) {
				preferred = append(preferred, g)
			} else {
				others = append(others, g)
			}
		}
	}
	// Put the site server (base cookie names) at the front of the fallbacks.
	for i := 0; i < len(others); i++ {
		if others[i].cookieID == "" {
			others[0], others[i] = others[i], others[0]
			break
		}
	}

	var first *bufferedResponseWriter
	tried := 0
	for _, g := range append(preferred, others...) {
		rr := r.Clone(r.Context())
		if body != nil {
			rr.Body = io.NopCloser(bytes.NewReader(body))
			rr.ContentLength = int64(len(body))
		} else {
			rr.Body = http.NoBody
			rr.ContentLength = 0
		}
		renameRequestCookies(rr, g.cookieID)
		_, cookieErr := rr.Cookie(anubis.TestCookieName)
		noTestCookie := errors.Is(cookieErr, http.ErrNoCookie)

		buf := &bufferedResponseWriter{}
		g.server.ServeHTTP(buf, rr)
		if g.cookieID != "" && buf.header != nil {
			if sc := buf.header["Set-Cookie"]; len(sc) > 0 {
				out := make([]string, 0, len(sc))
				for _, c := range sc {
					out = append(out, renameSetCookie(c, g.cookieID))
				}
				buf.header["Set-Cookie"] = out
			}
		}
		tried++
		if !buf.miss(noTestCookie) {
			buf.copyTo(w)
			return true
		}
		if first == nil {
			first = buf
		}
	}
	if tried > 0 && first != nil {
		first.copyTo(w)
		return true
	}
	return false
}

func hasGateCookies(r *http.Request, id string) bool {
	suffix := "__" + id
	for _, c := range r.Cookies() {
		if strings.HasSuffix(c.Name, suffix) && strings.HasPrefix(strings.TrimSuffix(c.Name, suffix), anubis.CookieName) {
			return true
		}
	}
	return false
}

// --- version stamping ------------------------------------------------------

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
// At the top level of a site block it provisions the site Anubis server
// (policy_file, target, protect) and emits the static-asset route plus,
// with protect, the challenge route. Inside a handle or route block it is
// an independent gate: its own Anubis server, its own challenge store and
// signing key, and its own cookie names, so clearing one gate neither
// invalidates nor auto-validates another.
type AnubisMiddleware struct {
	// Target redirects verified requests to a fixed URL instead of
	// continuing to the next handler.
	Target *string `json:"target,omitempty"`
	// PolicyFile is the path to a custom Anubis policy file. Handle-block
	// instances inherit the top-level policy_file when they set none.
	PolicyFile string `json:"policy_file,omitempty"`
	// Protect challenges non-static requests. Set by the parser: true for
	// handle-block usage and the top-level protect route, false for the
	// static route. Unset means challenge (hand-written JSON default).
	Protect *bool `json:"protect,omitempty"`
	// Authoritative marks the top-level instances that provision the site
	// server and claim the global slot used for static assets.
	Authoritative bool `json:"authoritative,omitempty"`

	// Instance is the 1-based position of the gate within its site,
	// assigned by the parser. It keeps cookie names distinct when two
	// gates share a policy_file and target.
	Instance int `json:"instance,omitempty"`

	anubisPolicy *policy.ParsedConfig
	server       *libanubis.Server
	gate         *apiGate
	cookieID     string
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

	m.cookieID = ""
	if !m.Authoritative {
		// Independent gate: dedicated store, key, and cookie names.
		m.cookieID = gateCookieID(m.PolicyFile, m.Target, m.Instance)
	}
	m.gate = &apiGate{server: m.server, cookieID: m.cookieID}
	registerGate(m.gate)
	if m.Authoritative {
		anubisServer.Store(m.server)
	}
	m.logger.Info("anubis provisioned",
		zap.Bool("authoritative", m.Authoritative),
		zap.String("cookie_id", m.cookieID))
	return nil
}

func (m *AnubisMiddleware) Validate() error { return nil }

// Cleanup deregisters this instance's gate and releases the global pointer
// when it still points at this instance's server.
func (m *AnubisMiddleware) Cleanup() error {
	if m.gate != nil {
		unregisterGate(m.gate)
	}
	if m.server != nil {
		releaseGlobalServer(m.server)
	}
	return nil
}

// nextHandlerCtxKey passes the Caddy next handler through the request
// context, avoiding shared mutable state on the middleware struct.
type nextHandlerCtxKey struct{}

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
	if strings.HasPrefix(r.URL.Path, staticPrefix) {
		ensureRealIP(r)
		r.RequestURI = r.URL.RequestURI()
		if strings.HasPrefix(r.URL.Path, anubis.APIPrefix) && serveAPI(w, r) {
			return nil
		}
		srv := anubisServer.Load()
		if srv == nil {
			srv = m.server
		}
		if srv == nil {
			return next.ServeHTTP(w, r)
		}
		srv.ServeHTTP(w, r)
		return nil
	}
	if !m.challenges() {
		return next.ServeHTTP(w, r)
	}
	srv := m.server
	if srv == nil {
		srv = anubisServer.Load()
	}
	if srv == nil {
		return next.ServeHTTP(w, r)
	}
	ensureRealIP(r)
	ctx := context.WithValue(r.Context(), nextHandlerCtxKey{}, next)
	r = r.WithContext(ctx)
	if m.cookieID != "" {
		renameRequestCookies(r, m.cookieID)
		w = &gateCookieWriter{ResponseWriter: w, id: m.cookieID}
	}
	srv.ServeHTTP(w, r)
	return nil
}

// UnmarshalCaddyfile parses the handle-block flavor: an independent gate
// with its own policy_file and target.
func (m *AnubisMiddleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name

	if d.NextArg() {
		return d.Errf("unexpected argument %q; put options in a block", d.Val())
	}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "policy_file":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.PolicyFile = d.Val()
		case "target":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.Target = new(d.Val())
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
		// Independent-gate flavor: challenge the enclosing block with its
		// own server. Inherit the site policy recorded by the top-level
		// directive when this block sets none.
		var m AnubisMiddleware
		if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
			return nil, err
		}
		if m.PolicyFile == "" {
			if site, ok := h.State[statePolicyKey].(string); ok {
				m.PolicyFile = site
			}
		}
		m.Protect = new(true)
		if h.State != nil {
			n, _ := h.State[stateGateCountKey].(int)
			n++
			h.State[stateGateCountKey] = n
			m.Instance = n
		}
		return h.NewRoute(matcherSet, &m), nil
	}

	// Top-level flavor: provision the site server and emit routes.
	d := h.Dispenser
	d.Next() // ExtractMatcherSet resets the dispenser; skip the directive name again
	if d.NextArg() {
		return nil, d.Errf("unexpected argument %q; put options in a block", d.Val())
	}
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
	if m.PolicyFile != "" && h.State != nil {
		h.State[statePolicyKey] = m.PolicyFile
	}

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
		// Static assets and provisioning only; gates live in handle blocks.
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
// delegating to the site server. Kept for backward compatibility with
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
	if strings.HasPrefix(r.URL.Path, anubis.APIPrefix) && serveAPI(w, r) {
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

// Interface guards
var (
	_ caddy.Provisioner           = (*AnubisMiddleware)(nil)
	_ caddy.Validator             = (*AnubisMiddleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*AnubisMiddleware)(nil)
	_ caddyfile.Unmarshaler       = (*AnubisMiddleware)(nil)
	_ caddy.CleanerUpper          = (*AnubisMiddleware)(nil)
)
