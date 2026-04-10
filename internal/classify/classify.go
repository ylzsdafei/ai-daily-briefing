// Package classify is the Step 2 LLM classifier: given a list of
// RawItems already filtered by rank, assign each one to exactly one of
// the five briefing-v3 sections (product_update, research, industry,
// opensource, social).
//
// The classifier is LLM-first but falls back to simple URL/domain rules
// when the LLM response is malformed or missing an item. This guarantees
// every ranked item lands in some section, so compose can never end up
// with a nil bucket.
package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// Config parameterizes the classifier.
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	BatchSize  int           // items per LLM request, default 25
	MaxRetries int           // per-batch retries, default 3
	Timeout    time.Duration // per-request timeout, default 120s
}

func (c *Config) fillDefaults() {
	if c.BatchSize <= 0 {
		c.BatchSize = 25
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.Timeout <= 0 {
		c.Timeout = 120 * time.Second
	}
}

// Classifier buckets RawItems into sections.
type Classifier interface {
	// Classify returns a map from section id (store.SectionProductUpdate
	// etc.) to the RawItems assigned to that section. Every non-nil
	// input item is guaranteed to appear in exactly one bucket.
	Classify(ctx context.Context, items []*store.RawItem) (map[string][]*store.RawItem, error)
}

// New constructs an LLM-backed Classifier.
func New(cfg Config) (Classifier, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("classify: Config.BaseURL is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("classify: Config.APIKey is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("classify: Config.Model is required")
	}
	cfg.fillDefaults()
	return &llmClassifier{cfg: cfg, hc: &http.Client{}}, nil
}

// classifySystemPrompt is the rubric the LLM follows.
const classifySystemPrompt = `你是 AI 日报编辑。给定一批候选条目，把每一条分配到以下 5 个 section 之一:

- product_update: AI 公司/产品的发布、更新、新功能 (ChatGPT/Claude/Gemini/DeepSeek 等产品消息)
- research: 学术论文、新算法、技术突破 (arxiv/会议/新模型研究)
- industry: 政策、商业、伦理、社会话题 (融资/监管/讨论/伦理辩论)
- opensource: GitHub 项目、开源工具 (仓库发布/star 热门)
- social: 社区讨论、热门观点、博客文章 (HN/Reddit/个人博客/社媒)

规则:
1. 每条必须分到且仅分到一个 section
2. 同时属于多个 section 时优先按最核心的
3. arxiv / HuggingFace Papers → research
4. GitHub 项目 → opensource
5. Reddit/HN/blog → social (除非是公司产品发布则 product_update)

输出严格 JSON 数组:
[{"id": 原 id, "section": "section_id"}, ...]`

// classifyUserPromptTemplate is the per-batch user message.
const classifyUserPromptTemplate = `以下是候选条目，请按规则分类。

%s

只输出 JSON 数组。`

// validSections is the allowlist that LLM output must fall into.
var validSections = map[string]bool{
	store.SectionProductUpdate: true,
	store.SectionResearch:      true,
	store.SectionIndustry:      true,
	store.SectionOpenSource:    true,
	store.SectionSocial:        true,
}

// llmClassifier is the concrete Classifier implementation.
type llmClassifier struct {
	cfg Config
	hc  *http.Client
}

// chatMessage / chatRequest / chatResponse duplicate the minimal OpenAI
// chat-completions structs; kept local so classify has no build-time
// dependency on the generate package.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
		Index   int         `json:"index"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// classifyVerdict is one element of the LLM-emitted JSON array.
type classifyVerdict struct {
	ID      int64  `json:"id"`
	Section string `json:"section"`
}

// Classify batches items, calls the LLM once per batch, merges verdicts
// and uses fallbackSection for any item the LLM missed or mislabeled.
func (c *llmClassifier) Classify(ctx context.Context, items []*store.RawItem) (map[string][]*store.RawItem, error) {
	result := map[string][]*store.RawItem{
		store.SectionProductUpdate: nil,
		store.SectionResearch:      nil,
		store.SectionIndustry:      nil,
		store.SectionOpenSource:    nil,
		store.SectionSocial:        nil,
	}
	if len(items) == 0 {
		return result, nil
	}

	byID := make(map[int64]*store.RawItem, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		byID[it.ID] = it
	}

	assigned := make(map[int64]string, len(items))
	for start := 0; start < len(items); start += c.cfg.BatchSize {
		end := start + c.cfg.BatchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]

		verdicts, err := c.classifyBatchWithRetry(ctx, batch)
		if err != nil {
			// Batch failed outright; leave these items unassigned so the
			// fallback below picks them up.
			continue
		}
		for _, v := range verdicts {
			if _, ok := byID[v.ID]; !ok {
				continue
			}
			if !validSections[v.Section] {
				continue
			}
			assigned[v.ID] = v.Section
		}
	}

	// Bucket assigned items, fallback-classify unassigned ones.
	for id, it := range byID {
		sec, ok := assigned[id]
		if !ok {
			sec = fallbackSection(it)
		}
		result[sec] = append(result[sec], it)
	}

	return result, nil
}

// classifyBatchWithRetry calls the LLM up to MaxRetries times for a batch
// and returns the first parseable verdict slice.
func (c *llmClassifier) classifyBatchWithRetry(ctx context.Context, batch []*store.RawItem) ([]classifyVerdict, error) {
	userPrompt := fmt.Sprintf(classifyUserPromptTemplate, formatItemsForClassify(batch))

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxRetries; attempt++ {
		raw, err := c.chatComplete(ctx, classifySystemPrompt, userPrompt)
		if err != nil {
			lastErr = err
			continue
		}
		verdicts, perr := parseClassifyJSON(raw)
		if perr != nil {
			lastErr = perr
			continue
		}
		return verdicts, nil
	}
	if lastErr == nil {
		lastErr = errors.New("classify: batch failed with no specific error")
	}
	return nil, lastErr
}

// formatItemsForClassify is the per-batch item renderer.
func formatItemsForClassify(batch []*store.RawItem) string {
	var b strings.Builder
	for _, it := range batch {
		if it == nil {
			continue
		}
		desc := firstRunes(strings.TrimSpace(it.Content), 80)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "[id=%d] %s | %s | %s\n",
			it.ID,
			truncateOneLine(it.Title, 140),
			it.URL,
			truncateOneLine(desc, 160),
		)
	}
	return b.String()
}

// parseClassifyJSON unwraps the LLM response into a []classifyVerdict.
func parseClassifyJSON(raw string) ([]classifyVerdict, error) {
	s := extractJSONArray(raw)
	if s == "" {
		return nil, fmt.Errorf("classify: no JSON array found: %q", truncateOneLine(raw, 200))
	}
	var verdicts []classifyVerdict
	if err := json.Unmarshal([]byte(s), &verdicts); err != nil {
		return nil, fmt.Errorf("classify: parse JSON: %w", err)
	}
	return verdicts, nil
}

// fallbackSection is a deterministic rule-based classifier used when the
// LLM fails or skips an item. It inspects URL host and title keywords to
// pick the most plausible bucket.
func fallbackSection(it *store.RawItem) string {
	url := strings.ToLower(it.URL)
	title := strings.ToLower(it.Title)

	switch {
	case strings.Contains(url, "arxiv.org"),
		strings.Contains(url, "huggingface.co/papers"),
		strings.Contains(url, "papers.cool"):
		return store.SectionResearch

	case strings.Contains(url, "github.com"),
		strings.Contains(url, "ossinsight.io"),
		strings.Contains(url, "gitlab.com"):
		return store.SectionOpenSource

	case strings.Contains(url, "openai.com"),
		strings.Contains(url, "anthropic.com"),
		strings.Contains(url, "deepmind.google"),
		strings.Contains(url, "meta.ai"),
		strings.Contains(url, "ai.meta.com"),
		strings.Contains(url, "deepseek.com"),
		strings.Contains(url, "mistral.ai"),
		strings.Contains(url, "x.ai"):
		return store.SectionProductUpdate

	case strings.Contains(url, "reddit.com"),
		strings.Contains(url, "ycombinator.com"),
		strings.Contains(url, "news.ycombinator"),
		strings.Contains(url, "hacker-news"),
		strings.Contains(url, "simonwillison.net"),
		strings.Contains(url, "oneusefulthing.org"),
		strings.Contains(url, "jack-clark.net"),
		strings.Contains(url, "sebastianraschka"),
		strings.Contains(url, "lilianweng"),
		strings.Contains(url, "baoyu.io"),
		strings.Contains(url, "ruanyifeng.com"):
		return store.SectionSocial

	case strings.Contains(url, "techcrunch.com"),
		strings.Contains(url, "the-decoder.com"),
		strings.Contains(url, "news.smol.ai"),
		strings.Contains(url, "news.google.com"),
		strings.Contains(title, "融资"),
		strings.Contains(title, "监管"),
		strings.Contains(title, "政策"),
		strings.Contains(title, "funding"),
		strings.Contains(title, "regulation"):
		return store.SectionIndustry
	}

	// Default catch-all: the social bucket. It is the most permissive of
	// the five and least likely to mislead downstream composition.
	return store.SectionSocial
}

// extractJSONArray / firstRunes / truncateOneLine are duplicated from
// rank.go to keep classify self-contained.

func extractJSONArray(s string) string {
	start := strings.Index(s, "[")
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && inStr {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

func truncateOneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	return firstRunes(s, n)
}

// chatComplete POSTs a single chat-completions request. Identical shape
// to rank.go / openai.go but local to keep package boundaries clean.
func (c *llmClassifier) chatComplete(parent context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, c.cfg.Timeout)
	defer cancel()

	reqBody := chatRequest{
		Model: c.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		MaxTokens:   2000,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("classify marshal: %w", err)
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("classify new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("classify http do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("classify read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		return "", fmt.Errorf("classify openai http %d: %s", resp.StatusCode, snippet)
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("classify unmarshal response: %w", err)
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("classify openai error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("classify openai: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
}
