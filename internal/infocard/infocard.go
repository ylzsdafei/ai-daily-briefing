// Package infocard asks the LLM to distill every IssueItem into a
// structured info-card JSON that a downstream PIL renderer turns into
// an editorial infographic (米黄报纸底 + 杂志排版).
//
// One LLM call per run generates info-card JSON for ALL items at once,
// which keeps token costs linear (not per-item). The prompt is hard-
// locked to return a JSON array so the parser does not have to do
// fancy natural-language extraction.
//
// Package layout mirrors internal/rank: standalone, minimal deps, its
// own tiny HTTP client. Does not import generate/ or rank/.
package infocard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// Config parameterizes the LLM client used by the info-card pass.
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	MaxRetries int
	Timeout    time.Duration
}

func (c *Config) fillDefaults() {
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.Timeout <= 0 {
		c.Timeout = 120 * time.Second
	}
}

// Card is the structured info-card payload for a single IssueItem,
// exactly the shape the Python PIL template consumes. Every field is
// optional — the renderer handles empty values gracefully, but the
// LLM prompt asks it to fill them all.
type Card struct {
	ItemSeq        int      `json:"item_seq"`
	MainTitle      string   `json:"main_title"`
	Subtitle       string   `json:"subtitle"`
	Intro          string   `json:"intro"`
	HeroNumber     string   `json:"hero_number"`
	HeroLabel      string   `json:"hero_label"`
	StatNumbers    []Stat   `json:"stat_numbers"`
	KeyPoints      []Point  `json:"key_points"`
	FooterSummary  string   `json:"footer_summary"`
	BrandTag       string   `json:"brand_tag"`
	CategoryTag    string   `json:"category_tag"`
}

type Stat struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type Point struct {
	Title string `json:"title"`
	Desc  string `json:"desc"`
}

// HeaderCard is the once-per-issue "大字报" front-page payload. The
// PIL renderer turns it into the hero banner shown at the top of the
// HTML page. It is structurally similar to Card but the semantics
// are different: the hero banner summarises the WHOLE issue, whereas
// Card summarises one news item.
type HeaderCard struct {
	IssueDate     string     `json:"issue_date"`
	MainHeadline  string     `json:"main_headline"`
	SubHeadline   string     `json:"sub_headline"`
	TopStories    []TopStory `json:"top_stories"`
	FooterSlogan  string     `json:"footer_slogan"`
}

type TopStory struct {
	Title string `json:"title"`
	Tag   string `json:"tag"`
}

// Generator is the public interface of this package.
type Generator interface {
	// Generate produces a card for every item plus a single page-wide
	// header card. items must be non-empty.
	Generate(ctx context.Context, items []*store.IssueItem, summary string) (*HeaderCard, []*Card, error)
}

// New builds an LLM-backed Generator.
func New(cfg Config) (Generator, error) {
	if cfg.BaseURL == "" || cfg.APIKey == "" || cfg.Model == "" {
		return nil, errors.New("infocard: BaseURL / APIKey / Model are required")
	}
	cfg.fillDefaults()
	return &llmGenerator{cfg: cfg, hc: &http.Client{}}, nil
}

type llmGenerator struct {
	cfg Config
	hc  *http.Client
}

const infoCardSystemPrompt = `你是一名 AI 日报视觉编辑, 负责把每条新闻提炼成"信息卡片"的结构化要点, 供下游的报纸风格信息图模板渲染使用。

你会收到:
1) 今日全部新闻条目 (每条有 seq、section、title、body)
2) 今日早报整体摘要 (3 行)

你必须输出一段严格 JSON, 形如:
{
  "header": {
    "main_headline": "整期早报一句话大字报标题 (12-22 字)",
    "sub_headline": "整期早报的 10-16 字副标题",
    "top_stories": [
      {"title": "一条重磅新闻标题党短句, 10-18 字", "tag": "产品/研究/开源/..."},
      {"title": "...", "tag": "..."},
      {"title": "...", "tag": "..."}
    ],
    "footer_slogan": "一句品牌口号, 8-14 字"
  },
  "cards": [
    {
      "item_seq": 1,
      "main_title": "一句话提炼的产品/技术名称 10-15 字",
      "subtitle": "一句话补充卖点 15-25 字",
      "intro": "导语段落 40-80 字, 用口语化中文解释这条新闻为什么重要",
      "hero_number": "最核心的一个数字或关键词 (例 '68%' / '4.6' / '3 秒' / '750B')",
      "hero_label": "这个数字代表什么, 4-8 字",
      "stat_numbers": [
        {"value": "次要数据", "label": "解释"},
        {"value": "次要数据", "label": "解释"}
      ],
      "key_points": [
        {"title": "要点 1 小标题 4-8 字", "desc": "一句话 15-30 字"},
        {"title": "要点 2", "desc": "..."},
        {"title": "要点 3", "desc": "..."}
      ],
      "footer_summary": "底部一行总结 20-30 字",
      "brand_tag": "item 所属 section 中文",
      "category_tag": "新闻类别的 1-2 个英文标签 (如 'AGENT' / 'MODEL' / 'OPENSOURCE')"
    },
    ... 每一条 item 一条 card ...
  ]
}

硬规则:
- 必须严格 JSON (无注释、无多余 markdown 围栏)
- cards 数组的长度必须等于输入 items 的数量
- 每个 card 的 item_seq 必须和输入 item 的 seq 匹配
- 所有数字字段 (hero_number/stat_numbers.value) 必须来自原文, 不得捏造
- 如果某条新闻天然缺少明显数字, hero_number 用一个关键短语 (例 "安全下线" / "首次开源"), 不要填 "N/A"
- 专业术语加括号注释 (非技术用户友好)
- 不要输出任何 JSON 以外的文字
`

