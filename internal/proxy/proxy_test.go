package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/North-web-dev/poolctl/internal/config"
	"github.com/North-web-dev/poolctl/internal/metrics"
	"github.com/North-web-dev/poolctl/internal/pool"
)

// upstreamConfig returns an Upstream pointing at baseURL with sane test values.
func upstreamConfig(baseURL string, retryOn []int, maxRetries int) config.Upstream {
	return config.Upstream{
		Enabled:       true,
		BaseURL:       baseURL,
		AuthHeader:    "Authorization",
		AuthTemplate:  "Bearer {token}",
		RetryOn:       retryOn,
		MaxRetries:    maxRetries,
		QuarantineSec: 300,
		CooldownSec:   30,
		TimeoutSec:    5,
	}
}

func newProxy(t *testing.T, p *pool.Pool, up config.Upstream) *Proxy {
	t.Helper()
	px, err := New(p, up, metrics.New(p))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return px
}

func TestProxyRetriesWithDifferentToken(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seen = append(seen, tok)
		if tok == "t1" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok:"+tok)
	}))
	defer upstream.Close()

	p := pool.New(pool.Options{Rotation: "round_robin"})
	p.SetTokens([]string{"t1,t1", "t2,t2"}) // round_robin hands out t1 first
	px := newProxy(t, p, upstreamConfig(upstream.URL, []int{429}, 2))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat", strings.NewReader("payload"))
	px.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "ok:t2" {
		t.Fatalf("body = %q, want ok:t2", got)
	}
	if len(seen) != 2 || seen[0] != "t1" || seen[1] != "t2" {
		t.Fatalf("upstream saw %v, want [t1 t2]", seen)
	}
	// t1 took a soft 429 → cooling but still alive; t2 served the request.
	if s := p.Status(); s.Dead != 0 || s.Quarantined != 0 {
		t.Fatalf("no token should be dead/quarantined on 429: %+v", s)
	}
}

func TestProxyQuarantinesOn401(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	p := pool.New(pool.Options{})
	p.SetTokens([]string{"only,only"})
	// MaxRetries 0: a single attempt, so the 401 is returned to the client and
	// the token is quarantined for the hard failure.
	px := newProxy(t, p, upstreamConfig(upstream.URL, []int{401, 403, 429}, 0))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	px.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if s := p.Status(); s.Quarantined != 1 {
		t.Fatalf("quarantined = %d, want 1", s.Quarantined)
	}
}

func TestProxyNoTokenAvailable(t *testing.T) {
	p := pool.New(pool.Options{}) // empty pool
	px := newProxy(t, p, upstreamConfig("http://127.0.0.1:1", nil, 1))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	px.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestProxyForwardsBodyAndPath(t *testing.T) {
	var gotPath, gotBody, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := pool.New(pool.Options{})
	p.SetTokens([]string{"k,k"})
	px := newProxy(t, p, upstreamConfig(upstream.URL+"/base", nil, 0))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat?stream=true", strings.NewReader("hello"))
	px.ServeHTTP(rec, req)

	if gotPath != "/base/v1/chat" {
		t.Fatalf("path = %q, want /base/v1/chat", gotPath)
	}
	if gotQuery != "stream=true" {
		t.Fatalf("query = %q, want stream=true", gotQuery)
	}
	if gotBody != "hello" {
		t.Fatalf("body = %q, want hello", gotBody)
	}
}
