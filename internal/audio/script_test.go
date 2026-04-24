package audio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"briefing-v3/internal/store"
)

// mockChatCompletionResponse emits an OpenAI-compatible chat response
// whose single choice.message.content is `content`. internal/llm uses
// this exact schema so the ScriptGenerator cannot tell the mock from
// the real api.gjs.ink endpoint.
func mockChatCompletionResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	// Matches llm.chatResponse shape (choices[0].message.content).
	payload := map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": content}},
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// buildTestFixtures returns an issue + items + insight triple with
// enough content that script.go's prompt builder doesn't drop things
// as empty. Shared by every script test below.
func buildTestFixtures() (*store.Issue, []*store.IssueItem, *store.IssueInsight) {
	issue := &store.Issue{
		ID:        42,
		DomainID:  "ai",
		IssueDate: time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC),
		Title:     "2026年4月24日 AI洞察日报",
		Summary:   "Anthropic 托管 Agent 降价; Google 推出生成式界面; DeepSeek V4 上线",
	}
	items := []*store.IssueItem{
		{
			IssueID: 42, Section: store.SectionProductUpdate, Seq: 1,
			Title:  "Anthropic 推出托管 Agent, 每小时 0.08 美元",
			BodyMD: "1. **Anthropic 推出托管 Agent。**\nAnthropic 放出托管式 Agent 平台, 开发者只需定义任务和规则就能让 Claude 自动跑完整个流程。[Anthropic 发布(briefing)](https://example.com)\n",
		},
		{
			IssueID: 42, Section: store.SectionResearch, Seq: 1,
			Title:  "DeepSeek 深夜暗更疑似 V4",
			BodyMD: "1. **DeepSeek 深夜暗更疑似 V4。**\nDeepSeek 推送重磅更新, 新增快速模式和专家模式。[DeepSeek 更新(briefing)](https://example.com)\n",
		},
		{
			IssueID: 42, Section: store.SectionIndustry, Seq: 1,
			Title:  "Google 推出生成式界面标准",
			BodyMD: "1. **Google 推出生成式界面标准。**\nGoogle 发布新的 UI 生成协议。[Google 官网(briefing)](https://example.com)\n",
		},
	}
	insight := &store.IssueInsight{
		IssueID:    42,
		IndustryMD: "1. Anthropic 和 Google 同日发布都指向 Agent 界面标准化。",
		OurMD:      "1. 我们要关注 Agent 界面标准, 提前兼容。",
	}
	return issue, items, insight
}

// validMonologue returns a ~1500-rune Chinese script that passes the
// length floor. The content itself doesn't need to be coherent — we
// only test the pipeline plumbing.
func validMonologue() string {
	line := "诶，今天 AI 圈儿最有意思的不是又有谁发了新模型，而是 Anthropic 突然把托管 Agent 价格砍到每小时八美分，这事儿说明 Agent 的成本已经进入白菜价时代，对我们平台的定位是巨大利好。说白了，我们的价值必须从算力转向信任和选择。"
	// Repeat until we clear the 1200 rune soft target (script.go's
	// minDraftRunes is 800, SystemPrompt asks for 1200-1800).
	var b strings.Builder
	for runeLen(b.String()) < 1400 {
		b.WriteString(line)
		b.WriteString("\n\n")
	}
	return b.String()
}

// TestScriptGenerator_Success verifies that when the LLM returns a
// healthy 1500-rune draft, Generate passes it through unchanged.
func TestScriptGenerator_Success(t *testing.T) {
	want := validMonologue()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		// Sanity-check the request carries both system + user messages
		// — if the prompt builder regresses this is how we catch it.
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "罗永浩") {
			t.Error("request body missing 罗永浩 persona text")
		}
		if !strings.Contains(string(body), "2026年4月24日") {
			t.Error("request body missing the Chinese date header")
		}
		mockChatCompletionResponse(w, want)
	}))
	defer srv.Close()

	cfg := Config{
		BaseURL:             srv.URL,
		APIKey:              "test-key",
		Model:               "gpt-5.4",
		Timeout:             5 * time.Second,
		MaxRetries:          1,
		RetryBackoffSeconds: []int{1},
	}
	sg := NewScriptGenerator(cfg, "")
	issue, items, insight := buildTestFixtures()

	got, err := sg.Generate(context.Background(), issue, items, insight)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.TrimSpace(got) != strings.TrimSpace(want) {
		t.Errorf("Generate returned %d runes, want %d runes (content mismatch)", runeLen(got), runeLen(want))
	}
}

// TestScriptGenerator_TooShort verifies that a too-short draft
// (below minDraftRunes) is rejected and triggers a retry.
func TestScriptGenerator_TooShort(t *testing.T) {
	longEnough := validMonologue()
	var requestCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		if requestCount == 1 {
			// First attempt: too-short draft.
			mockChatCompletionResponse(w, "太短了。")
			return
		}
		// Retry: full-length draft.
		mockChatCompletionResponse(w, longEnough)
	}))
	defer srv.Close()

	cfg := Config{
		BaseURL:             srv.URL,
		APIKey:              "test-key",
		Model:               "gpt-5.4",
		Timeout:             5 * time.Second,
		MaxRetries:          2,
		RetryBackoffSeconds: []int{1, 1},
	}
	sg := NewScriptGenerator(cfg, "")
	issue, items, insight := buildTestFixtures()

	got, err := sg.Generate(context.Background(), issue, items, insight)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if requestCount != 2 {
		t.Errorf("expected exactly 2 LLM calls (short → retry), got %d", requestCount)
	}
	if runeLen(got) < minDraftRunes {
		t.Errorf("final draft too short: %d runes", runeLen(got))
	}
}

