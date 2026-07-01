// Package proxy implements poolctl's passthrough reverse proxy: every request
// borrows a healthy token from the pool, injects it as a header, and forwards
// to a fixed upstream. Retryable upstream statuses are retried with a different
// token, and failing tokens are cooled down or quarantined.
package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/North-web-dev/poolctl/internal/config"
	"github.com/North-web-dev/poolctl/internal/metrics"
	"github.com/North-web-dev/poolctl/internal/pool"
)

// hopHeaders are per-connection headers that must not be forwarded.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// maxBody caps the request body buffered for replay across retries.
const maxBody = 32 << 20 // 32 MiB

// Proxy is an http.Handler that forwards to a single upstream using pooled
// tokens.
type Proxy struct {
	pool       *pool.Pool
	metrics    *metrics.Registry
	base       *url.URL
	authHeader string
	authTmpl   string
	retry      map[int]bool
	maxRetries int
	quarantine time.Duration
	cooldown   time.Duration
	client     *http.Client
}

// New builds a Proxy from the upstream config.
func New(p *pool.Pool, up config.Upstream, reg *metrics.Registry) (*Proxy, error) {
	base, err := url.Parse(up.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base_url: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("base_url must be an absolute URL, got %q", up.BaseURL)
	}
	retry := make(map[int]bool, len(up.RetryOn))
	for _, s := range up.RetryOn {
		retry[s] = true
	}
	// No client timeout: a long SSE stream must be allowed to run. Bound only
	// the time to first response byte via the transport.
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Duration(up.TimeoutSec) * time.Second,
	}
	return &Proxy{
		pool:       p,
		metrics:    reg,
		base:       base,
		authHeader: up.AuthHeader,
		authTmpl:   up.AuthTemplate,
		retry:      retry,
		maxRetries: up.MaxRetries,
		quarantine: time.Duration(up.QuarantineSec) * time.Second,
		cooldown:   time.Duration(up.CooldownSec) * time.Second,
		client:     &http.Client{Transport: tr},
	}, nil
}

func (px *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	px.metrics.AddRequest()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		px.metrics.AddError()
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	attempts := px.maxRetries + 1
	for i := 0; i < attempts; i++ {
		lease, ok := px.pool.Take()
		if !ok {
			px.metrics.AddError()
			http.Error(w, "no token available", http.StatusServiceUnavailable)
			return
		}

		resp, err := px.forward(r, body, lease.Value)
		if err != nil {
			px.pool.RecordUse(lease.ID, 0)
			px.pool.Penalize(lease.ID, px.cooldown)
			if i < attempts-1 {
				px.metrics.AddRetry()
				continue
			}
			px.metrics.AddError()
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
			return
		}

		px.pool.RecordUse(lease.ID, resp.StatusCode)
		if px.retry[resp.StatusCode] {
			px.penalize(lease.ID, resp.StatusCode)
			if i < attempts-1 {
				drain(resp)
				px.metrics.AddRetry()
				continue
			}
			// Out of retries: hand the last upstream response back as-is.
			px.metrics.AddError()
		}
		px.copyResponse(w, resp)
		return
	}
}

// forward builds and sends the outbound request for one attempt.
func (px *Proxy) forward(r *http.Request, body []byte, token string) (*http.Response, error) {
	out := *px.base
	out.Path = singleJoin(px.base.Path, r.URL.Path)
	out.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), r.Method, out.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Set(px.authHeader, strings.ReplaceAll(px.authTmpl, "{token}", token))
	req.ContentLength = int64(len(body))
	return px.client.Do(req)
}

// penalize applies the right rest to a token given a failing status: a hard
// 401/403 quarantines it; anything else (429, 5xx) is a soft cooldown.
func (px *Proxy) penalize(id string, status int) {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		px.pool.Quarantine(id, px.quarantine)
		return
	}
	px.pool.Penalize(id, px.cooldown)
}

// copyResponse streams the upstream response back to the client, flushing each
// chunk so server-sent events reach the client as they arrive.
func (px *Proxy) copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	dst := w.Header()
	for k, vs := range resp.Header {
		if isHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if isHop(k) || k == "Host" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func isHop(key string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(key, h) {
			return true
		}
	}
	return false
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

func singleJoin(a, b string) string {
	switch {
	case a == "" || a == "/":
		return b
	case b == "":
		return a
	case strings.HasSuffix(a, "/") && strings.HasPrefix(b, "/"):
		return a + b[1:]
	case !strings.HasSuffix(a, "/") && !strings.HasPrefix(b, "/"):
		return a + "/" + b
	default:
		return a + b
	}
}
