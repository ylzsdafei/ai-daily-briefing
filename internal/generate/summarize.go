package generate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// Summarizer is the Step 1B LLM text-generation interface used by the
// compose package to turn a batch of same-section RawItems into a single
// markdown chunk. The output format follows the upstream
// summarizationPromptStepOne style: ordered list, bold titles, adequate
// emoji, embedded hyperlinks.
//
// Concrete implementations are wired up via the existing
// openaiGenerator (which also implements Generator) so callers can share
// a single Config and HTTP client across both interfaces.
type Summarizer interface {
	// Summarize returns the markdown body for one section. sectionTitle
	// is the human-facing section name (e.g. "产品与功能更新") — NOT
	// the internal section id. Empty items returns empty string, nil.
	Summarize(ctx context.Context, sectionTitle string, items []*store.RawItem) (string, error)
}

// summarizeSystemPrompt is ported from upstream
// CloudFlare-AI-Insight-Daily/src/prompt/summarizationPromptStepOne.js
// then extended with the "好的示例" clause that the upstream evals
// rely on. Do not paraphrase: the emoji, quoting and numbered-list
// requirements are load-bearing, downstream render code trims leading
// markdown headers and expects ordered-list items.
const summarizeSystemPrompt = `你是一名专业的 AI 日报编辑。你的任务是把一批 AI 领域的候选条目整理成一个 section 的主体 markdown 内容。

输出格式要求:
1. 每条条目用有序列表 "1. **一句话概括标题。**\n紧跟一段 3-5 句话说明 🚀 带适度 emoji 💡"
2. 说明里必须引用超链接 **[简短中文锚文本(briefing)](原始URL)**
   - 锚文本由你根据内容自拟，6-14 个汉字，点出具体是什么（例："官方发布页(briefing)"、"GitHub 仓库(briefing)"、"完整报道(briefing)"、"arxiv 论文(briefing)"、"黄仁勋演讲视频(briefing)"）
   - **严禁** 输出裸露 URL（如 "详情见 https://xxx.com"），所有 URL 必须包在 [锚文本](url) 里
   - **严禁** 把 URL 本身当作锚文本（如 [https://xxx.com](https://xxx.com)）
   - 每条至少 1 个超链接引用，最多 3 个
3. 关键词用 **粗体** 强调
4. 语言风格: 通俗易懂、流畅自然、生动不失深度，有适度口语化 (๑•̀ㅂ•́)و 但不低俗
5. 非大众熟知的专业名词必须加括号注释 (非技术用户友好)
6. 严格来源于原材料，不捏造、不添加原文未提及的事实
7. 输出必须是简体中文 Markdown
8. 直接输出 markdown，不加任何前置说明或标题
9. 关于标题：如果原始条目的标题本身已经简洁有力、含有具体公司/产品名、读起来顺，允许直接引用或做轻度汉化/改写，不必强行重写；标题党风格的词（"突袭"/"炸裂"/"屠榜"/"重磅"等）每条最多用一个，不堆砌
10. 说明部分必须基于原文重新组织语言，不能只是原文复制粘贴

重要通用原则: 所有摘要内容必须严格来源于原文。不得捏造、歪曲或添加原文未提及的信息。读者是不懂技术的同事与领导，优先把话说清楚，其次才是吸引眼球。

参考好的示例:
1. **DeepSeek 深夜暗更疑似 V4 突袭。**
DeepSeek 昨晚推送重磅更新，新增**快速模式**和**专家模式**两档 ⚡。网友实测后模型竟 😲 自称**V4版本**，视觉模型也悄悄开启灰度测试。[DeepSeek 更新详情(briefing)](原始URL)

2. **Anthropic 托管 Agent 定价 0.08 美元/小时，Agent 白菜价时代到来。**
Anthropic 放出**托管式 Agent 平台**，开发者只需定义任务和规则就能让 Claude 自动 👉 跑完整个流程。Sentry（程序员自动查 bug 的工具）已实现代码自动修复，Notion（团队协作办公工具）支持多任务并行。[Anthropic 托管 Agent 发布(briefing)](原始URL)`

// summarizeUserPromptTemplate takes:
//
//	%s: section title (e.g. "产品与功能更新")
//	%s: joined candidate item lines
const summarizeUserPromptTemplate = `Section: %s

以下是本 section 的候选条目，请按要求整理输出:

%s`

// Summarize implements the Summarizer interface on openaiGenerator.
// The existing openaiGenerator (from openai.go) already carries a Config
// and http.Client, so we reuse them here rather than standing up a new
// struct. openaiGenerator therefore satisfies both the Generator and
// Summarizer interfaces.
func (g *openaiGenerator) Summarize(ctx context.Context, sectionTitle string, items []*store.RawItem) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	if strings.TrimSpace(sectionTitle) == "" {
		return "", errors.New("generate: Summarize requires a non-empty sectionTitle")
	}

	userPrompt := fmt.Sprintf(
		summarizeUserPromptTemplate,
		sectionTitle,
		formatItemsForSummarize(items),
	)

	// Retry with exponential backoff. Summarize calls can hit transient
	// upstream 502s on long prompts; simple linear retry without backoff
	// often re-hits the same flaky worker. We use 5 attempts with
	// 1s / 2s / 4s / 8s sleeps in between.
	const maxAttempts = 5
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err := g.chatComplete(ctx, summarizeSystemPrompt, userPrompt, g.cfg.MaxTokens*2)
		if err != nil {
			lastErr = err
			if attempt < maxAttempts {
				backoff := time.Duration(1<<uint(attempt-1)) * time.Second
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
			lastErr = errors.New("generate: Summarize produced empty output")
			continue
		}
		return cleaned, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("generate: Summarize failed after %d attempts", maxAttempts)
	}
	return "", lastErr
}

// formatItemsForSummarize renders each RawItem as one bullet in the
// user prompt. We include title, url, source id and a truncated excerpt
// of the extracted content so the LLM has enough grounding to summarize.
func formatItemsForSummarize(items []*store.RawItem) string {
	const maxItems = 8     // cap so prompt never blows token budget
	const maxExcerpt = 250 // runes per item excerpt (reduced from 400 to avoid 502)

	var b strings.Builder
	n := 0
	for _, it := range items {
		if it == nil {
			continue
		}
		if n >= maxItems {
			break
		}
		excerpt := strings.TrimSpace(it.Content)
		if excerpt == "" {
			excerpt = "(no excerpt)"
		}
		if len([]rune(excerpt)) > maxExcerpt {
			excerpt = string([]rune(excerpt)[:maxExcerpt]) + "..."
		}
		// Collapse newlines in excerpt so each item stays on a couple
		// of lines and the LLM can easily parse the list.
		excerpt = strings.ReplaceAll(excerpt, "\n", " ")
		title := strings.TrimSpace(it.Title)

		fmt.Fprintf(&b, "- 标题: %s\n  来源: source#%d\n  URL: %s\n  摘要: %s\n\n",
			title, it.SourceID, it.URL, excerpt)
		n++
	}
	return strings.TrimSpace(b.String())
}

// compile-time assertion: openaiGenerator implements Summarizer.
var _ Summarizer = (*openaiGenerator)(nil)

// (unused helper to silence the unused-import linter if time ends up
// unreferenced in a given build — kept intentionally near the var so a
// human reviewer sees the note.)
var _ = time.Second
