package config

import (
	"strings"
	"testing"
	"time"
)

const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

func TestParseValidConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
providers:
  groq:
    base_url: https://api.groq.com/openai/v1
    api_key_env: GROQ_API_KEY
  local:
    base_url: http://localhost:8000/v1
models:
  - name: fast
    targets:
      - provider: groq
        model: openai/gpt-oss-120b
        price: {input: 0.15, output: 0.60}
      - provider: local
        model: qwen
keys:
  - name: opspilot
    sha256: `+digest+`
    requests_per_minute: 60
    daily_budget_usd: 1.5
cache:
  enabled: true
retry:
  base_delay: 100ms
`), env(map[string]string{"GROQ_API_KEY": "gsk_test"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["groq"].APIKey; got != "gsk_test" {
		t.Errorf("groq key = %q", got)
	}
	if cfg.Providers["local"].APIKey != "" {
		t.Error("provider without api_key_env should have no key")
	}
	if cfg.Listen != ":8080" || cfg.Retry.MaxAttempts != 2 || cfg.Retry.BaseDelay != 100*time.Millisecond {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.Cache.TTL != 10*time.Minute || cfg.Cache.MaxEntries != 1000 {
		t.Errorf("cache defaults not applied: %+v", cfg.Cache)
	}
	if cfg.Breaker.Disabled || cfg.Breaker.FailureThreshold != 5 || cfg.Breaker.Cooldown != 30*time.Second {
		t.Errorf("breaker defaults not applied: %+v", cfg.Breaker)
	}
	if got := cfg.Models[0].Targets[0].Label(); got != "groq/openai/gpt-oss-120b" {
		t.Errorf("label = %q", got)
	}
}

func TestParseRejectsBadConfig(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want string
	}{
		"unknown field": {`
providers: {p: {base_url: "http://x"}}
models: [{name: m, targets: [{provider: p, model: x}]}]
allow_anonymous: true
retires: {}
`, "field retires not found"},
		"no keys": {`
providers: {p: {base_url: "http://x"}}
models: [{name: m, targets: [{provider: p, model: x}]}]
`, "no keys configured"},
		"unknown provider": {`
providers: {p: {base_url: "http://x"}}
models: [{name: m, targets: [{provider: q, model: x}]}]
allow_anonymous: true
`, `unknown provider "q"`},
		"bad url": {`
providers: {p: {base_url: "ftp://x"}}
models: [{name: m, targets: [{provider: p, model: x}]}]
allow_anonymous: true
`, "base_url must be"},
		"duplicate model": {`
providers: {p: {base_url: "http://x"}}
models:
  - {name: m, targets: [{provider: p, model: x}]}
  - {name: m, targets: [{provider: p, model: y}]}
allow_anonymous: true
`, "defined twice"},
		"bad digest": {`
providers: {p: {base_url: "http://x"}}
models: [{name: m, targets: [{provider: p, model: x}]}]
keys: [{name: k, sha256: abc}]
`, "64 hex characters"},
		"missing env": {`
providers: {p: {base_url: "http://x", api_key_env: NOPE}}
models: [{name: m, targets: [{provider: p, model: x}]}]
allow_anonymous: true
`, "NOPE is not set"},
		"negative breaker threshold": {`
providers: {p: {base_url: "http://x"}}
models: [{name: m, targets: [{provider: p, model: x}]}]
allow_anonymous: true
breaker: {failure_threshold: -1}
`, "failure_threshold must be at least 1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml), env(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
