// Package metrics exposes pool and proxy counters in Prometheus text format
// without pulling in the Prometheus client library.
package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/North-web-dev/poolctl/internal/pool"
)

// Registry holds process-wide counters and a reference to the pool for gauges.
// All counter methods are safe for concurrent use.
type Registry struct {
	pool     *pool.Pool
	requests atomic.Int64
	errors   atomic.Int64
	retries  atomic.Int64
}

// New builds a Registry reading gauges from p.
func New(p *pool.Pool) *Registry { return &Registry{pool: p} }

// AddRequest counts a client request served by the proxy.
func (r *Registry) AddRequest() { r.requests.Add(1) }

// AddError counts a request the proxy could not satisfy (no token, transport
// error, or a non-retryable error status).
func (r *Registry) AddError() { r.errors.Add(1) }

// AddRetry counts one proxy retry with a different token.
func (r *Registry) AddRetry() { r.retries.Add(1) }

// Handler writes the current metrics in Prometheus text exposition format.
func (r *Registry) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		s := r.pool.Status()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, "# HELP poolctl_tokens Number of tokens by status.\n")
		fmt.Fprint(w, "# TYPE poolctl_tokens gauge\n")
		fmt.Fprintf(w, "poolctl_tokens{status=\"live\"} %d\n", s.Live)
		fmt.Fprintf(w, "poolctl_tokens{status=\"dead\"} %d\n", s.Dead)
		fmt.Fprintf(w, "poolctl_tokens{status=\"quarantined\"} %d\n", s.Quarantined)
		fmt.Fprintf(w, "poolctl_tokens{status=\"unknown\"} %d\n", s.Unknown)
		fmt.Fprint(w, "# HELP poolctl_tokens_cooling Tokens currently in cooldown.\n")
		fmt.Fprint(w, "# TYPE poolctl_tokens_cooling gauge\n")
		fmt.Fprintf(w, "poolctl_tokens_cooling %d\n", s.Cooling)
		fmt.Fprint(w, "# HELP poolctl_requests_total Client requests served by the proxy.\n")
		fmt.Fprint(w, "# TYPE poolctl_requests_total counter\n")
		fmt.Fprintf(w, "poolctl_requests_total %d\n", r.requests.Load())
		fmt.Fprint(w, "# HELP poolctl_errors_total Requests the proxy could not satisfy.\n")
		fmt.Fprint(w, "# TYPE poolctl_errors_total counter\n")
		fmt.Fprintf(w, "poolctl_errors_total %d\n", r.errors.Load())
		fmt.Fprint(w, "# HELP poolctl_proxy_retries_total Proxy retries with a different token.\n")
		fmt.Fprint(w, "# TYPE poolctl_proxy_retries_total counter\n")
		fmt.Fprintf(w, "poolctl_proxy_retries_total %d\n", r.retries.Load())
	}
}
