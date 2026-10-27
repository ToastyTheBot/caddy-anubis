package caddyanubis_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	caddyanubis "github.com/ToastyTheBot/caddy-anubis"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// --- helpers -------------------------------------------------------------

// recordingNext is the caddyhttp next handler the middlewares must reach
// (or must not reach) for each behavior under test.
type recordingNext struct {
	Called bool
	Req    *http.Request
	Body   string
}

func (n *recordingNext) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	n.Called = true
	n.Req = r
	body := n.Body
	if body == "" {
		body = "upstream"
	}
	_, _ = w.Write([]byte(body))
	return nil
}

// funcNext adapts a function to caddyhttp.Handler.
type funcNext func(http.ResponseWriter, *http.Request) error

func (f funcNext) ServeHTTP(w http.ResponseWriter, r *http.Request) error { return f(w, r) }

func newContext(t *testing.T) caddy.Context {
	t.Helper()
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	return ctx
}

func provision(t *testing.T, m *caddyanubis.AnubisMiddleware) *caddyanubis.AnubisMiddleware {
	t.Helper()
	if err := m.Provision(newContext(t)); err != nil {
		t.Fatalf("provision anubis: %v", err)
	}
	t.Cleanup(func() { _ = m.Cleanup() })
	return m
}

// newHandleBlock creates a handle-block gate: own server, own cookies.
func newHandleBlock(t *testing.T, policyFile string) *caddyanubis.AnubisMiddleware {
	t.Helper()
	return newHandleBlockTarget(t, policyFile, nil)
}

func newHandleBlockTarget(t *testing.T, policyFile string, target *string) *caddyanubis.AnubisMiddleware {
	t.Helper()
	return newHandleBlockFull(t, policyFile, target, 1)
}

// newHandleBlockInstance creates a gate with an explicit per-site instance
// number, as the parser assigns.
func newHandleBlockInstance(t *testing.T, policyFile string, instance int) *caddyanubis.AnubisMiddleware {
	t.Helper()
	return newHandleBlockFull(t, policyFile, nil, instance)
}

func newHandleBlockFull(t *testing.T, policyFile string, target *string, instance int) *caddyanubis.AnubisMiddleware {
	t.Helper()
	return provision(t, &caddyanubis.AnubisMiddleware{
		PolicyFile: policyFile,
		Target:     target,
		Protect:    new(true),
		Instance:   instance,
	})
}

// newTopLevel creates the top-level flavor: provisions the site server.
func newTopLevel(t *testing.T, protect bool, policyFile string, target *string) *caddyanubis.AnubisMiddleware {
	t.Helper()
	return provision(t, &caddyanubis.AnubisMiddleware{
		PolicyFile:    policyFile,
		Protect:       new(protect),
		Target:        target,
		Authoritative: true,
	})
}

func newStatic(t *testing.T) caddyhttp.MiddlewareHandler {
	t.Helper()
	mod, err := caddy.GetModule("http.handlers.anubis_static")
	if err != nil {
		t.Fatal(err)
	}
	m := mod.New()
	if err := m.(caddy.Provisioner).Provision(newContext(t)); err != nil {
		t.Fatal(err)
	}
	return m.(caddyhttp.MiddlewareHandler)
}

func newRequest(method, target string, cookies map[string]string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "203.0.113.7:5555"
	r.Header.Set("User-Agent", "curl/8.5.0")
	if len(cookies) > 0 {
		var sb strings.Builder
		for k, v := range cookies {
			sb.WriteString(k)
			sb.WriteString("=")
			sb.WriteString(v)
			sb.WriteString("; ")
		}
		r.Header.Set("Cookie", strings.TrimSuffix(sb.String(), "; "))
	}
	return r
}

func cookiesFrom(rec *httptest.ResponseRecorder) map[string]string {
	jar := map[string]string{}
	for _, c := range rec.Result().Cookies() {
		jar[c.Name] = c.Value
	}
	return jar
}

type challengeJSON struct {
	Challenge struct {
		ID         string `json:"id"`
		RandomData string `json:"randomData"`
		Difficulty int    `json:"difficulty"`
	} `json:"challenge"`
}

