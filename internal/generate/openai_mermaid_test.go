package generate

import (
	"strings"
	"testing"

	"briefing-v3/internal/store"
)

// TestMermaidLabelFrom 验证 N1 mermaid 节点 label 提取.
func TestMermaidLabelFrom(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Anthropic 连发 Claude Code routines 和电脑控制", "Anthropi"},   // 前 8 rune (Anthropic 9 字, 截 8)
		{"1. **Claude 会点屏**", "Claude 会"},                          // "Claude 会点屏" = 10 rune, 截前 8
		{"教育部把 AI 列为必修课，力推", "教育部把 AI "},                           // "，" 切到 "教育部把 AI 列为必修课", 前 8 rune
		{"  ", ""},
		{"", ""},
		{"OpenAI 扩展 Trusted", "OpenAI 扩"},                         // 前 8 rune
	}
	for _, tc := range cases {
		got := mermaidLabelFrom(tc.in)
		if got != tc.want {
			t.Errorf("in=%q got=%q want=%q", tc.in, got, tc.want)
		}
	}
}

// TestExtractMermaidLabels 验证从 summary / items / 默认值三级兜底.
func TestExtractMermaidLabels(t *testing.T) {
	t.Run("from_summary_3_lines", func(t *testing.T) {
		in := &Input{Issue: &store.Issue{Summary: "第一件事\n第二件事\n第三件事"}}
		labels := extractMermaidLabels(in)
		if labels[0] != "第一件事" || labels[1] != "第二件事" || labels[2] != "第三件事" {
			t.Errorf("got %v", labels)
		}
	})
	t.Run("summary_short_fallback_items", func(t *testing.T) {
		in := &Input{
			Issue: &store.Issue{Summary: "唯一摘要"},
			Items: []*store.IssueItem{
				{Title: "第二标题"},
				{Title: "第三标题"},
			},
		}
		labels := extractMermaidLabels(in)
		if labels[0] != "唯一摘要" || labels[1] != "第二标题" || labels[2] != "第三标题" {
			t.Errorf("got %v", labels)
		}
	})
	t.Run("empty_all_fallback_default", func(t *testing.T) {
		in := &Input{Issue: &store.Issue{Summary: ""}}
		labels := extractMermaidLabels(in)
		for i, l := range labels {
			if l == "" {
				t.Errorf("label[%d] should never be empty, got %v", i, labels)
			}
		}
	})
	t.Run("nil_input_safe", func(t *testing.T) {
		labels := extractMermaidLabels(nil)
		for i, l := range labels {
			if l == "" {
				t.Errorf("label[%d] should never be empty on nil input, got %v", i, labels)
			}
		}
	})
}

// TestRuleBasedMermaidDiagram 验证 mermaid 块格式合法 + 命中 regex.
func TestRuleBasedMermaidDiagram(t *testing.T) {
	in := &Input{Issue: &store.Issue{Summary: "Anthropic 大升级\n教育部 AI 必修\n斯坦福 Index"}}
	md := ruleBasedMermaidDiagram(in)
	if !strings.HasPrefix(md, "```mermaid\n") {
		t.Errorf("expect ```mermaid prefix, got %q", md[:20])
	}
	if !strings.HasSuffix(md, "```") {
		t.Errorf("expect ``` suffix")
	}
	if !strings.Contains(md, "graph LR") {
		t.Errorf("expect graph LR")
	}
	if !strings.Contains(md, "classDef blue") {
		t.Errorf("expect classDef for styling")
	}
	// 能被 validator 的 mermaidBlockRegex 识别.
	if !mermaidBlockRegex.MatchString(md) {
		t.Errorf("mermaidBlockRegex should match fallback output, got: %s", md)
	}
}
