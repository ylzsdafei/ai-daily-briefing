package canvas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"time"

	"briefing-v3/internal/llm"
	"briefing-v3/internal/search"
	"briefing-v3/internal/store"
	"briefing-v3/internal/toolcall"
)

// LLMChatFunc matches llm.ChatComplete's signature. We take it as a
// field so tests can inject a fake. Production wiring passes
// llm.ChatComplete directly.
type LLMChatFunc func(ctx context.Context, hc *http.Client, cfg llm.Config, system, user string) (string, error)

// Config parameterises the Generator. BaseURL/APIKey/Model are forwarded
// to llm.ChatComplete. MaxTokens caps the response size (the flow JSON
// for 30 nodes runs ~6-8k tokens). RetryBackoffs drives the retry loop;
// length determines MaxAttempts. Timeout is per-request.
type Config struct {
	BaseURL       string
	APIKey        string
	Model         string
	Temperature   float64
	MaxTokens     int
	Timeout       time.Duration
	RetryBackoffs []time.Duration
}

// DefaultRetryBackoffs matches the plan document's recommendation:
// 5s / 15s / 45s total ~1 minute — enough for transient LLM flakes
// without blowing the pipeline's 30-minute budget.
var DefaultRetryBackoffs = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	45 * time.Second,
}

// Generator builds insight-flow diagrams. It is a standalone type (not
// an interface) so callers can wire it with a real llm.ChatComplete
// or a test fake via NewGeneratorWithChat.
//
// When a TavilyClient is attached via WithSearch, Generate switches
// from plain chat to a tool-loop that lets the LLM call web_search
// against Tavily to ground nodes in current industry reporting.
type Generator struct {
	cfg      Config
	hc       *http.Client
	chat     LLMChatFunc
	searcher *search.TavilyClient
}

// WithSearch enables tool-use mode on this Generator. Pass nil to
// disable. Returns g for chaining.
func (g *Generator) WithSearch(s *search.TavilyClient) *Generator {
	g.searcher = s
	return g
}

// NewGenerator constructs a Generator wired to the production
// llm.ChatComplete function. Returns an error if the config is
// obviously invalid so callers can fail fast at startup.
func NewGenerator(cfg Config) (*Generator, error) {
	if cfg.BaseURL == "" || cfg.APIKey == "" || cfg.Model == "" {
		return nil, errors.New("canvas: BaseURL / APIKey / Model are required")
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 8192
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 180 * time.Second
	}
	if len(cfg.RetryBackoffs) == 0 {
		cfg.RetryBackoffs = DefaultRetryBackoffs
	}
	return &Generator{
		cfg:  cfg,
		hc:   &http.Client{},
		chat: llm.ChatComplete,
	}, nil
}

// NewGeneratorWithChat is the test entry-point: it lets a unit test
// inject a fake LLMChatFunc so we can exercise parsing/validation
// without talking to a real model. BaseURL/APIKey/Model checks are
// relaxed because tests don't need them.
func NewGeneratorWithChat(cfg Config, chat LLMChatFunc) *Generator {
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 8192
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 180 * time.Second
	}
	if len(cfg.RetryBackoffs) == 0 {
		cfg.RetryBackoffs = DefaultRetryBackoffs
	}
	return &Generator{cfg: cfg, hc: &http.Client{}, chat: chat}
}

// promptVars is the text/template data passed to UserPromptTemplate.
type promptVars struct {
	DateZH     string
	FullMD     string
	IndustryMD string
	OurMD      string
	TopItems   string
	Feedback   string
}

