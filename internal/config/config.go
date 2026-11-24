package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type PluginConfig struct {
	Enabled       bool   `yaml:"enabled"` // read by the CPA host itself
	DefaultRegion string `yaml:"default_region"`
	ClientVersion string `yaml:"client_version"`
	OSVersion     string `yaml:"os_version"`
	NodeVersion   string `yaml:"node_version"`
	// Per-credential pacing. Parsed by Durations; empty keeps the default.
	MinTokenInterval string `yaml:"min_token_interval"`
	MaxTokenInterval string `yaml:"max_token_interval"`
	SuspendCooldown  string `yaml:"suspend_cooldown"`
	// Usage accounting. Amazon Q reports spend in credits and never sends token
	// counts, so usage has to be derived. Empty values keep the defaults.
	//
	// UsageMode selects the source: "estimate" keeps the local character
	// heuristic, "credit" derives tokens from the metered credit spend, and
	// "off" reports the estimate without cache attribution.
	UsageMode string `yaml:"usage_mode"`
	// CreditRates maps a model (or "*" for all) to the credits billed per
	// token. An entry may be a single number (input rate, output priced the
	// same) or a mapping with input/output. A longer, exact model name wins
	// over a wildcard.
	CreditRates map[string]RateSpec `yaml:"credit_rates"`
	// CacheReadRatio is the fraction of the input rate a cached input token is
	// billed at. Upstream does not itemize cached tokens, so the plugin splits
	// credit spend between fresh and cached input using this ratio.
	CacheReadRatio string `yaml:"cache_read_ratio"`
	// MaxCacheReadShare caps how much of the caller's prompt the plugin reports
	// as cached when it derives the split from credit spend. A cheap turn
	// implies "almost everything was cached", which without a cap becomes a
	// 100% cache hit: the caller pays almost nothing while the operator still
	// pays the service for the whole prompt. 1 leaves the derivation uncapped;
	// 0 disables the cache claim entirely.
	MaxCacheReadShare string `yaml:"max_cache_read_share"`
}

var defaults = PluginConfig{
	Enabled:          true,
	DefaultRegion:    "us-east-1",
	ClientVersion:    "1.4.5",
	OSVersion:        "darwin#24.6.0",
	NodeVersion:      "22.22.0",
	MinTokenInterval: "1s",
	MaxTokenInterval: "2s",
	SuspendCooldown:  "1h",
	UsageMode:        "credit",
	// Measured against the live service on 2026-11-19, in credits per token,
	// with 1000 credits = $20. Re-measure if Kiro changes its pricing.
	CreditRates: map[string]RateSpec{
		"*":              {Input: "4.24e-6", Output: "4.24e-6"},
		"claude-haiku*":  {Input: "4.39e-6", Output: "1.29e-5"},
		"claude-sonnet*": {Input: "3.53e-5", Output: "2.87e-4"},
		"claude-opus*":   {Input: "5.13e-5", Output: "4.48e-4"},
	},
	CacheReadRatio: "0.10",
	// Nine cached tokens in ten is already a warm conversation; claiming the
	// full prompt leaves no floor under the fresh input the service charged.
	MaxCacheReadShare: "0.9",
}

// RateSpec is one credits-per-token entry. A bare YAML scalar sets Input and
// prices output the same, so a single-rate config keeps working.
type RateSpec struct {
	Input  string
	Output string
}

