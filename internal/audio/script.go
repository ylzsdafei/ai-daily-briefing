package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"text/template"
	"time"

	"briefing-v3/internal/llm"
	"briefing-v3/internal/search"
	"briefing-v3/internal/store"
	"briefing-v3/internal/toolcall"
)

// Config parameterizes the LLM client used by the script generator.
// Mirrors infocard.Config / weekly.WeeklyConfig so pipeline-integrate
// can reuse the same config loader entry (config/ai.yaml audio.*).
type Config struct {
	BaseURL             string
	APIKey              string
	Model               string
	Temperature         float64
	Timeout             time.Duration
	MaxRetries          int
	RetryBackoffSeconds []int
	// EnableSelfCheck controls whether the generator runs SelfCheckPrompt
	// against the first draft. Leave false unless operators have
	// measured quality wins from it — the extra LLM call doubles latency.
	EnableSelfCheck bool
}

func (c *Config) fillDefaults() {
	if c.Temperature == 0 {
		c.Temperature = 0.8 // 罗永浩 voice wants more personality jitter
	}
	if c.Timeout <= 0 {
		c.Timeout = 180 * time.Second
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if len(c.RetryBackoffSeconds) == 0 {
		c.RetryBackoffSeconds = []int{10, 30, 90, 180, 300}
	}
}

// ScriptGenerator builds 罗永浩-style spoken monologue scripts for
// an Issue's audio broadcast. It reuses the main briefing LLM (the
// OpenAI-compatible gpt-5.4 endpoint) — no new upstream integration.
//
// When a TavilyClient is attached via WithSearch, Generate switches
// from plain chat to a tool-loop (shared with canvas.Generator) so
// the LLM can ground its commentary in current industry reporting
// rather than sounding like it only read today's briefing.
type ScriptGenerator struct {
	cfg      Config
	hc       *http.Client
	model    string // snapshot of cfg.Model for downstream audit logging
	searcher *search.TavilyClient
}

// WithSearch enables Tavily-powered web_search inside the script
// generation loop. Pass nil to disable. Returns sg for chaining.
func (sg *ScriptGenerator) WithSearch(s *search.TavilyClient) *ScriptGenerator {
	sg.searcher = s
	return sg
}

// NewScriptGenerator constructs a ScriptGenerator. `model` overrides
// cfg.Model if non-empty; otherwise cfg.Model is used. This matches
// the constructor idiom in other briefing-v3 packages (infocard
// passes model via Config, but the audio-frontend roadmap may want
// to A/B a different model for voice scripts later).
func NewScriptGenerator(cfg Config, model string) *ScriptGenerator {
	if strings.TrimSpace(model) != "" {
		cfg.Model = model
	}
	cfg.fillDefaults()
	return &ScriptGenerator{
		cfg:   cfg,
		hc:    &http.Client{},
		model: cfg.Model,
	}
}

// Generate produces the 罗永浩-style monologue markdown for `issue`.
// It combines the full daily markdown, the insight blocks, and a
// compact top-items digest as the user prompt.
//
// The return value is a single Chinese prose block (no markdown
// headers / bullets) ready to be fed to MeloTTS verbatim.
func (sg *ScriptGenerator) Generate(
	ctx context.Context,
	issue *store.Issue,
	items []*store.IssueItem,
	insight *store.IssueInsight,
) (string, error) {
	if issue == nil {
		return "", errors.New("audio: Generate requires non-nil issue")
	}
	if len(items) == 0 {
		return "", errors.New("audio: Generate requires at least one item")
	}

	userPrompt, err := sg.buildUserPrompt(issue, items, insight)
	if err != nil {
		return "", fmt.Errorf("audio: build prompt: %w", err)
	}

	llmCfg := llm.Config{
		BaseURL:     sg.cfg.BaseURL,
		APIKey:      sg.cfg.APIKey,
		Model:       sg.cfg.Model,
		Temperature: sg.cfg.Temperature,
		MaxTokens:   4096,
		Timeout:     sg.cfg.Timeout,
	}

	backoffs := sg.cfg.RetryBackoffSeconds
	maxAttempts := sg.cfg.MaxRetries
	if maxAttempts > len(backoffs) {
		maxAttempts = len(backoffs)
	}
	// Same reasoning as canvas.Generator: tool-loop 内部最多 6 轮已提供韧性,
	// 外层 searcher 模式收敛到 2 次避免 12+ 次 LLM 叠加.
	if sg.searcher != nil && maxAttempts > 2 {
		maxAttempts = 2
	}

	systemPrompt := SystemPromptLuoYonghao
	if sg.searcher != nil {
		systemPrompt = SystemPromptLuoYonghao + "\n\n" + SearchGuidelines
	}

	var lastErr error
	var draft string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var raw string
		var err error
		if sg.searcher != nil {
			raw, err = toolcall.ChatWithSearch(ctx, sg.hc, llmCfg, systemPrompt, userPrompt, sg.searcher, "audio")
		} else {
			raw, err = llm.ChatComplete(ctx, sg.hc, llmCfg, systemPrompt, userPrompt)
		}
		if err != nil {
			lastErr = err
			if attempt < maxAttempts {
				backoff := time.Duration(backoffs[attempt-1]) * time.Second
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(backoff):
				}
			}
			continue
		}
		cleaned := strings.TrimSpace(raw)
		if cleaned == "" {
			lastErr = errors.New("audio: LLM returned empty script")
			continue
		}
		// Pre-emptive sanity floor: if the draft is too short, treat
		// it as a soft failure and retry. A 3-5 min monologue under
		// 800 runes is either the LLM returning a one-paragraph tease
		// or a transport truncation.
		if runeLen(cleaned) < minDraftRunes {
			lastErr = fmt.Errorf("audio: draft too short (%d runes, need >=%d)", runeLen(cleaned), minDraftRunes)
			continue
		}
		draft = cleaned
		break
	}
	if draft == "" {
		if lastErr == nil {
			lastErr = errors.New("audio: Generate failed with no specific error")
		}
		return "", lastErr
	}

	if !sg.cfg.EnableSelfCheck {
		return draft, nil
	}

	// Self-check pass: one extra LLM call that either returns the
	// draft unchanged or a rewritten version. We do NOT backoff-retry
	// this pass — if it fails we just fall back to the original draft
	// rather than losing the whole audio feature to a self-check bug.
	reviewed, err := llm.ChatComplete(ctx, sg.hc, llmCfg, SelfCheckPrompt, draft)
	if err != nil {
		return draft, nil
	}
	reviewed = strings.TrimSpace(reviewed)
	if reviewed == "" || runeLen(reviewed) < minDraftRunes {
		return draft, nil
	}
	return reviewed, nil
}

