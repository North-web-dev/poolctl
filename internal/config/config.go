// Package config loads and validates the poolctl YAML configuration.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Check describes how a token is validated.
type Check struct {
	Type          string            `yaml:"type"` // http | command
	URL           string            `yaml:"url"`
	Method        string            `yaml:"method"`
	Headers       map[string]string `yaml:"headers"`
	SuccessStatus []int             `yaml:"success_status"`
	TimeoutSec    int               `yaml:"timeout_sec"`
	Cmd           string            `yaml:"cmd"`            // for type: command
	SuccessOutput string            `yaml:"success_output"` // substring that marks success
}

// Refresh describes an optional token-refresh call (OAuth-style).
type Refresh struct {
	Enabled    bool              `yaml:"enabled"`
	URL        string            `yaml:"url"`
	Method     string            `yaml:"method"`
	Headers    map[string]string `yaml:"headers"`
	Body       string            `yaml:"body"`
	TokenField string            `yaml:"token_field"` // JSON field holding the new token
	TimeoutSec int               `yaml:"timeout_sec"`
}

// Server holds the daemon HTTP settings.
type Server struct {
	Addr   string `yaml:"addr"`
	APIKey string `yaml:"api_key"`
}

// Upstream configures the passthrough reverse proxy (LLM/API-gateway mode).
// Each incoming request borrows a healthy token, injects it as a header, and
// forwards to BaseURL; a retryable status is retried with a different token.
type Upstream struct {
	Enabled       bool   `yaml:"enabled"`
	Listen        string `yaml:"listen"`         // proxy listen address
	BaseURL       string `yaml:"base_url"`       // e.g. https://api.openai.com
	AuthHeader    string `yaml:"auth_header"`    // header the token is injected into
	AuthTemplate  string `yaml:"auth_template"`  // value template, {token} substituted
	RetryOn       []int  `yaml:"retry_on"`       // upstream statuses that trigger a retry
	MaxRetries    int    `yaml:"max_retries"`    // extra attempts beyond the first
	QuarantineSec int    `yaml:"quarantine_sec"` // rest after a 401/403 (hard) failure
	CooldownSec   int    `yaml:"cooldown_sec"`   // rest after a 429/5xx (soft) failure
	TimeoutSec    int    `yaml:"timeout_sec"`    // per-attempt upstream timeout
}

// Metrics configures the Prometheus text endpoint.
type Metrics struct {
	Addr string `yaml:"addr"` // if set, serve /metrics here
}

// Config is the top-level poolctl configuration.
type Config struct {
	TokensFile         string   `yaml:"tokens_file"`
	Rotation           string   `yaml:"rotation"` // lru | round_robin | random | weighted
	CooldownSec        int      `yaml:"cooldown_sec"`
	RecheckIntervalSec int      `yaml:"recheck_interval_sec"`
	Check              Check    `yaml:"check"`
	Refresh            Refresh  `yaml:"refresh"`
	Proxy              string   `yaml:"proxy"`
	Server             Server   `yaml:"server"`
	Upstream           Upstream `yaml:"upstream"`
	Metrics            Metrics  `yaml:"metrics"`
	StateFile          string   `yaml:"state_file"`
}

// Load reads and parses the config file, applying defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if c.TokensFile == "" {
		return nil, fmt.Errorf("tokens_file is required")
	}
	switch c.Rotation {
	case "lru", "round_robin", "random", "weighted":
	default:
		return nil, fmt.Errorf("invalid rotation %q (want lru|round_robin|random|weighted)", c.Rotation)
	}
	if c.Upstream.Enabled && c.Upstream.BaseURL == "" {
		return nil, fmt.Errorf("upstream.base_url is required when upstream.enabled")
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Rotation == "" {
		c.Rotation = "lru"
	}
	if c.CooldownSec == 0 {
		c.CooldownSec = 30
	}
	if c.RecheckIntervalSec == 0 {
		c.RecheckIntervalSec = 300
	}
	if c.Check.Method == "" {
		c.Check.Method = "GET"
	}
	if c.Check.TimeoutSec == 0 {
		c.Check.TimeoutSec = 15
	}
	if c.Refresh.Method == "" {
		c.Refresh.Method = "POST"
	}
	if c.Refresh.TimeoutSec == 0 {
		c.Refresh.TimeoutSec = 20
	}
	if c.Refresh.TokenField == "" {
		c.Refresh.TokenField = "access_token"
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8787"
	}
	if c.StateFile == "" {
		c.StateFile = "pool_state.json"
	}
	c.Upstream.applyDefaults(c.CooldownSec)
}

func (u *Upstream) applyDefaults(poolCooldownSec int) {
	if u.Listen == "" {
		u.Listen = ":8080"
	}
	if u.AuthHeader == "" {
		u.AuthHeader = "Authorization"
	}
	if u.AuthTemplate == "" {
		u.AuthTemplate = "Bearer {token}"
	}
	if len(u.RetryOn) == 0 {
		u.RetryOn = []int{401, 403, 429, 500, 502, 503, 504}
	}
	if u.MaxRetries == 0 {
		u.MaxRetries = 2
	}
	if u.QuarantineSec == 0 {
		u.QuarantineSec = 300
	}
	if u.CooldownSec == 0 {
		u.CooldownSec = poolCooldownSec
	}
	if u.TimeoutSec == 0 {
		u.TimeoutSec = 60
	}
}
