package caddyanubis_test

import (
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/ToastyTheBot/caddy-anubis"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

type routeJSON struct {
	Match    []map[string]json.RawMessage `json:"match"`
	Handle   []handlerJSON                `json:"handle"`
	Group    string                       `json:"group"`
	Terminal bool                         `json:"terminal"`
}

type handlerJSON struct {
	Handler       string      `json:"handler"`
	Protect       *bool       `json:"protect"`
	PolicyFile    string      `json:"policy_file"`
	Target        *string     `json:"target"`
	Authoritative bool        `json:"authoritative"`
	Routes        []routeJSON `json:"routes"`
}

// handlersOf flattens handle-block subroute wrappers into the ordered
// middleware handler list of the block.
func handlersOf(r routeJSON) []handlerJSON {
	var out []handlerJSON
	for _, h := range r.Handle {
		if h.Handler == "subroute" {
			for _, sub := range h.Routes {
				out = append(out, handlersOf(sub)...)
			}
			continue
		}
		out = append(out, h)
	}
	return out
}

func adapt(t *testing.T, input string) []routeJSON {
	t.Helper()
	adapter := caddyconfig.GetAdapter("caddyfile")
	out, _, err := adapter.Adapt([]byte(input), map[string]any{"server_type": "http"})
	if err != nil {
		t.Fatalf("adapt: %v", err)
	}
	var cfg struct {
		Apps map[string]struct {
			Servers map[string]struct {
				Routes []routeJSON `json:"routes"`
			} `json:"servers"`
		} `json:"apps"`
		Servers map[string]struct {
			Routes []routeJSON `json:"routes"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("unmarshal adapted config: %v\n%s", err, out)
	}
	servers := cfg.Servers
	for _, app := range cfg.Apps {
		if len(app.Servers) > 0 {
			servers = app.Servers
		}
	}
	for _, srv := range servers {
		return srv.Routes
	}
	t.Fatalf("no servers in adapted config:\n%s", out)
	return nil
}

func isStaticRoute(r routeJSON) bool {
	if len(r.Handle) != 1 || r.Handle[0].Handler != "anubis" {
		return false
	}
	if len(r.Match) != 1 {
		return false
	}
	raw, ok := r.Match[0]["path"]
	if !ok {
		return false
	}
	var paths []string
	_ = json.Unmarshal(raw, &paths)
	return len(paths) == 1 && paths[0] == "/.within.website/*"
}

func matchPaths(r routeJSON) []string {
	var paths []string
	for _, m := range r.Match {
		if raw, ok := m["path"]; ok {
			var p []string
			_ = json.Unmarshal(raw, &p)
			paths = append(paths, p...)
		}
	}
	return paths
}

// Top-level anubis without protect: provisions the shared server and emits
// only the static-asset route.
func TestAdaptTopLevelStaticOnly(t *testing.T) {
	routes := adapt(t, `:8080 {
	anubis
}`)
	if len(routes) != 1 || !isStaticRoute(routes[0]) {
		t.Fatalf("want single static route, got %+v", routes)
	}
	h := routes[0].Handle[0]
	if h.Protect == nil || *h.Protect {
		t.Errorf("static route must have protect:false, got %v", h.Protect)
	}
	if !h.Authoritative {
		t.Error("top-level route must be authoritative")
	}
}

// Top-level anubis with protect: one catch-all route, no matcher.
func TestAdaptTopLevelProtect(t *testing.T) {
	routes := adapt(t, `:8080 {
	anubis {
		protect
	}
}`)
	if len(routes) != 1 {
		t.Fatalf("want single catch-all route, got %+v", routes)
	}
	r := routes[0]
	if len(r.Match) != 0 {
		t.Errorf("catch-all protect route must have no matcher, got %v", r.Match)
	}
	if len(r.Handle) != 1 || r.Handle[0].Handler != "anubis" {
		t.Fatalf("want anubis handler, got %+v", r.Handle)
	}
	h := r.Handle[0]
	if h.Protect == nil || !*h.Protect {
		t.Errorf("protect must be true, got %v", h.Protect)
	}
	if !h.Authoritative {
		t.Error("protect route must be authoritative")
	}
}

// protect accepts policy_file and target.
func TestAdaptTopLevelProtectOptions(t *testing.T) {
	routes := adapt(t, `:8080 {
	anubis {
		protect
		policy_file /tmp/p.yaml
		target https://example.com
	}
}`)
	if len(routes) != 1 || len(routes[0].Handle) != 1 {
		t.Fatalf("want single protect route, got %+v", routes)
	}
	h := routes[0].Handle[0]
	if h.PolicyFile != "/tmp/p.yaml" {
		t.Errorf("policy_file must be /tmp/p.yaml, got %q", h.PolicyFile)
	}
	if h.Target == nil || *h.Target != "https://example.com" {
		t.Errorf("target missing, got %v", h.Target)
	}
}

// protect off is the same as no protect: static route only.
func TestAdaptTopLevelProtectOff(t *testing.T) {
	routes := adapt(t, `:8080 {
	anubis {
		protect off
	}
}`)
	if len(routes) != 1 || !isStaticRoute(routes[0]) {
		t.Fatalf("want single static route, got %+v", routes)
	}
}

// Matcher-scoped top-level protect keeps the static route global and puts
// the user's matcher on the protect route.
func TestAdaptTopLevelMatcherProtect(t *testing.T) {
	routes := adapt(t, `:8080 {
	anubis /secure/* {
		protect
	}
}`)
	var static, scoped bool
	for _, r := range routes {
		switch {
		case isStaticRoute(r):
			static = true
		case len(r.Handle) == 1 && r.Handle[0].Handler == "anubis" &&
			r.Handle[0].Protect != nil && *r.Handle[0].Protect:
			if paths := matchPaths(r); len(paths) == 1 && paths[0] == "/secure/*" {
				scoped = true
			}
		}
	}
	if !static || !scoped {
		t.Fatalf("want static + /secure/* protect routes, got static=%v scoped=%v: %+v", static, scoped, routes)
	}
}

// A named matcher works the same as an inline path matcher.
func TestAdaptTopLevelNamedMatcherProtect(t *testing.T) {
	routes := adapt(t, `:8080 {
@admin path /admin/*
anubis @admin {
	protect
	policy_file /tmp/p.yaml
}
}`)
	var static, scoped bool
	for _, r := range routes {
		switch {
		case isStaticRoute(r):
			static = true
		case len(r.Handle) == 1 && r.Handle[0].Handler == "anubis" &&
			r.Handle[0].Protect != nil && *r.Handle[0].Protect &&
			r.Handle[0].PolicyFile == "/tmp/p.yaml":
			if paths := matchPaths(r); len(paths) == 1 && paths[0] == "/admin/*" {
				scoped = true
			}
		}
	}
	if !static || !scoped {
		t.Fatalf("want static + @admin protect routes, got static=%v scoped=%v: %+v", static, scoped, routes)
	}
}

// Inside a handle block, anubis is a middleware handler in that block's
// route, ordered before the block's other handlers (this pins the
// inHandleBlock position probe).
func TestAdaptHandleBlockUsesMiddlewareFlavor(t *testing.T) {
	routes := adapt(t, `:8080 {
	handle /a {
		anubis
		respond "area-a"
	}
	handle {
		respond "public"
	}
}`)
	if len(routes) != 2 {
		t.Fatalf("want 2 handle routes, got %+v", routes)
	}
	var a *routeJSON
	for i := range routes {
		if paths := matchPaths(routes[i]); len(paths) == 1 && paths[0] == "/a" {
			a = &routes[i]
		}
	}
	if a == nil {
		t.Fatalf("no /a route among %+v", routes)
	}
	hs := handlersOf(*a)
	if len(hs) != 2 {
		t.Fatalf("want anubis + static_response handlers, got %+v", hs)
	}
	if hs[0].Handler != "anubis" {
		t.Fatalf("anubis must run first in the block, got %+v", hs)
	}
	if hs[0].Protect == nil || !*hs[0].Protect {
		t.Errorf("handle-block anubis must protect, got %v", hs[0].Protect)
	}
	if hs[0].Authoritative {
		t.Error("handle-block anubis must not be authoritative")
	}
	if hs[1].Handler != "static_response" {
		t.Errorf("respond must follow anubis, got %+v", hs)
	}
}

// Multiple handle blocks in the same site each get their own anubis
// middleware; they share one server at provision time.
func TestAdaptMultipleHandleBlocks(t *testing.T) {
	routes := adapt(t, `:8080 {
	handle /a {
		anubis
		respond "a"
	}
	handle /b {
		anubis
		respond "b"
	}
	handle {
		respond "public"
	}
}`)
	if len(routes) != 3 {
		t.Fatalf("want 3 routes, got %+v", routes)
	}
	seen := map[string]bool{}
	for _, r := range routes {
		paths := matchPaths(r)
		hs := handlersOf(r)
		if len(paths) != 1 || len(hs) < 1 || hs[0].Handler != "anubis" {
			continue
		}
		if hs[0].Protect == nil || !*hs[0].Protect || hs[0].Authoritative {
			t.Errorf("bad handle-block anubis config on %v: %+v", paths, hs[0])
		}
		seen[paths[0]] = true
	}
	if !seen["/a"] || !seen["/b"] {
		t.Fatalf("anubis missing from one of the blocks: %+v", routes)
	}
}

// Handle-block gates accept their own policy_file and target.
func TestAdaptHandleBlockGateOptions(t *testing.T) {
	routes := adapt(t, `:8080 {
	handle /a {
		anubis {
			policy_file /tmp/a.yaml
			target https://example.com/done
		}
		respond "a"
	}
}`)
	hs := handlersOf(routes[0])
	if len(hs) != 2 || hs[0].Handler != "anubis" {
		t.Fatalf("want anubis first, got %+v", hs)
	}
	if hs[0].PolicyFile != "/tmp/a.yaml" || hs[0].Target == nil || *hs[0].Target != "https://example.com/done" {
		t.Errorf("block options not stored: %+v", hs[0])
	}
	if hs[0].Authoritative {
		t.Errorf("handle-block flavor must not be authoritative: %+v", hs[0])
	}
}

// A block without policy_file inherits the top-level directive's policy
// through the shared server-block state.
func TestAdaptBlockInheritsSitePolicy(t *testing.T) {
	routes := adapt(t, `:8080 {
	anubis {
		policy_file /etc/anubis/site.yaml
	}
	handle /a {
		anubis
		respond "a"
	}
}`)
	blocked := routes[1]
	hs := handlersOf(blocked)
	if hs[0].Handler != "anubis" {
		t.Fatalf("want anubis first, got %+v", hs)
	}
	if hs[0].PolicyFile != "/etc/anubis/site.yaml" {
		t.Errorf("block must inherit site policy_file, got %+v", hs[0])
	}
	if hs[0].Authoritative {
		t.Errorf("block flavor must not be authoritative: %+v", hs[0])
	}
}

// Unknown options inside a handle-block gate still fail adaptation.
func TestAdaptHandleBlockRejectsUnknownOption(t *testing.T) {
	adapter := caddyconfig.GetAdapter("caddyfile")
	_, _, err := adapter.Adapt([]byte(`:8080 {
	handle /a {
		anubis {
			policyfile /tmp/p.yaml
		}
		respond "a"
	}
}`), map[string]any{"server_type": "http"})
	if err == nil {
		t.Fatal("unknown option inside handle block must fail adaptation")
	}
}

// Options without a block at top level fail with a pointer to the block form.
func TestAdaptTopLevelRequiresBlockForOptions(t *testing.T) {
	adapter := caddyconfig.GetAdapter("caddyfile")
	_, _, err := adapter.Adapt([]byte(`:8080 {
	anubis policy_file /tmp/p.yaml
}`), map[string]any{"server_type": "http"})
	if err == nil {
		t.Fatal("top-level options without a block must fail adaptation")
	}
	if !strings.Contains(err.Error(), "block") {
		t.Fatalf("error must mention the block form, got: %v", err)
	}
}