// minDraftRunes is the lower-bound sanity floor for a 3-5 minute
// monologue. MeloTTS reads ~250-300 chars/min for Chinese, so 800
// runes is a conservative "obviously too short" threshold. The
// SystemPrompt asks for 1200-1800 runes; we gate well below that to
// avoid false positives on slightly-terser drafts.
const minDraftRunes = 800

func runeLen(s string) int { return len([]rune(s)) }

// scriptUserTmpl is the compiled text/template for UserPromptTemplate.
// Using text/template (instead of fmt.Sprintf) avoids percent-escaping
// headaches when the daily markdown happens to include "%d"/"%s"
// (URL query strings, etc.).
var scriptUserTmpl = template.Must(template.New("audio-user").Parse(UserPromptTemplate))

// buildUserPrompt renders UserPromptTemplate with today's data.
func (sg *ScriptGenerator) buildUserPrompt(
	issue *store.Issue,
	items []*store.IssueItem,
	insight *store.IssueInsight,
) (string, error) {
	data := struct {
		DateZH     string
		FullMD     string
		IndustryMD string
		OurMD      string
		TopItems   string
	}{
		DateZH:   formatDateZH(issue),
		FullMD:   buildFullMD(issue, items),
		TopItems: buildTopItems(items, 8),
	}
	if insight != nil {
		data.IndustryMD = strings.TrimSpace(insight.IndustryMD)
		data.OurMD = strings.TrimSpace(insight.OurMD)
	}
	if data.IndustryMD == "" {
		data.IndustryMD = "（今日未生成行业洞察，请根据日报正文自行提炼趋势）"
	}
	if data.OurMD == "" {
		data.OurMD = "（今日未生成团队启发，请从 Agent 调度平台视角自行推导）"
	}

	var buf bytes.Buffer
	if err := scriptUserTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	return buf.String(), nil
}

