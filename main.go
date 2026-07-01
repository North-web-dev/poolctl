// Command poolctl manages a pool of tokens/accounts: health checks, rotation,
// cooldown, optional refresh, and an HTTP API for handing tokens out.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/North-web-dev/poolctl/internal/check"
	"github.com/North-web-dev/poolctl/internal/config"
	"github.com/North-web-dev/poolctl/internal/pool"
	"github.com/North-web-dev/poolctl/internal/server"
)

const usage = `poolctl - token/account pool manager

usage:
  poolctl serve  -c pool.yaml   run the daemon (health loop + HTTP API)
  poolctl check  -c pool.yaml   check every token once and print a table
  poolctl status -c pool.yaml   query a running daemon's /status
  poolctl take   -c pool.yaml   take one token from a running daemon
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	cfgPath := flagValue(os.Args[2:], "-c", "pool.yaml")

	var err error
	switch cmd {
	case "serve":
		err = runServe(cfgPath)
	case "check":
		err = runCheck(cfgPath)
	case "status":
		err = runStatus(cfgPath)
	case "take":
		err = runTake(cfgPath)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func flagValue(args []string, name, def string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"=")
		}
	}
	return def
}

// buildPool loads config, tokens, and persisted state into a ready pool.
func buildPool(cfg *config.Config) (*pool.Pool, error) {
	p := pool.New(pool.Options{
		Rotation:  cfg.Rotation,
		Cooldown:  time.Duration(cfg.CooldownSec) * time.Second,
		StateFile: cfg.StateFile,
	})
	if err := loadTokens(p, cfg.TokensFile); err != nil {
		return nil, err
	}
	if err := p.LoadState(); err != nil {
		return nil, err
	}
	return p, nil
}

func loadTokens(p *pool.Pool, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	p.SetTokens(strings.Split(string(b), "\n"))
	return nil
}

func runServe(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	p, err := buildPool(cfg)
	if err != nil {
		return err
	}
	checker, err := check.New(cfg.Check, cfg.Proxy)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reload := func() error { return loadTokens(p, cfg.TokensFile) }
	srv := &http.Server{Addr: cfg.Server.Addr, Handler: server.New(p, cfg.Server.APIKey, reload).Handler()}

	go recheckLoop(ctx, p, checker, cfg)
	go saveLoop(ctx, p)

	go func() {
		fmt.Printf("poolctl serving on %s (%d tokens, rotation=%s)\n", cfg.Server.Addr, len(p.Entries()), cfg.Rotation)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "server:", err)
			stop()
		}
	}()

	<-ctx.Done()
	fmt.Println("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return p.Save()
}

// recheckLoop validates every token immediately and on an interval, attempting
// a refresh for any that fail when refresh is enabled.
func recheckLoop(ctx context.Context, p *pool.Pool, checker check.Checker, cfg *config.Config) {
	checkAll(ctx, p, checker, cfg)
	t := time.NewTicker(time.Duration(cfg.RecheckIntervalSec) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			checkAll(ctx, p, checker, cfg)
		}
	}
}

func checkAll(ctx context.Context, p *pool.Pool, checker check.Checker, cfg *config.Config) {
	entries := p.Entries()
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(e pool.Lease) {
			defer wg.Done()
			defer func() { <-sem }()
			res := checker.Check(ctx, e.Value)
			if res.OK {
				p.MarkChecked(e.ID, true)
				return
			}
			p.MarkChecked(e.ID, false)
			if cfg.Refresh.Enabled {
				if v, err := refresh(ctx, cfg.Refresh, e.Value); err == nil && v != "" {
					p.SetValue(e.ID, v)
				}
			}
		}(e)
	}
	wg.Wait()
}

func saveLoop(ctx context.Context, p *pool.Pool) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.Save(); err != nil {
				fmt.Fprintln(os.Stderr, "save:", err)
			}
		}
	}
}

// refresh calls the configured refresh endpoint and extracts the new token.
func refresh(ctx context.Context, r config.Refresh, token string) (string, error) {
	body := strings.ReplaceAll(r.Body, "{token}", token)
	req, err := http.NewRequestWithContext(ctx, r.Method, strings.ReplaceAll(r.URL, "{token}", token), bytes.NewBufferString(body))
	if err != nil {
		return "", err
	}
	for k, v := range r.Headers {
		req.Header.Set(k, strings.ReplaceAll(v, "{token}", token))
	}
	client := &http.Client{Timeout: time.Duration(r.TimeoutSec) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return "", err
	}
	if v, ok := m[r.TokenField].(string); ok {
		return v, nil
	}
	return "", fmt.Errorf("field %q not found in refresh response", r.TokenField)
}

func runCheck(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	p, err := buildPool(cfg)
	if err != nil {
		return err
	}
	checker, err := check.New(cfg.Check, cfg.Proxy)
	if err != nil {
		return err
	}
	entries := p.Entries()
	type row struct {
		id, status string
		ms         int64
	}
	rows := make([]row, len(entries))
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, e pool.Lease) {
			defer wg.Done()
			defer func() { <-sem }()
			res := checker.Check(context.Background(), e.Value)
			st := "live"
			if !res.OK {
				st = "dead"
				if res.Err != nil {
					st = "err"
				}
			}
			rows[i] = row{id: e.ID, status: st, ms: res.Latency.Milliseconds()}
		}(i, e)
	}
	wg.Wait()

	live := 0
	fmt.Printf("%-20s %-6s %8s\n", "ID", "STATUS", "LATENCY")
	for _, r := range rows {
		if r.status == "live" {
			live++
		}
		fmt.Printf("%-20s %-6s %6dms\n", trunc(r.id, 20), r.status, r.ms)
	}
	fmt.Printf("\n%d/%d live\n", live, len(rows))
	return p.Save()
}

func runStatus(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	var s pool.Snapshot
	if err := daemonGET(cfg, "/status", &s); err != nil {
		return err
	}
	fmt.Printf("total %d | live %d | dead %d | unknown %d | cooling %d\n",
		s.Total, s.Live, s.Dead, s.Unknown, s.Cooling)
	return nil
}

func runTake(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := daemonGET(cfg, "/take", &out); err != nil {
		return err
	}
	if out.Error != "" {
		return fmt.Errorf(out.Error)
	}
	fmt.Println(out.Token)
	return nil
}

func daemonGET(cfg *config.Config, path string, v any) error {
	addr := cfg.Server.Addr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	if cfg.Server.APIKey != "" {
		req.Header.Set("X-API-Key", cfg.Server.APIKey)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("daemon not reachable at %s: %w", addr, err)
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
