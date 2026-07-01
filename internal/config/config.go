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

// Config is the top-level poolctl configuration.
type Config struct {
	TokensFile         string  `yaml:"tokens_file"`
	Rotation           string  `yaml:"rotation"` // lru | round_robin | random
	CooldownSec        int     `yaml:"cooldown_sec"`
	RecheckIntervalSec int     `yaml:"recheck_interval_sec"`
	Check              Check   `yaml:"check"`
	Refresh            Refresh `yaml:"refresh"`
	Proxy              string  `yaml:"proxy"`
	Server             Server  `yaml:"server"`
	StateFile          string  `yaml:"state_file"`
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
	case "lru", "round_robin", "random":
	default:
		return nil, fmt.Errorf("invalid rotation %q (want lru|round_robin|random)", c.Rotation)
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
}
