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

// newHandleBlock creates the handle-block flavor: challenges, non-authoritative.
func newHandleBlock(t *testing.T, policyFile string) *caddyanubis.AnubisMiddleware {
	t.Helper()
	return provision(t, &caddyanubis.AnubisMiddleware{
		PolicyFile: policyFile,
		Protect:    new(true),
	})
}

// newTopLevel creates the top-level flavor: provisions the shared server.
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

func challengePage(t *testing.T, srv caddyhttp.MiddlewareHandler, path string) (*httptest.ResponseRecorder, *recordingNext) {
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
	return rec, next
}

// --- shared server across handle blocks ----------------------------------

// The user-facing contract: one anubis per handle block, one solved
// challenge verifies in every block.
func TestSharedServerAcrossHandleBlocks(t *testing.T) {
	a := newHandleBlock(t, "testdata/challenge.yaml")
	b := newHandleBlock(t, "testdata/challenge.yaml")

	rec, _ := challengePage(t, a, "/a")
	jar := passChallenge(t, a, rec.Body.String(), cookiesFrom(rec))

	// The same solved session passes the other block, and its pass-challenge
	// may even be routed through the other block's middleware.
	nextB := &recordingNext{}
	recB := httptest.NewRecorder()
	if err := b.ServeHTTP(recB, newRequest(http.MethodGet, "/b", jar), nextB); err != nil {
		t.Fatal(err)
	}
	if !nextB.Called {
		t.Fatalf("session from block A must verify in block B, body=%s", recB.Body.String())
	}
}

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

// --- top-level flavors ----------------------------------------------------

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

// --- challenge behaviors ---------------------------------------------------

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

	rec, _ := challengePage(t, m, "/")
	jar := passChallenge(t, m, rec.Body.String(), cookiesFrom(rec))

	next := &recordingNext{}
	rec = httptest.NewRecorder()
	if err := m.ServeHTTP(rec, newRequest(http.MethodGet, "/", jar), next); err != nil {
		t.Fatal(err)
	}
	if !next.Called {
		t.Fatalf("verified client must reach next, body=%s", rec.Body.String())
	}
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

// --- global server lifecycle -----------------------------------------------

// A reload that drops the top-level instance must re-point the global at a
// surviving handle-block server so static assets keep working.
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