// Generate implements Generator.Generate.
func (g *llmGenerator) Generate(ctx context.Context, items []*store.IssueItem, summary string) (*HeaderCard, []*Card, error) {
	if len(items) == 0 {
		return nil, nil, errors.New("infocard: no items")
	}

	userPrompt := buildInfoCardUserPrompt(items, summary)

	var lastErr error
	for attempt := 1; attempt <= g.cfg.MaxRetries; attempt++ {
		raw, err := g.chatComplete(ctx, infoCardSystemPrompt, userPrompt)
		if err != nil {
			lastErr = err
			continue
		}
		header, cards, perr := parseInfoCardJSON(raw, items)
		if perr != nil {
			lastErr = perr
			continue
		}
		return header, cards, nil
	}
	if lastErr == nil {
		lastErr = errors.New("infocard: failed with no specific error")
	}
	return nil, nil, lastErr
}

// buildInfoCardUserPrompt serializes the items for the LLM user turn.
// We include the section tag, title and a truncated body so the LLM
// has enough context to extract specific numbers and key points.
func buildInfoCardUserPrompt(items []*store.IssueItem, summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【今日整体摘要】\n%s\n\n", strings.TrimSpace(summary))
	b.WriteString("【全部候选新闻】(共 ")
	fmt.Fprintf(&b, "%d 条)\n\n", len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		title := strings.TrimSpace(it.Title)
		body := strings.TrimSpace(it.BodyMD)
		if n := len([]rune(body)); n > 500 {
			body = string([]rune(body)[:500]) + "……"
		}
		fmt.Fprintf(&b, "=== seq=%d | section=%s ===\n", it.Seq, it.Section)
		fmt.Fprintf(&b, "标题: %s\n", title)
		if body != "" {
			fmt.Fprintf(&b, "内容: %s\n", body)
		}
		b.WriteString("\n")
	}
	b.WriteString("请严格按 system message 要求输出 JSON。")
	return b.String()
}

// parseInfoCardJSON handles the common cases: raw JSON, JSON wrapped
// in a ```json ... ``` fence, or JSON with surrounding prose. Uses a
// brace counter to find the outermost object.
func parseInfoCardJSON(raw string, items []*store.IssueItem) (*HeaderCard, []*Card, error) {
	raw = strings.TrimSpace(raw)
	// Strip common markdown fences.
	raw = fencedJSONRe.ReplaceAllString(raw, "$1")
	raw = strings.TrimSpace(raw)

	// If still not a bare JSON object, extract the outermost braces.
	if !strings.HasPrefix(raw, "{") {
		start := strings.Index(raw, "{")
		end := strings.LastIndex(raw, "}")
		if start >= 0 && end > start {
			raw = raw[start : end+1]
		}
	}

	var wrapper struct {
		Header HeaderCard `json:"header"`
		Cards  []*Card    `json:"cards"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		return nil, nil, fmt.Errorf("infocard: parse json: %w", err)
	}
	if len(wrapper.Cards) == 0 {
		return nil, nil, errors.New("infocard: empty cards array")
	}
	// Backfill item_seq when the LLM lazily omits it by matching the
	// index order against the input items.
	if len(wrapper.Cards) == len(items) {
		for i, c := range wrapper.Cards {
			if c.ItemSeq == 0 && items[i] != nil {
				c.ItemSeq = items[i].Seq
			}
		}
	}
	// Return a pointer header because the caller mutates it later
	// (to set IssueDate). Convert the value to a pointer.
	h := wrapper.Header
	return &h, wrapper.Cards, nil
}

var fencedJSONRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*\\})\\s*```")

// --- OpenAI-compatible HTTP client (copied from rank/classify pattern) ---

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
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (g *llmGenerator) chatComplete(parent context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, g.cfg.Timeout)
	defer cancel()

	body := chatRequest{
		Model:       g.cfg.Model,
		Temperature: 0.3,
		MaxTokens:   8192,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	apiURL := strings.TrimRight(g.cfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.cfg.APIKey)

	resp, err := g.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(b)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, snippet)
	}
	var parsed chatResponse
	if err := json.Unmarshal(b, &parsed); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("empty choices")
	}
	return parsed.Choices[0].Message.Content, nil
}
