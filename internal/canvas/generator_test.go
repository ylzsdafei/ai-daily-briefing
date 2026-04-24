package canvas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"briefing-v3/internal/llm"
	"briefing-v3/internal/store"
)

// makeFlow builds a Flow with n valid layered nodes and chain edges.
// Used as the baseline for Validate and Generate positive paths.
// v1.1: uses the new Layer field (0=hero, 1..4 distributed) and drops
// X/Y so it exercises the frontend's auto-layout path.
func makeFlow(n int) *Flow {
	tiers := []string{TierSignal, TierTrend, TierOpportunity, TierAction}
	nodes := make([]FlowNode, 0, n)
	for i := 0; i < n; i++ {
		// Reserve index 0 as the single hero (layer 0); spread the
		// remainder across layers 1..4.
		var layer int
		var tier string
		if i == 0 {
			layer = 0
			tier = TierHero
		} else {
			layer = 1 + (i-1)%4
			tier = tiers[(i-1)%len(tiers)]
		}
		nodes = append(nodes, FlowNode{
			ID:    fmt.Sprintf("n%d", i+1),
			Shape: "rounded",
			Label: fmt.Sprintf("节点 %d", i+1),
			Data: FlowNodeData{
				Tier:        tier,
				Layer:       layer,
				Description: "这是一个测试用节点的描述, 两到三句话.",
				Highlight:   i < 3,
			},
		})
	}
	edges := make([]FlowEdge, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, FlowEdge{
			ID:     fmt.Sprintf("e%d", i+1),
			Source: nodes[i].ID,
			Target: nodes[i+1].ID,
			Label:  "推动",
			Style:  "solid",
		})
	}
	return &Flow{
		Title:   "测试洞察流程图",
		Summary: "这是一段测试导语, 说明今天的图在讲什么.",
		Nodes:   nodes,
		Edges:   edges,
	}
}

func TestFlow_Validate_Valid(t *testing.T) {
	f := makeFlow(12)
	if err := f.Validate(); err != nil {
		t.Fatalf("期望合法 12-node flow 通过校验, 实际失败: %v", err)
	}
}

func TestFlow_Validate_Valid_MaxNodes(t *testing.T) {
	f := makeFlow(MaxNodes)
	if err := f.Validate(); err != nil {
		t.Fatalf("期望 %d-node flow 通过校验, 实际: %v", MaxNodes, err)
	}
}

func TestFlow_Validate_OrphanEdge(t *testing.T) {
	f := makeFlow(12)
	f.Edges[0].Target = "does_not_exist"
	err := f.Validate()
	if err == nil {
		t.Fatal("期望悬空边失败, 实际通过")
	}
	if !strings.Contains(err.Error(), "not a known node id") {
		t.Errorf("错误消息应提示悬空边, 实际: %v", err)
	}
}

func TestFlow_Validate_TooFewNodes(t *testing.T) {
	f := makeFlow(MinNodes - 1)
	err := f.Validate()
	if err == nil {
		t.Fatal("期望节点数低于下限失败, 实际通过")
	}
	if !strings.Contains(err.Error(), "need at least") {
		t.Errorf("错误消息应提示节点数不足, 实际: %v", err)
	}
}

func TestFlow_Validate_TooManyNodes(t *testing.T) {
	f := makeFlow(MaxNodes + 1)
	err := f.Validate()
	if err == nil {
		t.Fatal("期望超上限节点失败, 实际通过")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("错误消息应提示超上限, 实际: %v", err)
	}
}

func TestFlow_Validate_InvalidTier(t *testing.T) {
	f := makeFlow(12)
	f.Nodes[0].Data.Tier = "bogus"
	err := f.Validate()
	if err == nil {
		t.Fatal("期望非法 tier 失败, 实际通过")
	}
	if !strings.Contains(err.Error(), "invalid tier") {
		t.Errorf("错误消息应提示非法 tier, 实际: %v", err)
	}
}

func TestFlow_Validate_EmptyTitle(t *testing.T) {
	f := makeFlow(12)
	f.Title = ""
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "title") {
		t.Errorf("期望空 title 失败, 实际: %v", err)
	}
}

func TestFlow_Validate_DuplicateID(t *testing.T) {
	f := makeFlow(12)
	f.Nodes[1].ID = f.Nodes[0].ID
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("期望重复 id 失败, 实际: %v", err)
	}
}

func TestFlow_Validate_TooFewEdges(t *testing.T) {
	f := makeFlow(12)
	f.Edges = f.Edges[:3] // 12 nodes needs >= 11 edges; 3 is too few
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "too few edges") {
		t.Errorf("期望边太少失败, 实际: %v", err)
	}
}

// v1.1 layer validation: node missing both layer & tier is rejected.
func TestFlow_Validate_MissingLayerAndTier(t *testing.T) {
	f := makeFlow(12)
	f.Nodes[0].Data.Layer = -1 // force out-of-range so hasLayer is false
	f.Nodes[0].Data.Tier = ""
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "data.layer") {
		t.Errorf("期望缺失 layer/tier 失败, 实际: %v", err)
	}
}