// formatDateZH returns "YYYY年M月D日" for the opening address.
// Duplicated locally rather than importing internal/render to keep
// the audio package decoupled from render (render imports store,
// and we want audio to only depend on store + llm).
func formatDateZH(issue *store.Issue) string {
	if issue == nil {
		return ""
	}
	d := issue.IssueDate
	return fmt.Sprintf("%d年%d月%d日", d.Year(), int(d.Month()), d.Day())
}

// buildFullMD reconstructs a lightweight version of the daily
// markdown from `items`. It is intentionally simpler than
// render.RenderMarkdown — the goal is to give the LLM enough context,
// not to render a publish-quality document, so we only emit:
//
//	## AI资讯日报 YYYY/M/D
//	### {Section Title}
//	{each item's BodyMD, or title fallback}
//
// Section ordering matches store.Section* constants. Items whose
// Section is not recognized are dropped.
func buildFullMD(issue *store.Issue, items []*store.IssueItem) string {
	var b strings.Builder
	if issue != nil {
		d := issue.IssueDate
		fmt.Fprintf(&b, "## AI资讯日报 %d/%d/%d\n\n", d.Year(), int(d.Month()), d.Day())
		if s := strings.TrimSpace(issue.Summary); s != "" {
			b.WriteString("今日摘要：")
			b.WriteString(s)
			b.WriteString("\n\n")
		}
	}

	bySection := groupBySection(items)
	for _, sec := range sectionOrder {
		secItems := bySection[sec.id]
		if len(secItems) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", sec.title)
		for _, it := range secItems {
			body := strings.TrimSpace(it.BodyMD)
			if body == "" {
				fmt.Fprintf(&b, "%d. %s\n\n", it.Seq, strings.TrimSpace(it.Title))
				continue
			}
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// buildTopItems renders up to `limit` items in "- [section] title"
// form so the LLM can quickly spot the day's anchor stories without
// re-parsing the full markdown.
func buildTopItems(items []*store.IssueItem, limit int) string {
	if limit <= 0 {
		limit = 8
	}
	// Sort by (section priority, seq) so the digest reads in a
	// stable, predictable order regardless of input ordering.
	bySection := groupBySection(items)
	var lines []string
	for _, sec := range sectionOrder {
		for _, it := range bySection[sec.id] {
			if it == nil {
				continue
			}
			title := strings.TrimSpace(it.Title)
			if title == "" {
				continue
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s", sec.title, title))
			if len(lines) >= limit {
				break
			}
		}
		if len(lines) >= limit {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// sectionInfo pairs a store.Section* id with its display title.
type sectionInfo struct {
	id    string
	title string
}

// sectionOrder matches the canonical display order used elsewhere
// in the pipeline (see internal/render/markdown.go sectionEmoji).
var sectionOrder = []sectionInfo{
	{id: store.SectionProductUpdate, title: "产品与功能更新"},
	{id: store.SectionResearch, title: "AI 研究"},
	{id: store.SectionIndustry, title: "产业新闻"},
	{id: store.SectionOpenSource, title: "开源项目"},
	{id: store.SectionSocial, title: "社区声音"},
}

// groupBySection buckets items by their Section field and sorts
// each bucket by Seq for determinism.
func groupBySection(items []*store.IssueItem) map[string][]*store.IssueItem {
	out := make(map[string][]*store.IssueItem, len(sectionOrder))
	for _, it := range items {
		if it == nil {
			continue
		}
		out[it.Section] = append(out[it.Section], it)
	}
	for k := range out {
		sort.SliceStable(out[k], func(i, j int) bool {
			return out[k][i].Seq < out[k][j].Seq
		})
	}
	return out
}