// UnmarshalYAML accepts either `4.24e-6` or `{input: 3.53e-5, output: 2.87e-4}`.
func (r *RateSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		r.Input, r.Output = node.Value, node.Value
		return nil
	}
	var raw struct {
		Input  string `yaml:"input"`
		Output string `yaml:"output"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	r.Input, r.Output = raw.Input, raw.Output
	if r.Output == "" {
		r.Output = r.Input
	}
	return nil
}

// MarshalYAML writes the compact scalar form when both rates agree, so a saved
// config stays readable.
func (r RateSpec) MarshalYAML() (any, error) {
	if r.Output == "" || r.Output == r.Input {
		return r.Input, nil
	}
	return struct {
		Input  string `yaml:"input"`
		Output string `yaml:"output"`
	}{r.Input, r.Output}, nil
}

var (
	currentConfig = defaults
	cfgLock       sync.RWMutex
)

func Get() PluginConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return currentConfig
}

// UpdateFromYAML parses the plugin's own config block. CPA passes only
// plugins.configs.<plugin-id>, not the root config file.
func UpdateFromYAML(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	cfg := defaults
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	cfg = cfg.withDefaults()
	// A rejected block keeps the previous configuration; failing here surfaces
	// the typo in CPA's log instead of silently pacing with something else.
	if _, err := cfg.Durations(); err != nil {
		return err
	}
	if _, err := cfg.Usage(); err != nil {
		return err
	}
	cfgLock.Lock()
	currentConfig = cfg
	cfgLock.Unlock()
	return nil
}

func (c PluginConfig) withDefaults() PluginConfig {
	if c.DefaultRegion == "" {
		c.DefaultRegion = defaults.DefaultRegion
	}
	if c.ClientVersion == "" {
		c.ClientVersion = defaults.ClientVersion
	}
	if c.OSVersion == "" {
		c.OSVersion = defaults.OSVersion
	}
	if c.NodeVersion == "" {
		c.NodeVersion = defaults.NodeVersion
	}
	if c.MinTokenInterval == "" {
		c.MinTokenInterval = defaults.MinTokenInterval
	}
	if c.MaxTokenInterval == "" {
		c.MaxTokenInterval = defaults.MaxTokenInterval
	}
	if c.SuspendCooldown == "" {
		c.SuspendCooldown = defaults.SuspendCooldown
	}
	if c.UsageMode == "" {
		c.UsageMode = defaults.UsageMode
	}
	if len(c.CreditRates) == 0 {
		c.CreditRates = defaults.CreditRates
	}
	if c.CacheReadRatio == "" {
		c.CacheReadRatio = defaults.CacheReadRatio
	}
	if c.MaxCacheReadShare == "" {
		c.MaxCacheReadShare = defaults.MaxCacheReadShare
	}
	return c
}

// DefaultUsageSettings returns the shipped usage accounting configuration. It is
// the fallback for callers whose parsed settings were rejected.
func DefaultUsageSettings() UsageSettings {
	settings, _ := PluginConfig{}.withUsageDefaults().Usage()
	return settings
}

// withUsageDefaults fills only the usage accounting fields, so a caller whose
// parse failed can still fall back to the shipped rates.
func (c PluginConfig) withUsageDefaults() PluginConfig {
	if c.UsageMode == "" {
		c.UsageMode = defaults.UsageMode
	}
	if len(c.CreditRates) == 0 {
		c.CreditRates = defaults.CreditRates
	}
	if c.CacheReadRatio == "" {
		c.CacheReadRatio = defaults.CacheReadRatio
	}
	if c.MaxCacheReadShare == "" {
		c.MaxCacheReadShare = defaults.MaxCacheReadShare
	}
	return c
}

// UsageSettings carries the parsed usage accounting configuration.
type UsageSettings struct {
	Mode           string
	CreditRates    []ModelCreditRate
	CacheReadRatio float64
	// MaxCacheReadShare bounds the cached share of the caller's prompt that the
	// credit derivation may claim; 1 means uncapped, 0 means no cache reads.
	MaxCacheReadShare float64
}

// ModelCreditRate is one credits-per-token entry; Pattern may end in "*".
type ModelCreditRate struct {
	Pattern string
	// Input and Output are credits per token. They differ: the service bills an
	// output token around eight times an input token.
	Input  float64
	Output float64
}

// Usage parses the usage accounting settings, reporting the first invalid value.
func (c PluginConfig) Usage() (UsageSettings, error) {
	mode := strings.ToLower(strings.TrimSpace(c.UsageMode))
	switch mode {
	case "estimate", "credit", "off":
	case "":
		mode = "credit"
	default:
		return UsageSettings{}, fmt.Errorf("usage_mode must be estimate, credit or off, got %q", c.UsageMode)
	}
	rates := make([]ModelCreditRate, 0, len(c.CreditRates))
	for pattern, raw := range c.CreditRates {
		input, err := strconv.ParseFloat(strings.TrimSpace(raw.Input), 64)
		if err != nil || input <= 0 {
			return UsageSettings{}, fmt.Errorf("credit_rates[%q] input must be a positive number, got %q", pattern, raw.Input)
		}
		output := input
		if raw.Output != "" {
			output, err = strconv.ParseFloat(strings.TrimSpace(raw.Output), 64)
			if err != nil || output <= 0 {
				return UsageSettings{}, fmt.Errorf("credit_rates[%q] output must be a positive number, got %q", pattern, raw.Output)
			}
		}
		rates = append(rates, ModelCreditRate{Pattern: strings.TrimSpace(pattern), Input: input, Output: output})
	}
	// Longest pattern first so an exact name beats a wildcard, and a wildcard
	// named "*" is consulted last.
	sort.Slice(rates, func(i, j int) bool {
		if rates[i].Pattern == "*" {
			return false
		}
		if rates[j].Pattern == "*" {
			return true
		}
		return len(rates[i].Pattern) > len(rates[j].Pattern)
	})
	ratio, err := strconv.ParseFloat(strings.TrimSpace(c.CacheReadRatio), 64)
	if err != nil || ratio < 0 || ratio > 1 {
		return UsageSettings{}, fmt.Errorf("cache_read_ratio must be between 0 and 1, got %q", c.CacheReadRatio)
	}
	maxShareRaw := strings.TrimSpace(c.MaxCacheReadShare)
	if maxShareRaw == "" {
		maxShareRaw = defaults.MaxCacheReadShare
	}
	maxShare, err := strconv.ParseFloat(maxShareRaw, 64)
	if err != nil || maxShare < 0 || maxShare > 1 {
		return UsageSettings{}, fmt.Errorf("max_cache_read_share must be between 0 and 1, got %q", c.MaxCacheReadShare)
	}
	return UsageSettings{
		Mode:              mode,
		CreditRates:       rates,
		CacheReadRatio:    ratio,
		MaxCacheReadShare: maxShare,
	}, nil
}

// CreditRateFor resolves the credits-per-token entry for one model.
func (s UsageSettings) CreditRateFor(model string) (ModelCreditRate, bool) {
	lowered := strings.ToLower(model)
	for _, entry := range s.CreditRates {
		if entry.Pattern == "*" {
			continue
		}
		if pattern := strings.ToLower(entry.Pattern); strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(lowered, strings.TrimSuffix(pattern, "*")) {
				return entry, true
			}
			continue
		}
		if strings.EqualFold(entry.Pattern, model) {
			return entry, true
		}
	}
	for _, entry := range s.CreditRates {
		if entry.Pattern == "*" {
			return entry, true
		}
	}
	return ModelCreditRate{}, false
}

// Pacing carries the parsed per-credential limits.
type Pacing struct {
	MinTokenInterval time.Duration
	MaxTokenInterval time.Duration
	SuspendCooldown  time.Duration
}

// Durations parses the pacing settings, reporting the first invalid value.
func (c PluginConfig) Durations() (Pacing, error) {
	minInterval, err := time.ParseDuration(c.MinTokenInterval)
	if err != nil {
		return Pacing{}, fmt.Errorf("invalid min_token_interval %q: %w", c.MinTokenInterval, err)
	}
	if minInterval <= 0 {
		return Pacing{}, fmt.Errorf("min_token_interval must be positive, got %q", c.MinTokenInterval)
	}
	maxInterval, err := time.ParseDuration(c.MaxTokenInterval)
	if err != nil {
		return Pacing{}, fmt.Errorf("invalid max_token_interval %q: %w", c.MaxTokenInterval, err)
	}
	if maxInterval < minInterval {
		return Pacing{}, fmt.Errorf("max_token_interval %q is shorter than min_token_interval %q", c.MaxTokenInterval, c.MinTokenInterval)
	}
	cooldown, err := time.ParseDuration(c.SuspendCooldown)
	if err != nil {
		return Pacing{}, fmt.Errorf("invalid suspend_cooldown %q: %w", c.SuspendCooldown, err)
	}
	if cooldown <= 0 {
		return Pacing{}, fmt.Errorf("suspend_cooldown must be positive, got %q", c.SuspendCooldown)
	}
	return Pacing{MinTokenInterval: minInterval, MaxTokenInterval: maxInterval, SuspendCooldown: cooldown}, nil
}
