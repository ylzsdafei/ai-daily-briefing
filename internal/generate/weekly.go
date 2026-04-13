package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// WeeklyConfig parameterizes the LLM client used by the weekly pass.
type WeeklyConfig struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	Timeout     time.Duration
	MaxRetries  int
}

func (c *WeeklyConfig) fillDefaults() {
	if c.Temperature == 0 {
		c.Temperature = 0.4
	}
	if c.Timeout <= 0 {
		c.Timeout = 180 * time.Second
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
}

// DailyBundle groups the data for one daily issue needed by the weekly prompt.
type DailyBundle struct {
	Issue   *store.Issue
	Items   []*store.IssueItem
	Insight *store.IssueInsight
}

// WeeklyResult is the structured output of GenerateWeekly.
type WeeklyResult struct {
	TitleKeywords  string
	FocusMD        string
	SignalsMD      string
	TrendsMD       string
	TrendsDiagram  string // Mermaid code for trends overview diagram
	TakeawaysMD    string
	PonderMD       string
}

const weeklySystemPrompt = `你是一位资深AI行业分析师，负责撰写每周综合分析报告。

你的读者是一家AI创业公司的全体员工——有CEO、技术、设计、HR、运营，大部分人不懂技术。
他们已经看过本周的每日日报，现在需要你做一份"周度总结"，帮他们把碎片化的每日信息串成线索。

公司背景：产品尚未上市的早期团队，方向是Agent调度与进化平台——简单说就是帮普通人像叫外卖一样使用AI，让好的AI方案能被评价、选择和信任。to C为主to B为辅。

你会收到本周每日日报的标题、条目摘要和行业洞察。请输出以下内容（严格 JSON）：

{
  "title_keywords": "2-4 个本周最核心关键词, 用顿号分隔",
  "focus": "本周聚焦 — 选 2-3 件本周最重要的事做深度拆解。每件事: 发生了什么 → 为什么重要 → 对行业意味着什么。共 1200-1800 字。使用 markdown 格式，每件事用 ### 小标题分隔。每件事的分析末尾，附一段 mermaid 代码块，用流程图或关系图展示该事件的核心逻辑链（因果关系/利益链条/影响传导），节点用简短中文标签（4-10字），不超过 8 个节点。",
  "signals": "信号与噪音 — 5-7 条本周值得注意但没有大到需要深度拆解的事件。每条用有序列表: 一句话事实 + 一句话点评。共 800-1200 字。",
  "trends": "宏观趋势 — 从本周事件中提炼 3-4 个趋势方向。每个趋势: 趋势名称 + 本周哪些事件验证了这个趋势 + 未来可能走向。共 400-600 字。",
  "trends_diagram": "一段 mermaid 代码（不含围栏标记），用 graph LR 或 graph TD 画一张本周趋势全景关系图，展示 3-4 个趋势方向之间的关联，以及关键事件如何支撑这些趋势。节点用简短中文（4-12字），边上标注关系词（2-4字）。不超过 15 个节点。",
  "takeaways": "对我们的启发 — 从 Agent 调度平台的角度, 本周事件给我们什么参考。产品方向/竞争策略/时机判断, 各 1-2 条。共 300-500 字。",
  "ponder": "本周思考 — 一个引发深度思考的问题, 不需要有答案, 让读者带着问题进入下一周。1-2 句话。"
}

【写作规则】
1. 每件事都追溯到本周具体日报中的具体事件, 不凭空分析
2. 严格客观, 好消息坏消息都说, 不讨好读者
3. 非大众熟知的概念必须加括号注释（标准：会用ChatGPT但不会写代码的老板是否认识）
4. 不硬凑: 如果本周某个板块素材不足, 宁可少写, 不要注水
5. 禁止输出任何运维、排障、调度、发送、监控信息
6. 不要输出任何 JSON 以外的文字`

// GenerateWeekly calls the LLM to produce a weekly analysis from daily bundles.
func GenerateWeekly(ctx context.Context, cfg WeeklyConfig, startDate, endDate time.Time, dailies []DailyBundle) (*WeeklyResult, error) {
	if len(dailies) == 0 {
		return nil, fmt.Errorf("weekly: no daily bundles")
	}
	cfg.fillDefaults()

	userPrompt := buildWeeklyUserPrompt(startDate, endDate, dailies)

	hc := &http.Client{}
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxRetries; attempt++ {
		raw, err := weeklyChat(ctx, hc, cfg, weeklySystemPrompt, userPrompt)
		if err != nil {
			lastErr = err
			continue
		}
		result, perr := parseWeeklyJSON(raw)
		if perr != nil {
			lastErr = perr
			continue
		}
		return result, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("weekly: failed with no specific error")
	}
	return nil, lastErr
}

func buildWeeklyUserPrompt(startDate, endDate time.Time, dailies []DailyBundle) string {
	var b strings.Builder
	fmt.Fprintf(&b, "本周日报汇总（%s ~ %s，共 %d 天）:\n\n",
		startDate.Format("2006-01-02"), endDate.Format("2006-01-02"), len(dailies))

	for _, d := range dailies {
		if d.Issue == nil {
			continue
		}
		fmt.Fprintf(&b, "=== %s ===\n", d.Issue.IssueDate.Format("2006-01-02"))
		fmt.Fprintf(&b, "日报标题: %s\n", d.Issue.Title)
		b.WriteString("条目摘要:\n")

		items := d.Items
		if len(items) > 15 {
			items = items[:15]
		}
		for _, it := range items {
			if it == nil {
				continue
			}
			fmt.Fprintf(&b, "- [%s] %s\n", it.Section, it.Title)
		}

		if d.Insight != nil {
			if ind := strings.TrimSpace(d.Insight.IndustryMD); ind != "" {
				fmt.Fprintf(&b, "行业洞察:\n%s\n", ind)
			}
			if our := strings.TrimSpace(d.Insight.OurMD); our != "" {
				fmt.Fprintf(&b, "对我们的启发:\n%s\n", our)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("请严格按 system message 要求输出 JSON。")
	return b.String()
}

var weeklyFencedRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*\\})\\s*```")

func parseWeeklyJSON(raw string) (*WeeklyResult, error) {
	raw = strings.TrimSpace(raw)
	raw = weeklyFencedRe.ReplaceAllString(raw, "$1")
	raw = strings.TrimSpace(raw)

	if !strings.HasPrefix(raw, "{") {
		start := strings.Index(raw, "{")
		end := strings.LastIndex(raw, "}")
		if start >= 0 && end > start {
			raw = raw[start : end+1]
		}
	}

	var parsed struct {
		TitleKeywords string `json:"title_keywords"`
		Focus         string `json:"focus"`
		Signals       string `json:"signals"`
		Trends        string `json:"trends"`
		TrendsDiagram string `json:"trends_diagram"`
		Takeaways     string `json:"takeaways"`
		Ponder        string `json:"ponder"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("weekly: parse json: %w", err)
	}
	if strings.TrimSpace(parsed.Focus) == "" {
		return nil, fmt.Errorf("weekly: focus section is empty")
	}
	// LLM sometimes outputs literal "\n" (two chars) instead of real
	// newlines inside JSON string values. Replace them so markdown
	// renders correctly with proper paragraphs and headings.
	fix := func(s string) string {
		return strings.ReplaceAll(s, `\n`, "\n")
	}
	return &WeeklyResult{
		TitleKeywords:  parsed.TitleKeywords,
		FocusMD:        fix(parsed.Focus),
		SignalsMD:      fix(parsed.Signals),
		TrendsMD:       fix(parsed.Trends),
		TrendsDiagram:  fix(parsed.TrendsDiagram),
		TakeawaysMD:    fix(parsed.Takeaways),
		PonderMD:       fix(parsed.Ponder),
	}, nil
}

func weeklyChat(parent context.Context, hc *http.Client, cfg WeeklyConfig, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()

	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := struct {
		Model       string  `json:"model"`
		Messages    []msg   `json:"messages"`
		Temperature float64 `json:"temperature"`
		MaxTokens   int     `json:"max_tokens"`
	}{
		Model:       cfg.Model,
		Temperature: cfg.Temperature,
		MaxTokens:   16384,
		Messages: []msg{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	apiURL := strings.TrimRight(cfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := hc.Do(req)
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
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("empty choices")
	}
	return parsed.Choices[0].Message.Content, nil
}