// TestScriptGenerator_LLMError verifies transport-level failure is
// surfaced cleanly rather than swallowed as an empty script.
func TestScriptGenerator_LLMError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream overloaded"}}`))
	}))
	defer srv.Close()

	cfg := Config{
		BaseURL:             srv.URL,
		APIKey:              "test-key",
		Model:               "gpt-5.4",
		Timeout:             5 * time.Second,
		MaxRetries:          1,
		RetryBackoffSeconds: []int{1},
	}
	sg := NewScriptGenerator(cfg, "")
	issue, items, insight := buildTestFixtures()

	_, err := sg.Generate(context.Background(), issue, items, insight)
	if err == nil {
		t.Fatal("expected error from 503 upstream")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error missing HTTP status: %v", err)
	}
}

// TestScriptGenerator_SelfCheck verifies the optional self-check
// pass: when enabled AND the first draft is accepted, a second LLM
// call applies SelfCheckPrompt. If that call returns a longer
// reviewed version, we use it.
func TestScriptGenerator_SelfCheck(t *testing.T) {
	firstDraft := validMonologue()
	reviewed := firstDraft + "\n\n补充一段：那么问题就来了，我们到底该怎么办呢？说白了，就是得把信任机制做扎实。"

	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		body, _ := io.ReadAll(r.Body)
		if requestCount == 1 {
			// First call uses SystemPromptLuoYonghao.
			if !strings.Contains(string(body), "人设铁律") {
				t.Error("first call system prompt mismatch")
			}
			mockChatCompletionResponse(w, firstDraft)
			return
		}
		// Second call uses SelfCheckPrompt.
		if !strings.Contains(string(body), "终审校对") {
			t.Error("second call should use SelfCheckPrompt")
		}
		mockChatCompletionResponse(w, reviewed)
	}))
	defer srv.Close()

	cfg := Config{
		BaseURL:             srv.URL,
		APIKey:              "test-key",
		Model:               "gpt-5.4",
		Timeout:             5 * time.Second,
		MaxRetries:          1,
		RetryBackoffSeconds: []int{1},
		EnableSelfCheck:     true,
	}
	sg := NewScriptGenerator(cfg, "")
	issue, items, insight := buildTestFixtures()

	got, err := sg.Generate(context.Background(), issue, items, insight)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if requestCount != 2 {
		t.Errorf("expected 2 LLM calls (draft + self-check), got %d", requestCount)
	}
	if !strings.Contains(got, "补充一段") {
		t.Error("reviewed draft not returned; self-check result was dropped")
	}
}

// TestBuildFullMD verifies the internal markdown reconstructor emits
// section headers and item bodies in canonical order. Useful as a
// regression guard for the prompt template.
func TestBuildFullMD(t *testing.T) {
	issue, items, _ := buildTestFixtures()
	md := buildFullMD(issue, items)
	mustContain(t, md, "2026/4/24")
	mustContain(t, md, "### 产品与功能更新")
	mustContain(t, md, "### AI 研究")
	mustContain(t, md, "### 产业新闻")
	mustContain(t, md, "Anthropic 推出托管 Agent")
	// Sections with no items must be elided to keep the prompt lean.
	if strings.Contains(md, "### 开源项目") {
		t.Error("empty section 开源项目 should not appear in buildFullMD output")
	}
}

// TestBuildTopItems verifies the top-items digest respects the
// canonical section order + limit.
func TestBuildTopItems(t *testing.T) {
	_, items, _ := buildTestFixtures()
	got := buildTopItems(items, 10)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "- [产品与功能更新]") {
		t.Errorf("first line ordering wrong: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "- [AI 研究]") {
		t.Errorf("second line ordering wrong: %q", lines[1])
	}

	limited := buildTopItems(items, 2)
	limitedLines := strings.Split(strings.TrimSpace(limited), "\n")
	if len(limitedLines) != 2 {
		t.Errorf("limit=2 not respected, got %d lines", len(limitedLines))
	}
}

// TestFormatDateZH verifies the date stringifier used in the prompt.
func TestFormatDateZH(t *testing.T) {
	issue := &store.Issue{IssueDate: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)}
	got := formatDateZH(issue)
	want := "2026年1月5日"
	if got != want {
		t.Errorf("formatDateZH = %q, want %q", got, want)
	}
	if got := formatDateZH(nil); got != "" {
		t.Errorf("formatDateZH(nil) = %q, want empty", got)
	}
}

// mustContain is a tiny helper that fails the test if `want` is not
// a substring of `have`, with a snipped diff that is easier to read
// than the default Errorf output for long strings.
func mustContain(t *testing.T, have, want string) {
	t.Helper()
	if strings.Contains(have, want) {
		return
	}
	snippet := have
	if len(snippet) > 300 {
		snippet = snippet[:300] + "..."
	}
	t.Errorf("missing substring %q\n-- got --\n%s", want, snippet)
	_ = fmt.Sprintf // keep fmt referenced if snippet truncation is refactored out
}