// solvePoW computes a sha256 proof for the embedded challenge, mirroring
// what Anubis' browser client does.
func solvePoW(t *testing.T, page string) (id, response string, nonce int) {
	t.Helper()
	const marker = `<script id="anubis_challenge" type="application/json">`
	start := strings.Index(page, marker)
	if start < 0 {
		t.Fatal("no anubis_challenge script in challenge page")
	}
	rest := page[start:]
	end := strings.Index(rest, "</script>")
	var cj challengeJSON
	if err := json.Unmarshal([]byte(rest[len(marker):end]), &cj); err != nil {
		t.Fatalf("challenge json: %v", err)
	}
	c := cj.Challenge
	target := strings.Repeat("0", c.Difficulty)
	for n := range 10_000_000 {
		sum := sha256.Sum256([]byte(c.RandomData + strconv.Itoa(n)))
		if strings.HasPrefix(hex.EncodeToString(sum[:]), target) {
			return c.ID, hex.EncodeToString(sum[:]), n
		}
	}
	t.Fatal("no proof found")
	return "", "", 0
}

// passChallenge solves the challenge from page and verifies it through srv,
// returning the cookies a verified client would carry.
func passChallenge(t *testing.T, srv caddyhttp.MiddlewareHandler, page string, cookies map[string]string) map[string]string {
	t.Helper()
	id, response, nonce := solvePoW(t, page)
	next := &recordingNext{}
	qs := fmt.Sprintf("/.within.website/x/cmd/anubis/api/pass-challenge?id=%s&response=%s&nonce=%d&elapsedTime=1&redir=/", id, response, nonce)
	r := newRequest(http.MethodGet, qs, cookies)
	rec := httptest.NewRecorder()
	if err := srv.ServeHTTP(rec, r, next); err != nil {
		t.Fatalf("pass-challenge: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("pass-challenge: want 302, got %d body=%s", rec.Code, rec.Body.String())
	}
	jar := cookiesFrom(rec)
	for k, v := range cookies {
		if _, ok := jar[k]; !ok {
			jar[k] = v
		}
	}
	return jar
}

func challengePage(t *testing.T, srv caddyhttp.MiddlewareHandler, path string) *httptest.ResponseRecorder {
	t.Helper()
	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := srv.ServeHTTP(rec, newRequest(http.MethodGet, path, nil), next); err != nil {
		t.Fatal(err)
	}
	if next.Called {
		t.Fatalf("challenged request must not reach next (%s)", path)
	}
	if !strings.Contains(rec.Body.String(), "anubis_challenge") {
		body := rec.Body.String()
		t.Fatalf("want challenge page for %s, got %d %s", path, rec.Code, body[:min(300, len(body))])
	}
	return rec
}

func verifiedReaches(t *testing.T, srv caddyhttp.MiddlewareHandler, path string, jar map[string]string) {
	t.Helper()
	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := srv.ServeHTTP(rec, newRequest(http.MethodGet, path, jar), next); err != nil {
		t.Fatal(err)
	}
	if !next.Called {
		t.Fatalf("verified client must reach next at %s, body=%s", path, rec.Body.String())
	}
}

// --- independent gates -----------------------------------------------------

// The user-facing contract: each handle-block gate is separate. Clearing
// one neither auto-validates another nor overwrites its clearance.
func TestIndependentGates(t *testing.T) {
	a := newHandleBlock(t, "testdata/challenge.yaml")
	b := newHandleBlock(t, "testdata/challenge2.yaml")

	// Clear gate A.
	recA := challengePage(t, a, "/a")
	jarA := passChallenge(t, a, recA.Body.String(), cookiesFrom(recA))
	verifiedReaches(t, a, "/a", jarA)

	// Gate cookie names are per-instance and disjoint between gates.
	if !strings.Contains(firstKey(jarA), "__") {
		t.Errorf("gate cookies must be renamed per instance, got %v", names(jarA))
	}

	// Gate B still challenges: A's clearance did not auto-validate B.
	recB := challengePage(t, b, "/b")
	jarB := passChallenge(t, b, recB.Body.String(), cookiesFrom(recB))
	verifiedReaches(t, b, "/b", jarB)
	for name := range jarB {
		if _, ok := jarA[name]; ok {
			t.Errorf("gates must not share cookie %q", name)
		}
	}

	// A is still cleared after B was solved: B did not invalidate A.
	verifiedReaches(t, a, "/a", jarA)
	// And B still requires its own clearance: A's jar does not pass B.
	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := b.ServeHTTP(rec, newRequest(http.MethodGet, "/b", jarA), next); err != nil {
		t.Fatal(err)
	}
	if next.Called {
		t.Error("gate A's clearance must not validate gate B")
	}
}

// pass-challenge reaches the gate that issued the challenge even when the
// request arrives at another instance (the static route owns the API path).
func TestAPIDispatchRoutesToIssuingGate(t *testing.T) {
	issuer := newHandleBlock(t, "testdata/challenge.yaml")
	other := newHandleBlock(t, "testdata/challenge2.yaml")

	rec := challengePage(t, issuer, "/admin/")
	jar := passChallenge(t, other, rec.Body.String(), cookiesFrom(rec)) // pass via the OTHER instance
	verifiedReaches(t, issuer, "/admin/", jar)
}

// Two gates with the same policy_file and no target keep separate
// clearances: the parser assigns per-site instance numbers, so the cookie
// names differ and neither clearance overwrites the other in a browser.
func TestSamePolicyGatesStayIndependent(t *testing.T) {
	a := newHandleBlockInstance(t, "testdata/challenge.yaml", 1)
	b := newHandleBlockInstance(t, "testdata/challenge.yaml", 2)

	recA := challengePage(t, a, "/a")
	jarA := passChallenge(t, a, recA.Body.String(), cookiesFrom(recA))
	verifiedReaches(t, a, "/a", jarA)

	recB := challengePage(t, b, "/b")
	jarB := passChallenge(t, b, recB.Body.String(), cookiesFrom(recB))
	verifiedReaches(t, b, "/b", jarB)

	for name := range jarB {
		if _, ok := jarA[name]; ok {
			t.Errorf("same-policy gates must not share cookie %q", name)
		}
	}
	verifiedReaches(t, a, "/a", jarA)
	verifiedReaches(t, b, "/b", jarB)

	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := b.ServeHTTP(rec, newRequest(http.MethodGet, "/b", jarA), next); err != nil {
		t.Fatal(err)
	}
	if next.Called {
		t.Error("gate A's clearance must not validate same-policy gate B")
	}
}

// A chain of two gates: the first block's target short-circuits, so the
// second gate is never consulted and its store stays untouched.
func TestChainFirstTargetShortCircuits(t *testing.T) {
	target := "https://example.com/after-first"
	first := newHandleBlockTarget(t, "testdata/allow.yaml", &target)
	second := newHandleBlock(t, "testdata/challenge.yaml")

	secondNext := &recordingNext{}
	chain := funcNext(func(w http.ResponseWriter, r *http.Request) error {
		return second.ServeHTTP(w, r, secondNext)
	})

	rec := httptest.NewRecorder()
	if err := first.ServeHTTP(rec, newRequest(http.MethodGet, "/", nil), chain); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != target {
		t.Fatalf("want 307 to target, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if secondNext.Called {
		t.Error("second gate must not run when the first redirects")
	}
}

func firstKey(m map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}

func names(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- handle-block static serving --------------------------------------------

func TestHandleBlockServesStaticAssets(t *testing.T) {
	a := newHandleBlock(t, "testdata/challenge.yaml")

	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := a.ServeHTTP(rec, newRequest(http.MethodGet, "/.within.website/x/xess/xess.css", nil), next); err != nil {
		t.Fatal(err)
	}
	if next.Called {
		t.Error("static request must not reach next")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Errorf("want 200 text/css, got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// --- top-level flavors ------------------------------------------------------

func TestStaticInstancePassesNonStaticToNext(t *testing.T) {
	m := newTopLevel(t, false, "testdata/challenge.yaml", nil)

	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := m.ServeHTTP(rec, newRequest(http.MethodGet, "/", nil), next); err != nil {
		t.Fatal(err)
	}
	if !next.Called {
		t.Fatal("protect off: non-static requests must reach next")
	}

	next = &recordingNext{}
	rec = httptest.NewRecorder()
	if err := m.ServeHTTP(rec, newRequest(http.MethodGet, "/.within.website/x/xess/xess.css", nil), next); err != nil {
		t.Fatal(err)
	}
	if next.Called || rec.Code != http.StatusOK {
		t.Errorf("static must be served, got %d next=%v", rec.Code, next.Called)
	}
}

func TestProtectInstanceServesRobots(t *testing.T) {
	m := newTopLevel(t, true, "testdata/challenge.yaml", nil)
	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := m.ServeHTTP(rec, newRequest(http.MethodGet, "/robots.txt", nil), next); err != nil {
		t.Fatal(err)
	}
	if next.Called {
		t.Error("robots.txt must be served by anubis, not next")
	}
	if rec.Code != http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "disallow") {
		t.Errorf("want generated robots.txt, got %d", rec.Code)
	}
}

// --- challenge behaviors ------------------------------------------------------

func TestRealIPSetAndPreserved(t *testing.T) {
	m := newTopLevel(t, true, "testdata/allow.yaml", nil)

	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := m.ServeHTTP(rec, newRequest(http.MethodGet, "/", nil), next); err != nil {
		t.Fatal(err)
	}
	if !next.Called {
		t.Fatal("allowed request must reach next")
	}
	if got := next.Req.Header.Get("X-Real-IP"); got != "203.0.113.7" {
		t.Errorf("X-Real-IP must be set from peer address, got %q", got)
	}

	next = &recordingNext{}
	rec = httptest.NewRecorder()
	r := newRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Real-IP", "198.51.100.9")
	if err := m.ServeHTTP(rec, r, next); err != nil {
		t.Fatal(err)
	}
	if got := next.Req.Header.Get("X-Real-IP"); got != "198.51.100.9" {
		t.Errorf("existing X-Real-IP must win, got %q", got)
	}
}

func TestChallengeSolvedReachesUpstream(t *testing.T) {
	m := newTopLevel(t, true, "testdata/challenge.yaml", nil)

	rec := challengePage(t, m, "/")
	jar := passChallenge(t, m, rec.Body.String(), cookiesFrom(rec))
	verifiedReaches(t, m, "/", jar)
}

func TestDenyBlocksUpstream(t *testing.T) {
	m := newTopLevel(t, true, "testdata/deny.yaml", nil)
	next := &recordingNext{}
	r := newRequest(http.MethodGet, "/", nil)
	r.Header.Set("User-Agent", "denying-bot/1.0")
	rec := httptest.NewRecorder()
	if err := m.ServeHTTP(rec, r, next); err != nil {
		t.Fatal(err)
	}
	if next.Called {
		t.Fatal("denied request must not reach next")
	}
	if !strings.Contains(rec.Body.String(), "Oh noes") {
		t.Errorf("want anubis deny page, got %d", rec.Code)
	}
}

func TestTargetRedirectsInsteadOfNext(t *testing.T) {
	target := "https://example.com/landing"
	m := newTopLevel(t, true, "testdata/allow.yaml", &target)
	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := m.ServeHTTP(rec, newRequest(http.MethodGet, "/", nil), next); err != nil {
		t.Fatal(err)
	}
	if next.Called {
		t.Fatal("target mode must not call next")
	}
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != target {
		t.Errorf("want 307 to target, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// --- global server lifecycle --------------------------------------------------

// A reload that drops the top-level instance must re-point the global at a
// surviving gate's server so static assets keep working.
func TestCleanupRepointsGlobalToSurvivor(t *testing.T) {
	top := newTopLevel(t, false, "testdata/allow.yaml", nil)
	survivor := newHandleBlock(t, "testdata/allow.yaml")
	static := newStatic(t)

	if err := top.Cleanup(); err != nil {
		t.Fatal(err)
	}
	_ = survivor

	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := static.ServeHTTP(rec, newRequest(http.MethodGet, "/.within.website/x/xess/xess.css", nil), next); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("static assets must survive top-level cleanup, got %d", rec.Code)
	}
}

// With no provisioned server left, the legacy static handler must fall
// through to next instead of erroring. Run last: it drains the global.
func TestStaticFallsThroughWhenNoServer(t *testing.T) {
	static := newStatic(t)
	next := &recordingNext{}
	rec := httptest.NewRecorder()
	if err := static.ServeHTTP(rec, newRequest(http.MethodGet, "/.within.website/x/xess/xess.css", nil), next); err != nil {
		t.Fatal(err)
	}
	if !next.Called {
		t.Fatal("static handler must delegate to next when no server is provisioned")
	}
}