// v1.1: multiple hero/layer-0 nodes get rejected.
func TestFlow_Validate_MultipleHero(t *testing.T) {
	f := makeFlow(12)
	// Node 0 is already hero by construction; promote node 1 too.
	f.Nodes[1].Data.Layer = 0
	f.Nodes[1].Data.Tier = TierHero
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "hero") {
		t.Errorf("期望多 hero 节点失败, 实际: %v", err)
	}
}

func TestFlow_ToJSON_RoundTrip(t *testing.T) {
	f := makeFlow(12)
	raw, err := f.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON 失败: %v", err)
	}
	var back Flow
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("roundtrip unmarshal 失败: %v", err)
	}
	if back.Title != f.Title || len(back.Nodes) != len(f.Nodes) || len(back.Edges) != len(f.Edges) {
		t.Errorf("roundtrip 结构不一致")
	}
}

// TestGenerator_Generate_Success mocks the LLM to return a fenced-JSON
// flow and asserts the generator extracts + validates it end-to-end.
func TestGenerator_Generate_Success(t *testing.T) {
	flow := makeFlow(14)
	raw, err := json.Marshal(flow)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	wrapped := "```json\n" + string(raw) + "\n```"

	called := 0
	fakeChat := func(ctx context.Context, hc *http.Client, cfg llm.Config, system, user string) (string, error) {
		called++
		// sanity: the system prompt must be our canvas-specific text.
		// v1.1 rename: "洞察流程图" → "洞察信息图谱"
		if !strings.Contains(system, "洞察信息图谱") {
			t.Errorf("system prompt 没走到 canvas 模板: %q", system[:min(80, len(system))])
		}
		if !strings.Contains(user, "今天是 2026") {
			t.Errorf("user prompt 缺少日期占位: %q", user[:min(80, len(user))])
		}
		return wrapped, nil
	}

	g := NewGeneratorWithChat(Config{Model: "test-model"}, fakeChat)
	issue := &store.Issue{
		IssueDate: time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC),
		Title:     "2026 年 4 月 24 日 AI 洞察日报",
		Summary:   "今日要闻...",
	}
	items := []*store.IssueItem{
		{Section: "product_update", Seq: 1, Title: "Claude 4.7 发布", BodyMD: "正文"},
		{Section: "research", Seq: 1, Title: "Qwen3 新方法", BodyMD: "正文"},
	}
	insight := &store.IssueInsight{IndustryMD: "行业洞察 xxx", OurMD: "对我们的启发 xxx"}

	got, err := g.Generate(context.Background(), issue, items, insight)
	if err != nil {
		t.Fatalf("Generate 期望成功, 实际: %v", err)
	}
	if called != 1 {
		t.Errorf("期望 chat 被调用 1 次, 实际 %d 次", called)
	}
	if got == nil || len(got.Nodes) != 14 {
		t.Errorf("返回 flow 不符合预期: %+v", got)
	}
}

// TestGenerator_Generate_RetriesOnInvalidJSON verifies the backoff
// loop re-prompts the model with failure feedback when the first
// reply is malformed, and succeeds on the retry.
func TestGenerator_Generate_RetriesOnInvalidJSON(t *testing.T) {
	flow := makeFlow(12)
	raw, _ := json.Marshal(flow)

	called := 0
	fakeChat := func(ctx context.Context, hc *http.Client, cfg llm.Config, system, user string) (string, error) {
		called++
		if called == 1 {
			return "这里完全不是 JSON, 也没有括号.", nil
		}
		// second attempt should contain the failure feedback from attempt 1
		if !strings.Contains(user, "上一版输出不合格") {
			t.Errorf("第二次调用 user prompt 应含 feedback, 实际首行: %q", firstLine(user))
		}
		return string(raw), nil
	}

	g := NewGeneratorWithChat(
		Config{Model: "test-model", RetryBackoffs: []time.Duration{10 * time.Millisecond}},
		fakeChat,
	)
	issue := &store.Issue{IssueDate: time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC), Title: "T", Summary: "S"}
	got, err := g.Generate(context.Background(), issue, nil, &store.IssueInsight{IndustryMD: "x", OurMD: "y"})
	if err != nil {
		t.Fatalf("第二次应成功, 实际错误: %v", err)
	}
	if called != 2 {
		t.Errorf("期望调用 2 次, 实际 %d 次", called)
	}
	if got == nil || len(got.Nodes) != 12 {
		t.Errorf("flow 校验失败: %+v", got)
	}
}

// TestGenerator_Generate_TerminalFailure ensures we stop after
// exhausting retries and return the last error (caller decides whether
// to fail-soft).
func TestGenerator_Generate_TerminalFailure(t *testing.T) {
	calls := 0
	fakeChat := func(ctx context.Context, hc *http.Client, cfg llm.Config, system, user string) (string, error) {
		calls++
		return "", errors.New("boom")
	}
	g := NewGeneratorWithChat(
		Config{Model: "test-model", RetryBackoffs: []time.Duration{
			5 * time.Millisecond, 5 * time.Millisecond,
		}},
		fakeChat,
	)
	issue := &store.Issue{IssueDate: time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC), Title: "T", Summary: "S"}
	_, err := g.Generate(context.Background(), issue, nil, &store.IssueInsight{IndustryMD: "x", OurMD: "y"})
	if err == nil {
		t.Fatal("期望终态失败")
	}
	if calls != 3 {
		t.Errorf("期望三次尝试 (1 初 + 2 重试), 实际 %d", calls)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
