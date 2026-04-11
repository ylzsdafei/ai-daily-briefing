// Package render — Slack Block Kit payload builder.
//
// This file converts a publish.RenderedIssue into a Slack-ready JSON
// payload. The block layout mirrors scripts/slack-notify.js in the
// legacy AI-Insight-Daily repo so existing user expectations carry over,
// but the source of truth is the structured RenderedIssue rather than a
// re-parsed markdown string. The key additions for v3 are:
//
//   - a headline image block at the top (when HeadlineImageURL is set)
//   - one block per section with truncated markdown, so readers get a
//     real preview of the day's content inside Slack instead of only
//     the insight lines.
package render

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"briefing-v3/internal/publish"
)

// BuildSlackPayload returns the Slack webhook JSON body for rendered.
// It returns an error only if JSON marshalling fails, which should be
// impossible for the simple map/slice structure it builds.
func BuildSlackPayload(rendered *publish.RenderedIssue) ([]byte, error) {
	payload := buildSlackPayloadMap(rendered)
	return json.Marshal(payload)
}

// buildSlackPayloadMap is the testable counterpart of BuildSlackPayload
// that returns the raw map. Kept private; tests can call it via the
// package if needed.
func buildSlackPayloadMap(rendered *publish.RenderedIssue) map[string]any {
	blocks := make([]map[string]any, 0, 24)

	if rendered == nil || rendered.Issue == nil {
		return map[string]any{"blocks": blocks}
	}
	issue := rendered.Issue
	dateStr := issue.IssueDate.Format("2006-01-02")

	chineseDate := rendered.DateZH
	if chineseDate == "" {
		chineseDate = FormatDateZH(issue)
	}

	// 1. Headline image block (optional).
	if strings.TrimSpace(rendered.HeadlineImageURL) != "" {
		blocks = append(blocks, map[string]any{
			"type":      "image",
			"image_url": rendered.HeadlineImageURL,
			"alt_text":  fmt.Sprintf("briefing-v3 %s 头版", dateStr),
		})
	}

	// 2. Header block. When the upstream gate reported a soft-warn state
	// (v1.0.0 D7b), prefix the header with "🟡 质量待审 | " so downstream
	// readers immediately see the briefing passed through a degraded
	// state and should be double-checked before any prod promotion.
	headerText := fmt.Sprintf("🤖 AI 资讯日报 - %s", chineseDate)
	if rendered.QualityWarn {
		headerText = "🟡 质量待审 | " + headerText
	}
	blocks = append(blocks, map[string]any{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  headerText,
			"emoji": true,
		},
	})

	// 3. Industry insight + 4. divider + 5. Our takeaways + 6. divider.
	if rendered.Insight != nil {
		industryMD := strings.TrimSpace(rendered.Insight.IndustryMD)
		ourMD := strings.TrimSpace(rendered.Insight.OurMD)
		if industryMD != "" {
			n := countSlackNumberedItems(industryMD)
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*📊 行业洞察（今日 %d 条）*\n\n%s",
						n, convertToSlackMrkdwn(industryMD)),
				},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
		if ourMD != "" {
			n := countSlackNumberedItems(ourMD)
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*💭 对我们的启发（今日 %d 条）*\n\n%s",
						n, convertToSlackMrkdwn(ourMD)),
				},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
	}

	// 7. Today's summary. Upstream numbers each non-blank line; we keep
	// that behaviour and skip double-numbering when the line already
	// starts with "N.".
	summary := strings.TrimSpace(issue.Summary)
	if summary != "" {
		rawLines := strings.Split(summary, "\n")
		kept := make([]string, 0, len(rawLines))
		for _, l := range rawLines {
			if t := strings.TrimSpace(l); t != "" {
				kept = append(kept, t)
			}
		}
		if len(kept) > 0 {
			numbered := make([]string, 0, len(kept))
			for i, l := range kept {
				if slackLeadingNumRe.MatchString(l) {
					numbered = append(numbered, l)
				} else {
					numbered = append(numbered, fmt.Sprintf("%d. %s", i+1, l))
				}
			}
			blocks = append(blocks, map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*📋 今日摘要（%d 条）*\n\n%s",
						len(kept), convertToSlackMrkdwn(strings.Join(numbered, "\n"))),
				},
			})
			blocks = append(blocks, map[string]any{"type": "divider"})
		}
	}

	// (Section body previews intentionally REMOVED in v1.0.0.)
	// The full per-section content is only visible on the web viewer via
	// the "📖 查看完整早报" button below. This keeps Slack messages
	// scannable — user feedback was that section prose was too dense
	// and hurt readability inside Slack.

	// 9. Actions — view full report button.
	reportURL := strings.TrimSpace(rendered.ReportURL)
	if reportURL == "" {
		reportURL = "https://github.com/ylzsdafei/briefing-v3"
	}
	blocks = append(blocks, map[string]any{
		"type": "actions",
		"elements": []map[string]any{
			{
				"type": "button",
				"text": map[string]any{
					"type":  "plain_text",
					"text":  "📖 查看完整日报",
					"emoji": true,
				},
				"url":   reportURL,
				"style": "primary",
			},
		},
	})

	// 10. Footer context.
	// v1.0.0: 只保留一行简洁的 "briefing-v3 自动推送 | 日期"，
	// 不向 Slack 用户暴露 FailedSections / QualityWarnings 这种内部
	// 质量信号 (用户反馈: 这些字段不用暴露出来).
	footerElems := []map[string]any{
		{
			"type": "mrkdwn",
			"text": fmt.Sprintf("briefing-v3 自动推送 | %s", dateStr),
		},
	}
	blocks = append(blocks, map[string]any{
		"type":     "context",
		"elements": footerElems,
	})

	return map[string]any{"blocks": blocks}
}

