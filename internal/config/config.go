// Package config loads the briefing-v3 YAML config and resolves
// environment variable overrides.
package config

import (
	"fmt"
	"os"
	"strings"
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
	Canvas   CanvasConfig    `yaml:"canvas"`
	Audio    AudioConfig     `yaml:"audio"`
	Sections []SectionConfig `yaml:"sections"`
	Sources  []SourceConfig  `yaml:"sources"`
}

// CanvasConfig gates the v1.1 insight-flow canvas feature. When Enabled
// is false the pipeline must behave exactly as v1.0 — no LLM call, no
// canvas JSON written anywhere. Enabled is further overridable from the
// environment via CANVAS_ENABLED so smoke tests can turn it on without
// editing config/ai.yaml.
type CanvasConfig struct {
	Enabled             bool  `yaml:"enabled"`
	RetryBackoffSeconds []int `yaml:"retry_backoff_seconds"`
	MinNodes            int   `yaml:"min_nodes"`
	MaxNodes            int   `yaml:"max_nodes"`
}

// AudioConfig gates the v1.1 Luo Yonghao-style voice broadcast feature.
// Env-overrides: AUDIO_ENABLED / CF_API_TOKEN / CF_ACCOUNT_ID
// (the *_env keys are resolved at Load time).
type AudioConfig struct {
	Enabled bool `yaml:"enabled"`
	// Backend selects the TTS provider. "edge" (default) = Microsoft
	// edge-tts CLI (free, open-source wrapper, Chinese quality
	// verified good). "cf" = Cloudflare Workers AI MeloTTS (retained
	// for rollback but known broken on Chinese as of 2026-04-24).
	Backend             string `yaml:"backend"`
	CFAPITokenEnv       string `yaml:"cf_api_token_env"`
	CFAccountIDEnv      string `yaml:"cf_account_id_env"`
	VoiceLang           string `yaml:"voice_lang"`
	OutputFormat        string `yaml:"output_format"`
	// Voice is the edge-tts short-name, e.g. "zh-CN-YunjianNeural".
	// Only consumed when Backend == "edge".
	Voice string `yaml:"voice"`
	// Rate is the edge-tts prosody adjustment, e.g. "+5%", "-10%".
	// Only consumed when Backend == "edge".
	Rate                string `yaml:"rate"`
	SelfCheck           bool   `yaml:"self_check"`
	RetryBackoffSeconds []int  `yaml:"retry_backoff_seconds"`
	// Resolved values (populated by Load).
	CFAPIToken  string `yaml:"-"`
	CFAccountID string `yaml:"-"`
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
	// v1.0.1: LLM 502 分钟级退避序列 (秒). 数组长度 = 最大重试次数.
	// 每次重试前 sleep 对应索引的秒数. 旧 1/2/4/8s 共 15s 对上游分钟级
	// 抖动无效, 是 2026-04-14 故障的根因之一.
	RetryBackoffSeconds []int `yaml:"retry_backoff_seconds"`
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
	// v1.0.1: API key 缺失只 warning, 不立即 error. 这样 migrate/seed/status
	// 等不调用 LLM 的命令能独立跑 (方便本地测试 Step 3 migration dry-run).
	// 真正用 LLM 的 sub-package (generate/classify/rank/infocard) 在
	// New() 里自己校验 APIKey 非空并返回 error.
	if cfg.LLM.APIKey == "" {
		fmt.Fprintf(os.Stderr, "WARNING: %s not set — LLM commands (run/weekly/regen) will fail, but migrate/seed/status OK\n", cfg.LLM.APIKeyEnv)
	}

	// Resolve Slack env. Missing webhooks are warnings, not errors, so
	// that dev/test flows that skip slack publishing still succeed.
	cfg.Slack.TestWebhook = firstNonEmpty(
		os.Getenv(cfg.Slack.TestWebhookEnv),
		os.Getenv("SLACK_WEBHOOK_TEST"),
	)
	cfg.Slack.ProdWebhook = firstNonEmpty(
		os.Getenv(cfg.Slack.ProdWebhookEnv),
		os.Getenv("SLACK_WEBHOOK_PROD"),
	)
	if cfg.Slack.TestWebhook == "" {
		fmt.Fprintf(os.Stderr, "WARNING: %s not set, slack test publish will fail\n", cfg.Slack.TestWebhookEnv)
	}

	// Apply defaults.
	if cfg.Slack.DefaultTarget == "" {
		cfg.Slack.DefaultTarget = "test"
	}

	// v1.1 canvas/audio env overrides. CANVAS_ENABLED / AUDIO_ENABLED
	// flip the YAML default; this is how smoke tests opt-in without
	// touching config/ai.yaml. Any of the usual truthy values are
	// accepted; everything else (including unset) keeps the YAML value.
	if v, ok := parseBoolEnv("CANVAS_ENABLED"); ok {
		cfg.Canvas.Enabled = v
	}
	if v, ok := parseBoolEnv("AUDIO_ENABLED"); ok {
		cfg.Audio.Enabled = v
	}

	// Audio CF credentials — env var names come from YAML (*_env fields),
	// so operators can rename the env vars without changing code. When
	// audio is disabled we still resolve them so `--dry-run --audio`
	// failures surface as a clear "credential missing" error instead of
	// a confusing HTTP 401 mid-pipeline.
	if cfg.Audio.CFAPITokenEnv != "" {
		cfg.Audio.CFAPIToken = os.Getenv(cfg.Audio.CFAPITokenEnv)
	}
	if cfg.Audio.CFAccountIDEnv != "" {
		cfg.Audio.CFAccountID = os.Getenv(cfg.Audio.CFAccountIDEnv)
	}
	// Backfill audio defaults so a partially-filled YAML doesn't crash.
	if cfg.Audio.Backend == "" {
		// edge-tts is the new default backend (2026-04-24). MeloTTS
		// was retained under "cf" for rollback but produces silence
		// on Chinese input.
		cfg.Audio.Backend = "edge"
	}
	if cfg.Audio.Voice == "" {
		// YunjianNeural is a sports-commentator male voice — the
		// closest edge-tts match to the Luo Yonghao register.
		cfg.Audio.Voice = "zh-CN-YunjianNeural"
	}
	if cfg.Audio.Rate == "" {
		cfg.Audio.Rate = "+5%"
	}
	if cfg.Audio.VoiceLang == "" {
		cfg.Audio.VoiceLang = "zh"
	}
	if cfg.Audio.OutputFormat == "" {
		cfg.Audio.OutputFormat = "mp3"
	}
	if len(cfg.Audio.RetryBackoffSeconds) == 0 {
		cfg.Audio.RetryBackoffSeconds = []int{5, 15, 45}
	}
	if len(cfg.Canvas.RetryBackoffSeconds) == 0 {
		cfg.Canvas.RetryBackoffSeconds = []int{5, 15, 45}
	}
	if cfg.Canvas.MinNodes == 0 {
		cfg.Canvas.MinNodes = 15
	}
	if cfg.Canvas.MaxNodes == 0 {
		cfg.Canvas.MaxNodes = 30
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

// parseBoolEnv reads an env var and interprets truthy strings as bool.
// Returns (value, ok). ok == false means the env was unset/empty, so
// callers keep their existing default instead of forcing false.
func parseBoolEnv(name string) (bool, bool) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return false, false
	}
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on", true
}
