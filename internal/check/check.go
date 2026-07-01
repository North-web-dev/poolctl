// Package check validates a token via HTTP or a shell command.
package check

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/North-web-dev/poolctl/internal/config"
)

// Result is the outcome of a single check.
type Result struct {
	OK      bool
	Latency time.Duration
	Status  int // HTTP status, 0 for command checks
	Err     error
}

// Checker validates a token value.
type Checker interface {
	Check(ctx context.Context, token string) Result
}

// New builds a Checker from config. proxy applies to http checks only.
func New(c config.Check, proxy string) (Checker, error) {
	switch c.Type {
	case "", "http":
		return newHTTP(c, proxy)
	case "command":
		return &commandChecker{cmd: c.Cmd, want: c.SuccessOutput, timeout: dur(c.TimeoutSec)}, nil
	default:
		return nil, fmt.Errorf("unknown check type %q", c.Type)
	}
}

func dur(sec int) time.Duration { return time.Duration(sec) * time.Second }

type httpChecker struct {
	url     string
	method  string
	headers map[string]string
	success map[int]bool
	client  *http.Client
}

func newHTTP(c config.Check, proxy string) (*httpChecker, error) {
	if c.URL == "" {
		return nil, fmt.Errorf("check.url is required for http checks")
	}
	tr := &http.Transport{}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("bad proxy: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	success := map[int]bool{}
	for _, s := range c.SuccessStatus {
		success[s] = true
	}
	if len(success) == 0 {
		success[200] = true
	}
	return &httpChecker{
		url:     c.URL,
		method:  c.Method,
		headers: c.Headers,
		success: success,
		client:  &http.Client{Timeout: dur(c.TimeoutSec), Transport: tr},
	}, nil
}

func (h *httpChecker) Check(ctx context.Context, token string) Result {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, h.method, sub(h.url, token), nil)
	if err != nil {
		return Result{Err: err}
	}
	for k, v := range h.headers {
		req.Header.Set(k, sub(v, token))
	}
	resp, err := h.client.Do(req)
	lat := time.Since(start)
	if err != nil {
		return Result{Latency: lat, Err: err}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return Result{OK: h.success[resp.StatusCode], Latency: lat, Status: resp.StatusCode}
}

type commandChecker struct {
	cmd     string
	want    string
	timeout time.Duration
}

func (c *commandChecker) Check(ctx context.Context, token string) Result {
	start := time.Now()
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	out, err := exec.CommandContext(ctx, "sh", "-c", sub(c.cmd, token)).CombinedOutput()
	lat := time.Since(start)
	if err != nil {
		return Result{Latency: lat, Err: err}
	}
	ok := c.want == "" || strings.Contains(string(out), c.want)
	return Result{OK: ok, Latency: lat}
}

func sub(s, token string) string { return strings.ReplaceAll(s, "{token}", token) }
