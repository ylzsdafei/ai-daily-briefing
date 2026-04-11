// Package config loads the briefing-v3 YAML config and resolves
// environment variable overrides.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full briefing-v3 configuration loaded from YAML.
type Config struct {
	Domain   DomainConfig    `yaml:"domain"`
	Window   WindowConfig    `yaml:"window"`
	LLM      LLMConfig       `yaml:"llm"`
	Rank     RankConfig      `yaml:"rank"`
	Gate     GateConfig      `yaml:"gate"`
	Slack    SlackConfig     `yaml:"slack"`
	Image    ImageConfig     `yaml:"image"`
	Sections []SectionConfig `yaml:"sections"`
	Sources  []SourceConfig  `yaml:"sources"`
}

// RankConfig mirrors the `rank:` block in config/ai.yaml. Currently only
// PerCategoryQuota is consumed; add more fields here as rank gains
// knobs. All fields are optional — an empty block keeps v0 behaviour.
type RankConfig struct {
	// PerCategoryQuota maps source category (news/blog/paper/project/
	// community) to the maximum number of top-scoring items rank will
	// keep from that group. Empty means no quota (pure global top-N).
	PerCategoryQuota map[string]int `yaml:"per_category_quota"`
}

// DomainConfig captures identity and presentation for a briefing domain.
type DomainConfig struct {
	ID            string `yaml:"id"`
	Name          string `yaml:"name"`
	TitleTemplate string `yaml:"title_template"`
	Subtitle      string `yaml:"subtitle"`
	Timezone      string `yaml:"timezone"`
}

// WindowConfig controls the lookback time window for ingested items.
type WindowConfig struct {
	LookbackHours int `yaml:"lookback_hours"`
	ExtendedHours int `yaml:"extended_hours"`
}

// LLMConfig holds LLM client configuration. *Env fields name the env vars
// used to override defaults. The BaseURL/APIKey/Model fields are populated
// by Load() after env resolution and are not unmarshaled from YAML.
type LLMConfig struct {
	BaseURLEnv     string  `yaml:"base_url_env"`
	APIKeyEnv      string  `yaml:"api_key_env"`
	ModelEnv       string  `yaml:"model_env"`
	DefaultBaseURL string  `yaml:"default_base_url"`
	DefaultModel   string  `yaml:"default_model"`
	Temperature    float64 `yaml:"temperature"`
	MaxTokens      int     `yaml:"max_tokens"`
	TimeoutSeconds int     `yaml:"timeout_seconds"`
	MaxRetries     int     `yaml:"max_retries"`
	// Resolved values (populated by Load).
	BaseURL string `yaml:"-"`
	APIKey  string `yaml:"-"`
	Model   string `yaml:"-"`
}

// GateConfig contains thresholds for the hard quality gate.
type GateConfig struct {
	MinItems               int `yaml:"min_items"`
	MinSectionsWithContent int `yaml:"min_sections_with_content"`
	MinInsightChars        int `yaml:"min_insight_chars"`
	MinIndustryBullets     int `yaml:"min_industry_bullets"`
	MaxIndustryBullets     int `yaml:"max_industry_bullets"`
	MinTakeawayBullets     int `yaml:"min_takeaway_bullets"`
	MaxTakeawayBullets     int `yaml:"max_takeaway_bullets"`
	MinSourceDomains       int `yaml:"min_source_domains"`
}

// SlackConfig holds Slack webhook selection. TestWebhook / ProdWebhook
// are resolved by Load() from the named env vars.
type SlackConfig struct {
	TestWebhookEnv       string `yaml:"test_webhook_env"`
	ProdWebhookEnv       string `yaml:"prod_webhook_env"`
	DefaultTarget        string `yaml:"default_target"`
	EnableAlertOnFailure bool   `yaml:"enable_alert_on_failure"`
	// Resolved values.
	TestWebhook string `yaml:"-"`
	ProdWebhook string `yaml:"-"`
}

// ImageConfig controls headline cover image generation.
type ImageConfig struct {
	Enabled         bool   `yaml:"enabled"`
	Width           int    `yaml:"width"`
	Height          int    `yaml:"height"`
	OutputDir       string `yaml:"output_dir"`
	PythonBin       string `yaml:"python_bin"`
	GeneratorScript string `yaml:"generator_script"`
	FontBold        string `yaml:"font_bold"`
	FontRegular     string `yaml:"font_regular"`
}

// SectionConfig defines one rendered section of the briefing.
type SectionConfig struct {
	ID       string `yaml:"id"`
	Title    string `yaml:"title"`
	MinItems int    `yaml:"min_items"`
	MaxItems int    `yaml:"max_items"`
}

// SourceConfig describes one ingest data source. Type-specific options
// (query, hl, gl, ceid, when, limit, top_n, ...) are captured into Extra
// via YAML inline semantics so adapters can pull them as needed.
type SourceConfig struct {
	ID       string `yaml:"id"`
	Type     string `yaml:"type"`
	Category string `yaml:"category"`
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Enabled  bool   `yaml:"enabled"`
	Priority int    `yaml:"priority"`
	// Extra captures any source-type-specific keys not listed above.
	Extra map[string]any `yaml:",inline"`
}

// Load reads the YAML from path, parses it, and resolves env variable
// overrides. Returns an error if any required field (such as the LLM
// API key) cannot be resolved.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	// Resolve LLM env overrides. Env takes precedence over YAML defaults.
	cfg.LLM.BaseURL = firstNonEmpty(os.Getenv(cfg.LLM.BaseURLEnv), cfg.LLM.DefaultBaseURL)
	cfg.LLM.APIKey = os.Getenv(cfg.LLM.APIKeyEnv)
	cfg.LLM.Model = firstNonEmpty(os.Getenv(cfg.LLM.ModelEnv), cfg.LLM.DefaultModel)
	if cfg.LLM.APIKey == "" {
		return nil, fmt.Errorf("config: %s env var is required for LLM API key", cfg.LLM.APIKeyEnv)
	}

	// Resolve Slack env. Missing webhooks are warnings, not errors, so
	// that dev/test flows that skip slack publishing still succeed.
	cfg.Slack.TestWebhook = os.Getenv(cfg.Slack.TestWebhookEnv)
	cfg.Slack.ProdWebhook = os.Getenv(cfg.Slack.ProdWebhookEnv)
	if cfg.Slack.TestWebhook == "" {
		fmt.Fprintf(os.Stderr, "WARNING: %s not set, slack test publish will fail\n", cfg.Slack.TestWebhookEnv)
	}

	// Apply defaults.
	if cfg.Slack.DefaultTarget == "" {
		cfg.Slack.DefaultTarget = "test"
	}
	return &cfg, nil
}

// LLMTimeout returns the configured LLM client timeout as a time.Duration,
// falling back to 120s when TimeoutSeconds is unset or non-positive.
func (c *LLMConfig) LLMTimeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return 120 * time.Second
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// EnabledSources returns only the sources whose Enabled flag is true.
// The order matches the YAML declaration order.
func (c *Config) EnabledSources() []SourceConfig {
	var out []SourceConfig
	for _, s := range c.Sources {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
