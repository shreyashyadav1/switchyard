// Package config loads and validates the gateway's YAML configuration.
package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole gateway configuration.
type Config struct {
	Listen string `yaml:"listen"`
	// AllowAnonymous lets requests without an API key through. It must be set
	// explicitly when no keys are configured, so a missing keys section never
	// silently opens the gateway.
	AllowAnonymous bool                `yaml:"allow_anonymous"`
	Providers      map[string]Provider `yaml:"providers"`
	Models         []Model             `yaml:"models"`
	Keys           []Key               `yaml:"keys"`
	Cache          Cache               `yaml:"cache"`
	Retry          Retry               `yaml:"retry"`
	// UpstreamTimeout bounds the wait for an upstream's response headers.
	UpstreamTimeout time.Duration `yaml:"upstream_timeout"`
	// RequestTimeout bounds a whole client request, including retries,
	// fallbacks and the streamed body.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	MaxBodyBytes   int64         `yaml:"max_body_bytes"`
}

// Provider is an OpenAI-compatible upstream API.
type Provider struct {
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names the environment variable holding the provider's key.
	// Leave it empty for upstreams without auth, such as a local vLLM server.
	APIKeyEnv string            `yaml:"api_key_env"`
	Headers   map[string]string `yaml:"headers"`

	APIKey string `yaml:"-"`
}

// Model is a name clients send in the "model" field, served by an ordered
// list of targets. Later targets are fallbacks for earlier ones.
type Model struct {
	Name    string   `yaml:"name"`
	Targets []Target `yaml:"targets"`
}

// Target is one provider/model pair behind a model name.
type Target struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	Price    Price  `yaml:"price"`
}

// Label identifies the target in logs, metrics and response headers.
func (t Target) Label() string { return t.Provider + "/" + t.Model }

// Price is in US dollars per million tokens.
type Price struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

// Key is a client API key. Only the SHA-256 digest of the token is stored.
type Key struct {
	Name              string  `yaml:"name"`
	SHA256            string  `yaml:"sha256"`
	RequestsPerMinute int     `yaml:"requests_per_minute"`
	DailyBudgetUSD    float64 `yaml:"daily_budget_usd"`
}

// Cache configures the exact-match response cache.
type Cache struct {
	Enabled    bool          `yaml:"enabled"`
	TTL        time.Duration `yaml:"ttl"`
	MaxEntries int           `yaml:"max_entries"`
}

// Retry configures retries against a single target before falling back.
type Retry struct {
	MaxAttempts int           `yaml:"max_attempts"`
	BaseDelay   time.Duration `yaml:"base_delay"`
	MaxDelay    time.Duration `yaml:"max_delay"`
}

// Load reads, defaults and validates the file at path, and resolves provider
// keys from the environment.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, os.LookupEnv)
}

// Parse is Load without the file read; lookupEnv resolves provider keys.
func Parse(data []byte, lookupEnv func(string) (string, bool)) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	for name, p := range c.Providers {
		if p.APIKeyEnv == "" {
			continue
		}
		key, ok := lookupEnv(p.APIKeyEnv)
		if !ok || key == "" {
			return nil, fmt.Errorf("provider %q: environment variable %s is not set", name, p.APIKeyEnv)
		}
		p.APIKey = key
		c.Providers[name] = p
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 2
	}
	if c.Retry.BaseDelay == 0 {
		c.Retry.BaseDelay = 250 * time.Millisecond
	}
	if c.Retry.MaxDelay == 0 {
		c.Retry.MaxDelay = 4 * time.Second
	}
	if c.UpstreamTimeout == 0 {
		c.UpstreamTimeout = 30 * time.Second
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 5 * time.Minute
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 4 << 20
	}
	if c.Cache.Enabled {
		if c.Cache.TTL == 0 {
			c.Cache.TTL = 10 * time.Minute
		}
		if c.Cache.MaxEntries == 0 {
			c.Cache.MaxEntries = 1000
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	if len(c.Providers) == 0 {
		errs = append(errs, errors.New("at least one provider is required"))
	}
	for name, p := range c.Providers {
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("provider %q: base_url must be an http(s) URL", name))
		}
	}
	if len(c.Models) == 0 {
		errs = append(errs, errors.New("at least one model is required"))
	}
	seenModels := map[string]bool{}
	for i, m := range c.Models {
		if m.Name == "" {
			errs = append(errs, fmt.Errorf("models[%d]: name is required", i))
		}
		if seenModels[m.Name] {
			errs = append(errs, fmt.Errorf("model %q is defined twice", m.Name))
		}
		seenModels[m.Name] = true
		if len(m.Targets) == 0 {
			errs = append(errs, fmt.Errorf("model %q: at least one target is required", m.Name))
		}
		for j, t := range m.Targets {
			if _, ok := c.Providers[t.Provider]; !ok {
				errs = append(errs, fmt.Errorf("model %q target %d: unknown provider %q", m.Name, j, t.Provider))
			}
			if t.Model == "" {
				errs = append(errs, fmt.Errorf("model %q target %d: model is required", m.Name, j))
			}
			if t.Price.Input < 0 || t.Price.Output < 0 {
				errs = append(errs, fmt.Errorf("model %q target %d: prices cannot be negative", m.Name, j))
			}
		}
	}
	if len(c.Keys) == 0 && !c.AllowAnonymous {
		errs = append(errs, errors.New("no keys configured: add keys or set allow_anonymous: true"))
	}
	seenKeys := map[string]bool{}
	for i, k := range c.Keys {
		if k.Name == "" {
			errs = append(errs, fmt.Errorf("keys[%d]: name is required", i))
		}
		if seenKeys[k.Name] {
			errs = append(errs, fmt.Errorf("key %q is defined twice", k.Name))
		}
		seenKeys[k.Name] = true
		if b, err := hex.DecodeString(k.SHA256); err != nil || len(b) != 32 {
			errs = append(errs, fmt.Errorf("key %q: sha256 must be 64 hex characters", k.Name))
		}
		if k.RequestsPerMinute < 0 || k.DailyBudgetUSD < 0 {
			errs = append(errs, fmt.Errorf("key %q: limits cannot be negative", k.Name))
		}
	}
	if c.Retry.MaxAttempts < 1 {
		errs = append(errs, errors.New("retry.max_attempts must be at least 1"))
	}
	if c.Cache.Enabled && c.Cache.MaxEntries < 0 {
		errs = append(errs, errors.New("cache.max_entries cannot be negative"))
	}
	return errors.Join(errs...)
}