// Generate produces a validated Flow for the given issue. On each
// attempt it:
//  1. Renders the user prompt (attaching the previous failure as
//     Feedback so the model can self-correct).
//  2. Calls ChatComplete.
//  3. Extracts the JSON object from the reply.
//  4. Unmarshals into Flow.
//  5. Runs Validate.
//
// On success returns the Flow. On terminal failure returns the last
// error — the caller (cmd/briefing/run.go) decides whether to fail-soft
// (canvas errors must not block publication per the plan).
func (g *Generator) Generate(
	ctx context.Context,
	issue *store.Issue,
	items []*store.IssueItem,
	insight *store.IssueInsight,
) (*Flow, error) {
	if issue == nil {
		return nil, errors.New("canvas: issue is nil")
	}
	if insight == nil {
		return nil, errors.New("canvas: insight is nil (upstream step failed, nothing to visualize)")
	}

	vars := promptVars{
		DateZH:     formatDateZH(issue.IssueDate),
		FullMD:     buildFullMD(issue, items),
		IndustryMD: strings.TrimSpace(insight.IndustryMD),
		OurMD:      strings.TrimSpace(insight.OurMD),
		TopItems:   buildTopItemsDigest(items, 5),
	}

	llmCfg := llm.Config{
		BaseURL:     g.cfg.BaseURL,
		APIKey:      g.cfg.APIKey,
		Model:       g.cfg.Model,
		Temperature: g.cfg.Temperature,
		MaxTokens:   g.cfg.MaxTokens,
		Timeout:     g.cfg.Timeout,
	}

	// Append search guidelines to the system prompt only when a search
	// client is attached; otherwise the LLM would get confused about a
	// tool it can't actually call.
	systemPrompt := SystemPrompt
	if g.searcher != nil {
		systemPrompt = SystemPrompt + "\n\n" + SearchGuidelines
	}

	maxAttempts := len(g.cfg.RetryBackoffs) + 1 // backoff slots + the initial try
	// Tool-loop 内部已最多 6 轮 LLM 交互; 再叠加外层 4 次 retry = 最坏 24 次
	// LLM 调用 + token 消耗. searcher 模式下外层收敛到 2 次足够覆盖 transient 502.
	if g.searcher != nil && maxAttempts > 2 {
		maxAttempts = 2
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		userPrompt, err := renderUserPrompt(vars)
		if err != nil {
			return nil, fmt.Errorf("canvas: render prompt: %w", err)
		}

		var raw string
		if g.searcher != nil {
			raw, err = toolcall.ChatWithSearch(ctx, g.hc, llmCfg, systemPrompt, userPrompt, g.searcher, "canvas")
		} else {
			raw, err = g.chat(ctx, g.hc, llmCfg, systemPrompt, userPrompt)
		}
		if err != nil {
			lastErr = fmt.Errorf("canvas: chat call (attempt %d): %w", attempt, err)
			vars.Feedback = fmt.Sprintf("上次调用 LLM 本身失败: %v", err)
		} else {
			flow, perr := parseFlowJSON(raw)
			if perr != nil {
				lastErr = fmt.Errorf("canvas: parse (attempt %d): %w", attempt, perr)
				vars.Feedback = fmt.Sprintf("上次输出不是合法 JSON: %v. 请严格输出一个 JSON 对象, 不要 markdown 围栏.", perr)
			} else if verr := flow.Validate(); verr != nil {
				lastErr = fmt.Errorf("canvas: validate (attempt %d): %w", attempt, verr)
				vars.Feedback = fmt.Sprintf("上次 JSON 结构校验失败: %v", verr)
			} else {
				return flow, nil
			}
		}

		if attempt < maxAttempts {
			wait := g.cfg.RetryBackoffs[attempt-1]
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("canvas: failed after %d attempts with no specific error", maxAttempts)
	}
	return nil, lastErr
}

// renderUserPrompt executes the template; separated so tests can
// exercise it without wiring the full generator.
func renderUserPrompt(vars promptVars) (string, error) {
	tmpl, err := template.New("canvas_user").Parse(UserPromptTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// fencedJSONRe strips a ```json ... ``` wrapper if the model ignored
// the "no fences" instruction. Non-capturing language tag, DOTALL so
// the body can span newlines.
var fencedJSONRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*\\})\\s*```")

// parseFlowJSON is tolerant of the three common LLM failure modes:
// bare JSON, fenced JSON, or JSON with a prose preface/suffix. Uses
// the outermost braces for extraction when a fence is absent.
func parseFlowJSON(raw string) (*Flow, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("canvas: empty LLM response")
	}

	if m := fencedJSONRe.FindStringSubmatch(trimmed); len(m) == 2 {
		trimmed = m[1]
	} else if !strings.HasPrefix(trimmed, "{") {
		start := strings.Index(trimmed, "{")
		end := strings.LastIndex(trimmed, "}")
		if start < 0 || end <= start {
			return nil, errors.New("canvas: LLM response has no JSON object")
		}
		trimmed = trimmed[start : end+1]
	}

	var f Flow
	if err := json.Unmarshal([]byte(trimmed), &f); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &f, nil
}

// buildFullMD stitches the issue summary with each section's items
// into a single markdown payload for the prompt. Capped at ~8k runes
// so the user-message stays comfortably under a 32k token budget.
func buildFullMD(issue *store.Issue, items []*store.IssueItem) string {
	const maxRunes = 8000
	var b strings.Builder
	if t := strings.TrimSpace(issue.Title); t != "" {
		fmt.Fprintf(&b, "# %s\n\n", t)
	}
	if s := strings.TrimSpace(issue.Summary); s != "" {
		fmt.Fprintf(&b, "%s\n\n", s)
	}
	for _, it := range items {
		if it == nil {
			continue
		}
		fmt.Fprintf(&b, "## [%s seq=%d] %s\n%s\n\n",
			it.Section, it.Seq, strings.TrimSpace(it.Title), strings.TrimSpace(it.BodyMD))
	}
	out := b.String()
	if n := len([]rune(out)); n > maxRunes {
		out = string([]rune(out)[:maxRunes]) + "\n……(已截断)"
	}
	return out
}

// buildTopItemsDigest produces a short, LLM-friendly digest of the
// first n items. We sort nothing — the pipeline already orders items
// by seq within section and sections by priority, so the first n is
// a sensible "today's top" slice.
func buildTopItemsDigest(items []*store.IssueItem, n int) string {
	const bodyCap = 240
	var b strings.Builder
	picked := 0
	for _, it := range items {
		if it == nil {
			continue
		}
		if picked >= n {
			break
		}
		picked++
		body := strings.TrimSpace(it.BodyMD)
		if rs := []rune(body); len(rs) > bodyCap {
			body = string(rs[:bodyCap]) + "……"
		}
		body = strings.ReplaceAll(body, "\n", " ")
		fmt.Fprintf(&b, "%d. [%s] %s — %s\n", picked, it.Section, strings.TrimSpace(it.Title), body)
	}
	if picked == 0 {
		return "(今日无可用条目)"
	}
	return strings.TrimSpace(b.String())
}

// formatDateZH renders a time.Time as "2026 年 4 月 24 日" without
// relying on any locale library — briefing-v3 standardizes on
// Asia/Shanghai via upstream callers, so we just render the wall
// clock as-is.
func formatDateZH(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d 年 %d 月 %d 日", t.Year(), int(t.Month()), t.Day())
}
