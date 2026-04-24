// Package toolcall implements the OpenAI-style chat completion
// tool-loop used by canvas.Generator and audio.ScriptGenerator.
// It is its own package so neither feature module needs to import the
// other just to share a function — those modules are siblings, not
// parent/child.
//
// The loop exposes exactly one tool (`web_search`) backed by
// search.TavilyClient. When the LLM returns `tool_calls`, we run
// each query through Tavily, feed the results back as `role:tool`
// messages, and re-invoke the LLM — up to `MaxRounds` cycles.
//
// Payloads follow the standard OpenAI chat/completions schema
// (verified 2026-04-24 against api.gjs.ink's gpt-5.4).
package toolcall

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

	"briefing-v3/internal/llm"
	"briefing-v3/internal/search"
)

const (
	MaxRounds        = 6
	MaxResultsPerQry = 5
)

// webSearchToolSpec is the single tool we advertise to the model.
// Detailed "when to search" guidance lives in each caller's system
// prompt (canvas.SearchGuidelines / audio.SearchGuidelines).
var webSearchToolSpec = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "web_search",
		"description": "Search the public web for recent, authoritative information about an AI / tech topic. Use when the daily briefing alone isn't enough to know how today's news fits the bigger industry context. Prefer English queries for international topics, Chinese for domestic ones.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Short focused search query (5-15 words). Include the month or year for recency when relevant.",
				},
			},
			"required": []string{"query"},
		},
	},
}

type toolCallPart struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMsg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []toolCallPart `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type chatReq struct {
	Model       string           `json:"model"`
	Messages    []chatMsg        `json:"messages"`
	Temperature float64          `json:"temperature"`
	MaxTokens   int              `json:"max_tokens"`
	Tools       []map[string]any `json:"tools,omitempty"`
	ToolChoice  string           `json:"tool_choice,omitempty"`
}

type chatResp struct {
	Choices []struct {
		Message      chatMsg `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ChatWithSearch runs the tool-loop to produce the final assistant
// content. `logSrc` tags the stdout lines we emit for each tool call
// (operators see "[canvas-toolloop]" vs "[audio-toolloop]" so they
// can distinguish which caller is spending Tavily quota).
func ChatWithSearch(
	ctx context.Context,
	hc *http.Client,
	cfg llm.Config,
	system, user string,
	searcher *search.TavilyClient,
	logSrc string,
) (string, error) {
	if searcher == nil {
		return "", errors.New("toolcall: searcher is required")
	}

	messages := []chatMsg{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}

	url := strings.TrimRight(cfg.BaseURL, "/") + "/v1/chat/completions"
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}

	for round := 0; round < MaxRounds; round++ {
		buf, err := json.Marshal(chatReq{
			Model:       cfg.Model,
			Messages:    messages,
			Temperature: cfg.Temperature,
			MaxTokens:   cfg.MaxTokens,
			Tools:       []map[string]any{webSearchToolSpec},
			ToolChoice:  "auto",
		})
		if err != nil {
			return "", fmt.Errorf("toolcall: marshal: %w", err)
		}

		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, "POST", url, bytes.NewReader(buf))
		if err != nil {
			cancel()
			return "", fmt.Errorf("toolcall: new request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

		resp, err := hc.Do(req)
		if err != nil {
			cancel()
			return "", fmt.Errorf("toolcall: http: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if err != nil {
			return "", fmt.Errorf("toolcall: read: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			snippet := string(body)
			if len(snippet) > 500 {
				snippet = snippet[:500]
			}
			return "", fmt.Errorf("toolcall http %d: %s", resp.StatusCode, snippet)
		}

		var parsed chatResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return "", fmt.Errorf("toolcall: parse response: %w", err)
		}
		if parsed.Error != nil {
			return "", fmt.Errorf("toolcall: api error: %s", parsed.Error.Message)
		}
		if len(parsed.Choices) == 0 {
			return "", errors.New("toolcall: no choices in response")
		}
		assistant := parsed.Choices[0].Message

		if len(assistant.ToolCalls) == 0 {
			final := strings.TrimSpace(assistant.Content)
			if final == "" {
				return "", errors.New("toolcall: assistant returned empty content")
			}
			return final, nil
		}

		// OpenAI protocol requires the assistant message with tool_calls
		// to be present in the conversation before the tool replies.
		messages = append(messages, assistant)
		for _, tc := range assistant.ToolCalls {
			messages = append(messages, dispatchToolCall(ctx, tc, searcher, logSrc, round))
		}
	}

	return "", fmt.Errorf("toolcall: exceeded %d rounds without a final answer", MaxRounds)
}

func dispatchToolCall(ctx context.Context, tc toolCallPart, searcher *search.TavilyClient, logSrc string, round int) chatMsg {
	reply := chatMsg{Role: "tool", ToolCallID: tc.ID}

	if tc.Function.Name != "web_search" {
		reply.Content = fmt.Sprintf("(error) unknown tool: %s", tc.Function.Name)
		return reply
	}

	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		reply.Content = fmt.Sprintf("(error) bad tool arguments: %v", err)
		return reply
	}

	fmt.Printf("[%s-toolloop] search[%d]: %q\n", logSrc, round, args.Query)
	result, err := searcher.Search(ctx, args.Query, search.Opts{
		MaxResults:    MaxResultsPerQry,
		IncludeAnswer: true,
		SearchDepth:   "basic",
	})
	if err != nil {
		reply.Content = fmt.Sprintf("(error) search failed: %v", err)
		return reply
	}
	reply.Content = result.FormatForLLM()
	return reply
}