// defaultSectionOrder mirrors config/ai.yaml sections[]. Hard-coded here
// so slack rendering does not need to import the config package; if
// ai.yaml is ever reordered this list must be updated too.
var defaultSectionOrder = []SectionMeta{
	{ID: "product_update", Title: "产品与功能更新"},
	{ID: "research", Title: "前沿研究"},
	{ID: "industry", Title: "行业展望与社会影响"},
	{ID: "opensource", Title: "开源TOP项目"},
	{ID: "social", Title: "社媒分享"},
}

// ------- markdown → Slack mrkdwn helpers -------
//
// These are deliberately local to the render package so slack.go can
// live under internal/render without a cross-package call back into
// internal/publish. If both copies ever drift they should be re-unified.

var (
	slackBoldPattern  = regexp.MustCompile(`\*\*(.+?)\*\*`)
	slackLinkPattern  = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	slackNumberedRe   = regexp.MustCompile(`(?m)^\d+\.`)
	slackLeadingNumRe = regexp.MustCompile(`^\d+\.\s`)
)

// convertToSlackMrkdwn translates a narrow subset of CommonMark into the
// custom mrkdwn Slack expects:
//
//   - **bold**  → *bold*
//   - [t](u)    → <u|t>
//   - trailing  truncation at 2900 characters with a literal "..."
//
// We intentionally do NOT touch headings or list bullets — Slack
// renders them fine as-is, and stripping them would lose structure.
func convertToSlackMrkdwn(text string) string {
	if text == "" {
		return ""
	}
	out := slackBoldPattern.ReplaceAllString(text, `*$1*`)
	out = slackLinkPattern.ReplaceAllString(out, `<$2|$1>`)
	// Slack's hard limit on a section text is 3000 characters. Leave
	// a small margin so emoji width and the "..." marker fit cleanly.
	const limit = 2900
	if len([]rune(out)) > limit {
		runes := []rune(out)
		out = string(runes[:limit]) + "..."
	}
	return out
}

// countSlackNumberedItems counts "N." lines in text. Mirrors the
// equivalent helper in internal/generate and internal/publish so each
// layer can compute bullet counts without a cross-package dependency.
func countSlackNumberedItems(text string) int {
	return len(slackNumberedRe.FindAllString(text, -1))
}
